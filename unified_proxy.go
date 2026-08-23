package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

const (
	EntryModePerTunnel = "per-tunnel"
	EntryModeUnified   = "unified"

	// poolOwner 是免认证汇聚模式（仅本机监听）下所有连接共用的路由归属名，
	// 用于连接计数与轮询游标；它不代表任何真实用户。
	poolOwner = "pool"

	ProxyStrategyRoundRobin = "round-robin"
	ProxyStrategyRandom     = "random"
	ProxyStrategyTime       = "time"
)

type ProxyUser struct {
	User            string `json:"user"`
	Pass            string `json:"pass"`
	Tag             string `json:"tag,omitempty"`
	TunnelSlots     []int  `json:"tunnel_slots"`
	Strategy        string `json:"strategy,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
}

type ProxyConfig struct {
	Mode          string      `json:"mode"`
	Port          int         `json:"port"`
	ListenAddr    string      `json:"listen_addr,omitempty"`
	Users         []ProxyUser `json:"users,omitempty"`
	MaxConnUser   int         `json:"max_conn_user,omitempty"`
	MaxConnTunnel int         `json:"max_conn_tunnel,omitempty"`
}

func defaultProxyConfig() ProxyConfig {
	return ProxyConfig{Mode: EntryModePerTunnel, MaxConnUser: 256, MaxConnTunnel: 128}
}

func cloneProxyConfig(cfg ProxyConfig) ProxyConfig {
	out := cfg
	out.Users = make([]ProxyUser, len(cfg.Users))
	for i, user := range cfg.Users {
		out.Users[i] = user
		out.Users[i].TunnelSlots = append([]int(nil), user.TunnelSlots...)
	}
	return out
}

// validateProxyConfig enforces the routing graph invariant that a Tunnel is
// owned by at most one user. This prevents users from competing for a netns
// and exit address.
func validateProxyConfig(cfg ProxyConfig, tunnels map[int]*Tunnel) error {
	mode := cfg.Mode
	if mode == "" {
		mode = EntryModePerTunnel
	}
	if mode != EntryModePerTunnel && mode != EntryModeUnified {
		return fmt.Errorf("invalid proxy mode %q", mode)
	}
	if mode == EntryModeUnified && (cfg.Port < 1 || cfg.Port > 65535) {
		return errors.New("unified proxy port must be between 1 and 65535")
	}
	if cfg.MaxConnUser < 0 || cfg.MaxConnTunnel < 0 {
		return errors.New("connection limits cannot be negative")
	}

	// 仅本机监听的统一入口免认证并自动汇聚全部隧道，用户绑定不参与路由，
	// 因此不校验用户名密码与隧道归属（切回外网监听后用户重新生效）。
	if mode == EntryModeUnified && loopbackOnly(cfg.ListenAddr) {
		return nil
	}

	owners := make(map[int]string)
	seenUsers := make(map[string]struct{})
	for _, user := range cfg.Users {
		if err := validateCred(SocksCred{User: user.User, Pass: user.Pass}); err != nil {
			return fmt.Errorf("user %q: %w", user.User, err)
		}
		if _, exists := seenUsers[user.User]; exists {
			return fmt.Errorf("duplicate proxy user %q", user.User)
		}
		seenUsers[user.User] = struct{}{}

		strategy := user.Strategy
		if strategy == "" {
			strategy = ProxyStrategyRoundRobin
		}
		switch strategy {
		case ProxyStrategyRoundRobin, ProxyStrategyRandom, ProxyStrategyTime:
		default:
			return fmt.Errorf("user %q has unsupported strategy %q", user.User, strategy)
		}
		if strategy == ProxyStrategyTime && user.IntervalSeconds < 0 {
			return fmt.Errorf("user %q has a negative time interval", user.User)
		}
		if len(user.TunnelSlots) == 0 {
			return fmt.Errorf("user %q has no tunnels", user.User)
		}

		seenSlots := make(map[int]struct{}, len(user.TunnelSlots))
		for _, slot := range user.TunnelSlots {
			if _, duplicate := seenSlots[slot]; duplicate {
				return fmt.Errorf("user %q lists tunnel %d more than once", user.User, slot)
			}
			seenSlots[slot] = struct{}{}
			if _, exists := tunnels[slot]; !exists {
				return fmt.Errorf("user %q references unknown tunnel %d", user.User, slot)
			}
			if owner, exists := owners[slot]; exists && owner != user.User {
				return fmt.Errorf("tunnel %d already belongs to user %q", slot, owner)
			}
			owners[slot] = user.User
		}
	}
	return nil
}

type unifiedProxy struct {
	manager *Manager

	mu  sync.Mutex
	cfg ProxyConfig
	ln  net.Listener

	wg          sync.WaitGroup
	cursors     map[string]uint64
	userConns   map[string]int
	tunnelConns map[int]int

	// Injectable only for tests. Production always dials inside the selected
	// Tunnel network namespace.
	dialTunnel func(*Tunnel, string) (net.Conn, error)
}

var proxyRegistry sync.Map

func proxyFor(m *Manager) *unifiedProxy {
	if p, ok := proxyRegistry.Load(m); ok {
		return p.(*unifiedProxy)
	}
	p := &unifiedProxy{
		manager:     m,
		cfg:         defaultProxyConfig(),
		cursors:     make(map[string]uint64),
		userConns:   make(map[string]int),
		tunnelConns: make(map[int]int),
	}
	actual, _ := proxyRegistry.LoadOrStore(m, p)
	return actual.(*unifiedProxy)
}

func (p *unifiedProxy) Config() ProxyConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneProxyConfig(p.cfg)
}

// replaceConfig is called after validation. Counters are retained because
// reconciliation can happen while existing connections are still alive.
func (p *unifiedProxy) replaceConfig(cfg ProxyConfig) {
	p.mu.Lock()
	p.cfg = cloneProxyConfig(cfg)
	p.cursors = make(map[string]uint64)
	p.mu.Unlock()
}

func (p *unifiedProxy) start(cfg ProxyConfig) error {
	p.mu.Lock()
	if p.ln != nil {
		p.mu.Unlock()
		return nil
	}
	addr := net.JoinHostPort(cfg.ListenAddr, fmt.Sprintf("%d", cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	p.ln = ln
	p.cfg = cloneProxyConfig(cfg)
	// Publish the listener and register the accept loop atomically with
	// respect to stop(), otherwise stop could observe a zero WaitGroup before
	// start() adds its worker.
	p.wg.Add(1)
	p.mu.Unlock()

	go p.acceptLoop(ln)
	log.Printf("unified SOCKS5 listening on %s", addr)
	return nil
}

func (p *unifiedProxy) stop() {
	p.mu.Lock()
	ln := p.ln
	p.ln = nil
	p.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	// The accept loop is counted, so it cannot finish before all workers it
	// creates have also finished.
	p.wg.Wait()
}

func (p *unifiedProxy) acceptLoop(ln net.Listener) {
	defer p.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.serve(conn)
		}()
	}
}

func (p *unifiedProxy) serve(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))

	cred, err := p.handshake(client)
	if err != nil {
		return
	}
	addr, err := socksReadRequest(client)
	if err != nil {
		code := byte(repGenFail)
		if errors.Is(err, errCmdNotSupported) {
			code = repCmdNotSupp
		}
		_ = socksReply(client, code)
		return
	}

	remote, err := p.dial(cred, addr)
	if err != nil {
		// There is deliberately no mother-host fallback here.
		_ = socksReply(client, repHostUnre)
		return
	}
	defer remote.Close()
	if err := socksReply(client, repSuccess); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	_ = remote.SetDeadline(time.Time{})
	relay(client, remote)
}

// handshake authenticates the client. In normal mode it requires RFC 1929
// username/password and authNone is never accepted. When the entry listens
// only on loopback (127.0.0.1) it trusts SSH local forwarding as the
// authentication boundary and accepts authNone without any credentials.
func (p *unifiedProxy) handshake(c net.Conn) (SocksCred, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return SocksCred{}, err
	}
	if head[0] != socksVer5 {
		return SocksCred{}, errors.New("not socks5")
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return SocksCred{}, err
	}
	if p.loopbackOnly() {
		// 仅本机监听：SSH 本地转发就是认证边界，免认证放行，
		// 客户端直接填 socks5://127.0.0.1:<本地端口> 即可。
		if _, err := c.Write([]byte{socksVer5, authNone}); err != nil {
			return SocksCred{}, err
		}
		return SocksCred{}, nil
	}
	if !bytes.Contains(methods, []byte{authUserPass}) {
		_, _ = c.Write([]byte{socksVer5, authNoAccept})
		return SocksCred{}, errors.New("username/password auth required")
	}
	if _, err := c.Write([]byte{socksVer5, authUserPass}); err != nil {
		return SocksCred{}, err
	}

	ver := make([]byte, 1)
	if _, err := io.ReadFull(c, ver); err != nil {
		return SocksCred{}, err
	}
	if ver[0] != authSubVer {
		return SocksCred{}, errors.New("bad auth version")
	}
	user, err := readLenPrefixed(c)
	if err != nil {
		return SocksCred{}, err
	}
	pass, err := readLenPrefixed(c)
	if err != nil {
		return SocksCred{}, err
	}
	cred := SocksCred{User: string(user), Pass: string(pass)}
	if err := validateCred(cred); err != nil {
		_, _ = c.Write([]byte{authSubVer, 0x01})
		return SocksCred{}, err
	}
	if !p.authenticate(cred) {
		_, _ = c.Write([]byte{authSubVer, 0x01})
		return SocksCred{}, errors.New("invalid credentials")
	}
	if _, err := c.Write([]byte{authSubVer, 0x00}); err != nil {
		return SocksCred{}, err
	}
	return cred, nil
}

func (p *unifiedProxy) authenticate(cred SocksCred) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, user := range p.cfg.Users {
		if subtle.ConstantTimeCompare([]byte(user.User), []byte(cred.User)) == 1 &&
			subtle.ConstantTimeCompare([]byte(user.Pass), []byte(cred.Pass)) == 1 {
			return true
		}
	}
	return false
}

// dial selects only from the authenticated user's private slot list. A
// failed attempt may try the next configured slot, but every attempt checks
// that Tunnel is up and dials from that Tunnel's netns.
func (p *unifiedProxy) dial(cred SocksCred, addr string) (net.Conn, error) {
	owner := cred.User
	var slots []int
	if p.loopbackOnly() {
		// 仅本机监听：入口免认证，没有用户名可路由，直接汇聚全部在线隧道。
		owner = poolOwner
		slots = p.pooledSlots()
	} else {
		user, ok := p.user(cred.User)
		if !ok {
			return nil, errors.New("unknown proxy user")
		}
		owner = user.User
		slots = p.candidates(user)
	}

	var lastErr error
	for _, slot := range slots {
		tunnel, exists := p.manager.tunnel(slot)
		if !exists {
			lastErr = fmt.Errorf("tunnel %d is unavailable", slot)
			continue
		}
		if !tunnel.isUp() {
			lastErr = fmt.Errorf("tunnel %d is not up", slot)
			continue
		}
		if !p.acquire(owner, slot) {
			lastErr = fmt.Errorf("tunnel %d connection limit reached", slot)
			continue
		}

		conn, err := p.dialThroughTunnel(tunnel, addr)
		if err == nil && conn != nil {
			return &countedConn{
				Conn: conn,
				release: func() {
					p.release(owner, slot)
				},
			}, nil
		}
		p.release(owner, slot)
		if err == nil {
			err = errors.New("tunnel dial returned a nil connection")
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no available tunnel")
	}
	return nil, lastErr
}

func (p *unifiedProxy) dialThroughTunnel(tunnel *Tunnel, addr string) (net.Conn, error) {
	if p.dialTunnel != nil {
		return p.dialTunnel(tunnel, addr)
	}
	return dialerInNetns(tunnel.nsName())("tcp", addr)
}

// loopbackOnly 报告统一入口是否只监听本机。仅本机时信任 SSH 本地转发作为
// 认证边界：入口免认证，并自动汇聚全部隧道，无需统一用户用户名密码。
func (p *unifiedProxy) loopbackOnly() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return loopbackOnly(p.cfg.ListenAddr)
}

// pooledSlots 返回免认证汇聚模式下全部在线隧道的槽位，按轮询顺序排好。
// 没有用户名可路由，所有请求在在线隧道间轮流分配。
func (p *unifiedProxy) pooledSlots() []int {
	var slots []int
	for _, t := range p.manager.Tunnels() {
		if t.isUp() {
			slots = append(slots, t.Slot)
		}
	}
	if len(slots) < 2 {
		return slots
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.cursors[poolOwner]
	p.cursors[poolOwner] = n + 1
	offset := int(n % uint64(len(slots)))
	ordered := make([]int, 0, len(slots))
	ordered = append(ordered, slots[offset:]...)
	ordered = append(ordered, slots[:offset]...)
	return ordered
}

func (p *unifiedProxy) user(name string) (ProxyUser, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, user := range p.cfg.Users {
		if user.User == name {
			user.TunnelSlots = append([]int(nil), user.TunnelSlots...)
			return user, true
		}
	}
	return ProxyUser{}, false
}

// candidates returns a private copy. Round-robin state is scoped by username
// and protected by p.mu, so concurrent users cannot affect one another.
func (p *unifiedProxy) candidates(user ProxyUser) []int {
	slots := append([]int(nil), user.TunnelSlots...)
	if len(slots) < 2 {
		return slots
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	offset := 0
	switch user.Strategy {
	case ProxyStrategyRandom:
		var b [1]byte
		if _, err := rand.Read(b[:]); err == nil {
			offset = int(b[0]) % len(slots)
		}
	case ProxyStrategyTime:
		interval := user.IntervalSeconds
		if interval <= 0 {
			interval = 60
		}
		offset = int((time.Now().Unix() / int64(interval)) % int64(len(slots)))
	default:
		n := p.cursors[user.User]
		p.cursors[user.User] = n + 1
		offset = int(n % uint64(len(slots)))
	}

	ordered := make([]int, 0, len(slots))
	ordered = append(ordered, slots[offset:]...)
	ordered = append(ordered, slots[:offset]...)
	return ordered
}

// acquire/release form a lease held for exactly the lifetime of the returned
// connection. countedConn makes release idempotent under concurrent Close.
func (p *unifiedProxy) acquire(user string, slot int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg.MaxConnUser > 0 && p.userConns[user] >= p.cfg.MaxConnUser {
		return false
	}
	if p.cfg.MaxConnTunnel > 0 && p.tunnelConns[slot] >= p.cfg.MaxConnTunnel {
		return false
	}
	p.userConns[user]++
	p.tunnelConns[slot]++
	return true
}

func (p *unifiedProxy) release(user string, slot int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := p.userConns[user]; n > 1 {
		p.userConns[user] = n - 1
	} else {
		delete(p.userConns, user)
	}
	if n := p.tunnelConns[slot]; n > 1 {
		p.tunnelConns[slot] = n - 1
	} else {
		delete(p.tunnelConns, slot)
	}
}

type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error {
	if c.release != nil {
		c.once.Do(c.release)
	}
	return c.Conn.Close()
}

func (m *Manager) tunnel(slot int) (*Tunnel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tunnel, ok := m.tunnels[slot]
	return tunnel, ok
}

func (t *Tunnel) isUp() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Status == "up"
}
