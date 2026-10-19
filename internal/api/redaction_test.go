package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/redact"
)

// TestRedactionGetRulesContainsBuiltIns asserts the GET /redaction/rules
// response shape: built_in is the full bundled pack; custom is the
// persisted (possibly empty) array. Any logged-in user may read.
func TestRedactionGetRulesContainsBuiltIns(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "user"},
		Settings: &fakeCacheSettingsStore{}, // shared in-mem settings fake
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/redaction/rules")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d want 200", r.StatusCode)
	}
	var got redactionRulesResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()
	if len(got.BuiltIn) != len(redact.BuiltIn) {
		t.Fatalf("built_in count=%d want %d", len(got.BuiltIn), len(redact.BuiltIn))
	}
	// Spot-check a few well-known IDs.
	want := map[string]bool{"email": true, "ipv4": true, "credit_card_luhn": true}
	for _, b := range got.BuiltIn {
		delete(want, b.ID)
	}
	if len(want) > 0 {
		t.Fatalf("missing built-in ids: %v", want)
	}
	if got.Custom == nil || len(got.Custom) != 0 {
		t.Fatalf("custom=%v want empty slice", got.Custom)
	}
}

// TestRedactionPostRequiresAdmin asserts that non-admin users cannot
// create custom rules (admin-or-ai:configure:any gate).
func TestRedactionPostRequiresAdmin(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "user"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/redaction/rules", map[string]any{
		"name":    "test",
		"pattern": `foo`,
		"action":  "mask",
		"scope":   "both",
	})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin status=%d want 403", r.StatusCode)
	}
	r.Body.Close()
}

// TestRedactionPostCreatesRule asserts a successful admin POST: the rule
// is persisted (round-trip via GET) and the response includes a server-
// assigned id.
func TestRedactionPostCreatesRule(t *testing.T) {
	ss := &fakeCacheSettingsStore{}
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "admin"},
		Settings: ss,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/redaction/rules", map[string]any{
		"name":    "phone",
		"pattern": `\d{10}`,
		"action":  "hash",
		"scope":   "both",
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d want 201; body=%s", r.StatusCode, readBody(t, r))
	}
	var created redact.Rule
	if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()
	if created.ID == "" {
		t.Fatal("expected server-assigned id; got empty")
	}
	if created.Name != "phone" || created.Action != "hash" || created.Scope != "both" {
		t.Fatalf("unexpected rule: %+v", created)
	}

	// GET should now include it under custom.
	r = c.get(t, "/api/v1/redaction/rules")
	var got redactionRulesResp
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if len(got.Custom) != 1 || got.Custom[0].ID != created.ID {
		t.Fatalf("GET after POST custom=%+v", got.Custom)
	}
}

// TestRedactionPostInvalidRegex asserts the spec-mandated 400 when the
// supplied pattern fails to compile.
func TestRedactionPostInvalidRegex(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "admin"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/redaction/rules", map[string]any{
		"name":    "bad",
		"pattern": `(unclosed`,
		"action":  "mask",
		"scope":   "both",
	})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", r.StatusCode)
	}
	r.Body.Close()
}

// TestRedactionPutInvalidRegex asserts the spec-mandated 400 on PUT with a
// custom rule's pattern that fails to compile. Setup: create a valid rule
// then PUT it with an invalid regex.
func TestRedactionPutInvalidRegex(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "admin"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/redaction/rules", map[string]any{
		"name": "x", "pattern": `ok`, "action": "mask", "scope": "both",
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("seed POST status=%d", r.StatusCode)
	}
	var created redact.Rule
	_ = json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()

	r = c.put(t, "/api/v1/redaction/rules/"+created.ID, map[string]any{
		"name": "x", "pattern": `(broken`, "action": "mask", "scope": "both",
	})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT status=%d want 400", r.StatusCode)
	}
	r.Body.Close()
}

// TestRedactionDeleteBuiltInIs409 asserts the spec-mandated 409 when DELETE
// targets a built-in rule id (built-ins are read-only).
func TestRedactionDeleteBuiltInIs409(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "admin"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.delete(t, "/api/v1/redaction/rules/email")
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d want 409", r.StatusCode)
	}
	r.Body.Close()
}

// TestRedactionDeleteCustom asserts a successful admin DELETE on a custom
// rule: 204, and the rule disappears from GET.
func TestRedactionDeleteCustom(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "admin"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	// Seed.
	r := c.post(t, "/api/v1/redaction/rules", map[string]any{
		"name": "x", "pattern": `ok`, "action": "mask", "scope": "both",
	})
	var created redact.Rule
	_ = json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()

	// Delete.
	r = c.delete(t, "/api/v1/redaction/rules/"+created.ID)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status=%d want 204; body=%s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()

	// GET should not list it.
	r = c.get(t, "/api/v1/redaction/rules")
	var got redactionRulesResp
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if len(got.Custom) != 0 {
		t.Fatalf("custom should be empty after delete: %+v", got.Custom)
	}
}

// TestRedactionGetSettingsDefaults asserts the GET /redaction/settings
// response shape under defaults (no row): global is the documented zero
// state; per_service is always a non-nil empty slice.
func TestRedactionGetSettingsDefaults(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "user"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/redaction/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", r.StatusCode)
	}
	body := readBody(t, r)
	// per_service must render as [] (not null) so the UI can iterate.
	if !strings.Contains(body, `"per_service":[]`) {
		t.Fatalf("response missing empty per_service: %s", body)
	}
	var got redactionSettingsResp
	_ = json.Unmarshal([]byte(body), &got)
	if got.Global.Enabled != false || got.Global.RedactForLogsOnly != false {
		t.Fatalf("default global: %+v", got.Global)
	}
}

// TestRedactionPutSettingsRoundTrip asserts a successful admin PUT: 204,
// and the next GET reflects the saved values.
func TestRedactionPutSettingsRoundTrip(t *testing.T) {
	ss := &fakeCacheSettingsStore{}
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "admin"},
		Settings: ss,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/redaction/settings", map[string]any{
		"enabled":              true,
		"redact_for_logs_only": true,
		"rule_ids":             []string{"email", "ipv4"},
		"presidio_enabled":     false,
	})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status=%d want 204", r.StatusCode)
	}
	r.Body.Close()

	r = c.get(t, "/api/v1/redaction/settings")
	var got redactionSettingsResp
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if !got.Global.Enabled || !got.Global.RedactForLogsOnly {
		t.Fatalf("global not saved: %+v", got.Global)
	}
	if len(got.Global.RuleIDs) != 2 || got.Global.RuleIDs[0] != "email" {
		t.Fatalf("rule_ids: %v", got.Global.RuleIDs)
	}
}

// TestRedactionPutSettingsRequiresAdmin asserts the same gate applies to
// PUT /redaction/settings: a regular user gets 403.
func TestRedactionPutSettingsRequiresAdmin(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "user"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/redaction/settings", map[string]any{"enabled": true})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin status=%d want 403", r.StatusCode)
	}
	r.Body.Close()
}

// TestRedactionPreviewMatchesEmail asserts the preview endpoint returns
// the regex hits for a known input. Admin-only.
func TestRedactionPreviewMatchesEmail(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "admin"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	body := "ping alice@example.com and bob@example.org"
	r := c.post(t, "/api/v1/redaction/preview", map[string]any{
		"body":  body,
		"scope": "request_body",
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("preview status=%d want 200", r.StatusCode)
	}
	var got redactionPreviewResp
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if len(got.Matches) < 2 {
		t.Fatalf("expected ≥2 matches, got %+v", got.Matches)
	}
	emails := 0
	for _, m := range got.Matches {
		if m.Rule == "email" {
			emails++
		}
	}
	if emails != 2 {
		t.Fatalf("email matches=%d want 2; got=%+v", emails, got.Matches)
	}
}

// TestRedactionPerServicePopulatedFromAIConfig asserts that GET
// /redaction/settings renders one per_service row per service_ai_config
// row whose .redaction sub-object is set. Reuses the cache CacheServices
// fake for parity with the cache test pattern.
func TestRedactionPerServicePopulatedFromAIConfig(t *testing.T) {
	svc := &fakeCacheServiceLookup{
		list: []CacheServiceConfigRow{
			{ServiceID: "svc-1", Config: []byte(`{"redaction":{"enabled":true,"redact_for_logs_only":true,"rule_ids":["email"],"presidio_enabled":false}}`)},
			{ServiceID: "svc-2", Config: []byte(`{}`)}, // no redaction block → override=false
		},
	}
	d := Deps{
		Log:           discardLog(),
		Users:         &fakeUserStore{role: "user"},
		Settings:      &fakeCacheSettingsStore{},
		CacheServices: svc,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/redaction/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", r.StatusCode)
	}
	var got redactionSettingsResp
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if len(got.PerService) != 2 {
		t.Fatalf("per_service len=%d want 2", len(got.PerService))
	}
	if !got.PerService[0].Override || !got.PerService[0].Enabled || !got.PerService[0].RedactForLogsOnly {
		t.Fatalf("svc-1: %+v", got.PerService[0])
	}
	if got.PerService[1].Override {
		t.Fatalf("svc-2 should not override: %+v", got.PerService[1])
	}
}

// TestRedactionToolScopes: a custom rule may be scoped to the arguments or
// the result of a tool call (D16); an unknown scope is still refused.
func TestRedactionToolScopes(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "admin"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	for scope, want := range map[string]int{"tool_arguments": http.StatusCreated, "tool_result": http.StatusCreated, "tool_everything": http.StatusBadRequest} {
		r := c.post(t, "/api/v1/redaction/rules", map[string]any{
			"name": "r-" + scope, "pattern": `ok`, "action": "mask", "scope": scope,
		})
		if r.StatusCode != want {
			t.Errorf("scope %s: status=%d want %d", scope, r.StatusCode, want)
		}
		r.Body.Close()
	}
}
