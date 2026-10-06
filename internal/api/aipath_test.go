package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/aigateway"
	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/store"
)

type noProviders struct{}

func (noProviders) ProviderBySlug(_ context.Context, _ string) (db.AIProvider, error) {
	return db.AIProvider{}, db.ErrNotFound
}

func TestAIPathHandler_UnknownProviderIsJSON404(t *testing.T) {
	g := &aigateway.Gateway{Providers: noProviders{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := chi.NewRouter()
	h := AIPathHandler(g)
	r.Handle("/ai/{provider}", h)
	r.Handle("/ai/{provider}/*", h)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/ai/nope/v1/models", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d content-type %q body %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

// strictProviders fails the test on any lookup: a slug that is not a valid
// provider name must be refused before the store is asked.
type strictProviders struct{ t *testing.T }

func (s strictProviders) ProviderBySlug(_ context.Context, slug string) (db.AIProvider, error) {
	s.t.Errorf("store asked for invalid slug %q", slug)
	return db.AIProvider{Slug: slug, Kind: "tunnel", ServiceID: "svc1"}, nil
}

func TestAIPathHandler_ReservedAndInvalidSlugs(t *testing.T) {
	g := &aigateway.Gateway{Providers: strictProviders{t}, Log: discardLog()}
	r := chi.NewRouter()
	h := AIPathHandler(g)
	r.Handle("/ai/{provider}/*", h)
	for _, p := range []string{"/ai/v1/models", "/ai/A.B/v1/models"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound || aiErrCode(t, rec) != "provider_not_found" {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
}

func aiErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Type != "burrow_error" {
		t.Fatalf("not the /ai/ error shape: %q", rec.Body.String())
	}
	return body.Error.Code
}

// Token streams only work if nothing between the listener and the gateway
// buffers. The probe sits where the gate goes: same middleware stack as /ai/.
func TestRouter_TopLevelMiddlewareKeepsFlush(t *testing.T) {
	flushed := make(chan error, 1)
	probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data: one\n\n"))
		flushed <- http.NewResponseController(w).Flush()
	})
	srv := httptest.NewServer(NewRouter(Deps{Gate: probe, Log: discardLog()}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/__burrow/probe")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if flushErr := <-flushed; flushErr != nil {
		t.Fatalf("a middleware wraps the ResponseWriter without Flush support: %v", flushErr)
	}
}

func TestRouter_MountsAIGateway(t *testing.T) {
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>"))
	})
	g := &aigateway.Gateway{Providers: noProviders{}, Log: discardLog()}
	h := NewRouter(Deps{AIGateway: g, SPA: spa, Log: discardLog()})
	// "/ai" and "/ai/" name no provider; they must not fall through to the SPA.
	for _, p := range []string{"/ai", "/ai/", "/ai/nope", "/ai/nope/v1/models"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound || aiErrCode(t, rec) != "provider_not_found" {
			t.Errorf("%s: status %d body %s", p, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Burrow-Request-Id") == "" {
			t.Errorf("%s: missing Burrow-Request-Id", p)
		}
	}

}

type gwKeys map[string]store.GatewayKey

func (k gwKeys) ValidateGatewayKey(_ context.Context, presented string) (store.GatewayKey, bool, error) {
	key, ok := k[presented]
	return key, ok, nil
}

// The dialect endpoints answer at /openai/v1 and /ai/v1. Nothing under
// /openai/ or /ai/v1 is the dashboard's or a provider named "v1".
func TestRouter_MountsDialectEndpoints(t *testing.T) {
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>"))
	})
	// strictProviders: no path below may be taken for a provider path.
	strict := NewRouter(Deps{AIGateway: &aigateway.Gateway{Providers: strictProviders{t}, Log: discardLog()}, SPA: spa, Log: discardLog()})
	for _, p := range []string{"/openai/v1/models", "/ai/v1/models", "/openai/v1", "/ai/v1", "/ai/v1/", "/openai", "/openai/", "/openai/index.html", "/openai/v1/chat/completions"} {
		for _, method := range []string{"GET", "POST"} {
			rec := httptest.NewRecorder()
			strict.ServeHTTP(rec, httptest.NewRequest(method, p, strings.NewReader(`{"model":"x"}`)))
			if rec.Code != http.StatusUnauthorized || aiErrCode(t, rec) != "invalid_api_key" || rec.Header().Get("Burrow-Error-Code") != "invalid_api_key" {
				t.Errorf("%s %s: status %d body %s", method, p, rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Burrow-Request-Id") == "" {
				t.Errorf("%s %s: missing Burrow-Request-Id", method, p)
			}
		}
	}

	// With a key the gateway sees the path from "/v1" on, under both prefixes.
	var gotPath string
	g := &aigateway.Gateway{
		Providers:   oneDirect{},
		GatewayKeys: gwKeys{"bgw_k": {ID: "gk"}},
		Direct: func(db.AIProvider, aiprovider.ErrorWriter) (http.Handler, error) {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotPath = r.URL.Path }), nil
		},
		ServicePolicy: func(_ context.Context, id string) (*proxy.Resolved, error) {
			return &proxy.Resolved{ServiceID: id, AccessMode: "api_key"}, nil
		},
		Log: discardLog(),
	}
	h := NewRouter(Deps{AIGateway: g, SPA: spa, Log: discardLog()})
	for _, prefix := range []string{"/openai", "/ai"} {
		gotPath = ""
		req := httptest.NewRequest("POST", prefix+"/v1/chat/completions", strings.NewReader(`{"model":"zai/glm"}`))
		req.Header.Set("Authorization", "Bearer bgw_k")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || gotPath != "/v1/chat/completions" || rec.Header().Get("Burrow-Provider") != "zai" {
			t.Errorf("%s: status %d upstream path %q body %s", prefix, rec.Code, gotPath, rec.Body.String())
		}
	}
	for p, want := range map[string]string{"/openai/": "endpoint_not_found", "/openai/v2/models": "endpoint_not_found", "/ai/v1/nope": "endpoint_not_found"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Authorization", "Bearer bgw_k")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound || aiErrCode(t, rec) != want {
			t.Errorf("%s: status %d body %s", p, rec.Code, rec.Body.String())
		}
	}

	// /ai/<provider>/… still reaches the provider path.
	rec := httptest.NewRecorder()
	NewRouter(Deps{AIGateway: &aigateway.Gateway{Providers: noProviders{}, Log: discardLog()}, SPA: spa, Log: discardLog()}).
		ServeHTTP(rec, httptest.NewRequest("GET", "/ai/ollama/v1/models", nil))
	if rec.Code != http.StatusNotFound || aiErrCode(t, rec) != "provider_not_found" {
		t.Errorf("/ai/ollama/v1/models: status %d body %s", rec.Code, rec.Body.String())
	}
}

// Odd spellings of a path stay inside the dialect endpoint and are no
// endpoint of it: they reach neither an upstream, nor the API, nor the SPA.
func TestRouter_DialectOddPaths(t *testing.T) {
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the SPA was served")
		_, _ = w.Write([]byte("<html>"))
	})
	g := &aigateway.Gateway{
		Providers:   oneDirect{},
		GatewayKeys: gwKeys{"bgw_k": {ID: "gk"}},
		Direct: func(db.AIProvider, aiprovider.ErrorWriter) (http.Handler, error) {
			t.Error("an upstream was built")
			return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("an upstream was called") }), nil
		},
		ServicePolicy: func(_ context.Context, id string) (*proxy.Resolved, error) {
			return &proxy.Resolved{ServiceID: id, AccessMode: "api_key"}, nil
		},
		Log: discardLog(),
	}
	h := NewRouter(Deps{AIGateway: g, SPA: spa, Log: discardLog()})
	for _, prefix := range []string{"/openai", "/ai"} {
		for _, rest := range []string{
			"/v1/../../api/v1/users", "/v1/../../api/v1/auth/me", "/v1/../ollama/v1/chat/completions",
			"//v1/chat/completions", "/v1//chat/completions", "/v1/chat%2fcompletions", "/v1/chat%2Fcompletions",
			"/v1/chat/completions.", "/v1/./chat/completions", "/v1/chat/completions/..", "/v1/%2e%2e/%2e%2e/api/v1/users",
		} {
			if prefix == "/ai" && strings.HasPrefix(rest, "//") {
				continue // "/ai//v1/…" names no provider and is not this endpoint
			}
			for _, method := range []string{"GET", "POST"} {
				for key, want := range map[string]string{"": "invalid_api_key", "bgw_k": "endpoint_not_found"} {
					req := httptest.NewRequest(method, prefix+rest, strings.NewReader(`{"model":"zai/glm"}`))
					if key != "" {
						req.Header.Set("Authorization", "Bearer "+key)
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					status := http.StatusNotFound
					if key == "" {
						status = http.StatusUnauthorized
					}
					if rec.Code != status || aiErrCode(t, rec) != want || rec.Header().Get("Burrow-Error-Code") != want {
						t.Errorf("%s %s%s (key %q): status %d body %s", method, prefix, rest, key, rec.Code, rec.Body.String())
					}
					if rec.Header().Get("Burrow-Provider") != "" {
						t.Errorf("%s %s%s: routed to %s", method, prefix, rest, rec.Header().Get("Burrow-Provider"))
					}
				}
			}
		}
	}
	// "/ai//v1/…" is a provider path without a provider: the JSON 404.
	req := httptest.NewRequest("POST", "/ai//v1/chat/completions", strings.NewReader(`{"model":"zai/glm"}`))
	req.Header.Set("Authorization", "Bearer bgw_k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || aiErrCode(t, rec) != "provider_not_found" {
		t.Errorf("/ai//v1/chat/completions: status %d body %s", rec.Code, rec.Body.String())
	}
}

type oneDirect struct{}

func (oneDirect) ProviderBySlug(_ context.Context, slug string) (db.AIProvider, error) {
	return db.AIProvider{Slug: slug, Kind: "direct", ServiceID: "prov-" + slug, APIFormat: "openai"}, nil
}

type panicKeys struct{}

func (panicKeys) ValidateGatewayKey(context.Context, string) (store.GatewayKey, bool, error) {
	panic("boom: secret-detail")
}

// A panic on a dialect endpoint is answered as a JSON 500, like on /ai/<provider>/.
func TestRouter_DialectPanicIsJSON500(t *testing.T) {
	h := NewRouter(Deps{AIGateway: &aigateway.Gateway{GatewayKeys: panicKeys{}, Log: discardLog()}, Log: discardLog()})
	req := httptest.NewRequest("GET", "/openai/v1/models", nil)
	req.Header.Set("Authorization", "Bearer bgw_k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || aiErrCode(t, rec) != "internal_error" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-detail") {
		t.Fatal("panic text leaked to the client")
	}
}

type panicProviders struct{}

func (panicProviders) ProviderBySlug(context.Context, string) (db.AIProvider, error) {
	panic("boom: secret-detail")
}

// A panic under /ai/ is still answered in the /ai/ error shape.
func TestRouter_AIPanicIsJSON500(t *testing.T) {
	h := NewRouter(Deps{AIGateway: &aigateway.Gateway{Providers: panicProviders{}, Log: discardLog()}, Log: discardLog()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ai/ollama/v1/models", nil))
	if rec.Code != http.StatusInternalServerError || aiErrCode(t, rec) != "internal_error" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-detail") {
		t.Fatal("panic text leaked to the client")
	}
}

type oneProvider struct{}

func (oneProvider) ProviderBySlug(_ context.Context, slug string) (db.AIProvider, error) {
	return db.AIProvider{Slug: slug, Kind: "tunnel", ServiceID: "svc1"}, nil
}

type anyKey struct{}

func (anyKey) ValidateAPIKey(context.Context, string, string) (string, bool, error) {
	return "key-1", true, nil
}

type liveTunnel struct{}

func (liveTunnel) LookupByServiceID(context.Context, string) (*proxy.Resolved, error) {
	return &proxy.Resolved{ServiceID: "svc1", AccessMode: "api_key", LocalHost: "127.0.0.1:11434"}, nil
}

func (liveTunnel) DialTunnelStreamByServiceID(context.Context, string) (net.Conn, error) {
	return nil, proxy.ErrNotFound
}

// chainFunc answers in place of the AI chain.
type chainFunc func(w http.ResponseWriter, r *http.Request)

func (f chainFunc) Dispatch(w http.ResponseWriter, r *http.Request, _, _, _, _ string, _ http.Handler) {
	f(w, r)
}

func (f chainFunc) DispatchMetered(w http.ResponseWriter, r *http.Request, _, _, _, _ string, _ bool, _ http.Handler) {
	f(w, r)
}

// servePanicking runs one /ai/ request whose handler is f and returns what
// the handler panicked with.
func servePanicking(t *testing.T, f chainFunc) (rec *httptest.ResponseRecorder, panicked any) {
	t.Helper()
	g := &aigateway.Gateway{Providers: oneProvider{}, Keys: anyKey{}, Tunnels: liveTunnel{}, Chain: f, Log: discardLog()}
	r := chi.NewRouter()
	r.Handle("/ai/{provider}/*", AIPathHandler(g))
	req := httptest.NewRequest("GET", "/ai/ollama/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-good")
	rec = httptest.NewRecorder()
	defer func() { panicked = recover() }()
	r.ServeHTTP(rec, req)
	return rec, nil
}

// http.ErrAbortHandler is how a handler asks the server to drop the
// connection: it is passed on, not turned into a 500.
func TestAIPathHandler_AbortPanicPropagates(t *testing.T) {
	rec, panicked := servePanicking(t, func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	if panicked != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", panicked)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("a body was written: %s", rec.Body.String())
	}
}

// Once part of a response is out, a JSON error can only corrupt it: the
// connection is aborted instead.
func TestAIPathHandler_PanicAfterWriteAbortsWithoutJSON(t *testing.T) {
	rec, panicked := servePanicking(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: one\n\n"))
		panic("boom: secret-detail")
	})
	if panicked != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", panicked)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "data: one\n\n" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}
