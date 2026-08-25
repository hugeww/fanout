package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func basicProxyAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func startUnifiedServe(t *testing.T, p *unifiedProxy) (net.Conn, func()) {
	t.Helper()
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		p.serve(server)
		close(done)
	}()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	return client, func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("serve did not return")
		}
	}
}

func TestProxyBasicCreds(t *testing.T) {
	user, pass, ok := proxyBasicCreds(basicProxyAuth("alice", "secret"))
	if !ok || user != "alice" || pass != "secret" {
		t.Fatalf("got %q %q %v", user, pass, ok)
	}
	if _, _, ok := proxyBasicCreds(""); ok {
		t.Fatal("empty header must not authenticate")
	}
	if _, _, ok := proxyBasicCreds("Bearer abc"); ok {
		t.Fatal("non-basic header must not authenticate")
	}
}

func connectReq(host string) *http.Request {
	return &http.Request{
		Method: http.MethodConnect,
		Host:   host,
		URL:    &url.URL{Host: host},
	}
}

func TestHTTPTargetAddr(t *testing.T) {
	got, err := httpTargetAddr(connectReq("example.com:443"))
	if err != nil || got != "example.com:443" {
		t.Fatalf("CONNECT: got %q %v", got, err)
	}

	got, err = httpTargetAddr(connectReq("example.com"))
	if err != nil || got != "example.com:443" {
		t.Fatalf("CONNECT default port: got %q %v", got, err)
	}

	abs, err := http.NewRequest(http.MethodGet, "http://example.com/foo", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err = httpTargetAddr(abs)
	if err != nil || got != "example.com:80" {
		t.Fatalf("GET absolute: got %q %v", got, err)
	}

	origin, err := http.NewRequest(http.MethodGet, "http://ignored/", nil)
	if err != nil {
		t.Fatal(err)
	}
	origin.URL.Host = ""
	origin.Host = "origin.example:8080"
	got, err = httpTargetAddr(origin)
	if err != nil || got != "origin.example:8080" {
		t.Fatalf("GET origin-form: got %q %v", got, err)
	}

	if _, err := httpTargetAddr(connectReq("[::1]:443")); err == nil {
		t.Fatal("IPv6 CONNECT must be rejected")
	}
}

func TestHTTPURL(t *testing.T) {
	got := httpURL("1.2.3.4", 20000, SocksCred{User: "u", Pass: "p"})
	if got != "http://u:p@1.2.3.4:20000" {
		t.Fatalf("带凭据 URL 不对: %s", got)
	}
	got = httpURL("1.2.3.4", 20000, SocksCred{})
	if got != "http://1.2.3.4:20000" {
		t.Fatalf("无凭据 URL 不对: %s", got)
	}
	got = httpsURL("1.2.3.4", 20000, SocksCred{User: "u", Pass: "p"})
	if got != "https://u:p@1.2.3.4:20000" {
		t.Fatalf("HTTPS 带凭据 URL 不对: %s", got)
	}
}

func TestUnifiedProxyTLSConnect(t *testing.T) {
	p, _ := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
	}, 10, 10)

	site, remote := net.Pipe()
	defer site.Close()
	dialed := make(chan string, 1)
	p.dialTunnel = func(_ *Tunnel, addr string) (net.Conn, error) {
		dialed <- addr
		return remote, nil
	}

	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		p.serve(server)
		close(done)
	}()
	defer func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("serve did not return")
		}
	}()

	_ = client.SetDeadline(time.Now().Add(8 * time.Second))
	tlsClient := tls.Client(client, &tls.Config{InsecureSkipVerify: true})
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("TLS handshake to unified entry: %v", err)
	}
	req := "CONNECT imap.gmail.com:993 HTTP/1.1\r\nHost: imap.gmail.com:993\r\nProxy-Authorization: " +
		basicProxyAuth("alice", "secret") + "\r\n\r\n"
	if _, err := io.WriteString(tlsClient, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsClient), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	select {
	case addr := <-dialed:
		if addr != "imap.gmail.com:993" {
			t.Fatalf("dialed %q", addr)
		}
	default:
		t.Fatal("TLS CONNECT did not dial")
	}
}

func TestUnifiedProxyHTTPConnectRequiresAuth(t *testing.T) {
	p, _ := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
	}, 10, 10)
	p.dialTunnel = func(*Tunnel, string) (net.Conn, error) {
		t.Fatal("unauthenticated CONNECT must not dial")
		return nil, io.EOF
	}

	client, done := startUnifiedServe(t, p)
	defer done()
	if _, err := io.WriteString(client, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("status %d, want 407", resp.StatusCode)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Proxy-Authenticate")), "basic") {
		t.Fatalf("missing Proxy-Authenticate: %q", resp.Header.Get("Proxy-Authenticate"))
	}
}

func TestUnifiedProxyHTTPConnectRoutesAndRelays(t *testing.T) {
	p, _ := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
	}, 10, 10)

	site, remote := net.Pipe()
	defer site.Close()
	dialed := make(chan string, 1)
	p.dialTunnel = func(tunnel *Tunnel, addr string) (net.Conn, error) {
		if tunnel.Slot != 1 {
			t.Errorf("unexpected tunnel %d", tunnel.Slot)
		}
		dialed <- addr
		return remote, nil
	}

	client, done := startUnifiedServe(t, p)
	defer done()
	req := "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: " +
		basicProxyAuth("alice", "secret") + "\r\n\r\n"
	if _, err := io.WriteString(client, req); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(client)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	select {
	case addr := <-dialed:
		if addr != "example.com:443" {
			t.Fatalf("dialed %q", addr)
		}
	default:
		t.Fatal("CONNECT did not dial a tunnel")
	}

	if _, err := io.WriteString(client, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = site.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(site, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("relayed %q", buf)
	}
	if _, err := site.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "pong" {
		t.Fatalf("return relay %q", buf)
	}
}

func TestUnifiedProxyHTTPConnectRejectsBadPassword(t *testing.T) {
	p, _ := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
	}, 10, 10)
	p.dialTunnel = func(*Tunnel, string) (net.Conn, error) {
		t.Fatal("bad password must not dial")
		return nil, io.EOF
	}

	client, done := startUnifiedServe(t, p)
	defer done()
	req := "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: " +
		basicProxyAuth("alice", "wrong") + "\r\n\r\n"
	if _, err := io.WriteString(client, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("status %d, want 407", resp.StatusCode)
	}
}

func TestUnifiedProxyHTTPForwardAbsoluteURI(t *testing.T) {
	p, _ := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
	}, 10, 10)

	site, remote := net.Pipe()
	defer site.Close()
	gotOrigin := make(chan *http.Request, 1)
	go func() {
		_ = site.SetDeadline(time.Now().Add(3 * time.Second))
		req, err := http.ReadRequest(bufio.NewReader(site))
		if err != nil {
			close(gotOrigin)
			return
		}
		gotOrigin <- req
		_, _ = io.WriteString(site, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")
	}()
	p.dialTunnel = func(_ *Tunnel, addr string) (net.Conn, error) {
		if addr != "example.com:80" {
			t.Errorf("forward addr %q", addr)
		}
		return remote, nil
	}

	client, done := startUnifiedServe(t, p)
	defer done()
	req := "GET http://example.com/foo HTTP/1.1\r\nHost: example.com\r\nProxy-Authorization: " +
		basicProxyAuth("alice", "secret") + "\r\nProxy-Connection: close\r\n\r\n"
	if _, err := io.WriteString(client, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	originReq, ok := <-gotOrigin
	if !ok || originReq == nil {
		t.Fatal("origin did not receive the forwarded request")
	}
	if originReq.URL.String() != "/foo" {
		t.Fatalf("origin-form URI %q", originReq.URL.String())
	}
	if originReq.Header.Get("Proxy-Authorization") != "" {
		t.Fatal("Proxy-Authorization must not be forwarded")
	}
}

func TestUnifiedProxyHTTPLoopbackSkipsAuth(t *testing.T) {
	p, _ := newUnifiedProxyForTest(nil, 10, 10)
	p.replaceConfig(ProxyConfig{Mode: EntryModeUnified, Port: 19090, ListenAddr: "127.0.0.1"})

	site, remote := net.Pipe()
	defer site.Close()
	p.dialTunnel = func(tunnel *Tunnel, addr string) (net.Conn, error) {
		if addr != "example.com:443" {
			t.Errorf("dialed %q", addr)
		}
		return remote, nil
	}

	client, done := startUnifiedServe(t, p)
	defer done()
	if _, err := io.WriteString(client, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback HTTP CONNECT status %d", resp.StatusCode)
	}
}

func TestUnifiedProxyServeStillAcceptsSOCKS5(t *testing.T) {
	p, _ := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
	}, 10, 10)

	site, remote := net.Pipe()
	defer site.Close()
	dialed := make(chan string, 1)
	p.dialTunnel = func(_ *Tunnel, addr string) (net.Conn, error) {
		dialed <- addr
		return remote, nil
	}

	client, done := startUnifiedServe(t, p)
	defer done()

	if _, err := client.Write([]byte{socksVer5, 1, authUserPass}); err != nil {
		t.Fatal(err)
	}
	sel := make([]byte, 2)
	if _, err := io.ReadFull(client, sel); err != nil {
		t.Fatal(err)
	}
	if sel[1] != authUserPass {
		t.Fatalf("method %v", sel)
	}
	auth := []byte{authSubVer, 5}
	auth = append(auth, "alice"...)
	auth = append(auth, 6)
	auth = append(auth, "secret"...)
	if _, err := client.Write(auth); err != nil {
		t.Fatal(err)
	}
	authResp := make([]byte, 2)
	if _, err := io.ReadFull(client, authResp); err != nil {
		t.Fatal(err)
	}
	if authResp[1] != 0 {
		t.Fatalf("auth status %v", authResp)
	}

	req := []byte{socksVer5, cmdConnect, 0x00, atypDomain, byte(len("example.com"))}
	req = append(req, "example.com"...)
	req = append(req, 0x01, 0xbb)
	if _, err := client.Write(req); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatal(err)
	}
	if reply[1] != repSuccess {
		t.Fatalf("SOCKS reply %v", reply)
	}
	select {
	case addr := <-dialed:
		if addr != "example.com:443" {
			t.Fatalf("SOCKS dialed %q", addr)
		}
	default:
		t.Fatal("SOCKS CONNECT did not dial a tunnel")
	}
}
