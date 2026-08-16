package main

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type unifiedProxyTestConn struct {
	closed chan struct{}
	once   sync.Once
}

func newUnifiedProxyTestConn() *unifiedProxyTestConn {
	return &unifiedProxyTestConn{closed: make(chan struct{})}
}

func (c *unifiedProxyTestConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *unifiedProxyTestConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *unifiedProxyTestConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *unifiedProxyTestConn) LocalAddr() net.Addr              { return testAddr("local") }
func (c *unifiedProxyTestConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (c *unifiedProxyTestConn) SetDeadline(time.Time) error      { return nil }
func (c *unifiedProxyTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *unifiedProxyTestConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

func newUnifiedProxyForTest(users []ProxyUser, maxUser, maxTunnel int) (*unifiedProxy, map[int]*Tunnel) {
	tunnels := map[int]*Tunnel{}
	for slot := 1; slot <= 4; slot++ {
		tunnels[slot] = &Tunnel{Slot: slot, Status: "up"}
	}
	manager := &Manager{tunnels: tunnels}
	p := &unifiedProxy{
		manager: manager,
		cfg: ProxyConfig{
			Mode:          EntryModeUnified,
			Port:          19090,
			Users:         users,
			MaxConnUser:   maxUser,
			MaxConnTunnel: maxTunnel,
		},
		cursors:     make(map[string]uint64),
		userConns:   make(map[string]int),
		tunnelConns: make(map[int]int),
	}
	return p, tunnels
}

func TestValidateProxyConfigRejectsSharedAndDuplicateTunnel(t *testing.T) {
	tunnels := map[int]*Tunnel{
		1: {Slot: 1},
		2: {Slot: 2},
	}
	base := ProxyConfig{
		Mode: EntryModeUnified,
		Port: 19090,
		Users: []ProxyUser{
			{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
			{User: "bob", Pass: "secret", TunnelSlots: []int{2}},
		},
	}
	if err := validateProxyConfig(base, tunnels); err != nil {
		t.Fatalf("valid isolated users rejected: %v", err)
	}

	shared := base
	shared.Users = append([]ProxyUser(nil), base.Users...)
	shared.Users[1].TunnelSlots = []int{1}
	if err := validateProxyConfig(shared, tunnels); err == nil {
		t.Fatal("shared tunnel must be rejected")
	}

	duplicate := base
	duplicate.Users = append([]ProxyUser(nil), base.Users...)
	duplicate.Users[0].TunnelSlots = []int{1, 1}
	if err := validateProxyConfig(duplicate, tunnels); err == nil {
		t.Fatal("duplicate tunnel in one user must be rejected")
	}
}

func TestUnifiedProxyHandshakeRequiresCredentials(t *testing.T) {
	p, _ := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
	}, 10, 10)

	server, client := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		_, err := p.handshake(server)
		_ = server.Close()
		done <- err
	}()

	if _, err := client.Write([]byte{socksVer5, 1, authNone}); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 2)
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatal(err)
	}
	if response[1] != authNoAccept {
		t.Fatalf("expected auth rejection, got %#x", response[1])
	}
	if err := <-done; err == nil {
		t.Fatal("authNone must not authenticate unified entry")
	}
}

func TestUnifiedProxyRoutesByUserAndRoundRobin(t *testing.T) {
	p, _ := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1, 3}, Strategy: ProxyStrategyRoundRobin},
		{User: "bob", Pass: "secret", TunnelSlots: []int{2}, Strategy: ProxyStrategyRoundRobin},
	}, 10, 10)

	var mu sync.Mutex
	var calls []int
	p.dialTunnel = func(tunnel *Tunnel, _ string) (net.Conn, error) {
		mu.Lock()
		calls = append(calls, tunnel.Slot)
		mu.Unlock()
		return newUnifiedProxyTestConn(), nil
	}

	for _, want := range []int{1, 3, 1} {
		conn, err := p.dial(SocksCred{User: "alice", Pass: "secret"}, "example.com:443")
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		if got := calls[len(calls)-1]; got != want {
			t.Fatalf("alice route %d: got tunnel %d, want %d", len(calls), got, want)
		}
	}
	conn, err := p.dial(SocksCred{User: "bob", Pass: "secret"}, "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if got := calls[len(calls)-1]; got != 2 {
		t.Fatalf("bob must use its private tunnel, got %d", got)
	}
}

func TestUnifiedProxyChecksUpAndDoesNotUseHostFallback(t *testing.T) {
	p, tunnels := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
	}, 10, 10)
	tunnels[1].Status = "failed"

	var calls int
	p.dialTunnel = func(*Tunnel, string) (net.Conn, error) {
		calls++
		return newUnifiedProxyTestConn(), nil
	}
	if _, err := p.dial(SocksCred{User: "alice", Pass: "secret"}, "example.com:443"); err == nil {
		t.Fatal("a non-up tunnel must fail")
	}
	if calls != 0 {
		t.Fatalf("non-up tunnel was dialled %d times", calls)
	}

	tunnels[1].Status = "up"
	p.dialTunnel = func(*Tunnel, string) (net.Conn, error) {
		calls++
		return nil, errors.New("tunnel dial failed")
	}
	if _, err := p.dial(SocksCred{User: "alice", Pass: "secret"}, "example.com:443"); err == nil {
		t.Fatal("tunnel dial failure must be returned")
	}
	if calls != 1 {
		t.Fatalf("expected one tunnel attempt, got %d", calls)
	}
}

func TestUnifiedProxyConnectionIsolationAndLimits(t *testing.T) {
	p, _ := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
		{User: "bob", Pass: "secret", TunnelSlots: []int{2}},
	}, 1, 1)

	p.dialTunnel = func(tunnel *Tunnel, _ string) (net.Conn, error) {
		return newUnifiedProxyTestConn(), nil
	}
	alice, err := p.dial(SocksCred{User: "alice", Pass: "secret"}, "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()

	if _, err := p.dial(SocksCred{User: "alice", Pass: "secret"}, "example.com:443"); err == nil {
		t.Fatal("second alice connection should hit user limit")
	}

	bob, err := p.dial(SocksCred{User: "bob", Pass: "secret"}, "example.com:443")
	if err != nil {
		t.Fatalf("bob should not be blocked by alice: %v", err)
	}
	if err := bob.Close(); err != nil {
		t.Fatal(err)
	}
	if got := p.userConns["bob"]; got != 0 {
		t.Fatalf("bob counter not released: %d", got)
	}

	if err := alice.Close(); err != nil {
		t.Fatal(err)
	}
	if len(p.userConns) != 0 || len(p.tunnelConns) != 0 {
		t.Fatalf("connection leases leaked: users=%v tunnels=%v", p.userConns, p.tunnelConns)
	}
}

func TestCountedConnCloseIsIdempotent(t *testing.T) {
	var released int
	conn := &countedConn{
		Conn:    newUnifiedProxyTestConn(),
		release: func() { released++ },
	}
	_ = conn.Close()
	_ = conn.Close()
	if released != 1 {
		t.Fatalf("release called %d times", released)
	}
}

func TestUnifiedProxyConcurrentUsersRemainIsolated(t *testing.T) {
	p, _ := newUnifiedProxyForTest([]ProxyUser{
		{User: "alice", Pass: "secret", TunnelSlots: []int{1}},
		{User: "bob", Pass: "secret", TunnelSlots: []int{2}},
	}, 0, 0)

	var mu sync.Mutex
	counts := map[int]int{}
	p.dialTunnel = func(tunnel *Tunnel, _ string) (net.Conn, error) {
		mu.Lock()
		counts[tunnel.Slot]++
		mu.Unlock()
		return newUnifiedProxyTestConn(), nil
	}

	var wg sync.WaitGroup
	for _, user := range []string{"alice", "bob"} {
		user := user
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				conn, err := p.dial(SocksCred{User: user, Pass: "secret"}, "example.com:443")
				if err != nil {
					t.Errorf("%s dial failed: %v", user, err)
					return
				}
				_ = conn.Close()
			}
		}()
	}
	wg.Wait()

	if counts[1] != 100 || counts[2] != 100 {
		t.Fatalf("unexpected isolated route counts: %v", counts)
	}
	if len(p.userConns) != 0 || len(p.tunnelConns) != 0 {
		t.Fatalf("concurrent leases leaked: users=%v tunnels=%v", p.userConns, p.tunnelConns)
	}
}
