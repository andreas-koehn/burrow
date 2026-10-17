package aigateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/db"
)

// What the chain is told to put before the app's system content: the model's
// text, then the key's, whichever address the app used.
func TestServeDialect_SystemTextInContext(t *testing.T) {
	var got, gotKey string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		got, gotKey = aigw.SystemTextFrom(r.Context()), aigw.KeySystemTextFrom(r.Context())
		w.WriteHeader(200)
	})
	g := globalGateway(up, &spyChain{})
	// burrow-intelligence goes to zai, a direct provider: the tunnel fake
	// carries the request over a pipe and loses its context.
	syn := g.Synthetic.(fakeSynthetic)
	m := syn["burrow-intelligence"]
	m.SystemText = "M"
	syn["burrow-intelligence"] = m
	g.GatewayKeys = fakeGatewayKeys{
		"bgw_k":    {ID: "gk-k", SystemText: "K"},
		"bgw_none": {ID: "gk-n"},
	}
	do := func(key, model string) string {
		got = "unset"
		rec := httptest.NewRecorder()
		g.ServeDialect(rec, post("/v1/chat/completions", key, `{"model":"`+model+`","messages":[]}`), DialectOpenAI)
		if rec.Code != 200 {
			t.Fatalf("%s %s: status %d %s", key, model, rec.Code, rec.Body.String())
		}
		return got
	}
	if v := do("bgw_k", "burrow-intelligence"); v != "M\n\nK" || gotKey != "K" {
		t.Errorf("model and key: %q, key part %q", v, gotKey)
	}
	if v := do("bgw_none", "burrow-intelligence"); v != "M" || gotKey != "" {
		t.Errorf("model only: %q, key part %q", v, gotKey)
	}
	if v := do("bgw_k", "openrouter/google/gemini-x"); v != "K" {
		t.Errorf("direct address with a key text: %q", v)
	}
	if v := do("bgw_none", "openrouter/google/gemini-x"); v != "" {
		t.Errorf("direct address, no text: %q", v)
	}
}

func TestServe_SystemTextOfGatewayKeyOnProviderPath(t *testing.T) {
	var got, gotKey string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		got, gotKey = aigw.SystemTextFrom(r.Context()), aigw.KeySystemTextFrom(r.Context())
		w.WriteHeader(200)
	})
	g := directGateway(t, func(db.AIProvider, aiprovider.ErrorWriter) (http.Handler, error) { return up, nil }, &spyChain{})
	g.GatewayKeys = fakeGatewayKeys{"bgw_k": {ID: "gk-k", SystemText: "K"}}
	do := func(key string) string {
		got = "unset"
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Body = io.NopCloser(strings.NewReader(`{"model":"mistral","messages":[]}`))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		g.Serve(rec, r, "openrouter")
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", key, rec.Code)
		}
		return got
	}
	if v := do("bgw_k"); v != "K" || gotKey != "K" {
		t.Errorf("gateway key: %q, key part %q", v, gotKey)
	}
	if v := do("sk-good"); v != "" {
		t.Errorf("service key: %q", v)
	}
}

// Through the real chain, with a fallback: the first target fails, the second
// answers and receives the text exactly once, in front of the app's own.
// Direct providers are used: the tunnel fake loses the request context.
func TestServeDialect_SystemTextOnceAfterFallback(t *testing.T) {
	var bodies []string
	var slugs []string
	g := globalGateway(nil, aigw.NewChain(nil, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
	g.Direct = func(p db.AIProvider, _ aiprovider.ErrorWriter) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			bodies, slugs = append(bodies, string(b)), append(slugs, p.Slug)
			if p.Slug == "zai" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"x","choices":[]}`))
		}), nil
	}
	syn := g.Synthetic.(fakeSynthetic)
	m := syn["burrow-intelligence"]
	m.SystemText = "M"
	syn["burrow-intelligence"] = m
	g.GatewayKeys = fakeGatewayKeys{"bgw_k": {ID: "gk-k", SystemText: "K"}}

	rec := httptest.NewRecorder()
	g.ServeDialect(rec, post("/v1/chat/completions", "bgw_k", `{"model":"burrow-intelligence","messages":[{"role":"system","content":"Be brief."}]}`), DialectOpenAI)
	if rec.Code != 200 || len(bodies) != 2 || slugs[0] != "zai" || slugs[1] != "openrouter" {
		t.Fatalf("status %d, attempts %v", rec.Code, slugs)
	}
	for i, b := range bodies {
		if n := strings.Count(b, "M\\n\\nK"); n != 1 {
			t.Errorf("attempt %d: text appears %d times: %s", i, n, b)
		}
		if !strings.Contains(b, "Be brief.") {
			t.Errorf("attempt %d lost the app's system message: %s", i, b)
		}
	}
}
