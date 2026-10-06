package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// fakeAttempts implements the attempt-log lookup over a map and records the
// ids it was asked for.
type fakeAttempts struct {
	rows  map[string][]db.UsageAttempt
	err   error
	asked []string
}

func (f *fakeAttempts) AttemptsForRequest(_ context.Context, id string) ([]db.UsageAttempt, error) {
	f.asked = append(f.asked, id)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[id], nil
}

func attemptDeps(role string) (Deps, *fakeAttempts) {
	ts := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	fa := &fakeAttempts{rows: map[string][]db.UsageAttempt{
		"req-1": {
			{RequestID: "req-1", Position: 0, Ts: ts, ProviderSlug: "zai", TargetModel: "glm-5.1", Status: 500, ErrorCode: "http_500", DurationMs: 812},
			{RequestID: "req-1", Position: 1, Ts: ts.Add(time.Second), ProviderSlug: "openrouter", TargetModel: "google/gemini-x", Status: 200, DurationMs: 1403},
		},
		"relay-1/AbC-000042": {{RequestID: "relay-1/AbC-000042", ProviderSlug: "zai", TargetModel: "glm-5.1", ErrorCode: "timeout", DurationMs: 60000, Ts: ts}},
	}}
	auto := newFakeAutomationStore()
	return Deps{
		Users: &fakeUserStore{role: role}, AIAttempts: fa, Automation: auto, Bearer: auto,
		AuthDomain: "burrow.example.com", Log: discardLog(),
	}, fa
}

func TestGetAttempts(t *testing.T) {
	const path = "/api/v1/ai/requests/req-1/attempts"
	serve := func(t *testing.T, d Deps) (*httptest.Server, *authClient) {
		srv, c := newAIProviderServer(t, d)
		t.Cleanup(srv.Close)
		return srv, c
	}

	t.Run("admin reads the log in position order", func(t *testing.T) {
		d, _ := attemptDeps("admin")
		_, c := serve(t, d)
		resp := c.get(t, path)
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q", cc)
		}
		body := strings.TrimSpace(wantStatus(t, resp, http.StatusOK))
		want := `[{"position":0,"provider":"zai","model":"glm-5.1","status":500,"error_code":"http_500","duration_ms":812,"ts":"2026-10-06T09:30:00Z"},` +
			`{"position":1,"provider":"openrouter","model":"google/gemini-x","status":200,"error_code":"","duration_ms":1403,"ts":"2026-10-06T09:30:01Z"}]`
		if body != want {
			t.Errorf("got  %s\nwant %s", body, want)
		}
	})

	t.Run("unknown id and no store are an empty list", func(t *testing.T) {
		d, _ := attemptDeps("admin")
		_, c := serve(t, d)
		if body := strings.TrimSpace(wantStatus(t, c.get(t, "/api/v1/ai/requests/nope/attempts"), http.StatusOK)); body != "[]" {
			t.Errorf("body = %s", body)
		}
		d.AIAttempts = nil
		_, c = serve(t, d)
		if body := strings.TrimSpace(wantStatus(t, c.get(t, path), http.StatusOK)); body != "[]" {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("an id with a slash is sent percent-encoded", func(t *testing.T) {
		d, fa := attemptDeps("admin")
		_, c := serve(t, d)
		body := wantStatus(t, c.get(t, "/api/v1/ai/requests/relay-1%2FAbC-000042/attempts"), http.StatusOK)
		if !strings.Contains(body, `"error_code":"timeout"`) || len(fa.asked) != 1 || fa.asked[0] != "relay-1/AbC-000042" {
			t.Errorf("asked %q, body = %s", fa.asked, body)
		}
	})

	t.Run("refused callers reach no store", func(t *testing.T) {
		for _, role := range []string{"user", "viewer"} {
			d, fa := attemptDeps(role)
			_, c := serve(t, d)
			if body := wantStatus(t, c.get(t, path), http.StatusForbidden); strings.Contains(body, "zai") {
				t.Errorf("%s: body = %s", role, body)
			}
			if len(fa.asked) != 0 {
				t.Errorf("%s: store was asked for %q", role, fa.asked)
			}
		}

		d, fa := attemptDeps("admin")
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		wantStatus(t, (&authClient{base: srv.URL, hc: &http.Client{}}).get(t, path), http.StatusUnauthorized)
		// The role of a token's user alone opens nothing: the log is read
		// from the dashboard.
		for _, perms := range [][]string{nil, {"ai:configure:any"}, {"inspector:read:any"}} {
			_, token, err := d.Automation.(*fakeAutomationStore).MintAutomationToken(context.Background(), "u-self", "admin", "ci", perms, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus(t, bearerDo(t, srv, token, http.MethodGet, path, nil), http.StatusForbidden)
		}
		if len(fa.asked) != 0 {
			t.Errorf("store was asked for %q", fa.asked)
		}
	})

	t.Run("malformed ids", func(t *testing.T) {
		d, fa := attemptDeps("admin")
		_, c := serve(t, d)
		for _, id := range []string{strings.Repeat("a", 129), "a%00b", "a%0Ab", "a%7Fb"} {
			body := wantStatus(t, c.get(t, "/api/v1/ai/requests/"+id+"/attempts"), http.StatusBadRequest)
			if !strings.Contains(body, msgAttemptRequestID) {
				t.Errorf("%q: body = %s", id, body)
			}
		}
		wantStatus(t, c.get(t, "/api/v1/ai/requests/"+strings.Repeat("a", 128)+"/attempts"), http.StatusOK)
		if len(fa.asked) != 1 {
			t.Errorf("a malformed id reached the store: %q", fa.asked)
		}
	})

	t.Run("a store failure is a plain 500", func(t *testing.T) {
		d, fa := attemptDeps("admin")
		fa.err = errors.New("pq: relation usage_attempts is on fire")
		_, c := serve(t, d)
		if body := wantStatus(t, c.get(t, path), http.StatusInternalServerError); strings.Contains(body, "fire") {
			t.Errorf("body = %s", body)
		}
	})
}
