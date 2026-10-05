package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/testutil"
)

func startServer(t *testing.T, dir, token string) (*server.Server, context.CancelFunc) {
	t.Helper()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	_ = token // server-side validation is now an authenticator; client still sends --token
	s, err := server.New(server.Options{
		Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey: filepath.Join(dir, "dev-server-key.pem"), Auth: testAuth(),
		PublicBind: "127.0.0.1", // loopback-only: avoids Windows Firewall prompts in tests
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	for i := 0; i < 100 && s.Addr() == ""; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	return s, cancel
}

// testAuth accepts any non-empty token (clients still send --token secret).
func testAuth() server.AuthFunc {
	return func(_ context.Context, tok string) (string, error) {
		if tok == "" {
			return "", fmt.Errorf("empty token")
		}
		return "u1", nil
	}
}

func TestClientConnectsAndRegisters(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	s, cancel := startServer(t, dir, "secret")
	defer func() { cancel(); s.Wait() }()
	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)

	c := New(Options{
		Server: s.Addr(), Token: "secret", RootCAs: pool, ServerName: "localhost",
		Tunnels: []TunnelSpec{{Name: "web", Type: "tcp", RemotePort: 9000, LocalAddr: "127.0.0.1:3000"}},
	})
	ctx, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	go func() { _ = c.Run(ctx) }()

	if !waitTrue(func() bool { return c.Registered() }, 2*time.Second) {
		t.Fatal("client never registered a tunnel")
	}
}

func TestClientReconnectsAfterServerRestart(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	s, cancel := startServer(t, dir, "secret")
	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	addr := s.Addr()

	c := New(Options{Server: addr, Token: "secret", RootCAs: pool, ServerName: "localhost",
		Tunnels: []TunnelSpec{{Type: "tcp", RemotePort: 9000, LocalAddr: "127.0.0.1:3000"}}})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = c.Run(ctx) }()
	if !waitTrue(c.Registered, 2*time.Second) {
		t.Fatal("initial register failed")
	}
	cancel() // kill server
	s.Wait()
	c.resetRegisteredForTest()
	// restart on the SAME addr
	s2, err := server.New(server.Options{Listen: addr, TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey: filepath.Join(dir, "dev-server-key.pem"), Auth: testAuth(), PublicBind: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer func() { cancel2(); s2.Wait() }()
	go func() { _ = s2.Serve(ctx2) }()
	if !waitTrue(c.Registered, 5*time.Second) {
		t.Fatal("client did not reconnect within 5s")
	}
}

func waitTrue(f func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestClientBridgesDataEndToEnd(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	// local echo service
	ls, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	go func() {
		for {
			c, e := ls.Accept()
			if e != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 5)
				if _, e := io.ReadFull(c, b); e == nil {
					_, _ = c.Write(append([]byte("R:"), b...))
				}
			}(c)
		}
	}()
	_, lport, _ := net.SplitHostPort(ls.Addr().String())

	dir := t.TempDir()
	s, cancel := startServer(t, dir, "secret") // Phase-2 helper; New() now defaults PublicBind/ports
	defer func() { cancel(); s.Wait() }()
	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)

	c := New(Options{
		Server: s.Addr(), Token: "secret", RootCAs: pool, ServerName: "localhost",
		Tunnels: []TunnelSpec{{Name: "echo", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:" + lport}},
	})
	ctx, c2 := context.WithCancel(context.Background())
	defer c2()
	go func() { _ = c.Run(ctx) }()
	if !waitTrue(c.Registered, 3*time.Second) {
		t.Fatal("client never registered")
	}
	port := c.lastRemotePortForTest()
	if port == 0 {
		t.Fatal("no remote port assigned")
	}
	vc, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("visitor dial :%d: %v", port, err)
	}
	defer vc.Close()
	_, _ = vc.Write([]byte("PINGS"))
	vc.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, 7)
	if _, err := io.ReadFull(vc, got); err != nil || string(got) != "R:PINGS" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestConcurrentVisitors(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	ls, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ls.Close()
	go func() {
		for {
			c, e := ls.Accept()
			if e != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 4)
				if _, e := io.ReadFull(c, b); e == nil {
					_, _ = c.Write(b)
				}
			}(c)
		}
	}()
	_, lport, _ := net.SplitHostPort(ls.Addr().String())
	dir := t.TempDir()
	s, cancel := startServer(t, dir, "secret")
	defer func() { cancel(); s.Wait() }()
	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	cl := New(Options{Server: s.Addr(), Token: "secret", RootCAs: pool, ServerName: "localhost",
		Tunnels: []TunnelSpec{{Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:" + lport}}})
	ctx, c2 := context.WithCancel(context.Background())
	defer c2()
	go func() { _ = cl.Run(ctx) }()
	if !waitTrue(cl.Registered, 3*time.Second) {
		t.Fatal("register failed")
	}
	port := itoa(cl.lastRemotePortForTest())
	var wg sync.WaitGroup
	errc := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vc, e := net.DialTimeout("tcp", "127.0.0.1:"+port, 3*time.Second)
			if e != nil {
				errc <- e
				return
			}
			defer vc.Close()
			_, _ = vc.Write([]byte("abcd"))
			vc.SetReadDeadline(time.Now().Add(3 * time.Second))
			b := make([]byte, 4)
			if _, e := io.ReadFull(vc, b); e != nil || string(b) != "abcd" {
				errc <- fmt.Errorf("got %q err=%v", b, e)
			}
		}()
	}
	wg.Wait()
	close(errc)
	for e := range errc {
		t.Fatalf("concurrent visitor failed: %v", e)
	}
}

// TestBackoffNotResetOnRegistrationFailure asserts B14: when auth succeeds but
// tunnel registration is rejected by the server, bo.Reset() must NOT be called,
// so the next retry delay reflects accumulated backoff rather than the minimum.
func TestBackoffNotResetOnRegistrationFailure(t *testing.T) {
	// Build dev certs for the fake server.
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(
		filepath.Join(dir, "dev-server.pem"),
		filepath.Join(dir, "dev-server-key.pem"),
	)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}

	// Start a fake TLS server that accepts auth but rejects tunnel registration.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Serve exactly one connection: auth OK (on raw conn), then accept yamux session
	// and reject tunnel registration (on control stream).
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, e := ln.Accept()
		if e != nil {
			return
		}
		defer conn.Close()

		// Phase 1: auth exchange on the raw TLS connection (before yamux).
		var env proto.Envelope
		if e := proto.ReadFrame(conn, &env); e != nil {
			return
		}
		if e := proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{
			OK: true, SessionID: "test-sess",
		}); e != nil {
			return
		}

		// Phase 2: client wraps conn in yamux.Client; we must wrap it in yamux.Server.
		ysess, e := yamux.Server(conn, yamux.DefaultConfig())
		if e != nil {
			return
		}
		defer ysess.Close()

		// Phase 3: client opens a stream and sends TunnelRegister on it.
		stream, e := ysess.Accept()
		if e != nil {
			return
		}
		defer stream.Close()
		if e := proto.ReadFrame(stream, &env); e != nil {
			return
		}
		// Reply: registration FAILED.
		_ = proto.WriteMessage(stream, proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{
			OK: false, Error: "port range exhausted",
		})
	}()

	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)

	c := New(Options{
		Server: ln.Addr().String(), Token: "secret", RootCAs: pool, ServerName: "localhost",
		Tunnels: []TunnelSpec{{Name: "web", Type: "tcp", RemotePort: 9000, LocalAddr: "127.0.0.1:3000"}},
	})

	// Prime the backoff with several calls so attempt > 0 and the next delay
	// would be larger than min if backoff was NOT reset.
	for i := 0; i < 4; i++ {
		c.bo.NextBackOff()
	}
	// Capture attempt count after priming (must be > 0).
	attemptAfterPrime := c.bo.AttemptForTest()
	if attemptAfterPrime == 0 {
		t.Fatal("priming did not advance attempt counter")
	}

	// Run one connectOnce — auth succeeds, registration fails.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.connectOnce(ctx)

	// Backoff attempt must still reflect the primed value (bo.Reset was NOT called).
	// AttemptForTest reads the counter without advancing it.
	attemptAfterFail := c.bo.AttemptForTest()
	if attemptAfterFail != attemptAfterPrime {
		t.Fatalf("backoff was reset on registration failure: attempt went from %d to %d (want %d)",
			attemptAfterPrime, attemptAfterFail, attemptAfterPrime)
	}
	// Client must not be marked registered.
	if c.Registered() {
		t.Fatal("client marked registered despite registration failure")
	}
	<-serverDone
}

// syncBuffer is a goroutine-safe log sink (the client logs from more than one
// goroutine once a session is up).
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// registerHTTPAgainstFake runs one connectOnce against a fake relay that
// accepts auth and answers the http tunnel registration with resp, then
// returns everything the client logged.
func registerHTTPAgainstFake(t *testing.T, resp proto.TunnelRegisterResponse) string {
	t.Helper()
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(
		filepath.Join(dir, "dev-server.pem"),
		filepath.Join(dir, "dev-server-key.pem"),
	)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverDone := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, e := ln.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		var env proto.Envelope
		if e := proto.ReadFrame(conn, &env); e != nil {
			return
		}
		if e := proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{OK: true, SessionID: "test-sess"}); e != nil {
			return
		}
		ysess, e := yamux.Server(conn, yamux.DefaultConfig())
		if e != nil {
			return
		}
		defer ysess.Close()
		stream, e := ysess.Accept()
		if e != nil {
			return
		}
		defer stream.Close()
		if e := proto.ReadFrame(stream, &env); e != nil {
			return
		}
		// Reply and keep the session open until the test has seen the
		// client's log line: hanging up at once races the client's read of
		// the reply against the closing session.
		_ = proto.WriteMessage(stream, proto.MsgTunnelRegisterResp, resp)
		<-release
	}()

	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)

	out := &syncBuffer{}
	c := New(Options{
		Server: ln.Addr().String(), Token: "secret", RootCAs: pool, ServerName: "localhost",
		Tunnels: []TunnelSpec{{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"}},
		Logger:  slog.New(slog.NewTextHandler(out, nil)),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clientDone := make(chan struct{})
	go func() { defer close(clientDone); _ = c.connectOnce(ctx) }()
	// Wait for the registration to be logged (or for the client to give up),
	// then end the session from both sides.
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
wait:
	for !strings.Contains(out.String(), "tunnel registered") {
		select {
		case <-clientDone:
			break wait
		case <-tick.C:
		}
	}
	close(release)
	cancel()
	<-serverDone
	<-clientDone
	return out.String()
}

// TestClientLogsHTTPTunnelURL asserts the client prints the path URL the relay
// reported for an http tunnel.
func TestClientLogsHTTPTunnelURL(t *testing.T) {
	out := registerHTTPAgainstFake(t, proto.TunnelRegisterResponse{
		OK: true, TunnelID: "t1", URL: "https://burrow.example.com/svc/p7baeh/",
	})
	if !strings.Contains(out, "tunnel registered") || !strings.Contains(out, "name=web") ||
		!strings.Contains(out, "url=https://burrow.example.com/svc/p7baeh/") {
		t.Fatalf("log output missing the tunnel URL: %s", out)
	}
}

// TestClientLogsHTTPTunnelHostnameFallback asserts a new client still reports
// where the tunnel lives when an older relay only sends Hostname.
func TestClientLogsHTTPTunnelHostnameFallback(t *testing.T) {
	out := registerHTTPAgainstFake(t, proto.TunnelRegisterResponse{
		OK: true, TunnelID: "t1", Hostname: "k7p2qx.tunnels.example.com",
	})
	if !strings.Contains(out, "url=k7p2qx.tunnels.example.com") {
		t.Fatalf("log output missing the hostname fallback: %s", out)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
