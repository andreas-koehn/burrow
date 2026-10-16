package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/guardrails"
)

// TestGuardrailGetSettings_ReturnsDefaultsWhenEmpty asserts the GET
// /guardrails/settings handler serves DefaultSettings (enabled=false,
// action=log_only) when no settings row exists. per_service is always
// a non-nil slice.
func TestGuardrailGetSettings_ReturnsDefaultsWhenEmpty(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "user"},
		Settings: &fakeCacheSettingsStore{}, // shared in-mem settings fake
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/guardrails/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d want 200; body=%s", r.StatusCode, readBody(t, r))
	}
	var got guardrailSettingsResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()
	if got.Global.Enabled {
		t.Errorf("global.enabled = true; want false (default)")
	}
	if got.Global.Action != guardrails.ActionLogOnly {
		t.Errorf("global.action = %q; want %q", got.Global.Action, guardrails.ActionLogOnly)
	}
	if got.PerService == nil {
		t.Errorf("per_service is nil; want empty slice")
	}
}

// TestGuardrailGetSettings_RoundTripsPersistedBlob asserts the handler
// decodes whatever the settings store has under guardrails.global and
// returns it verbatim (with default-fallback on corrupt input).
func TestGuardrailGetSettings_RoundTripsPersistedBlob(t *testing.T) {
	ss := &fakeCacheSettingsStore{
		saved: map[string]string{
			"guardrails.global": `{"enabled":true,"action":"refuse_safe"}`,
		},
	}
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "user"},
		Settings: ss,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/guardrails/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d want 200", r.StatusCode)
	}
	var got guardrailSettingsResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()
	if !got.Global.Enabled || got.Global.Action != guardrails.ActionRefuseSafe {
		t.Fatalf("global = %+v; want enabled=true action=refuse_safe", got.Global)
	}
}

// TestGuardrailGetSettings_PerServiceListsOverrides asserts the per_service
// list reflects which services have a .guardrails sub-object in their
// service_ai_config blob. A service with no override emits null fields
// (Enabled / Action are *pointer-to-T, so json marshals nil → null).
func TestGuardrailGetSettings_PerServiceListsOverrides(t *testing.T) {
	svc := &fakeCacheServiceLookup{
		list: []CacheServiceConfigRow{
			{ServiceID: "svc-1", Config: []byte(`{"guardrails":{"enabled":true,"action":"refuse_403"}}`)},
			{ServiceID: "svc-2", Config: []byte(`{}`)}, // no guardrails block
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

	r := c.get(t, "/api/v1/guardrails/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d want 200", r.StatusCode)
	}
	var got guardrailSettingsResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()
	if len(got.PerService) != 2 {
		t.Fatalf("per_service len=%d want 2", len(got.PerService))
	}
	// svc-1 overrides global.
	if got.PerService[0].ServiceID != "svc-1" {
		t.Fatalf("per_service[0].service_id = %q want svc-1", got.PerService[0].ServiceID)
	}
	if got.PerService[0].Enabled == nil || !*got.PerService[0].Enabled {
		t.Errorf("svc-1 enabled = %v want *true", got.PerService[0].Enabled)
	}
	if got.PerService[0].Action == nil || *got.PerService[0].Action != guardrails.ActionRefuse403 {
		t.Errorf("svc-1 action = %v want *refuse_403", got.PerService[0].Action)
	}
	// svc-2 has no override.
	if got.PerService[1].Enabled != nil || got.PerService[1].Action != nil {
		t.Errorf("svc-2 override unexpectedly populated: %+v", got.PerService[1])
	}
}

// TestGuardrailPutSettings_RequiresAdminOrAIConfigureAny asserts the spec
// gate: a non-admin user without ai:configure:any cannot PUT settings.
// Admin succeeds (204) and the blob round-trips through the settings store.
func TestGuardrailPutSettings_RequiresAdminOrAIConfigureAny(t *testing.T) {
	// non-admin → 403
	{
		ss := &fakeCacheSettingsStore{}
		d := Deps{
			Log:      discardLog(),
			Users:    &fakeUserStore{role: "user"},
			Settings: ss,
		}
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		r := c.put(t, "/api/v1/guardrails/settings", map[string]any{
			"enabled": true,
			"action":  guardrails.ActionLogOnly,
		})
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("non-admin PUT status=%d want 403", r.StatusCode)
		}
		r.Body.Close()
		if _, ok := ss.saved["guardrails.global"]; ok {
			t.Errorf("settings were saved despite 403")
		}
	}

	// admin → 204 + value persisted
	{
		ss := &fakeCacheSettingsStore{}
		d := Deps{
			Log:      discardLog(),
			Users:    &fakeUserStore{role: "admin"},
			Settings: ss,
		}
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		r := c.put(t, "/api/v1/guardrails/settings", map[string]any{
			"enabled": true,
			"action":  guardrails.ActionRefuse403,
		})
		if r.StatusCode != http.StatusNoContent {
			t.Fatalf("admin PUT status=%d want 204; body=%s", r.StatusCode, readBody(t, r))
		}
		r.Body.Close()
		got, ok := ss.saved["guardrails.global"]
		if !ok {
			t.Fatalf("guardrails.global not persisted; saved=%v", ss.saved)
		}
		var s guardrails.Settings
		if err := json.Unmarshal([]byte(got), &s); err != nil {
			t.Fatalf("persisted blob is invalid JSON: %v (%q)", err, got)
		}
		if !s.Enabled || s.Action != guardrails.ActionRefuse403 {
			t.Fatalf("persisted settings = %+v want enabled=true action=refuse_403", s)
		}
	}
}

// TestGuardrailPutSettings_RejectsInvalidAction asserts the closed-enum
// validation (spec: 400 on invalid action). The body shape itself is
// valid JSON; only the action value is rejected.
func TestGuardrailPutSettings_RejectsInvalidAction(t *testing.T) {
	ss := &fakeCacheSettingsStore{}
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "admin"},
		Settings: ss,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/guardrails/settings", map[string]any{
		"enabled": true,
		"action":  "bogus",
	})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid-action PUT status=%d want 400", r.StatusCode)
	}
	r.Body.Close()
	if _, ok := ss.saved["guardrails.global"]; ok {
		t.Errorf("settings were saved despite invalid action")
	}
}

// TestGuardrailPutSettings_RejectsMalformedJSON asserts the handler returns
// 400 when the body is not valid JSON at all (covering the io.ReadAll +
// json.Unmarshal seam).
func TestGuardrailPutSettings_RejectsMalformedJSON(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "admin"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	// authClient.put always wraps the body in mustJSON; bypass it by
	// crafting a raw request with garbage bytes.
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/guardrails/settings",
		strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", c.csrf)
	r, err := c.hc.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", r.StatusCode)
	}
}

// TestGuardrailGetPatterns_ShapeAndOrder asserts the patterns endpoint
// returns IDs + descriptions in iteration order, with NO regex source
// (spec invariant: "regex source NOT exposed publicly").
func TestGuardrailGetPatterns_ShapeAndOrder(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "user"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/guardrails/patterns")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d want 200", r.StatusCode)
	}
	body := readBody(t, r)
	var got []guardrailPatternResp
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The request patterns first ("request"), then the answer patterns
	// ("answer"), each in match order.
	all := append(append([]guardrails.Pattern(nil), guardrails.Patterns...), guardrails.AnswerPatterns...)
	if len(got) != len(all) {
		t.Fatalf("len=%d want %d", len(got), len(all))
	}
	for i := range got {
		if got[i].ID != all[i].ID {
			t.Errorf("[%d] id=%q want %q", i, got[i].ID, all[i].ID)
		}
		if got[i].Description != all[i].Description {
			t.Errorf("[%d] description=%q want %q", i, got[i].Description, all[i].Description)
		}
		want := "request"
		if i >= len(guardrails.Patterns) {
			want = "answer"
		}
		if got[i].Direction != want {
			t.Errorf("[%d] %s direction=%q want %q", i, got[i].ID, got[i].Direction, want)
		}
	}
	// Regex source MUST NOT appear in the response body. Each bundled
	// regex contains characters not present in the description set
	// (parens, backslashes, character classes); spot-check a few that
	// are sure to appear in the source but never in any description.
	for _, needle := range []string{`(?i)`, `\b`, `[\x{200B}`} {
		if jsonContainsSubstring(body, needle) {
			t.Errorf("patterns response leaks regex source token %q", needle)
		}
	}
}

// jsonContainsSubstring is a small helper that checks the literal body
// for a substring (used to assert regex tokens leaked into the response).
// We deliberately don't decode-then-marshal; the raw body is what the
// client sees.
func jsonContainsSubstring(body, needle string) bool {
	return strings.Contains(body, needle)
}
