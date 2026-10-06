package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigateway"
	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/quota"
)

type oneLimit struct{}

func (oneLimit) ListRateLimits(context.Context) ([]db.RateLimit, error) {
	return []db.RateLimit{{
		ID: "rl1", Scope: quota.ScopeAPIKey, Subject: "key-1", Dimension: quota.DimensionRPM,
		Lim: 1, Burst: 1, Window: quota.WindowMinute,
	}}, nil
}

type noDailyUsage struct{}

func (noDailyUsage) SumDailyUsageEventsByAPIKey(context.Context, string) (int64, error) {
	return 0, nil
}
func (noDailyUsage) SumDailyUsageEventsByService(context.Context, string) (int64, error) {
	return 0, nil
}
func (noDailyUsage) CountDailyUsageEventsByAPIKey(context.Context, string) (int64, error) {
	return 0, nil
}
func (noDailyUsage) CountDailyUsageEventsByService(context.Context, string) (int64, error) {
	return 0, nil
}

// The 429 keeps its shape on the host route and takes the /ai/ shape when the
// request carries the gateway's error writer.
func TestQuotaMiddleware_DenialShape(t *testing.T) {
	deny := func(ctx context.Context) *httptest.ResponseRecorder {
		e := quota.NewWithStores(oneLimit{}, noDailyUsage{})
		if err := e.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		h := buildQuotaMiddleware(e, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		ctx = quota.WithSubjects(ctx, quota.Subjects{ServiceID: "svc1", APIKeyID: "key-1"})
		var rec *httptest.ResponseRecorder
		for range 2 { // the first call uses up the burst
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx))
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status %d, want 429", rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
		return rec
	}

	if got := deny(context.Background()).Body.String(); got != `{"error":"rate limit exceeded"}` {
		t.Fatalf("host route body = %s", got)
	}

	rec := deny(aigw.WithErrorWriter(context.Background(), aigateway.WriteError))
	var body struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the /ai/ shape: %s", rec.Body.String())
	}
	if body.Error.Type != "burrow_error" || body.Error.Code != "rate_limited" || body.Error.Message == "" {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// The error writer owns the whole response, Content-Type included.
	bare := func(w http.ResponseWriter, status int, _, _ string) { w.WriteHeader(status) }
	if ct := deny(aigw.WithErrorWriter(context.Background(), bare)).Header().Get("Content-Type"); ct != "" {
		t.Fatalf("middleware set Content-Type %q ahead of the error writer", ct)
	}
	if ct := deny(context.Background()).Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("host route Content-Type = %q", ct)
	}
}

type limitFor string

func (s limitFor) ListRateLimits(context.Context) ([]db.RateLimit, error) {
	return []db.RateLimit{{
		ID: "rl-gw", Scope: quota.ScopeAPIKey, Subject: string(s), Dimension: quota.DimensionRPM,
		Lim: 1, Burst: 1, Window: quota.WindowMinute,
	}}, nil
}

// A per-key limit counts each gateway key on its own: a gateway key reaches
// the chain without a service key id and is charged as "gw:<key id>".
func TestQuotaMiddleware_GatewayKeysAreCountedSeparately(t *testing.T) {
	e := quota.NewWithStores(limitFor("gw:gk-a"), noDailyUsage{})
	if err := e.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := aigw.NewChain(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	c.RateLimit = buildQuotaMiddleware(e, nil)
	up := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	do := func(keyID string) int {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		route := aigw.NewRoute(keyID, "openai", "m", "req")
		r = r.WithContext(aigw.WithErrorWriter(aigw.WithRoute(r.Context(), route), aigateway.WriteError))
		rec := httptest.NewRecorder()
		c.DispatchMetered(rec, r, "prov-zai", "host", "Authorization", "", true, up)
		return rec.Code
	}
	if got := do("gk-a"); got != http.StatusOK {
		t.Fatalf("gk-a, first call: %d", got)
	}
	if got := do("gk-a"); got != http.StatusTooManyRequests {
		t.Fatalf("gk-a, second call: %d, want 429", got)
	}
	if got := do("gk-b"); got != http.StatusOK {
		t.Fatalf("gk-b is charged to gk-a's limit: %d", got)
	}
}
