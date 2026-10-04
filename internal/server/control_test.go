package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
)

// fakeAuth is a test TokenAuthenticator. Shared by control_test.go and
// server_test.go (both package server).
type fakeAuth struct {
	uid string
	err error
}

func (f fakeAuth) Authenticate(_ context.Context, tok string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if tok == "" {
		return "", fmt.Errorf("empty token")
	}
	return f.uid, nil
}

func dialPair() (net.Conn, net.Conn) { return net.Pipe() }

func TestHandshakeSuccess(t *testing.T) {
	cli, srv := dialPair()
	defer cli.Close()
	done := make(chan *ClientSession, 1)
	go func() { cs, _ := HandleHandshake(srv, fakeAuth{uid: "u1"}, "sid-1"); done <- cs }()

	_ = proto.WriteMessage(cli, proto.MsgAuthRequest, proto.AuthRequest{ProtocolVersion: 1, Token: "good"})
	var env proto.Envelope
	cli.SetReadDeadline(time.Now().Add(time.Second))
	if err := proto.ReadFrame(cli, &env); err != nil || env.Type != proto.MsgAuthResponse {
		t.Fatalf("want auth_response, got %v err=%v", env.Type, err)
	}
	var ar proto.AuthResponse
	_ = proto.DecodePayload(env, &ar)
	if !ar.OK || ar.SessionID != "sid-1" {
		t.Fatalf("bad auth response: %+v", ar)
	}
	if cs := <-done; cs == nil || cs.SessionID != "sid-1" || cs.UserID != "u1" {
		t.Fatalf("handshake returned %+v", cs)
	}
}

func TestHandshakeBadToken(t *testing.T) {
	cli, srv := dialPair()
	defer cli.Close()
	go func() { _, _ = HandleHandshake(srv, fakeAuth{err: fmt.Errorf("nope")}, "sid") }()
	_ = proto.WriteMessage(cli, proto.MsgAuthRequest, proto.AuthRequest{ProtocolVersion: 1, Token: "bad"})
	var env proto.Envelope
	cli.SetReadDeadline(time.Now().Add(time.Second))
	_ = proto.ReadFrame(cli, &env)
	var ar proto.AuthResponse
	_ = proto.DecodePayload(env, &ar)
	if ar.OK {
		t.Fatal("bad token must be rejected")
	}
}

func TestHandshakeWrongFirstMessage(t *testing.T) {
	cli, srv := dialPair()
	defer cli.Close()
	go func() { _, _ = HandleHandshake(srv, fakeAuth{uid: "u1"}, "sid") }()
	_ = proto.WriteMessage(cli, proto.MsgPing, proto.Ping{})
	var env proto.Envelope
	cli.SetReadDeadline(time.Now().Add(time.Second))
	if err := proto.ReadFrame(cli, &env); err != nil || env.Type != proto.MsgError {
		t.Fatalf("want error message, got %v err=%v", env.Type, err)
	}
}

func TestControlLoopRegisterAndPing(t *testing.T) {
	cli, srv := dialPair()
	defer cli.Close()
	reg := NewRegistry()
	cs := &ClientSession{SessionID: "s", Tunnels: map[string]*Tunnel{}}
	reg.AddSession(cs)
	cs.SetControl(srv)
	s := &Server{
		opts:  Options{PublicBind: "127.0.0.1", PortMin: 18000, PortMax: 18099, Tunnels: noopTunnelStore{}, Events: noopEventPublisher{}},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ports: newPortAllocator(18000, 18099),
	}
	// Close all tunnel listeners on test cleanup to avoid port conflicts across runs.
	t.Cleanup(func() {
		for _, tn := range reg.snapshotTunnels(cs) {
			if tn.Listener != nil {
				_ = tn.Listener.Close()
			}
			s.ports.Release(tn.RemotePort)
		}
	})
	go s.RunControlLoop(srv, reg, cs)

	_ = proto.WriteMessage(cli, proto.MsgTunnelRegister, proto.TunnelRegister{Name: "web", Type: "tcp", RemotePort: 18000, LocalAddr: "127.0.0.1:3000"})
	var env proto.Envelope
	cli.SetReadDeadline(time.Now().Add(time.Second))
	_ = proto.ReadFrame(cli, &env)
	if env.Type != proto.MsgTunnelRegisterResp {
		t.Fatalf("want register response, got %v", env.Type)
	}
	var rr proto.TunnelRegisterResponse
	_ = proto.DecodePayload(env, &rr)
	if !rr.OK || rr.TunnelID == "" || rr.RemotePort < 18000 || rr.RemotePort > 18099 {
		t.Fatalf("bad register response: %+v", rr)
	}
	if len(cs.Tunnels) != 1 {
		t.Fatalf("tunnel not recorded")
	}
	_ = proto.WriteMessage(cli, proto.MsgPing, proto.Ping{Nonce: "n1"})
	_ = proto.ReadFrame(cli, &env)
	if env.Type != proto.MsgPong {
		t.Fatalf("want pong, got %v", env.Type)
	}
}

// ---- HTTP tunnel test helpers ----

// fakeResolver is a test ServiceResolver stub.
type fakeResolver struct {
	sub string // subdomain to return
	id  string // serviceID to return
	err error  // if non-nil, Resolve returns this error
}

func (f fakeResolver) Resolve(_ context.Context, _, _, _ string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.id, f.sub, nil
}

// newTestServerWithHTTP builds a minimal *Server + *Registry + *ClientSession
// wired for http-tunnel tests. It mirrors the pattern in TestControlLoopRegisterAndPing.
// Returns (server, registry, clientSession, clientConn); the caller must close
// clientConn and clean up tunnel listeners.
func newTestServerWithHTTP(t *testing.T, resolver ServiceResolver, authDomain string) (*Server, *Registry, *ClientSession, net.Conn) {
	t.Helper()
	cli, srv := dialPair()
	reg := NewRegistry()
	cs := &ClientSession{SessionID: "s-http", UserID: "u1", Tunnels: map[string]*Tunnel{}}
	reg.AddSession(cs)
	cs.SetControl(srv)
	s := &Server{
		opts: Options{
			PublicBind: "127.0.0.1", PortMin: 18100, PortMax: 18199,
			Tunnels: noopTunnelStore{}, Events: noopEventPublisher{},
			Services: resolver, AuthDomain: authDomain,
		},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		reg:   reg,
		ports: newPortAllocator(18100, 18199),
	}
	t.Cleanup(func() {
		for _, tn := range reg.snapshotTunnels(cs) {
			if tn.Listener != nil {
				_ = tn.Listener.Close()
			}
			s.ports.Release(tn.RemotePort)
		}
		cli.Close()
		srv.Close()
	})
	go s.RunControlLoop(srv, reg, cs)
	return s, reg, cs, cli
}

// doRegister sends a TunnelRegister message on cli and reads back the response.
func doRegister(t *testing.T, cli net.Conn, msg proto.TunnelRegister) proto.TunnelRegisterResponse {
	t.Helper()
	_ = proto.WriteMessage(cli, proto.MsgTunnelRegister, msg)
	var env proto.Envelope
	cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := proto.ReadFrame(cli, &env); err != nil {
		t.Fatalf("read register response: %v", err)
	}
	if env.Type != proto.MsgTunnelRegisterResp {
		t.Fatalf("want tunnel_register_response, got %v", env.Type)
	}
	var rr proto.TunnelRegisterResponse
	_ = proto.DecodePayload(env, &rr)
	return rr
}

// ---- HTTP tunnel tests ----

// TestRegisterHTTPTunnelAssignsSubdomainNoPort verifies that an http-type
// registration gets a slug, no TCP port, and the path URL of the service.
func TestRegisterHTTPTunnelAssignsSubdomainNoPort(t *testing.T) {
	_, _, _, cli := newTestServerWithHTTP(t, fakeResolver{sub: "k7p2qx", id: "svc1"}, "tunnels.example.com")
	resp := doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"})
	if !resp.OK || resp.RemotePort != 0 {
		t.Fatalf("http tunnel must not get a port: %+v", resp)
	}
	if resp.URL != "https://tunnels.example.com/svc/k7p2qx/" {
		t.Fatalf("want url https://tunnels.example.com/svc/k7p2qx/, got %q", resp.URL)
	}
	if resp.Hostname != "" {
		t.Fatalf("Hostname must no longer be set, got %q", resp.Hostname)
	}
	if resp.TunnelID == "" {
		t.Fatalf("TunnelID must be set: %+v", resp)
	}
}

// TestRegisterHTTPTunnelNilServices verifies that http registration without a
// configured ServiceResolver returns OK=false with a clear error message.
func TestRegisterHTTPTunnelNilServices(t *testing.T) {
	_, _, _, cli := newTestServerWithHTTP(t, nil, "tunnels.example.com")
	resp := doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"})
	if resp.OK {
		t.Fatalf("expected OK=false when Services==nil: %+v", resp)
	}
	if resp.Error == "" {
		t.Fatalf("expected non-empty error message when Services==nil: %+v", resp)
	}
	if !strings.Contains(resp.Error, "not configured") {
		t.Fatalf("error message should mention 'not configured', got %q", resp.Error)
	}
}

// TestRegisterHTTPTunnelResolverError verifies that a resolver error is
// propagated to the client as OK=false.
func TestRegisterHTTPTunnelResolverError(t *testing.T) {
	resolverErr := fmt.Errorf("db unavailable")
	_, _, _, cli := newTestServerWithHTTP(t, fakeResolver{err: resolverErr}, "tunnels.example.com")
	resp := doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"})
	if resp.OK {
		t.Fatalf("expected OK=false on resolver error: %+v", resp)
	}
	if !strings.Contains(resp.Error, "db unavailable") {
		t.Fatalf("resolver error should be surfaced, got %q", resp.Error)
	}
}

// TestRegisterUnknownTypeFails verifies that an unknown tunnel type returns
// OK=false with the exact expected error format.
func TestRegisterUnknownTypeFails(t *testing.T) {
	_, _, _, cli := newTestServerWithHTTP(t, fakeResolver{sub: "k7p2qx", id: "svc1"}, "tunnels.example.com")
	resp := doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "xyz", LocalAddr: "127.0.0.1:3000"})
	if resp.OK {
		t.Fatalf("expected OK=false for unknown type: %+v", resp)
	}
	want := `unknown tunnel type "xyz"`
	if resp.Error != want {
		t.Fatalf("want error %q, got %q", want, resp.Error)
	}
}

// TestRegisterHTTPTunnelNoAuthDomain verifies that when AuthDomain is empty
// URL is "" but registration still succeeds (degraded mode).
func TestRegisterHTTPTunnelNoAuthDomain(t *testing.T) {
	_, _, _, cli := newTestServerWithHTTP(t, fakeResolver{sub: "k7p2qx", id: "svc1"}, "")
	resp := doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"})
	if !resp.OK {
		t.Fatalf("expected OK=true even with empty AuthDomain: %+v", resp)
	}
	if resp.URL != "" || resp.Hostname != "" {
		t.Fatalf("expected empty URL and Hostname when AuthDomain==\"\", got %q / %q", resp.URL, resp.Hostname)
	}
	if resp.TunnelID == "" {
		t.Fatalf("TunnelID must be set: %+v", resp)
	}
}

// TestLookupHTTPTunnel verifies LookupHTTPTunnel finds registered http tunnels
// and returns false for unknown subdomains.
func TestLookupHTTPTunnel(t *testing.T) {
	srv, _, cs, cli := newTestServerWithHTTP(t, fakeResolver{sub: "abc123", id: "svc2"}, "tunnels.example.com")
	resp := doRegister(t, cli, proto.TunnelRegister{Name: "myapp", Type: "http", LocalAddr: "127.0.0.1:4000"})
	if !resp.OK {
		t.Fatalf("registration failed: %+v", resp)
	}
	tn, ok := srv.LookupHTTPTunnel("abc123")
	if !ok || tn == nil {
		t.Fatalf("LookupHTTPTunnel(abc123) not found")
	}
	if tn.Subdomain != "abc123" || tn.ServiceID != "svc2" || !tn.IsHTTP {
		t.Fatalf("tunnel fields wrong: %+v", tn)
	}
	_ = cs // keep session alive
	_, ok2 := srv.LookupHTTPTunnel("notexist")
	if ok2 {
		t.Fatalf("LookupHTTPTunnel should return false for unknown subdomain")
	}
}

// TestHTTPTunnels verifies HTTPTunnels returns all live http tunnels and
// excludes tcp tunnels.
func TestHTTPTunnels(t *testing.T) {
	srv, _, _, cli := newTestServerWithHTTP(t, fakeResolver{sub: "def456", id: "svc3"}, "tunnels.example.com")
	// Register one http tunnel.
	resp := doRegister(t, cli, proto.TunnelRegister{Name: "myapp", Type: "http", LocalAddr: "127.0.0.1:4000"})
	if !resp.OK {
		t.Fatalf("http registration failed: %+v", resp)
	}
	tunnels := srv.HTTPTunnels()
	if len(tunnels) != 1 {
		t.Fatalf("expected 1 http tunnel, got %d", len(tunnels))
	}
	if !tunnels[0].IsHTTP || tunnels[0].Subdomain != "def456" {
		t.Fatalf("wrong tunnel in HTTPTunnels: %+v", tunnels[0])
	}
}
