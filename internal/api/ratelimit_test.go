package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/quota"
	"github.com/ankoehn/burrow/internal/store"
)

// TestLoginRateLimitPerIP verifies that the per-IP rate limiter on
// POST /api/v1/auth/login returns HTTP 429 after the configured limit and
// that the first N requests pass through to the handler (receiving 401 or
// 200, not 429).
func TestLoginRateLimitPerIP(t *testing.T) {
	const perIP = 3 // inject a tiny per-IP limit for the test
	au := &authUsers{verify: func(_, _ string) (bool, error) { return false, nil }}
	ts := newTestServer(Deps{
		Users:                        au,
		Log:                          discardLog(),
		LoginRateLimitPerIPOverride:  perIP,
		LoginRateLimitGlobalOverride: 1000, // don't let global fire
	})
	defer ts.Close()

	// First perIP requests must reach the handler (expect 401 invalid-creds).
	var body *strings.Reader
	for i := 0; i < perIP; i++ {
		body = strings.NewReader(`{"email":"a@x","password":"bad"}`)
		resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json", body)
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d: got 429 before limit exhausted (limit=%d)", i+1, perIP)
		}
	}

	// Request perIP+1 must be rate-limited (429).
	body = strings.NewReader(`{"email":"a@x","password":"bad"}`)
	resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json", body)
	if err != nil {
		t.Fatalf("over-limit request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want 429 on over-limit login, got %d", resp.StatusCode)
	}
	b := readBody(t, resp)
	if !strings.Contains(b, `"error"`) {
		t.Fatalf("429 body must contain JSON error field, got %q", b)
	}
}

// TestLoginRateLimitGlobal verifies that the global endpoint cap fires when
// the per-IP limit is high but the global limit is crossed.
func TestLoginRateLimitGlobal(t *testing.T) {
	const global = 3
	au := &authUsers{verify: func(_, _ string) (bool, error) { return false, nil }}
	ts := newTestServer(Deps{
		Users:                        au,
		Log:                          discardLog(),
		LoginRateLimitPerIPOverride:  1000, // don't let per-IP fire
		LoginRateLimitGlobalOverride: global,
	})
	defer ts.Close()

	for i := 0; i < global; i++ {
		body := strings.NewReader(`{"email":"a@x","password":"bad"}`)
		resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json", body)
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d: got 429 before global limit exhausted (global=%d)", i+1, global)
		}
	}

	body := strings.NewReader(`{"email":"a@x","password":"bad"}`)
	resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json", body)
	if err != nil {
		t.Fatalf("over-limit request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want 429 on over-global-limit login, got %d", resp.StatusCode)
	}
}

// TestLoginRateLimitDoesNotAffectOtherEndpoints asserts that the login rate
// limiter is scoped only to POST /auth/login and has no effect on other
// routes. We send many requests to GET /api/v1/me (a session-protected route)
// and confirm none receive 429.
func TestLoginRateLimitDoesNotAffectOtherEndpoints(t *testing.T) {
	const perIP = 2 // very low login limit to prove scoping
	au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
	ts := newTestServer(Deps{
		Users:                        au,
		Log:                          discardLog(),
		LoginRateLimitPerIPOverride:  perIP,
		LoginRateLimitGlobalOverride: perIP,
	})
	defer ts.Close()

	// Log in once to get a session cookie.
	jar, _ := cookiejarNew()
	cl := &http.Client{Jar: jar}
	loginResp, err := cl.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login: want 200, got %d", loginResp.StatusCode)
	}

	// Now call GET /api/v1/me many more times than perIP — none should be 429.
	const attempts = 10
	for i := 0; i < attempts; i++ {
		resp, err := cl.Get(ts.URL + "/api/v1/me")
		if err != nil {
			t.Fatalf("me request %d: %v", i+1, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("GET /api/v1/me request %d returned 429 — login limiter must NOT affect other endpoints", i+1)
		}
	}
}

// ---------------------------------------------------------------------------
// v0.4.0 Task 11: rate-limit + quota JSON API tests.
// ---------------------------------------------------------------------------

// fakeRateLimitStore is an in-memory RateLimitStore for the handler tests.
// It mirrors the *db.DB CRUD shape but lives entirely in memory.
type fakeRateLimitStore struct {
	mu   sync.Mutex
	rows map[string]db.RateLimit
}

func newFakeRateLimitStore() *fakeRateLimitStore {
	return &fakeRateLimitStore{rows: map[string]db.RateLimit{}}
}

func (f *fakeRateLimitStore) ListRateLimits(_ context.Context) ([]db.RateLimit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.RateLimit, 0, len(f.rows))
	for _, rl := range f.rows {
		out = append(out, rl)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].Dimension < out[j].Dimension
	})
	return out, nil
}

func (f *fakeRateLimitStore) GetRateLimit(_ context.Context, id string) (db.RateLimit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rl, ok := f.rows[id]
	if !ok {
		return db.RateLimit{}, db.ErrNotFound
	}
	return rl, nil
}

func (f *fakeRateLimitStore) CreateRateLimit(_ context.Context, rl db.RateLimit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[rl.ID] = rl
	return nil
}

func (f *fakeRateLimitStore) UpdateRateLimit(_ context.Context, rl db.RateLimit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[rl.ID]; !ok {
		return db.ErrNotFound
	}
	f.rows[rl.ID] = rl
	return nil
}

func (f *fakeRateLimitStore) DeleteRateLimit(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[id]; !ok {
		return db.ErrNotFound
	}
	delete(f.rows, id)
	return nil
}

// fakeQuotaEngine is a stub QuotaEngine that records Reload calls and
// returns canned UsageFor rows.
type fakeQuotaEngine struct {
	mu          sync.Mutex
	reloadCount int
	usage       []quota.Usage
	limits      []quota.Limit
	asked       []quota.Subjects
}

func (f *fakeQuotaEngine) Reload(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadCount++
	return nil
}

func (f *fakeQuotaEngine) UsageFor(_ context.Context, who quota.Subjects) []quota.Usage {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, who)
	out := make([]quota.Usage, len(f.usage))
	copy(out, f.usage)
	return out
}

func (f *fakeQuotaEngine) Limits() []quota.Limit {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]quota.Limit, len(f.limits))
	copy(out, f.limits)
	return out
}

func (f *fakeQuotaEngine) DropBucket(_, _, _, _ string) {}

// TestRateLimitHandler_GetEmpty — empty store returns a non-null JSON
// empty array.
func TestRateLimitHandler_GetEmpty(t *testing.T) {
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		RateLimitDB: newFakeRateLimitStore(),
		RateLimits:  &fakeQuotaEngine{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/rate-limits")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []rateLimitResp
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if out == nil || len(out) != 0 {
		t.Fatalf("want empty non-nil array; got %v", out)
	}
}

// TestRateLimitHandler_PostOK — happy path: a valid POST returns 201 with
// the created row including a server-allocated id and triggers a Reload.
func TestRateLimitHandler_PostOK(t *testing.T) {
	engine := &fakeQuotaEngine{}
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		RateLimitDB: newFakeRateLimitStore(),
		RateLimits:  engine,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	body := map[string]any{
		"scope": "api_key", "subject": "k1", "dimension": "rpm",
		"limit": 60, "burst": 60, "window": "minute",
	}
	r := c.post(t, "/api/v1/rate-limits", body)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got rateLimitResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if got.ID == "" {
		t.Fatal("server must allocate an id")
	}
	if got.Scope != "api_key" || got.Subject != "k1" ||
		got.Dimension != "rpm" || got.Limit != 60 ||
		got.Burst != 60 || got.Window != "minute" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if engine.reloadCount != 1 {
		t.Errorf("Reload not called after POST: count=%d", engine.reloadCount)
	}
}

// TestRateLimitHandler_PostBadInputs — 400 paths.
func TestRateLimitHandler_PostBadInputs(t *testing.T) {
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		RateLimitDB: newFakeRateLimitStore(),
		RateLimits:  &fakeQuotaEngine{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"bad scope", map[string]any{
			"scope": "garbage", "subject": "k1", "dimension": "rpm",
			"limit": 1, "burst": 1, "window": "minute"}},
		{"bad dimension", map[string]any{
			"scope": "api_key", "subject": "k1", "dimension": "xxx",
			"limit": 1, "burst": 1, "window": "minute"}},
		{"bad window", map[string]any{
			"scope": "api_key", "subject": "k1", "dimension": "rpm",
			"limit": 1, "burst": 1, "window": "century"}},
		{"zero limit", map[string]any{
			"scope": "api_key", "subject": "k1", "dimension": "rpm",
			"limit": 0, "burst": 1, "window": "minute"}},
		{"negative burst", map[string]any{
			"scope": "api_key", "subject": "k1", "dimension": "rpm",
			"limit": 5, "burst": -1, "window": "minute"}},
		{"global with subject", map[string]any{
			"scope": "global", "subject": "x", "dimension": "rpm",
			"limit": 5, "burst": 5, "window": "minute"}},
		{"non-global without subject", map[string]any{
			"scope": "api_key", "subject": "", "dimension": "rpm",
			"limit": 5, "burst": 5, "window": "minute"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := c.post(t, "/api/v1/rate-limits", tc.body)
			if r.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
			}
		})
	}
}

// TestRateLimitHandler_PutOK — successful update returns 204, persists the
// new fields, and triggers a Reload.
func TestRateLimitHandler_PutOK(t *testing.T) {
	store := newFakeRateLimitStore()
	store.rows["rl1"] = db.RateLimit{
		ID: "rl1", Scope: "api_key", Subject: "k1", Dimension: "rpm",
		Lim: 5, Burst: 5, Window: "minute",
	}
	engine := &fakeQuotaEngine{}
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		RateLimitDB: store,
		RateLimits:  engine,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.put(t, "/api/v1/rate-limits/rl1", map[string]any{
		"scope": "api_key", "subject": "k1", "dimension": "bpm",
		"limit": 100, "burst": 200, "window": "day",
	})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	row, _ := store.GetRateLimit(context.Background(), "rl1")
	if row.Dimension != "bpm" || row.Lim != 100 || row.Burst != 200 || row.Window != "day" {
		t.Fatalf("update did not persist: %+v", row)
	}
	if engine.reloadCount != 1 {
		t.Errorf("Reload not called after PUT: count=%d", engine.reloadCount)
	}
}

// TestRateLimitHandler_PutNotFound — PUT on missing id returns 404.
func TestRateLimitHandler_PutNotFound(t *testing.T) {
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		RateLimitDB: newFakeRateLimitStore(),
		RateLimits:  &fakeQuotaEngine{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.put(t, "/api/v1/rate-limits/nope", map[string]any{
		"scope": "api_key", "subject": "k1", "dimension": "rpm",
		"limit": 1, "burst": 1, "window": "minute",
	})
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestRateLimitHandler_DeleteOK — DELETE removes the row, returns 204,
// and triggers a Reload.
func TestRateLimitHandler_DeleteOK(t *testing.T) {
	store := newFakeRateLimitStore()
	store.rows["rl1"] = db.RateLimit{
		ID: "rl1", Scope: "api_key", Subject: "k1", Dimension: "rpm",
		Lim: 5, Burst: 5, Window: "minute",
	}
	engine := &fakeQuotaEngine{}
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		RateLimitDB: store,
		RateLimits:  engine,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.delete(t, "/api/v1/rate-limits/rl1")
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	if _, err := store.GetRateLimit(context.Background(), "rl1"); err == nil {
		t.Fatal("row should be gone")
	}
	if engine.reloadCount != 1 {
		t.Errorf("Reload not called after DELETE: count=%d", engine.reloadCount)
	}
}

// TestRateLimitHandler_DeleteNotFound — DELETE on missing id returns 404.
func TestRateLimitHandler_DeleteNotFound(t *testing.T) {
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		RateLimitDB: newFakeRateLimitStore(),
		RateLimits:  &fakeQuotaEngine{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.delete(t, "/api/v1/rate-limits/nope")
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestRateLimitHandler_Perms — admin can mutate, non-admin role "user" has
// quotas:read:own (so GET passes) but no quotas:manage:any (so POST/PUT/
// DELETE return 403).
func TestRateLimitHandler_Perms(t *testing.T) {
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "user"},
		RateLimitDB: newFakeRateLimitStore(),
		RateLimits:  &fakeQuotaEngine{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	// GET allowed (quotas:read:own).
	if r := c.get(t, "/api/v1/rate-limits"); r.StatusCode != http.StatusOK {
		t.Fatalf("GET as user status=%d", r.StatusCode)
	}
	if r := c.get(t, "/api/v1/rate-limits/usage"); r.StatusCode != http.StatusOK {
		t.Fatalf("GET usage as user status=%d", r.StatusCode)
	}
	// POST forbidden.
	r := c.post(t, "/api/v1/rate-limits", map[string]any{
		"scope": "api_key", "subject": "k1", "dimension": "rpm",
		"limit": 1, "burst": 1, "window": "minute",
	})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("POST as user status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	// PUT forbidden.
	r = c.put(t, "/api/v1/rate-limits/x", map[string]any{
		"scope": "api_key", "subject": "k1", "dimension": "rpm",
		"limit": 1, "burst": 1, "window": "minute",
	})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT as user status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	// DELETE forbidden.
	r = c.delete(t, "/api/v1/rate-limits/x")
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("DELETE as user status=%d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestRateLimitHandler_UsageEndpoint — exercises the usage handler with a
// canned engine response and verifies the JSON wire shape.
func TestRateLimitHandler_UsageEndpoint(t *testing.T) {
	engine := &fakeQuotaEngine{
		usage: []quota.Usage{{
			Limit: quota.Limit{
				ID: "rl1", Scope: "api_key", Subject: "k1",
				Dimension: "rpm", Limit: 60, Burst: 60, Window: "minute",
			},
			Used: 10, ResetSeconds: 10,
		}},
	}
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		RateLimitDB: newFakeRateLimitStore(),
		RateLimits:  engine,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/rate-limits/usage?scope=api_key&subject=k1")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var out usageResp
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(out.Limits) != 1 {
		t.Fatalf("limits len = %d, want 1", len(out.Limits))
	}
	row := out.Limits[0]
	if row.ID != "rl1" || row.Used != 10 || row.ResetSeconds != 10 {
		t.Fatalf("usage row mismatch: %+v", row)
	}
}

// TestRateLimitHandler_UsageBadScope — invalid scope query param returns
// 400, not 500/leak.
func TestRateLimitHandler_UsageBadScope(t *testing.T) {
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		RateLimitDB: newFakeRateLimitStore(),
		RateLimits:  &fakeQuotaEngine{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/rate-limits/usage?scope=garbage&subject=x")
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestRateLimitHandler_NilEngineDegrades — when RateLimits is nil (early
// wiring), the GET endpoints return empty bodies and the POST endpoint
// still succeeds (the engine is consulted only for the post-mutation
// reload, which is best-effort).
func TestRateLimitHandler_NilEngineDegrades(t *testing.T) {
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		RateLimitDB: newFakeRateLimitStore(),
		RateLimits:  nil,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/rate-limits/usage")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("usage with nil engine: status=%d", r.StatusCode)
	}
	r = c.post(t, "/api/v1/rate-limits", map[string]any{
		"scope": "api_key", "subject": "k1", "dimension": "rpm",
		"limit": 1, "burst": 1, "window": "minute",
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST with nil engine: status=%d body=%s", r.StatusCode, readBody(t, r))
	}
}

// Rate limits can be set per gateway key and per model.
func TestRateLimitHandler_GatewayKeyAndModelScopes(t *testing.T) {
	st := newFakeRateLimitStore()
	engine := &fakeQuotaEngine{}
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, RateLimitDB: st, RateLimits: engine}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	for scope, subject := range map[string]string{"gateway_key": "gk1", "model": "burrow-smart"} {
		r := c.post(t, "/api/v1/rate-limits", map[string]any{
			"scope": scope, "subject": subject, "dimension": "rpm", "limit": 5, "burst": 5, "window": "day"})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("POST %s: status=%d body=%s", scope, r.StatusCode, readBody(t, r))
		}
		var got rateLimitResp
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if got.Scope != scope || got.Subject != subject || got.Window != "day" {
			t.Fatalf("%s: %+v", scope, got)
		}
		r = c.put(t, "/api/v1/rate-limits/"+got.ID, map[string]any{
			"scope": scope, "subject": subject, "dimension": "bpm", "limit": 9, "burst": 9, "window": "minute"})
		if r.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s: status=%d body=%s", scope, r.StatusCode, readBody(t, r))
		}
		if r := c.post(t, "/api/v1/rate-limits", map[string]any{
			"scope": scope, "subject": "", "dimension": "rpm", "limit": 5, "burst": 5}); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST %s without a subject: status=%d", scope, r.StatusCode)
		}
	}
	r := c.post(t, "/api/v1/rate-limits", map[string]any{"scope": "provider", "subject": "x", "dimension": "rpm", "limit": 5, "burst": 5})
	if body := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "scope must be one of api_key|gateway_key|model|role|service|global") {
		t.Fatalf("bad scope: status=%d body=%s", r.StatusCode, body)
	}

	// The usage endpoint asks the engine for the right subject.
	for query, want := range map[string]quota.Subjects{
		"scope=gateway_key&subject=gk1":    {GatewayKeyID: "gk1"},
		"scope=model&subject=burrow-smart": {Model: "burrow-smart"},
	} {
		engine.asked = nil
		if r := c.get(t, "/api/v1/rate-limits/usage?"+query); r.StatusCode != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", query, r.StatusCode, readBody(t, r))
		}
		if len(engine.asked) != 1 || engine.asked[0] != want {
			t.Fatalf("%s: asked %+v", query, engine.asked)
		}
	}
	r = c.get(t, "/api/v1/rate-limits/usage?scope=provider&subject=x")
	if body := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "scope must be one of api_key|gateway_key|model|role|service|global") {
		t.Fatalf("bad usage scope: status=%d body=%s", r.StatusCode, body)
	}
}

// A user who may read only their own quotas sees the usage of their own
// gateway keys: another user's key and a model's usage across all keys are
// refused, with one answer whether or not the key exists.
func TestRateLimitHandler_UsageOfGatewayKeysIsOwnOnly(t *testing.T) {
	engine := &fakeQuotaEngine{usage: []quota.Usage{{Limit: quota.Limit{ID: "rl1", Scope: "gateway_key", Subject: "x", Dimension: "rpm", Limit: 5, Burst: 5, Window: "day"}, Used: 4}}}
	keys := &fakeKeyStore{rows: []store.GatewayKey{{ID: "gk-mine", UserID: "u-self"}, {ID: "gk-theirs", UserID: "u-other"}}}
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "user"}, RateLimitDB: newFakeRateLimitStore(), RateLimits: engine, AIGatewayKeys: keys}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/rate-limits/usage?scope=gateway_key&subject=gk-mine")
	if r.StatusCode != http.StatusOK || len(engine.asked) != 1 {
		t.Fatalf("own key: status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	_ = readBody(t, r)
	var answers []string
	for _, q := range []string{"scope=gateway_key&subject=gk-theirs", "scope=gateway_key&subject=gk-none", "scope=model&subject=burrow-smart"} {
		r := c.get(t, "/api/v1/rate-limits/usage?"+q)
		body := readBody(t, r)
		if r.StatusCode != http.StatusForbidden || strings.Contains(body, "rl1") {
			t.Fatalf("%s: status=%d body=%s", q, r.StatusCode, body)
		}
		answers = append(answers, body)
	}
	if answers[0] != answers[1] {
		t.Fatalf("an unknown key and another user's key are answered differently: %q vs %q", answers[0], answers[1])
	}
	if len(engine.asked) != 1 {
		t.Fatalf("the engine was asked for a refused subject: %+v", engine.asked)
	}
	// Without a key store nothing can be shown to be the caller's.
	d.AIGatewayKeys = nil
	srv2 := httptest.NewServer(NewRouter(d))
	defer srv2.Close()
	if r := authedClient(t, srv2).get(t, "/api/v1/rate-limits/usage?scope=gateway_key&subject=gk-mine"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("no key store: status=%d", r.StatusCode)
	}
}
