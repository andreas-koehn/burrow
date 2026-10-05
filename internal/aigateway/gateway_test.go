package aigateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
)

type fakeProviders map[string]db.AIProvider

func (f fakeProviders) ProviderBySlug(_ context.Context, slug string) (db.AIProvider, error) {
	p, ok := f[slug]
	if !ok {
		return db.AIProvider{}, db.ErrNotFound
	}
	return p, nil
}

type fakeKeys struct{ good, id string }

func (f fakeKeys) ValidateAPIKey(_ context.Context, _, presented string) (string, bool, error) {
	if presented == f.good {
		return f.id, true, nil
	}
	return "", false, nil
}

// fakeTunnels serves each dialled stream with upstream, over a net.Pipe.
type fakeTunnels struct {
	res      *proxy.Resolved
	upstream http.Handler
}

func (f fakeTunnels) LookupByServiceID(context.Context, string) (*proxy.Resolved, error) {
	if f.res == nil {
		return nil, proxy.ErrNotFound
	}
	cp := *f.res
	return &cp, nil
}

func (f fakeTunnels) DialTunnelStreamByServiceID(context.Context, string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		rec := httptest.NewRecorder()
		f.upstream.ServeHTTP(rec, req)
		_ = rec.Result().Write(server)
	}()
	return client, nil
}

type spyChain struct {
	serviceID, keyID string
	metered          bool
}

func (c *spyChain) Dispatch(w http.ResponseWriter, r *http.Request, serviceID, _, _, apiKeyID string, up http.Handler) {
	c.serviceID, c.keyID, c.metered = serviceID, apiKeyID, false
	up.ServeHTTP(w, r)
}

func (c *spyChain) DispatchMetered(w http.ResponseWriter, r *http.Request, serviceID, _, _, apiKeyID string, up http.Handler) {
	c.serviceID, c.keyID, c.metered = serviceID, apiKeyID, true
	up.ServeHTTP(w, r)
}

func newGateway(up http.Handler, chain Chain) *Gateway {
	return &Gateway{
		Providers: fakeProviders{"ollama": {Slug: "ollama", Name: "Ollama", Kind: "tunnel", ServiceID: "svc1", APIFormat: "openai"}},
		Keys:      fakeKeys{good: "sk-good", id: "key-1"},
		Tunnels: fakeTunnels{
			res:      &proxy.Resolved{ServiceID: "svc1", AccessMode: "api_key", LocalHost: "127.0.0.1:11434"},
			upstream: up,
		},
		Chain:      chain,
		PublicHost: "burrow.example.com",
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not the /ai/ shape: %s", rec.Body.String())
	}
	if body.Error.Type != "burrow_error" || body.Error.Message == "" {
		t.Fatalf("error body = %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	return body.Error.Code
}

func TestServe_ForwardsToTunnelAndStripsBurrowKey(t *testing.T) {
	var gotAuth, gotCookie, gotPath, gotHost string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		gotPath, gotHost = r.URL.Path, r.Host
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	chain := &spyChain{}
	g := newGateway(up, chain)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-good")
	req.Header.Set("Cookie", "burrow_session=abc")
	rec := httptest.NewRecorder()
	g.Serve(rec, req, "ollama")

	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if gotAuth != "" || gotCookie != "" {
		t.Fatalf("credentials reached the upstream: Authorization=%q Cookie=%q", gotAuth, gotCookie)
	}
	if gotPath != "/v1/chat/completions" || gotHost != "127.0.0.1:11434" {
		t.Fatalf("upstream saw path %q host %q", gotPath, gotHost)
	}
	if chain.serviceID != "svc1" || chain.keyID != "key-1" {
		t.Fatalf("chain got service %q key %q", chain.serviceID, chain.keyID)
	}
	if !chain.metered {
		t.Fatal("an inference call (POST) must be metered")
	}
	if rec.Header().Get("Burrow-Provider") != "ollama" {
		t.Fatalf("Burrow-Provider = %q", rec.Header().Get("Burrow-Provider"))
	}
}

// Listing models is not inference: it must not create usage rows.
func TestServe_GETIsNotMetered(t *testing.T) {
	chain := &spyChain{metered: true}
	g := newGateway(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }), chain)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-good")
	g.Serve(httptest.NewRecorder(), req, "ollama")
	if chain.serviceID != "svc1" || chain.metered {
		t.Fatalf("GET went through DispatchMetered (service %q metered %v)", chain.serviceID, chain.metered)
	}
}

func TestServe_AcceptsXAPIKeyHeader(t *testing.T) {
	g := newGateway(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("X-Api-Key", "sk-good")
	rec := httptest.NewRecorder()
	g.Serve(rec, req, "ollama")
	if rec.Code != 204 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestServe_Errors(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	withKey := func(k string) *http.Request {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		if k != "" {
			r.Header.Set("Authorization", "Bearer "+k)
		}
		return r
	}

	t.Run("unknown provider", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newGateway(ok, nil).Serve(rec, withKey("sk-good"), "nope")
		if rec.Code != 404 || errCode(t, rec) != "provider_not_found" {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newGateway(ok, nil).Serve(rec, withKey(""), "ollama")
		if rec.Code != 401 || errCode(t, rec) != "invalid_api_key" {
			t.Fatalf("status %d", rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("missing WWW-Authenticate")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newGateway(ok, nil).Serve(rec, withKey("sk-bad"), "ollama")
		if rec.Code != 401 || errCode(t, rec) != "invalid_api_key" {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("client offline", func(t *testing.T) {
		g := newGateway(ok, nil)
		g.Tunnels = fakeTunnels{}
		rec := httptest.NewRecorder()
		g.Serve(rec, withKey("sk-good"), "ollama")
		if rec.Code != 502 || errCode(t, rec) != "provider_offline" {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("service not key protected", func(t *testing.T) {
		g := newGateway(ok, nil)
		g.Tunnels = fakeTunnels{res: &proxy.Resolved{ServiceID: "svc1", AccessMode: "open"}, upstream: ok}
		rec := httptest.NewRecorder()
		g.Serve(rec, withKey("sk-good"), "ollama")
		if rec.Code != 403 || errCode(t, rec) != "provider_unavailable" {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("ip policy denies", func(t *testing.T) {
		g := newGateway(ok, nil)
		g.IPGeoDeny = func(*proxy.Resolved, *http.Request) bool { return true }
		rec := httptest.NewRecorder()
		g.Serve(rec, withKey("sk-good"), "ollama")
		if rec.Code != 403 || errCode(t, rec) != "forbidden" {
			t.Fatalf("status %d", rec.Code)
		}
	})
}

type errKeys struct{}

func (errKeys) ValidateAPIKey(context.Context, string, string) (string, bool, error) {
	return "", false, errors.New("db down: secret-detail")
}

func TestServe_ValidatorErrorDoesNotLeak(t *testing.T) {
	g := newGateway(http.NotFoundHandler(), nil)
	g.Keys = errKeys{}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-good")
	rec := httptest.NewRecorder()
	g.Serve(rec, req, "ollama")
	if rec.Code != 500 || errCode(t, rec) != "internal_error" {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret-detail") {
		t.Fatal("internal error text leaked to the client")
	}
}

func TestServe_StreamsWithoutBuffering(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: one\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	g := newGateway(up, nil)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-good")
	rec := httptest.NewRecorder()
	g.Serve(rec, req, "ollama")
	if !strings.Contains(rec.Body.String(), "data: one") || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
