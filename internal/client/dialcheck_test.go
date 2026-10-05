package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/devcert"
)

func checkOpts(t *testing.T, dir, addr, token string) Options {
	t.Helper()
	pem, err := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	return Options{Server: addr, Token: token, RootCAs: pool, ServerName: "localhost"}
}

func TestCheckAuth_AcceptedAndRejected(t *testing.T) {
	dir := t.TempDir()
	s, cancel := startServer(t, dir, "")
	defer cancel()
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()

	res, err := CheckAuth(ctx, checkOpts(t, dir, s.Addr(), "bur_test_0000"))
	if err != nil || !res.OK {
		t.Fatalf("valid token: %+v %v", res, err)
	}
	// testAuth refuses an empty token.
	res, err = CheckAuth(ctx, checkOpts(t, dir, s.Addr(), ""))
	if err != nil || res.OK || res.Error == "" {
		t.Fatalf("invalid token: %+v %v", res, err)
	}
	// No tunnel was registered, and the session ends once the check closes.
	if !waitTrue(func() bool { return len(s.SnapshotSessions()) == 0 }, 2*time.Second) {
		t.Fatalf("session still open: %+v", s.SnapshotSessions())
	}
}

func TestCheckAuth_Unreachable(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if _, err := CheckAuth(ctx, Options{Server: addr, Token: "bur_test_0000"}); err == nil {
		t.Fatal("expected an error")
	}
}

// A relay that completes the TLS handshake and then says nothing: the check is
// blocked reading the answer, which does not see ctx. Ending ctx must close the
// connection and with it the read.
func TestCheckAuth_CancelEndsBlockedRead(t *testing.T) {
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "dev-server.pem"), filepath.Join(dir, "dev-server-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	shaken := make(chan struct{})
	released := make(chan struct{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if c.(*tls.Conn).Handshake() != nil {
			return
		}
		close(shaken)
		// Never answers. The read returns when the client closes its side.
		_, _ = io.Copy(io.Discard, c)
		close(released)
	}()

	ctx, stop := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer stop()
	start := time.Now()
	_, err = CheckAuth(ctx, checkOpts(t, dir, l.Addr().String(), "bur_test_0000"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("returned after %v", took)
	}
	select {
	case <-shaken:
	default:
		t.Fatal("the handshake never completed: the blocked read was not reached")
	}
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("the connection was left open")
	}
}

// What Run reports when the relay cannot be reached and when it refuses the
// token is text other code and people rely on: "dial: …" and
// "auth failed: <the relay's reason>".
func TestRun_ReportsDialAndAuthFailures(t *testing.T) {
	run := func(t *testing.T, o Options) string {
		t.Helper()
		var logs syncBuffer
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = New(o).Run(ctx) }()
		ok := waitTrue(func() bool { return strings.Contains(logs.String(), "connection ended") }, 5*time.Second)
		cancel()
		<-done
		if !ok {
			t.Fatalf("no failed attempt was logged: %s", logs.String())
		}
		return logs.String()
	}

	t.Run("closed port", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		out := run(t, Options{Server: addr, Token: "bur_test_0000", Insecure: true})
		if !strings.Contains(out, `err="dial: `) {
			t.Fatalf("log does not carry the dial: wrap: %s", out)
		}
	})

	t.Run("rejected token", func(t *testing.T) {
		dir := t.TempDir()
		s, cancel := startServer(t, dir, "")
		defer func() { cancel(); s.Wait() }()
		// testAuth refuses an empty token.
		res, err := CheckAuth(context.Background(), checkOpts(t, dir, s.Addr(), ""))
		if err != nil || res.OK || res.Error == "" {
			t.Fatalf("setup: %+v %v", res, err)
		}
		out := run(t, checkOpts(t, dir, s.Addr(), ""))
		if !strings.Contains(out, `err="auth failed: `+res.Error+`"`) {
			t.Fatalf("log does not carry %q: %s", "auth failed: "+res.Error, out)
		}
	})
}
