package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const httpConnectEstablished = "HTTP/1.1 200 Connection Established\r\n\r\n"

func (p *unifiedProxy) serveHTTP(client net.Conn, br *bufio.Reader) {
	for {
		_ = client.SetDeadline(time.Now().Add(30 * time.Second))
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}

		keep := httpWantsKeepAlive(req)
		cred, err := p.authenticateHTTP(req)
		if err != nil {
			_ = req.Body.Close()
			_ = writeHTTPProxyStatus(client, http.StatusProxyAuthRequired, "Proxy Authentication Required",
				"Proxy-Authenticate", `Basic realm="fanout"`)
			return
		}

		if req.Method == http.MethodConnect {
			_ = req.Body.Close()
			p.serveHTTPConnect(client, br, cred, req)
			return
		}
		if !httpMethodAllowed(req.Method) {
			_ = req.Body.Close()
			_ = writeHTTPProxyStatus(client, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}

		err = p.serveHTTPForward(client, cred, req)
		_ = req.Body.Close()
		if err != nil || !keep {
			return
		}
	}
}

func (p *unifiedProxy) authenticateHTTP(req *http.Request) (SocksCred, error) {
	if p.loopbackOnly() {
		return SocksCred{}, nil
	}
	user, pass, ok := proxyBasicCreds(req.Header.Get("Proxy-Authorization"))
	if !ok {
		return SocksCred{}, errors.New("proxy authentication required")
	}
	cred := SocksCred{User: user, Pass: pass}
	if err := validateCred(cred); err != nil {
		return SocksCred{}, err
	}
	if !p.authenticate(cred) {
		return SocksCred{}, errors.New("invalid credentials")
	}
	return cred, nil
}

func proxyBasicCreds(header string) (string, string, bool) {
	const prefix = "Basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	raw := strings.TrimSpace(header[len(prefix):])
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(raw)
		if err != nil {
			return "", "", false
		}
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", "", false
	}
	return user, pass, true
}

func (p *unifiedProxy) serveHTTPConnect(client net.Conn, br *bufio.Reader, cred SocksCred, req *http.Request) {
	addr, err := httpTargetAddr(req)
	if err != nil {
		code, text := httpStatusForTarget(err)
		_ = writeHTTPProxyStatus(client, code, text)
		return
	}
	remote, err := p.dial(cred, addr)
	if err != nil {
		_ = writeHTTPProxyStatus(client, http.StatusBadGateway, "Bad Gateway")
		return
	}
	defer remote.Close()
	if _, err := io.WriteString(client, httpConnectEstablished); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	_ = remote.SetDeadline(time.Time{})
	relay(&bufferedConn{Conn: client, r: br}, remote)
}

func (p *unifiedProxy) serveHTTPForward(client net.Conn, cred SocksCred, req *http.Request) error {
	addr, err := httpTargetAddr(req)
	if err != nil {
		code, text := httpStatusForTarget(err)
		_ = writeHTTPProxyStatus(client, code, text)
		return err
	}
	remote, err := p.dial(cred, addr)
	if err != nil {
		_ = writeHTTPProxyStatus(client, http.StatusBadGateway, "Bad Gateway")
		return err
	}
	defer remote.Close()

	stripHopHeaders(req.Header)
	req.RequestURI = ""
	req.Close = false
	_ = client.SetDeadline(time.Time{})
	_ = remote.SetDeadline(time.Time{})
	if err := req.Write(remote); err != nil {
		return err
	}

	resp, err := http.ReadResponse(bufio.NewReader(remote), req)
	if err != nil {
		_ = writeHTTPProxyStatus(client, http.StatusBadGateway, "Bad Gateway")
		return err
	}
	defer resp.Body.Close()
	stripHopHeaders(resp.Header)
	return resp.Write(client)
}

func httpTargetAddr(req *http.Request) (string, error) {
	defaultPort := "80"
	host := ""
	if req.URL != nil {
		host = req.URL.Host
		if strings.EqualFold(req.URL.Scheme, "https") {
			defaultPort = "443"
		}
	}
	if req.Method == http.MethodConnect {
		defaultPort = "443"
	}
	if host == "" {
		host = req.Host
	}
	return httpAuthorityAddr(host, defaultPort)
}

func httpAuthorityAddr(host, defaultPort string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", errors.New("missing host")
	}
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		h, port = host, defaultPort
	}
	if strings.TrimSpace(h) == "" {
		return "", errors.New("missing host")
	}
	if port == "" {
		port = defaultPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("invalid port %q", port)
	}
	if ip := net.ParseIP(h); ip != nil && ip.To4() == nil {
		return "", errIPv6NotSupported
	}
	return net.JoinHostPort(h, port), nil
}

func httpStatusForTarget(err error) (int, string) {
	if errors.Is(err, errIPv6NotSupported) {
		return http.StatusForbidden, "Forbidden"
	}
	return http.StatusBadRequest, "Bad Request"
}

func httpMethodAllowed(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

func httpWantsKeepAlive(req *http.Request) bool {
	if strings.EqualFold(req.Header.Get("Proxy-Connection"), "close") ||
		strings.EqualFold(req.Header.Get("Connection"), "close") {
		return false
	}
	if req.ProtoAtLeast(1, 1) {
		return true
	}
	return strings.EqualFold(req.Header.Get("Proxy-Connection"), "keep-alive") ||
		strings.EqualFold(req.Header.Get("Connection"), "keep-alive")
}

func stripHopHeaders(h http.Header) {
	for _, extra := range h.Values("Connection") {
		for _, name := range strings.Split(extra, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Proxy-Connection",
		"TE",
		"Trailer",
		"Upgrade",
	} {
		h.Del(name)
	}
}

func writeHTTPProxyStatus(w io.Writer, status int, text string, headers ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, text)
	for i := 0; i+1 < len(headers); i += 2 {
		fmt.Fprintf(&b, "%s: %s\r\n", headers[i], headers[i+1])
	}
	b.WriteString("Connection: close\r\n\r\n")
	_, err := io.WriteString(w, b.String())
	return err
}
