package aigateway

import (
	"io"
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
	var got string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		got = aigw.SystemTextFrom(r.Context())
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
	if v := do("bgw_k", "burrow-intelligence"); v != "M\n\nK" {
		t.Errorf("model and key: %q", v)
	}
	if v := do("bgw_none", "burrow-intelligence"); v != "M" {
		t.Errorf("model only: %q", v)
	}
	if v := do("bgw_k", "openrouter/google/gemini-x"); v != "K" {
		t.Errorf("direct address with a key text: %q", v)
	}
	if v := do("bgw_none", "openrouter/google/gemini-x"); v != "" {
		t.Errorf("direct address, no text: %q", v)
	}
}

func TestServe_SystemTextOfGatewayKeyOnProviderPath(t *testing.T) {
	var got string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		got = aigw.SystemTextFrom(r.Context())
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
	if v := do("bgw_k"); v != "K" {
		t.Errorf("gateway key: %q", v)
	}
	if v := do("sk-good"); v != "" {
		t.Errorf("service key: %q", v)
	}
}
