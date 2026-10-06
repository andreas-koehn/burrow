package aigw_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/quota"
)

// gatewayKeyRequest is a request as the /ai/ gateway hands it to the chain
// for a gateway key: no service key id, the gateway key id in the route.
func gatewayKeyRequest(keyID string) *http.Request {
	r := httptest.NewRequest("POST", "https://burrow.example.com/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	route := aigw.NewRoute(keyID, "openai", "m", "req")
	route.SetTarget("zai", "m")
	return r.WithContext(aigw.WithRoute(r.Context(), route))
}

// A cache scoped per API key keeps the answers of two gateway keys apart.
func TestChain_GatewayKeysDoNotShareAPerKeyCache(t *testing.T) {
	var hits atomic.Int32
	up := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := fmt.Sprintf(`{"answer":%d}`, hits.Add(1))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write([]byte(body))
	})
	sink := newMemSink()
	c := aigw.NewChain(freshCache(t), nil, nil, nil, nil, nil, nil, sink, testLog())
	c.Loader = staticLoader{ok: true, svc: aigw.Service{ID: "prov-zai", AIConfig: aigw.ServiceAIConfig{
		Cache: &exact.Settings{Enabled: true, AppliesPer: "per_api_key", TTLSeconds: 300, MaxEntries: 100, MaxPerEntryKB: 64},
	}}}
	do := func(keyID string) string {
		rec := httptest.NewRecorder()
		c.DispatchMetered(rec, gatewayKeyRequest(keyID), "prov-zai", "host", "Authorization", "", true, up)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", keyID, rec.Code)
		}
		return rec.Body.String()
	}
	if got := do("gk-a"); got != `{"answer":1}` {
		t.Fatalf("gk-a: %s", got)
	}
	if got := do("gk-b"); got != `{"answer":2}` {
		t.Fatalf("gk-b was served gk-a's cached answer: %s", got)
	}
	// The scope still caches: each key gets its own answer again.
	if got := do("gk-a"); got != `{"answer":1}` {
		t.Fatalf("gk-a, second call: %s", got)
	}
	if hits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits.Load())
	}
	// The usage row has no service key id; the gateway key has its own column.
	samples := sink.all()
	if len(samples) != 3 {
		t.Fatalf("usage rows = %d, want 3", len(samples))
	}
	for i, want := range []string{"gk-a", "gk-b", "gk-a"} {
		if samples[i].APIKeyID != "" || samples[i].GatewayKeyID != want {
			t.Fatalf("row %d: api_key_id %q gateway_key_id %q", i, samples[i].APIKeyID, samples[i].GatewayKeyID)
		}
	}
}

// The rate limiter sees a per-key subject for a gateway key, and the service
// key id wherever there is one.
func TestChain_QuotaSubjectOfAGatewayKey(t *testing.T) {
	var got []string
	c := aigw.NewChain(nil, nil, nil, nil, nil, nil, nil, nil, testLog())
	c.RateLimit = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = append(got, quota.SubjectsFromCtx(r.Context()).APIKeyID)
			next.ServeHTTP(w, r)
		})
	}
	up := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	c.DispatchMetered(httptest.NewRecorder(), gatewayKeyRequest("gk-a"), "svc1", "host", "Authorization", "", false, up)
	c.DispatchMetered(httptest.NewRecorder(), gatewayKeyRequest("gk-b"), "svc1", "host", "Authorization", "", false, up)
	c.DispatchMetered(httptest.NewRecorder(), gatewayKeyRequest("gk-a"), "svc1", "host", "Authorization", "key-1", false, up)
	c.DispatchMetered(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`)), "svc1", "host", "Authorization", "", false, up)
	want := []string{"gw:gk-a", "gw:gk-b", "key-1", ""}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("subjects = %q, want %q", got, want)
	}
}
