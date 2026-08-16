package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
)

type proxyAPIState struct{ mu sync.Mutex }

var proxyAPIStates sync.Map

func proxyAPIStateFor(m *Manager) *proxyAPIState {
	state := &proxyAPIState{}
	actual, _ := proxyAPIStates.LoadOrStore(m, state)
	return actual.(*proxyAPIState)
}

type proxyValidationError struct{ err error }

func (e *proxyValidationError) Error() string { return e.err.Error() }
func (e *proxyValidationError) Unwrap() error { return e.err }

type proxyConflictError struct{ err error }

func (e *proxyConflictError) Error() string { return e.err.Error() }
func (e *proxyConflictError) Unwrap() error { return e.err }

type proxyNotFoundError struct{ err error }

func (e *proxyNotFoundError) Error() string { return e.err.Error() }
func (e *proxyNotFoundError) Unwrap() error { return e.err }

func proxyConfigPath(m *Manager) string { return filepath.Join(m.workDir, "proxy.json") }

func (m *Manager) LoadProxyConfig() error {
	s := proxyAPIStateFor(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	return m.loadProxyConfigLocked()
}

func (m *Manager) loadProxyConfigLocked() error {
	p := proxyFor(m)
	blob, err := os.ReadFile(proxyConfigPath(m))
	if os.IsNotExist(err) {
		cfg := defaultProxyConfig()
		if err := m.prepareProxyConfig(&cfg); err != nil {
			return err
		}
		if err := m.saveProxyConfig(cfg); err != nil {
			return fmt.Errorf("save default proxy config: %w", err)
		}
		p.replaceConfig(cfg)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read proxy config: %w", err)
	}

	var cfg ProxyConfig
	if err := decodeProxyJSON(strings.NewReader(string(blob)), &cfg); err != nil {
		return fmt.Errorf("parse proxy config: %w", err)
	}
	original := cloneProxyConfig(cfg)
	if err := m.prepareProxyConfig(&cfg); err != nil {
		return err
	}
	if !reflect.DeepEqual(original, cfg) {
		if err := m.saveProxyConfig(cfg); err != nil {
			return fmt.Errorf("save normalized proxy config: %w", err)
		}
	}

	running := proxyRunning(p)
	if running {
		p.stop()
	}
	p.replaceConfig(cfg)
	if running && cfg.Mode == EntryModeUnified {
		if err := p.start(cfg); err != nil {
			return fmt.Errorf("start unified proxy: %w", err)
		}
	}
	return nil
}

func (m *Manager) prepareProxyConfig(cfg *ProxyConfig) error {
	if cfg == nil {
		return &proxyValidationError{errors.New("proxy config is required")}
	}
	if cfg.Mode == "" {
		cfg.Mode = EntryModePerTunnel
	}
	cfg.ListenAddr = strings.TrimSpace(cfg.ListenAddr)
	if cfg.MaxConnUser == 0 {
		cfg.MaxConnUser = 256
	}
	if cfg.MaxConnTunnel == 0 {
		cfg.MaxConnTunnel = 128
	}
	for i := range cfg.Users {
		if cfg.Users[i].Strategy == "" {
			cfg.Users[i].Strategy = ProxyStrategyRoundRobin
		}
		if cfg.Users[i].Strategy == ProxyStrategyTime && cfg.Users[i].IntervalSeconds <= 0 {
			cfg.Users[i].IntervalSeconds = 60
		}
	}
	if cfg.Port != 0 && (cfg.Port < 1 || cfg.Port > 65535) {
		return &proxyValidationError{errors.New("proxy port must be between 1 and 65535")}
	}
	if cfg.Mode == EntryModeUnified && cfg.Port == 0 {
		port, err := freeRandomPort(m.tunnelPorts())
		if err != nil {
			return fmt.Errorf("allocate unified proxy port: %w", err)
		}
		cfg.Port = port
	}

	tunnels := m.tunnelMap()
	if err := validateProxyOwnership(*cfg, tunnels); err != nil {
		return err
	}
	if err := validateProxyConfig(*cfg, tunnels); err != nil {
		return &proxyValidationError{err}
	}
	if cfg.Mode == EntryModeUnified {
		for _, t := range tunnels {
			if t.Port == cfg.Port && t.Port > 0 {
				return &proxyConflictError{fmt.Errorf("unified proxy port %d conflicts with tunnel %d", cfg.Port, t.Slot)}
			}
		}
	}
	return nil
}

func (m *Manager) tunnelMap() map[int]*Tunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[int]*Tunnel, len(m.tunnels))
	for slot, t := range m.tunnels {
		out[slot] = t
	}
	return out
}

func (m *Manager) tunnelPorts() map[int]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[int]bool, len(m.tunnels))
	for _, t := range m.tunnels {
		if t.Port > 0 {
			out[t.Port] = true
		}
	}
	return out
}

func validateProxyOwnership(cfg ProxyConfig, tunnels map[int]*Tunnel) error {
	owners := make(map[int]string)
	for _, u := range cfg.Users {
		seen := make(map[int]bool, len(u.TunnelSlots))
		for _, slot := range u.TunnelSlots {
			if slot <= 0 {
				return &proxyValidationError{fmt.Errorf("user %q references invalid tunnel %d", u.User, slot)}
			}
			if _, ok := tunnels[slot]; !ok {
				return &proxyValidationError{fmt.Errorf("user %q references unknown tunnel %d", u.User, slot)}
			}
			if seen[slot] {
				return &proxyValidationError{fmt.Errorf("user %q references tunnel %d more than once", u.User, slot)}
			}
			seen[slot] = true
			if owner, ok := owners[slot]; ok && owner != u.User {
				return &proxyConflictError{fmt.Errorf("tunnel %d already belongs to user %q", slot, owner)}
			}
			owners[slot] = u.User
		}
	}
	return nil
}

func (m *Manager) saveProxyConfig(cfg ProxyConfig) error {
	blob, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := proxyConfigPath(m)
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(blob, '\n'), 0600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

func proxyRunning(p *unifiedProxy) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ln != nil
}

func (m *Manager) ProxyConfig() ProxyConfig {
	s := proxyAPIStateFor(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	return proxyFor(m).Config()
}

func (m *Manager) StartProxy() error {
	s := proxyAPIStateFor(m)
	s.mu.Lock()
	defer s.mu.Unlock()

	p := proxyFor(m)
	cfg := p.Config()
	original := cloneProxyConfig(cfg)
	if err := m.prepareProxyConfig(&cfg); err != nil {
		return err
	}
	if !reflect.DeepEqual(original, cfg) {
		if err := m.saveProxyConfig(cfg); err != nil {
			return fmt.Errorf("save proxy config: %w", err)
		}
	}
	if cfg.Mode != EntryModeUnified {
		p.stop()
		p.replaceConfig(cfg)
		return nil
	}
	if err := p.start(cfg); err != nil {
		return fmt.Errorf("start unified proxy: %w", err)
	}
	p.replaceConfig(cfg)
	return nil
}

func (m *Manager) StopProxy() {
	s := proxyAPIStateFor(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	proxyFor(m).stop()
}

func (m *Manager) SetProxyConfig(cfg ProxyConfig) error {
	s := proxyAPIStateFor(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	return m.setProxyConfigLocked(cfg)
}

func (m *Manager) setProxyConfigLocked(cfg ProxyConfig) error {
	if err := m.prepareProxyConfig(&cfg); err != nil {
		return err
	}
	p := proxyFor(m)
	old := p.Config()
	if err := m.saveProxyConfig(cfg); err != nil {
		return fmt.Errorf("save proxy config: %w", err)
	}

	p.stop()
	p.replaceConfig(cfg)
	if cfg.Mode == EntryModeUnified {
		if err := p.start(cfg); err != nil {
			p.replaceConfig(old)
			if old.Mode == EntryModeUnified {
				_ = p.start(old)
			}
			if restoreErr := m.saveProxyConfig(old); restoreErr != nil {
				return fmt.Errorf("start unified proxy: %w (restore config: %v)", err, restoreErr)
			}
			return fmt.Errorf("start unified proxy: %w", err)
		}
	}
	return nil
}

func (m *Manager) UpsertProxyUser(u ProxyUser) error {
	s := proxyAPIStateFor(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(u.User) == "" {
		return &proxyValidationError{errors.New("proxy user is required")}
	}
	if len(u.TunnelSlots) == 0 {
		return &proxyValidationError{errors.New("at least one tunnel is required")}
	}
	cfg := proxyFor(m).Config()
	for i := range cfg.Users {
		if cfg.Users[i].User == u.User {
			cfg.Users[i] = u
			return m.setProxyConfigLocked(cfg)
		}
	}
	cfg.Users = append(cfg.Users, u)
	return m.setProxyConfigLocked(cfg)
}

func (m *Manager) DeleteProxyUser(user string) error {
	s := proxyAPIStateFor(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(user) == "" {
		return &proxyValidationError{errors.New("proxy user is required")}
	}
	cfg := proxyFor(m).Config()
	users := make([]ProxyUser, 0, len(cfg.Users))
	found := false
	for _, u := range cfg.Users {
		if u.User == user {
			found = true
			continue
		}
		users = append(users, u)
	}
	if !found {
		return &proxyNotFoundError{fmt.Errorf("proxy user %q not found", user)}
	}
	cfg.Users = users
	return m.setProxyConfigLocked(cfg)
}

func (m *Manager) ReconcileProxy() {
	s := proxyAPIStateFor(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	p := proxyFor(m)
	cfg := p.Config()
	existing := m.tunnelMap()
	users := make([]ProxyUser, 0, len(cfg.Users))
	changed := false
	for _, u := range cfg.Users {
		slots := make([]int, 0, len(u.TunnelSlots))
		for _, slot := range u.TunnelSlots {
			if _, ok := existing[slot]; ok {
				slots = append(slots, slot)
			} else {
				changed = true
			}
		}
		if len(slots) == 0 {
			changed = true
			continue
		}
		if !reflect.DeepEqual(slots, u.TunnelSlots) {
			changed = true
		}
		u.TunnelSlots = slots
		users = append(users, u)
	}
	if !changed {
		return
	}
	cfg.Users = users
	p.replaceConfig(cfg)
	if err := m.saveProxyConfig(cfg); err != nil {
		log.Printf("save proxy config: %v", err)
	}
}

func decodeProxyJSON(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func proxyHTTPStatus(err error) int {
	var conflict *proxyConflictError
	if errors.As(err, &conflict) {
		return http.StatusConflict
	}
	var notFound *proxyNotFoundError
	if errors.As(err, &notFound) {
		return http.StatusNotFound
	}
	var validation *proxyValidationError
	if errors.As(err, &validation) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func writeProxyError(w http.ResponseWriter, err error) {
	writeJSON(w, proxyHTTPStatus(err), map[string]string{"error": err.Error()})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

func apiProxy(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, m.ProxyConfig())
		case http.MethodPost:
			var cfg ProxyConfig
			if err := decodeProxyJSON(r.Body, &cfg); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
				return
			}
			if err := m.SetProxyConfig(cfg); err != nil {
				writeProxyError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, m.ProxyConfig())
		default:
			methodNotAllowed(w, "GET, POST")
		}
	}
}

func apiProxyUser(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		switch r.Method {
		case http.MethodGet:
			cfg := m.ProxyConfig()
			if user == "" {
				users := cfg.Users
				if users == nil {
					users = []ProxyUser{}
				}
				writeJSON(w, http.StatusOK, users)
				return
			}
			for _, u := range cfg.Users {
				if u.User == user {
					writeJSON(w, http.StatusOK, u)
					return
				}
			}
			writeProxyError(w, &proxyNotFoundError{fmt.Errorf("proxy user %q not found", user)})
		case http.MethodPost, http.MethodPut:
			var in ProxyUser
			if err := decodeProxyJSON(r.Body, &in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
				return
			}
			if err := m.UpsertProxyUser(in); err != nil {
				writeProxyError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, m.ProxyConfig())
		case http.MethodDelete:
			if strings.TrimSpace(user) == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user query parameter is required"})
				return
			}
			if err := m.DeleteProxyUser(user); err != nil {
				writeProxyError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, m.ProxyConfig())
		default:
			methodNotAllowed(w, "GET, POST, PUT, DELETE")
		}
	}
}
