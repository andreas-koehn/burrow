// cmd/server/e2e_gateway_tunnel_test.go
//
// Tunnel providers on the global gateway, through the same wiring as
// e2e_gateway_test.go. The tunnel registry is replaced by a stand-in that
// opens a TCP connection to a local test server where the control server
// would open a stream to a client; everything from the router to the tunnel
// transport is the real thing.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/server"
)

// gwTunnels stands in for the tunnel registry: one live tunnel per service
// id, each ending at a local test server.
type gwTunnels struct {
	mu      sync.Mutex
	tunnels map[string]*server.Tunnel // by service id
	backend map[string]string         // tunnel id -> address of its test server
}

func (g *gwTunnels) connect(serviceID, addr string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tunnels == nil {
		g.tunnels, g.backend = map[string]*server.Tunnel{}, map[string]string{}
	}
	id := "tn-" + serviceID
	g.tunnels[serviceID] = &server.Tunnel{ID: id, Name: serviceID, Type: "http", LocalAddr: "127.0.0.1:11434", ServiceID: serviceID, IsHTTP: true}
	g.backend[id] = addr
}

func (g *gwTunnels) LookupHTTPTunnelByServiceID(serviceID string) (*server.Tunnel, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	tn, ok := g.tunnels[serviceID]
	return tn, ok
}

func (g *gwTunnels) OpenTunnelStream(ctx context.Context, tn *server.Tunnel) (net.Conn, error) {
	g.mu.Lock()
	addr := g.backend[tn.ID]
	g.mu.Unlock()
	if addr == "" {
		return nil, fmt.Errorf("no backend for tunnel %s", tn.ID)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (g *gwTunnels) LookupSessionByTunnelID(string) (sessionID, userID string, ok bool) {
	return "", "", false
}

// gwTunnelApp is what runs behind one tunnel. It records what it received.
type gwTunnelApp struct {
	srv *httptest.Server

	mu     sync.Mutex
	seen   []http.Header
	status int // 0 = 200
}

func newGWTunnelApp(t *testing.T, name string) *gwTunnelApp {
	t.Helper()
	a := &gwTunnelApp{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		a.mu.Lock()
		a.seen = append(a.seen, r.Header.Clone())
		status := a.status
		a.mu.Unlock()
		if status != 0 {
			gwJSON(w, status, map[string]any{"error": map[string]string{"message": "boom"}})
			return
		}
		gwJSON(w, 200, map[string]any{
			"id": "chatcmpl-tunnel", "object": "chat.completion", "model": "local",
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "hi from " + name}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
		})
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *gwTunnelApp) fail(status int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status = status
}

func (a *gwTunnelApp) requests() []http.Header {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]http.Header(nil), a.seen...)
}

// The upstream credential bound to the first target's service must not reach
// a fallback target, and each target gets the one bound to its own service.
func TestE2E_GatewayTunnelCredentials(t *testing.T) {
	const (
		secretOne = "sk-upstream-tunnel-one-0007"
		secretTwo = "sk-upstream-tunnel-two-0008"
	)
	t.Setenv("BURROW_UPSTREAM_KEY_TUNONE", secretOne)
	t.Setenv("BURROW_UPSTREAM_KEY_TUNTWO", secretTwo)

	tunnels := &gwTunnels{}
	e := bootGatewayE2EWith(t, tunnels)
	ctx := context.Background()
	wrapped := db.Wrap(e.sqldb)
	admin, err := wrapped.GetUserByEmail(ctx, e.adminEmail)
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	one, two := newGWTunnelApp(t, "one"), newGWTunnelApp(t, "two")
	for id, app := range map[string]*gwTunnelApp{"svc-tun-one": one, "svc-tun-two": two} {
		name := strings.TrimPrefix(id, "svc-")
		if err := wrapped.CreateService(ctx, db.Service{ID: id, UserID: admin.ID, Name: name, Type: "http", AccessMode: "api_key"}); err != nil {
			t.Fatalf("create service %s: %v", id, err)
		}
		tunnels.connect(id, app.srv.Listener.Addr().String())
		if code, body := e.admin(t, "POST", "/api/v1/ai/providers", map[string]any{"slug": name, "name": name, "kind": "tunnel", "service_id": id}); code != http.StatusCreated {
			t.Fatalf("create provider %s: %d %s", name, code, body)
		}
	}
	if code, body := e.admin(t, "POST", "/api/v1/ai/models", map[string]any{"name": "local", "targets": []map[string]string{
		{"dialect": "openai", "provider": "tun-one", "model": "m-one"},
		{"dialect": "openai", "provider": "tun-two", "model": "m-two"},
	}}); code != http.StatusCreated {
		t.Fatalf("create model: %d %s", code, body)
	}
	code, body := e.admin(t, "POST", "/api/v1/ai/keys", map[string]any{"name": "all"})
	var key struct{ Key string }
	if err := json.Unmarshal(body, &key); code != http.StatusCreated || err != nil || key.Key == "" {
		t.Fatalf("create key: %d %s", code, body)
	}
	bind := func(serviceID string, binding map[string]string) {
		t.Helper()
		if code, body := e.admin(t, "PUT", "/api/v1/services/"+serviceID+"/upstream-credential", binding); code/100 != 2 {
			t.Fatalf("bind %s: %d %s", serviceID, code, body)
		}
	}
	const chat = `{"model":"local","messages":[{"role":"user","content":"hi"}]}`
	send := func(t *testing.T) gwResp {
		t.Helper()
		return e.call(t, "POST", "/openai/v1/chat/completions", chat, "Authorization", "Bearer "+key.Key, "Cookie", "burrow_session=client")
	}
	// carries reports whether any header of h holds secret.
	carries := func(h http.Header, secret string) bool {
		for _, vals := range h {
			for _, v := range vals {
				if strings.Contains(v, secret) {
					return true
				}
			}
		}
		return false
	}
	last := func(t *testing.T, a *gwTunnelApp, before int) http.Header {
		t.Helper()
		reqs := a.requests()
		if len(reqs) != before+1 {
			t.Fatalf("the app received %d requests, want 1", len(reqs)-before)
		}
		return reqs[before]
	}

	t.Run("01 A binding on the first target only", func(t *testing.T) {
		bind("svc-tun-one", map[string]string{"slot": "TUNONE"}) // Authorization: Bearer <key>
		one.fail(500)
		defer one.fail(0)
		n1, n2 := len(one.requests()), len(two.requests())
		r := send(t)
		if r.Status != 200 || r.Header.Get("Burrow-Provider") != "tun-two" || r.Header.Get("Burrow-Attempts") != "2" {
			t.Fatalf("status %d headers %v body %s", r.Status, r.Header, r.Body)
		}
		h1, h2 := last(t, one, n1), last(t, two, n2)
		if got := h1.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+secretOne {
			t.Fatalf("the first target did not get its own credential (%d Authorization values)", len(got))
		}
		if carries(h2, secretOne) || len(h2.Values("Authorization")) != 0 {
			t.Fatal("the fallback target received the first target's credential")
		}
		for _, h := range []http.Header{h1, h2} {
			if carries(h, key.Key) || h.Get("Cookie") != "" || h.Get("X-Api-Key") != "" {
				t.Fatal("a client credential reached an upstream")
			}
		}
		if carries(r.Header, secretOne) || strings.Contains(string(r.Body), secretOne) {
			t.Fatal("the upstream credential is in the response")
		}
	})

	t.Run("02 Each target its own binding", func(t *testing.T) {
		bind("svc-tun-two", map[string]string{"slot": "TUNTWO", "header_name": "X-Upstream-Key", "header_format": "{key}"})
		one.fail(500)
		defer one.fail(0)
		n1, n2 := len(one.requests()), len(two.requests())
		r := send(t)
		if r.Status != 200 || r.Header.Get("Burrow-Provider") != "tun-two" {
			t.Fatalf("status %d headers %v body %s", r.Status, r.Header, r.Body)
		}
		h1, h2 := last(t, one, n1), last(t, two, n2)
		if h1.Get("Authorization") != "Bearer "+secretOne || carries(h1, secretTwo) || len(h1.Values("X-Upstream-Key")) != 0 {
			t.Fatal("the first target did not get exactly its own credential")
		}
		if got := h2.Values("X-Upstream-Key"); len(got) != 1 || got[0] != secretTwo || carries(h2, secretOne) || len(h2.Values("Authorization")) != 0 {
			t.Fatal("the fallback target did not get exactly its own credential")
		}
	})

	t.Run("03 One target, as before", func(t *testing.T) {
		n1, n2 := len(one.requests()), len(two.requests())
		r := send(t)
		if r.Status != 200 || r.Header.Get("Burrow-Provider") != "tun-one" || r.Header.Get("Burrow-Attempts") != "1" {
			t.Fatalf("status %d headers %v body %s", r.Status, r.Header, r.Body)
		}
		if got := last(t, one, n1).Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+secretOne {
			t.Fatalf("the target got %d Authorization values, want its own, once", len(got))
		}
		if len(two.requests()) != n2 {
			t.Fatal("the second target was called")
		}
		// The provider path, with a service key of that provider.
		code, body := e.admin(t, "POST", "/api/v1/services/svc-tun-two/api-keys", map[string]any{"name": "k"})
		var sk struct{ Key string }
		if err := json.Unmarshal(body, &sk); code/100 != 2 || err != nil || sk.Key == "" {
			t.Fatalf("create service key: %d %s", code, body)
		}
		r = e.call(t, "POST", "/ai/tun-two/v1/chat/completions", `{"model":"m-two","messages":[]}`, "Authorization", "Bearer "+sk.Key)
		if r.Status != 200 {
			t.Fatalf("provider path: status %d body %s", r.Status, r.Body)
		}
		h := last(t, two, n2)
		if got := h.Values("X-Upstream-Key"); len(got) != 1 || got[0] != secretTwo || carries(h, secretOne) || carries(h, sk.Key) {
			t.Fatal("provider path: the target did not get exactly its own credential")
		}
	})
}
