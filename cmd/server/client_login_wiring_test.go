package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/devcert"
)

// freeAddr returns a loopback address nothing listens on right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// TestServe_ClientLoginIsWired runs `burrowd serve` as main does and asks the
// sign-in start route. Without the store in the router's dependencies all
// five /client/login routes answer 404, which a client reads as "this relay
// has no browser sign-in": a missing wire would fail silently. The route must
// answer with a sign-in request.
func TestServe_ClientLoginIsWired(t *testing.T) {
	dir := t.TempDir()
	certs := filepath.Join(dir, "certs")
	if err := devcert.Generate(certs, false); err != nil {
		t.Fatal(err)
	}
	httpAddr := freeAddr(t)
	for k, v := range map[string]string{
		"BURROW_LISTEN":        freeAddr(t),
		"BURROW_HTTP_LISTEN":   httpAddr,
		"BURROW_PUBLIC_BIND":   "127.0.0.1",
		"BURROW_TLS_CERT":      filepath.Join(certs, "dev-server.pem"),
		"BURROW_TLS_KEY":       filepath.Join(certs, "dev-server-key.pem"),
		"BURROW_DATABASE_PATH": filepath.Join(dir, "burrow.db"),
		"BURROW_BACKUP_DIR":    filepath.Join(dir, "backups"),
		"BURROW_LOG_LEVEL":     "error",
		"BURROW_DATABASE_URL":  "",
		"BURROW_ACME_DOMAIN":   "",
		"BURROW_MCP_LISTEN":    "",
	} {
		t.Setenv(k, v)
	}

	ctx, cancel := context.WithCancel(context.Background())
	root := newRootCmd()
	root.SetArgs([]string{"serve"})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	stopped := false
	defer func() {
		cancel()
		if stopped {
			return
		}
		select {
		case <-done:
		case <-time.After(90 * time.Second):
			t.Error("serve did not stop after its context was cancelled")
		}
	}()

	url := "http://" + httpAddr + "/api/v1/client/login/start"
	body := `{"hostname":"h","os":"linux","arch":"amd64","client_version":"0.6.0"}`
	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("serve ended before it answered: %v", err)
		default:
		}
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				t.Fatal("POST /api/v1/client/login/start answers 404: the sign-in store is not wired into the router")
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("POST /api/v1/client/login/start: status %d, want 200", resp.StatusCode)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the dashboard listener never answered: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
