package server

// A tunnel is in the registry before its client has been told it exists
// (registerTCP adds it, then writes and answers). A visitor in that window
// must not be told to the client under a TunnelID it does not know yet: it
// waits for the answer, then is served.

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
)

// gatedTunnelStore holds SaveTunnel (between AddTunnel and the answer) until
// open is closed.
type gatedTunnelStore struct {
	held chan struct{}
	open chan struct{}
}

func (g *gatedTunnelStore) SaveTunnel(context.Context, string, *Tunnel) error {
	select {
	case g.held <- struct{}{}:
	default:
	}
	<-g.open
	return nil
}
func (g *gatedTunnelStore) MarkTunnelSeen(context.Context, string) error { return nil }

func TestTCPReady_AVisitorBeforeTheAnswerWaitsForIt(t *testing.T) {
	gate := &gatedTunnelStore{held: make(chan struct{}, 1), open: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(gate.open) }) }
	r := newForwardRelay(t, 19663, func(o *Options) { o.Tunnels = gate })
	t.Cleanup(release) // runs before the relay's cleanup: a failed test does not hang it
	registered := make(chan proto.TunnelRegisterResponse, 1)
	go func() {
		registered <- r.home.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open"})
	}()
	select {
	case <-gate.held:
	case <-time.After(5 * time.Second):
		t.Fatal("the registration did not reach SaveTunnel")
	}
	// The listener is up and the tunnel is in the registry; the client has
	// not been answered.
	if _, ok := r.srv.LookupTunnelByServiceID("svc-web"); !ok {
		t.Fatal("the tunnel is not in the registry yet")
	}
	v := dialFrom(t, "127.0.0.1", 19663)
	time.Sleep(300 * time.Millisecond)
	if n := r.told.Load(); n != 0 {
		t.Fatalf("the client was told of a visitor before it knew the tunnel: %d", n)
	}
	release()
	if rr := <-registered; !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	_ = v.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 3)
	if _, err := io.ReadFull(v, buf); err != nil || string(buf) != "hi " {
		t.Fatalf("the waiting visitor was not served: %q %v", buf, err)
	}
	if _, ok := r.srv.routeTunnelByServiceID("svc-web"); !ok {
		t.Fatal("the confirmed tunnel is not routed to")
	}
}

// A tunnel that is ending is not routed to, but enforcement still reaches
// its live connections: a ban closes a visitor of it.
func TestTCPReady_EnforcementReachesAnEndingTunnel(t *testing.T) {
	r := newGuardRelay(t, 19664, newFakeGuardStore(), nil, TCPLimits{})
	live := greeted(t, "127.0.0.4", 19664)
	tn, ok := r.srv.LookupTunnelByServiceID("svc-web")
	if !ok {
		t.Fatal("no tunnel")
	}
	r.srv.endTCPTunnel(tn)
	if _, ok := r.srv.routeTunnelByServiceID("svc-web"); ok {
		t.Fatal("an ending tunnel is routed to")
	}
	if n := len(r.srv.ServiceConnections("svc-web")); n != 1 {
		t.Fatalf("the connection list of an ending tunnel: %d", n)
	}
	r.srv.BanTCPSource("svc-web", "127.0.0.4", time.Now().Add(time.Hour), "manual")
	_ = live.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(live); err != nil {
		t.Fatalf("the ban did not close the visitor of the ending tunnel: %v", err)
	}
}
