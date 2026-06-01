package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/bridge"
	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/testutil"
)

type atomicUint64Alias = atomic.Uint64

// atomicU64 wraps atomic.Uint64 so the test can take &x.v.
type atomicU64 struct{ v atomicUint64Alias }

func TestServerEndToEndAuthRegister(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{
		Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey: filepath.Join(dir, "dev-server-key.pem"), Auth: fakeAuth{uid: "u1"},
		PublicBind: "127.0.0.1", // loopback-only: avoids Windows Firewall prompts in tests
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	defer func() { cancel(); srv.Wait() }()
	waitListening(t, srv)

	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	conn, err := tls.Dial("tcp", srv.Addr(), &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = proto.WriteMessage(conn, proto.MsgAuthRequest, proto.AuthRequest{ProtocolVersion: 1, Token: "secret"})
	var env proto.Envelope
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := proto.ReadFrame(conn, &env); err != nil || env.Type != proto.MsgAuthResponse {
		t.Fatalf("auth: %v %v", env.Type, err)
	}
	sess, err := yamux.Client(conn, yamux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	cs, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_ = proto.WriteMessage(cs, proto.MsgTunnelRegister, proto.TunnelRegister{Type: "tcp", RemotePort: 9001, LocalAddr: "127.0.0.1:3000"})
	_ = proto.ReadFrame(cs, &env)
	if env.Type != proto.MsgTunnelRegisterResp {
		t.Fatalf("want register resp, got %v", env.Type)
	}
}

func waitListening(t *testing.T, s *Server) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if s.Addr() != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never started listening")
}

func TestDataPlaneEndToEnd(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	// local "service": echo + a fixed banner
	lsrv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lsrv.Close()
	go func() {
		c, e := lsrv.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		_, _ = io.ReadFull(c, buf)
		_, _ = c.Write(append([]byte("echo:"), buf...))
	}()

	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{
		Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey: filepath.Join(dir, "dev-server-key.pem"), Auth: fakeAuth{uid: "u1"},
		PublicBind: "127.0.0.1", PortMin: 19000, PortMax: 19050,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	defer func() { cancel(); srv.Wait() }()
	waitListening(t, srv)

	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	conn, err := tls.Dial("tcp", srv.Addr(), &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = proto.WriteMessage(conn, proto.MsgAuthRequest, proto.AuthRequest{ProtocolVersion: 1, Token: "secret"})
	var env proto.Envelope
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := proto.ReadFrame(conn, &env); err != nil || env.Type != proto.MsgAuthResponse {
		t.Fatalf("auth: %v %v", env.Type, err)
	}
	sess, err := yamux.Client(conn, yamux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_, lport, _ := net.SplitHostPort(lsrv.Addr().String())
	_ = proto.WriteMessage(ctrl, proto.MsgTunnelRegister, proto.TunnelRegister{
		Type: "tcp", RemotePort: 19001, LocalAddr: "127.0.0.1:" + lport,
	})
	if err := proto.ReadFrame(ctrl, &env); err != nil || env.Type != proto.MsgTunnelRegisterResp {
		t.Fatalf("register resp: %v %v", env.Type, err)
	}
	var rr proto.TunnelRegisterResponse
	_ = proto.DecodePayload(env, &rr)
	if !rr.OK || rr.RemotePort != 19001 {
		t.Fatalf("register: %+v", rr)
	}

	go func() {
		for {
			var ce proto.Envelope
			if proto.ReadFrame(ctrl, &ce) != nil {
				return
			}
			if ce.Type != proto.MsgNewConnection {
				continue
			}
			var nc proto.NewConnection
			if proto.DecodePayload(ce, &nc) != nil {
				continue
			}
			go func(nc proto.NewConnection) {
				st, e := sess.OpenStream()
				if e != nil {
					return
				}
				defer st.Close()
				if proto.WriteMessage(st, proto.MsgStreamOpen, proto.StreamHeader{StreamID: nc.StreamID, TunnelID: nc.TunnelID}) != nil {
					return
				}
				lc, e := net.Dial("tcp", "127.0.0.1:"+lport)
				if e != nil {
					return
				}
				defer lc.Close()
				var x, y atomicU64
				bridge.Pipe(lc, st, &x.v, &y.v)
			}(nc)
		}
	}()

	// visitor hits the public port
	vc, err := net.DialTimeout("tcp", "127.0.0.1:19001", 2*time.Second)
	if err != nil {
		t.Fatalf("visitor dial: %v", err)
	}
	defer vc.Close()
	_, _ = vc.Write([]byte("HELLO"))
	vc.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp := make([]byte, 10)
	if _, err := io.ReadFull(vc, resp); err != nil || string(resp) != "echo:HELLO" {
		t.Fatalf("visitor got %q err=%v", resp, err)
	}

	// Phase-3 done-criterion: per-tunnel byte counters incremented.
	var bin, bout uint64
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		bin, bout = 0, 0
		for _, s := range srv.reg.Sessions() {
			for _, tn := range s.snapshotTunnelsForTest() {
				bin += tn.BytesIn.Load()
				bout += tn.BytesOut.Load()
			}
		}
		if bin > 0 && bout > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if bin == 0 || bout == 0 {
		t.Fatalf("byte counters not incremented: in=%d out=%d", bin, bout)
	}
}

func TestRegisterPortInUseFails(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	_ = devcert.Generate(dir, true)
	srv, _ := New(Options{
		Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey: filepath.Join(dir, "dev-server-key.pem"), Auth: fakeAuth{uid: "u1"},
		PublicBind: "127.0.0.1", PortMin: 19060, PortMax: 19060,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	defer func() { cancel(); srv.Wait() }()
	waitListening(t, srv)
	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	conn, _ := tls.Dial("tcp", srv.Addr(), &tls.Config{RootCAs: pool, ServerName: "localhost"})
	defer conn.Close()
	_ = proto.WriteMessage(conn, proto.MsgAuthRequest, proto.AuthRequest{ProtocolVersion: 1, Token: "secret"})
	var env proto.Envelope
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_ = proto.ReadFrame(conn, &env)
	sess, _ := yamux.Client(conn, yamux.DefaultConfig())
	ctrl, _ := sess.OpenStream()
	_ = proto.WriteMessage(ctrl, proto.MsgTunnelRegister, proto.TunnelRegister{Type: "tcp", RemotePort: 19060, LocalAddr: "127.0.0.1:1"})
	_ = proto.ReadFrame(ctrl, &env)
	var r1 proto.TunnelRegisterResponse
	_ = proto.DecodePayload(env, &r1)
	if !r1.OK {
		t.Fatalf("first register should succeed: %+v", r1)
	}
	_ = proto.WriteMessage(ctrl, proto.MsgTunnelRegister, proto.TunnelRegister{Type: "tcp", RemotePort: 19060, LocalAddr: "127.0.0.1:1"})
	_ = proto.ReadFrame(ctrl, &env)
	var r2 proto.TunnelRegisterResponse
	_ = proto.DecodePayload(env, &r2)
	if r2.OK {
		t.Fatal("second register on the same in-use port must fail")
	}
}

func TestNewServer_UsesInjectedGetCertificate(t *testing.T) {
	called := false
	getCert := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		called = true
		return nil, nil
	}
	s, err := New(Options{
		Listen:         "127.0.0.1:0",
		GetCertificate: getCert,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.tlsC.GetCertificate == nil {
		t.Fatal("expected tlsC.GetCertificate to be set from Options.GetCertificate")
	}
	_, _ = s.tlsC.GetCertificate(&tls.ClientHelloInfo{})
	if !called {
		t.Fatal("injected GetCertificate was not wired")
	}
	if len(s.tlsC.Certificates) != 0 {
		t.Fatal("static Certificates must be empty when GetCertificate is injected")
	}
}
