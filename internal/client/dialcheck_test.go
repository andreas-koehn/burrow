package client

import (
	"context"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// A server that accepts and then says nothing: Ctrl-C must end the check.
func TestCheckAuth_CancelEndsBlockedRead(t *testing.T) {
	dir := t.TempDir()
	s, cancel := startServer(t, dir, "")
	defer cancel()
	_ = s
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close() // never answers
		}
	}()
	ctx, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stop()
	start := time.Now()
	_, err := CheckAuth(ctx, Options{Server: l.Addr().String(), Token: "x", Insecure: true})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}
