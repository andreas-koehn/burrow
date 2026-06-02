package api

// contract_v4_test.go is the v0.4.0 contract sentinel test.
//
// Like contract_v3_test.go, this file is the *executable form* of the
// (reconciled) v0.4.0 spec — see
// dev/superpowers/specs/2026-05-19-v0.4.0-api-contract.md and especially
// its "Reconciled (Integration Task 1, 2026-05-21)" section. It boots the
// real /api/v1 router with an admin session + CSRF cookie and asserts the
// top-level JSON shape of every critical v0.4.0 endpoint matches what the
// (amended) spec promises.
//
// This is a SENTINEL — it pins the keyset at the top level only, not every
// nested field. If a sentinel fails the contract has drifted; either the
// shipped JSON moved (a code bug) or the spec moved (a doc fix needed).
//
// Per the spec's own rule ("code wins on drift"), the resolution is almost
// always to amend the spec doc; this test is the gate that prevents
// silent contract drift in the other direction.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// v040KeysOf returns the sorted top-level keys of a JSON object. Local
// helper rather than reusing contract_v3_test.go's keysOf so the v4
// sentinel stays self-contained and can be reasoned about in isolation.
func v040KeysOf(t *testing.T, m map[string]any) []string {
	t.Helper()
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// v040AssertContainsKeys checks that every key in `want` is present in
// `got`. Unlike contract_v3_test.go's assertKeys this is a SUBSET check —
// sentinel-grade: extra keys are not a drift signal (the spec only pins
// the documented minimum), missing ones are.
func v040AssertContainsKeys(t *testing.T, label string, got map[string]any, want []string) {
	t.Helper()
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s: missing top-level key %q (got=%v)",
				label, k, v040KeysOf(t, got))
		}
	}
}

// v040Decode decodes an HTTP response body into a generic JSON object.
func v040Decode(t *testing.T, r *http.Response) map[string]any {
	t.Helper()
	defer r.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode object: %v", err)
	}
	return m
}

// v040Deps assembles a Deps with the fakes the v0.4.0 sentinel needs.
// One server per sub-test keeps fakes isolated; the seed admin user is
// fixed by fakeUserStore (admin@x / password1).
//
// We use the SAME fakes the per-feature tests use so the sentinel exercises
// the same handler code path the targeted tests already cover — the goal
// here is to pin the *wire shape* across all endpoints, not to re-test
// per-feature behavior. ModelAliases is intentionally left nil — the
// handler degrades to an empty JSON array, which is the documented zero
// state we want to pin.
func v040Deps() Deps {
	auto := newFakeAutomationStore()
	return Deps{
		Log:           discardLog(),
		Users:         &fakeUserStore{role: "admin"},
		Settings:      &fakeCacheSettingsStore{},
		CacheServices: &fakeCacheServiceLookup{},
		// CacheEngine left nil — handler degrades to zero-stats response.
		// ModelAliases left nil — handler emits [] (documented zero state).
		RateLimitDB: newFakeRateLimitStore(),
		RateLimits:  &fakeQuotaEngine{},
		CostEngine:  &fakeCostEngine{},
		Budgets:     newFakeBudgetStore(),
		Webhooks:    newFakeWebhookStore(),
		Automation:  auto,
		Bearer:      auto,
	}
}

// --- The sentinel tests start here. -----------------------------------------

// TestV040Contract_CacheSettings_Shape pins the top-level shape of
// GET /api/v1/cache/settings to {global, per_service} (Reconciled spec
// Part B.3).
func TestV040Contract_CacheSettings_Shape(t *testing.T) {
	d := v040Deps()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/cache/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v040Decode(t, r)
	v040AssertContainsKeys(t, "GET /cache/settings", obj, []string{"global", "per_service"})
	if _, ok := obj["global"].(map[string]any); !ok {
		t.Errorf("global: want object, got %T", obj["global"])
	}
	if _, ok := obj["per_service"].([]any); !ok {
		t.Errorf("per_service: want array, got %T", obj["per_service"])
	}
}

// TestV040Contract_RedactionSettings_Shape pins
// GET /api/v1/redaction/settings to {global, per_service} (Reconciled
// spec Part B.4).
func TestV040Contract_RedactionSettings_Shape(t *testing.T) {
	d := v040Deps()
	d.ModelAliases = nil
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/redaction/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v040Decode(t, r)
	v040AssertContainsKeys(t, "GET /redaction/settings", obj,
		[]string{"global", "per_service"})
	if _, ok := obj["per_service"].([]any); !ok {
		t.Errorf("per_service: want array, got %T", obj["per_service"])
	}
}

// TestV040Contract_GuardrailSettings_Shape pins
// GET /api/v1/guardrails/settings to {global, per_service} (Reconciled
// spec Part B.5).
func TestV040Contract_GuardrailSettings_Shape(t *testing.T) {
	d := v040Deps()
	d.ModelAliases = nil
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/guardrails/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v040Decode(t, r)
	v040AssertContainsKeys(t, "GET /guardrails/settings", obj,
		[]string{"global", "per_service"})
	if _, ok := obj["per_service"].([]any); !ok {
		t.Errorf("per_service: want array, got %T", obj["per_service"])
	}
}

// TestV040Contract_ModelAliases_Shape pins GET /api/v1/models/aliases to
// a JSON array (spec Part C.1). With a nil ModelAliases dep the handler
// degrades to `[]`, which is the documented zero state.
func TestV040Contract_ModelAliases_Shape(t *testing.T) {
	d := v040Deps()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/models/aliases")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	// Spec: response is a JSON array; empty case MUST be `[]` not `null`.
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("want JSON array; got %s", body)
	}
}

// TestV040Contract_RateLimits_Shape pins GET /api/v1/rate-limits to a
// JSON array (spec Part D.2). Empty store yields `[]`.
func TestV040Contract_RateLimits_Shape(t *testing.T) {
	d := v040Deps()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/rate-limits")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("want JSON array; got %s", body)
	}
}

// TestV040Contract_RateLimits_PostShape proves the write side of the
// rate-limit contract (admin session + CSRF). Asserts 201 and that the
// response is an object carrying {id, scope, subject, dimension, limit,
// burst, window} — the closed RateLimit shape from spec Part D.2.
func TestV040Contract_RateLimits_PostShape(t *testing.T) {
	d := v040Deps()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/rate-limits", map[string]any{
		"scope": "api_key", "subject": "k1", "dimension": "rpm",
		"limit": 60, "burst": 60, "window": "minute",
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v040Decode(t, r)
	v040AssertContainsKeys(t, "POST /rate-limits", obj,
		[]string{"id", "scope", "subject", "dimension", "limit", "burst", "window"})
}

// TestV040Contract_CostPricing_Shape pins GET /api/v1/cost/pricing to
// {version, entries} (spec Part F.1). Even with a nil cost engine the
// handler emits the well-formed empty shape (version:"", entries:[]).
func TestV040Contract_CostPricing_Shape(t *testing.T) {
	d := v040Deps()
	d.CostEngine = nil // handler degrades to empty pricing
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/cost/pricing")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v040Decode(t, r)
	v040AssertContainsKeys(t, "GET /cost/pricing", obj,
		[]string{"version", "entries"})
	if _, ok := obj["entries"].([]any); !ok {
		t.Errorf("entries: want array, got %T", obj["entries"])
	}
}

// TestV040Contract_AuditFingerprint_Shape pins
// GET /api/v1/audit/fingerprint to {public_key, fingerprint} (spec
// Part G.2). Uses a real *audit.Logger seeded in the auditTestStack
// helper.
func TestV040Contract_AuditFingerprint_Shape(t *testing.T) {
	s := newAuditTestStack(t)
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		AuditEvents: s.x,
		AuditChain:  s.chain,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/audit/fingerprint")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v040Decode(t, r)
	v040AssertContainsKeys(t, "GET /audit/fingerprint", obj,
		[]string{"public_key", "fingerprint"})
	if s, ok := obj["fingerprint"].(string); !ok || s == "" {
		t.Errorf("fingerprint: want non-empty string, got %v", obj["fingerprint"])
	}
}

// TestV040Contract_Webhooks_Shape pins GET /api/v1/webhooks to a JSON
// array (spec Part H.1). Empty store yields `[]`.
func TestV040Contract_Webhooks_Shape(t *testing.T) {
	d := v040Deps()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/webhooks")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("want JSON array; got %s", body)
	}
}

// TestV040Contract_BearerAuth_GetAutomationTokens proves the bearer-auth
// surface is alive — spec Part M.1. The flow:
//  1. Mint an automation token via the cookie path (POST).
//  2. Re-issue GET /api/v1/automation/tokens with Authorization: Bearer
//     bua_... and NO cookies / NO CSRF.
//  3. Assert 200 with a JSON array body.
//
// This is the single bearer-auth assertion required by the Reconciled
// spec — automation_test.go covers the rest of the bearer matrix.
func TestV040Contract_BearerAuth_GetAutomationTokens(t *testing.T) {
	d := v040Deps()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	// 1. Mint a token via cookie+CSRF.
	r := c.post(t, "/api/v1/automation/tokens", map[string]any{
		"name":        "sentinel",
		"permissions": []string{"automation:tokens:manage:any"},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("mint status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var created createTokenWrap
	if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	r.Body.Close()
	if !strings.HasPrefix(created.Plaintext, "bua_") {
		t.Fatalf("plaintext prefix: %q", created.Plaintext)
	}

	// 2. Re-issue GET /automation/tokens with Bearer + NO cookies + NO CSRF.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/automation/tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Plaintext)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("bearer GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer GET status=%d want 200 body=%s",
			resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("want JSON array body, got %s", body)
	}
}

// --- Compile-time guard against fake-store signature drift -----------------
//
// The sentinel relies on the per-feature fakes (fakeCacheServiceLookup,
// fakeRateLimitStore, fakeQuotaEngine, fakeCostEngine, fakeBudgetStore,
// fakeWebhookStore, fakeAutomationStore) which all live in their own
// _test.go siblings and each carry their own `var _ XxxStore = ...`
// guards. This file therefore only needs to assert nothing it touches
// has rotted at the top-level Deps wiring.
var _ CacheServiceLookup = (*fakeCacheServiceLookup)(nil)
var _ CacheSettingsStore = (*fakeCacheSettingsStore)(nil)
