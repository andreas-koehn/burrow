package main

import (
	"context"
	"encoding/json"
	"io"
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
		// A retired setting must not stop the relay, even a path that does not exist.
		"BURROW_MCP_TOKEN_FILE": filepath.Join(dir, "no-such-token-file"),
		// An auth domain and an admin, for the MCP check below.
		"BURROW_AUTH_DOMAIN":    "relay.test",
		"BURROW_ADMIN_EMAIL":    serveTestAdminEmail,
		"BURROW_ADMIN_PASSWORD": serveTestAdminPassword,
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
			// The MCP server is mounted on the same address: a POST without a
			// token is challenged. 503 would mean MCPInfo.Handler is not wired.
			mresp, merr := http.Post("http://"+httpAddr+"/mcp/burrow", "application/json", strings.NewReader("{}"))
			if merr != nil {
				t.Fatalf("POST /mcp/burrow: %v", merr)
			}
			_ = mresp.Body.Close()
			if mresp.StatusCode != http.StatusUnauthorized || mresp.Header.Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("POST /mcp/burrow without a token: status %d, WWW-Authenticate %q; want 401 \"Bearer\" (the MCP handler is not wired into the router)",
					mresp.StatusCode, mresp.Header.Get("WWW-Authenticate"))
			}
			assertMCPKnowsTheAuthDomain(t, "http://"+httpAddr)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the dashboard listener never answered: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

const (
	serveTestAdminEmail    = "admin@relay.test"
	serveTestAdminPassword = "serve-test-password-1"
)

// assertMCPKnowsTheAuthDomain checks, on the relay `serve` started with an
// auth domain, that the MCP server replays its calls through the relay's own
// router (main.go's SetAPIHandler): service_access_mode_update accepts
// burrow_login, which the router refuses on a relay without an auth domain,
// and a server without the router answers "the relay is starting". Everything is done as an operator does it:
// log in, create a service and an automation token over the JSON API, then
// call the tool at /mcp/burrow.
func assertMCPKnowsTheAuthDomain(t *testing.T, base string) {
	t.Helper()
	var cookies []*http.Cookie
	csrf := ""
	do := func(path string, body any, want int) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("POST %s: status %d, want %d: %s", path, resp.StatusCode, want, out)
		}
		if len(resp.Cookies()) > 0 {
			cookies = resp.Cookies()
			for _, c := range cookies {
				if c.Name == "burrow_csrf" {
					csrf = c.Value
				}
			}
		}
		var decoded map[string]any
		_ = json.Unmarshal(out, &decoded)
		return decoded
	}

	do("/api/v1/auth/login", map[string]string{"email": serveTestAdminEmail, "password": serveTestAdminPassword}, http.StatusOK)
	if csrf == "" {
		t.Fatal("login set no burrow_csrf cookie")
	}
	const serviceID = "mcp-auth-domain"
	do("/api/v1/services", map[string]string{"service_id": serviceID, "title": "wiring"}, http.StatusCreated)
	minted := do("/api/v1/automation/tokens", map[string]any{
		"name": "mcp-auth-domain", "permissions": []string{"services:configure:own"},
	}, http.StatusCreated)
	bearer, _ := minted["plaintext"].(string)
	if bearer == "" {
		t.Fatalf("no automation token in the answer: %v", minted)
	}

	cs := mcpConnect(t, base+"/mcp/burrow", bearer, "2026-07-28")
	if got := mcpCall(t, cs, false, "service_access_mode_update", map[string]any{
		"serviceID": serviceID, "body": map[string]any{"access_mode": "burrow_login"},
	}); got != `{"status":204}` {
		t.Fatalf("service_access_mode_update burrow_login on a relay with an auth domain: %s", got)
	}
	var svc struct {
		AccessMode string `json:"access_mode"`
	}
	if err := json.Unmarshal([]byte(mcpCall(t, cs, false, "service_get", map[string]any{"serviceID": serviceID})), &svc); err != nil || svc.AccessMode != "burrow_login" {
		t.Fatalf("service_get after the change: access_mode %q, err %v", svc.AccessMode, err)
	}
}
