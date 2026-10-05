package aiprovider

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The control hook runs after DNS resolution, on the address actually dialled,
// so a hostname that starts resolving to an internal address is still refused.
func TestDialControl(t *testing.T) {
	guard := dialControl(false)
	for _, addr := range []string{
		"127.0.0.1:443", "10.1.2.3:443", "[::1]:443", "169.254.169.254:80",
		"172.16.0.1:443", "192.168.0.1:443", "100.64.0.1:443", "0.0.0.0:443", "224.0.0.1:443",
		"[fe80::1]:443", "[fe80::1%eth0]:443", "[fd00::1]:443", "[::]:443",
		"[::ffff:127.0.0.1]:443", "[::ffff:10.0.0.1]:443", "[::ffff:169.254.169.254]:80",
	} {
		if err := guard("tcp", addr, nil); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("guard(%s) = %v, want ErrBlockedAddress", addr, err)
		}
	}
	if err := guard("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("public address refused: %v", err)
	}
	if err := guard("tcp", "not-an-ip:443", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("unparseable address err = %v", err)
	}
	if err := dialControl(true)("tcp", "127.0.0.1:443", nil); err != nil {
		t.Errorf("allowPrivate still refused: %v", err)
	}
}

func TestNewTransport_HasTimeoutsAndNoProxyFromEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	tr := NewTransport(false)
	if tr.TLSHandshakeTimeout == 0 || tr.ResponseHeaderTimeout == 0 || tr.DialContext == nil {
		t.Fatalf("transport lacks timeouts: %+v", tr)
	}
	if tr.Proxy != nil {
		t.Fatal("an environment proxy would bypass the dial guard")
	}
	if tr.MaxIdleConns != 100 {
		t.Fatalf("MaxIdleConns = %d, want 100", tr.MaxIdleConns)
	}
}

// listenLoopback returns a loopback listener and a counter of accepted
// connections: a refused dial must never get as far as the listener.
func listenLoopback(t *testing.T) (port string, accepted *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, accepted
}

// flipResolver answers with public addresses first and internal ones
// afterwards: the DNS rebinding sequence.
type flipResolver struct {
	calls  atomic.Int32
	first  []string
	second []string
}

func (f *flipResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	ips := f.second
	if f.calls.Add(1) == 1 {
		ips = f.first
	}
	out := make([]net.IPAddr, len(ips))
	for i, s := range ips {
		out[i] = net.IPAddr{IP: net.ParseIP(s)}
	}
	return out, nil
}

// Review Focus 4: the name was public when the provider was saved and points
// at the relay's own loopback when the request is made.
func TestGuardedDial_RefusesRebindAfterValidation(t *testing.T) {
	port, accepted := listenLoopback(t)
	r := &flipResolver{first: []string{"93.184.216.34"}, second: []string{"127.0.0.1"}}
	ctx := context.Background()
	if err := CheckHostPublic(ctx, r, "rebind.example"); err != nil {
		t.Fatalf("save-time check: %v", err)
	}
	tr := newTransport(false, r)
	conn, err := tr.DialContext(ctx, "tcp", net.JoinHostPort("rebind.example", port))
	if err == nil {
		_ = conn.Close()
		t.Fatal("dial to a rebound loopback address succeeded")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	time.Sleep(20 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Fatalf("listener accepted %d connections", n)
	}
}

func TestGuardedDial_RefusesLiteralAndMixedAnswers(t *testing.T) {
	port, accepted := listenLoopback(t)
	r := fakeResolver{
		"mixed.example":  {"93.184.216.34", "127.0.0.1"},
		"mapped.example": {"::ffff:127.0.0.1"},
		"empty.example":  {},
	}
	tr := newTransport(false, r)
	for _, host := range []string{"127.0.0.1", "::ffff:127.0.0.1", "mixed.example", "mapped.example"} {
		conn, err := tr.DialContext(context.Background(), "tcp", net.JoinHostPort(host, port))
		if err == nil {
			_ = conn.Close()
			t.Errorf("dial %s succeeded", host)
			continue
		}
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("dial %s err = %v, want ErrBlockedAddress", host, err)
		}
	}
	for _, host := range []string{"empty.example", "missing.example"} {
		if conn, err := tr.DialContext(context.Background(), "tcp", net.JoinHostPort(host, port)); err == nil {
			_ = conn.Close()
			t.Errorf("dial %s succeeded", host)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Fatalf("listener accepted %d connections", n)
	}
}

// The control hook is the enforcement point: it runs for every address of a
// multi-address answer, even if the check before dialling were skipped.
func TestGuardedDial_ControlRunsForEveryAddress(t *testing.T) {
	port, accepted := listenLoopback(t)
	var seen []string
	inner := dialControl(false)
	g := &guardedDialer{
		resolver:     fakeResolver{"multi.example": {"10.0.0.1", "192.168.0.1", "127.0.0.1"}},
		allowPrivate: true, // skips the check before dialling, leaving only the hook
		timeout:      2 * time.Second,
		dial: (&net.Dialer{Control: func(network, address string, c syscall.RawConn) error {
			seen = append(seen, address)
			return inner(network, address, c)
		}}).DialContext,
	}
	conn, err := g.DialContext(context.Background(), "tcp", net.JoinHostPort("multi.example", port))
	if err == nil {
		_ = conn.Close()
		t.Fatal("dial succeeded")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if len(seen) != 3 {
		t.Fatalf("control hook saw %v, want all three addresses", seen)
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("listener accepted %d connections", n)
	}
}

func TestGuardedDial_AllowPrivateReachesLoopback(t *testing.T) {
	port, _ := listenLoopback(t)
	tr := newTransport(true, fakeResolver{"lan.example": {"127.0.0.1"}})
	conn, err := tr.DialContext(context.Background(), "tcp", net.JoinHostPort("lan.example", port))
	if err != nil {
		t.Fatalf("allowPrivate dial: %v", err)
	}
	_ = conn.Close()
}

// A blackholed first address must not hold a new connection for a full dial
// timeout: the answers share one deadline and each gets a fair part of it.
func TestGuardedDial_BlackholedAddressDoesNotStallTheRest(t *testing.T) {
	var tried []string
	g := &guardedDialer{
		resolver: fakeResolver{"multi.example": {"93.184.216.34", "93.184.216.35"}},
		timeout:  400 * time.Millisecond,
		dial: func(ctx context.Context, _, address string) (net.Conn, error) {
			tried = append(tried, address)
			if len(tried) == 1 {
				<-ctx.Done() // never answers
				return nil, ctx.Err()
			}
			c, peer := net.Pipe()
			_ = peer.Close()
			return c, nil
		},
	}
	start := time.Now()
	conn, err := g.DialContext(context.Background(), "tcp", "multi.example:443")
	if err != nil {
		t.Fatalf("dial: %v (tried %v)", err, tried)
	}
	_ = conn.Close()
	if len(tried) != 2 || tried[1] != "93.184.216.35:443" {
		t.Fatalf("tried %v", tried)
	}
	// The first of two answers gets half the budget, not all of it.
	if d := time.Since(start); d < 150*time.Millisecond || d > 350*time.Millisecond {
		t.Fatalf("second address reached after %v, want about 200ms", d)
	}
}

// With every answer blackholed the dial ends at the overall deadline, not at
// one dial timeout per address.
func TestGuardedDial_OverallDeadlineBoundsAllAttempts(t *testing.T) {
	attempts := 0
	g := &guardedDialer{
		resolver: fakeResolver{"multi.example": {"93.184.216.34", "93.184.216.35", "93.184.216.36", "93.184.216.37"}},
		timeout:  300 * time.Millisecond,
		dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			attempts++
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	start := time.Now()
	_, err := g.DialContext(context.Background(), "tcp", "multi.example:443")
	if err == nil {
		t.Fatal("dial succeeded")
	}
	if d := time.Since(start); d > 600*time.Millisecond {
		t.Fatalf("dial took %v, want about 300ms", d)
	}
	if attempts != 4 {
		t.Fatalf("attempts = %d, want every address tried", attempts)
	}
	if tr := newTransport(false, fakeResolver{}); tr.DialContext == nil {
		t.Fatal("no dialer")
	}
}
