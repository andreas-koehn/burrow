package server

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/proto"
)

func newEvtServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Listen:  "127.0.0.1:0",
		TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey:  filepath.Join(dir, "dev-server-key.pem"),
		Auth:    fakeAuth{uid: "u1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewDefaultsEventsToNoop(t *testing.T) {
	s := newEvtServer(t)
	if s.opts.Events == nil {
		t.Fatal("New must default Events to a non-nil noop publisher")
	}
	s.opts.Events.PublishTunnelsChanged("u1") // must not panic
}

func TestListUserTunnelsEmpty(t *testing.T) {
	s := newEvtServer(t)
	if got := s.ListUserTunnels("nobody"); len(got) != 0 {
		t.Fatalf("want 0 tunnels, got %d", len(got))
	}
}

// recordPub records every PublishTunnelsChanged call for assertion.
type recordPub struct {
	mu  sync.Mutex
	ids []string
}

func (p *recordPub) PublishTunnelsChanged(userID string) {
	p.mu.Lock()
	p.ids = append(p.ids, userID)
	p.mu.Unlock()
}

func (p *recordPub) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ids...)
}

// TestRegisterPublishesTunnelsChanged mirrors the exact harness from
// TestControlLoopRegisterAndPing but injects a recordPub so we can assert
// that RunControlLoop calls PublishTunnelsChanged exactly once on a successful
// tunnel_register. This is a genuine regression guard: removing control.go's
// s.opts.Events.PublishTunnelsChanged call would leave rp.ids empty and fail
// the len==1 assertion.
func TestRegisterPublishesTunnelsChanged(t *testing.T) {
	cli, srv := dialPair()
	defer cli.Close()

	reg := NewRegistry()
	cs := &ClientSession{SessionID: "s2", Scope: "all", UserID: "u1", Tunnels: map[string]*Tunnel{}}
	reg.AddSession(cs)
	cs.SetControl(srv)

	rp := &recordPub{}
	s := &Server{
		opts:  Options{PublicBind: "127.0.0.1", PortMin: 18100, PortMax: 18199, Tunnels: noopTunnelStore{}, Events: rp},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ports: newPortAllocator(18100, 18199),
	}
	t.Cleanup(func() {
		for _, tn := range reg.snapshotTunnels(cs) {
			if tn.Listener != nil {
				_ = tn.Listener.Close()
			}
			s.ports.Release(tn.RemotePort)
		}
	})

	go s.RunControlLoop(srv, reg, cs)

	_ = proto.WriteMessage(cli, proto.MsgTunnelRegister, proto.TunnelRegister{
		Name: "web", Type: "tcp", RemotePort: 18100, LocalAddr: "127.0.0.1:3000",
	})
	var env proto.Envelope
	cli.SetReadDeadline(time.Now().Add(time.Second))
	_ = proto.ReadFrame(cli, &env)
	if env.Type != proto.MsgTunnelRegisterResp {
		t.Fatalf("want register response, got %v", env.Type)
	}
	var rr proto.TunnelRegisterResponse
	_ = proto.DecodePayload(env, &rr)
	if !rr.OK {
		t.Fatalf("register failed: %+v", rr)
	}

	// The publish is synchronous in RunControlLoop (no goroutine), so it has
	// already fired before the response was sent. No sleep needed.
	got := rp.snapshot()
	if len(got) != 1 {
		t.Fatalf("want exactly 1 publish after register, got %d: %v", len(got), got)
	}
	if got[0] != "u1" {
		t.Fatalf("want publish for userID %q, got %q", "u1", got[0])
	}
}

// TestUserByteSumHelper verifies the pure userByteSum helper used by byteTicker.
func TestUserByteSumHelper(t *testing.T) {
	reg := NewRegistry()

	cs1 := &ClientSession{SessionID: "s1", Scope: "all", UserID: "alice", Tunnels: map[string]*Tunnel{}}
	cs2 := &ClientSession{SessionID: "s2", Scope: "all", UserID: "bob", Tunnels: map[string]*Tunnel{}}
	reg.AddSession(cs1)
	reg.AddSession(cs2)

	tn1 := &Tunnel{ID: "t1"}
	tn2 := &Tunnel{ID: "t2"}
	reg.AddTunnel(cs1, tn1)
	reg.AddTunnel(cs2, tn2)

	sessions := reg.Sessions()

	// Both users start at zero.
	sum, has := userByteSum(sessions, reg, "alice")
	if sum != 0 || !has {
		t.Fatalf("alice: want sum=0 hasTunnels=true, got sum=%d has=%v", sum, has)
	}

	// Unknown user has no tunnels.
	sum, has = userByteSum(sessions, reg, "nobody")
	if sum != 0 || has {
		t.Fatalf("nobody: want sum=0 hasTunnels=false, got sum=%d has=%v", sum, has)
	}

	// Increment alice's tunnel bytes and verify sum reflects it.
	tn1.BytesIn.Store(100)
	tn1.BytesOut.Store(200)
	sessions = reg.Sessions()
	sum, has = userByteSum(sessions, reg, "alice")
	if sum != 300 || !has {
		t.Fatalf("alice after increment: want 300, got %d (has=%v)", sum, has)
	}

	// Bob's sum is still zero.
	sum, has = userByteSum(sessions, reg, "bob")
	if sum != 0 || !has {
		t.Fatalf("bob: want 0, got %d (has=%v)", sum, has)
	}
}

// TestByteTickerNoPubWhenIdle injects a live session+tunnel with static byte
// counters and asserts that after the first publish (first observation), no
// additional publish fires while bytes stay unchanged.
func TestByteTickerNoPubWhenIdle(t *testing.T) {
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	rp := &recordPub{}
	s, err := New(Options{
		Listen:  "127.0.0.1:0",
		TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey:  filepath.Join(dir, "dev-server-key.pem"),
		Auth:    fakeAuth{uid: "u1"},
		Events:  rp,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	defer func() { cancel(); s.Wait() }()
	waitListening(t, s)

	cs := &ClientSession{SessionID: "idle-s1", Scope: "all", UserID: "u2", Tunnels: map[string]*Tunnel{}}
	tn := &Tunnel{ID: "idle-t1", Name: "idle", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:1"}
	// Set fixed byte values; they will not change during the test.
	tn.BytesIn.Store(42)
	tn.BytesOut.Store(58)
	s.reg.AddSession(cs)
	s.reg.AddTunnel(cs, tn)
	t.Cleanup(func() { s.reg.RemoveSession(cs) })

	// Wait for the first publish (first-observation publish is expected).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countPubs(rp, "u2") >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if countPubs(rp, "u2") < 1 {
		t.Fatal("first-observation publish did not arrive within 2s")
	}

	// Record how many publishes happened, wait 2.5 more ticks, then assert no new ones.
	before := countPubs(rp, "u2")
	time.Sleep(2500 * time.Millisecond)
	after := countPubs(rp, "u2")
	if after != before {
		t.Fatalf("idle tunnel: got %d extra publish(es) with unchanged bytes (before=%d after=%d)", after-before, before, after)
	}
}

// TestByteTickerPubOnByteChange injects a live tunnel, waits for the first
// publish, then increments bytes and asserts exactly one additional publish fires.
func TestByteTickerPubOnByteChange(t *testing.T) {
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	rp := &recordPub{}
	s, err := New(Options{
		Listen:  "127.0.0.1:0",
		TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey:  filepath.Join(dir, "dev-server-key.pem"),
		Auth:    fakeAuth{uid: "u1"},
		Events:  rp,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	defer func() { cancel(); s.Wait() }()
	waitListening(t, s)

	cs := &ClientSession{SessionID: "chg-s1", Scope: "all", UserID: "u3", Tunnels: map[string]*Tunnel{}}
	tn := &Tunnel{ID: "chg-t1", Name: "chg", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:1"}
	s.reg.AddSession(cs)
	s.reg.AddTunnel(cs, tn)
	t.Cleanup(func() { s.reg.RemoveSession(cs) })

	// Wait for the first-observation publish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countPubs(rp, "u3") >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if countPubs(rp, "u3") < 1 {
		t.Fatal("first publish did not arrive within 2s")
	}

	// Freeze: no more publishes expected until bytes change.
	beforeChange := countPubs(rp, "u3")

	// Now change the byte counter.
	tn.BytesIn.Add(1024)

	// Expect at least one additional publish within 2s.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countPubs(rp, "u3") > beforeChange {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no publish after byte increment: still at %d pub(s)", countPubs(rp, "u3"))
}

// TestByteTickerNoMapLeakAfterDisconnect injects a session with a tunnel,
// waits for a publish, removes the session (simulating disconnect), then waits
// two more ticks and asserts no additional publish fires (map cleaned up).
func TestByteTickerNoMapLeakAfterDisconnect(t *testing.T) {
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	rp := &recordPub{}
	s, err := New(Options{
		Listen:  "127.0.0.1:0",
		TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey:  filepath.Join(dir, "dev-server-key.pem"),
		Auth:    fakeAuth{uid: "u1"},
		Events:  rp,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	defer func() { cancel(); s.Wait() }()
	waitListening(t, s)

	cs := &ClientSession{SessionID: "leak-s1", Scope: "all", UserID: "u4", Tunnels: map[string]*Tunnel{}}
	tn := &Tunnel{ID: "leak-t1", Name: "leak", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:1"}
	s.reg.AddSession(cs)
	s.reg.AddTunnel(cs, tn)

	// Wait for first publish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countPubs(rp, "u4") >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if countPubs(rp, "u4") < 1 {
		t.Fatal("first publish did not arrive within 2s")
	}

	// Simulate disconnect: remove the session.
	s.reg.RemoveSession(cs)

	// After removal there are no live tunnels for u4 → the lastSum entry must be
	// dropped. Verify no further publishes occur for u4 over the next 2.5s.
	before := countPubs(rp, "u4")
	time.Sleep(2500 * time.Millisecond)
	after := countPubs(rp, "u4")
	if after != before {
		t.Fatalf("publish fired after disconnect (map leak): before=%d after=%d", before, after)
	}
}

// countPubs counts how many publishes rp has recorded for userID.
func countPubs(rp *recordPub, userID string) int {
	n := 0
	for _, id := range rp.snapshot() {
		if id == userID {
			n++
		}
	}
	return n
}

// TestByteTickerPublishesForLiveTunnel starts a real Server with a recordPub,
// injects a live session+tunnel directly into s.reg (same package), and asserts
// that byteTicker emits at least one publish for the tunnel owner within 2s.
func TestByteTickerPublishesForLiveTunnel(t *testing.T) {
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	rp := &recordPub{}
	s, err := New(Options{
		Listen:  "127.0.0.1:0",
		TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey:  filepath.Join(dir, "dev-server-key.pem"),
		Auth:    fakeAuth{uid: "u1"},
		Events:  rp,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	defer func() { cancel(); s.Wait() }()
	waitListening(t, s)

	// Wire a live session with one tunnel directly into the server's registry.
	cs := &ClientSession{SessionID: "bt-s1", Scope: "all", UserID: "u1", Tunnels: map[string]*Tunnel{}}
	tn := &Tunnel{ID: "bt-t1", Name: "bt", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:1"}
	s.reg.AddSession(cs)
	s.reg.AddTunnel(cs, tn)
	t.Cleanup(func() { s.reg.RemoveSession(cs) })

	// Poll up to 2s for byteTicker to fire at least once for "u1".
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := rp.snapshot()
		found := false
		for _, id := range snap {
			if id == "u1" {
				found = true
				break
			}
		}
		if found {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("byteTicker did not publish for 'u1' within 2s")
}
