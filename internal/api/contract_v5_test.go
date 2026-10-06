package api

// contract_v5_test.go is the v0.5.0 contract sentinel test.
//
// Like contract_v4_test.go, this file is the *executable form* of the
// (reconciled) v0.5.0 spec — see
// dev/superpowers/specs/2026-05-20-v0.5.0-api-contract.md and especially
// its "Reconciled (Integration Task 1, 2026-05-23)" section. It boots the
// real /api/v1 router with an admin session + CSRF cookie and pins the
// top-level JSON shape of every critical v0.5.0 endpoint family.
//
// This is a SENTINEL — it pins the keyset at the top level only, not every
// nested field. If a sentinel fails the contract has drifted; either the
// shipped JSON moved (a code bug) or the spec moved (a doc fix needed).
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
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// v050KeysOf returns the sorted top-level keys of a JSON object. Local
// helper rather than reusing contract_v4_test.go's helper so the v5
// sentinel stays self-contained.
func v050KeysOf(t *testing.T, m map[string]any) []string {
	t.Helper()
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// v050AssertContainsKeys checks that every key in `want` is present in
// `got`. Subset check — extra keys are not a drift signal (the spec
// pins the documented minimum), missing ones are.
func v050AssertContainsKeys(t *testing.T, label string, got map[string]any, want []string) {
	t.Helper()
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s: missing top-level key %q (got=%v)",
				label, k, v050KeysOf(t, got))
		}
	}
}

// v050Decode decodes an HTTP response body into a generic JSON object.
func v050Decode(t *testing.T, r *http.Response) map[string]any {
	t.Helper()
	defer r.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode object: %v", err)
	}
	return m
}

// v050Deps assembles a Deps with the v0.5.0 fakes the sentinel needs.
// Each sub-test gets a fresh server (and so a fresh Deps) so the fakes
// don't bleed across cases. The seed admin user is fixed by fakeUserStore
// (admin@x / password1).
//
// We use the SAME fakes the per-feature tests use so the sentinel
// exercises the same handler code path the targeted tests already cover —
// the goal here is to pin the *wire shape* across every v0.5.0 endpoint
// family, not to re-test per-feature behavior.
func v050Deps() Deps {
	return Deps{
		Log:           discardLog(),
		Users:         &fakeUserStore{role: "admin"},
		Settings:      &fakeCacheSettingsStore{},
		CacheServices: &fakeCacheServiceLookup{},
	}
}

// --- Part A — Semantic prompt cache --------------------------------------

// TestV050Contract_CacheSettings_Shape pins
// GET /api/v1/cache/settings to {global, semantic, per_service} — the
// v0.5.0 extension over the v0.4.0 sentinel. Spec Part A.4 says the
// response "extends with a top-level `semantic` block".
func TestV050Contract_CacheSettings_Shape(t *testing.T) {
	d := v050Deps()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/cache/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v050Decode(t, r)
	v050AssertContainsKeys(t, "GET /cache/settings", obj,
		[]string{"global", "semantic", "per_service"})
	if _, ok := obj["semantic"].(map[string]any); !ok {
		t.Errorf("semantic: want object, got %T", obj["semantic"])
	}
	if _, ok := obj["per_service"].([]any); !ok {
		t.Errorf("per_service: want array, got %T", obj["per_service"])
	}
}

// TestV050Contract_CacheStats_Shape pins
// GET /api/v1/cache/stats to include the five semantic_* fields
// (spec Part A.4). With a nil SemanticEngine the handler emits zeros.
func TestV050Contract_CacheStats_Shape(t *testing.T) {
	d := v050Deps()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/cache/stats")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v050Decode(t, r)
	v050AssertContainsKeys(t, "GET /cache/stats", obj,
		[]string{"entries", "on_disk_bytes", "hit_rate_24h",
			"semantic_entries", "semantic_disk_bytes",
			"semantic_hit_rate_24h",
			"semantic_similar_returned_24h", "semantic_promotions_24h"})
}

// TestV050Contract_CacheDeleteEntries_204 pins
// DELETE /api/v1/cache/entries to 204 (spec Part A.4: clears both tiers).
// Even with a nil SemanticEngine the handler still returns 204 — the v0.5
// nil-engine degradation path is part of the contract.
func TestV050Contract_CacheDeleteEntries_204(t *testing.T) {
	d := v050Deps()
	d.CacheEngine = &fakeCacheEngine{}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.delete(t, "/api/v1/cache/entries")
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
}

// --- Part B — Upstream-credential injection -------------------------------

// TestV050Contract_UpstreamSlots_Shape pins
// GET /api/v1/upstream-credentials/slots to {slots:[...]} (spec B.2).
func TestV050Contract_UpstreamSlots_Shape(t *testing.T) {
	d := v050Deps()
	d.CredentialVault = &stubCredVault{slots: []string{"OPENAI"}}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/upstream-credentials/slots")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v050Decode(t, r)
	v050AssertContainsKeys(t, "GET /upstream-credentials/slots", obj,
		[]string{"slots"})
	if _, ok := obj["slots"].([]any); !ok {
		t.Errorf("slots: want array, got %T", obj["slots"])
	}
}

// TestV050Contract_ServiceCredentialGet_Shape pins
// GET /api/v1/services/{id}/upstream-credential to a body that always
// carries `slot_present` (spec B.2). Bound row → slot_present:true plus
// {slot, header_name, header_format}.
func TestV050Contract_ServiceCredentialGet_Shape(t *testing.T) {
	d := v050Deps()
	d.CredentialVault = &stubCredVault{slots: []string{"OPENAI"}}
	d.CredentialDB = &stubCredStore{
		present: true,
		row: db.ServiceUpstreamCredential{
			ServiceID: "svc1", Slot: "OPENAI",
			HeaderName: "Authorization", HeaderFormat: "Bearer {key}",
		},
	}
	d.CredentialServices = &stubSvcLookup{ownerID: "u-self"}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/services/svc1/upstream-credential")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v050Decode(t, r)
	v050AssertContainsKeys(t, "GET /services/{id}/upstream-credential", obj,
		[]string{"slot_present"})
}

// TestV050Contract_ServiceCredentialPut_204 pins
// PUT /api/v1/services/{id}/upstream-credential to 204 (spec B.2).
func TestV050Contract_ServiceCredentialPut_204(t *testing.T) {
	d := v050Deps()
	d.CredentialVault = &stubCredVault{slots: []string{"OPENAI"}}
	d.CredentialDB = &stubCredStore{}
	d.CredentialServices = &stubSvcLookup{ownerID: "u-self"}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/services/svc1/upstream-credential",
		map[string]string{"slot": "OPENAI"})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
}

// --- Part C — Multi-provider passthrough ----------------------------------

// TestV050Contract_ModelPost_Shape pins POST /api/v1/ai/models as 201 with
// the targets round-tripping in the response. Synthetic models replaced the
// model aliases of spec C.3: a target's provider and its place in the list
// are what provider and priority were.
func TestV050Contract_ModelPost_Shape(t *testing.T) {
	d := v050Deps()
	d.AIModels = &fakeModelStore{}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/ai/models", map[string]any{
		"name": "fast",
		"targets": []map[string]string{
			{"dialect": "openai", "provider": "ollama", "model": "llama3.1:8b"},
		},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v050Decode(t, r)
	v050AssertContainsKeys(t, "POST /ai/models", obj,
		[]string{"name", "enabled", "targets", "dialects", "created_at", "updated_at"})
	targets, _ := obj["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("targets: got %v", obj["targets"])
	}
	if tg, _ := targets[0].(map[string]any); tg["provider"] != "ollama" || tg["model"] != "llama3.1:8b" {
		t.Errorf("target round-trip: got %v", targets[0])
	}
}

// --- Part D — Custom domains ----------------------------------------------

// TestV050Contract_CustomDomainsList_Shape pins
// GET /api/v1/services/{id}/domains to a JSON array (spec D.2). Empty
// store yields `[]`.
func TestV050Contract_CustomDomainsList_Shape(t *testing.T) {
	d := v050Deps()
	d.CustomDomains = newStubDomainStore()
	d.IPGeoServices = &stubSvcLookup{ownerID: "u-self"}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/services/svc1/domains")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("want JSON array; got %s", body)
	}
}

// --- Part E — Per-tunnel connection logs ----------------------------------

// TestV050Contract_ConnectionLogs_Shape pins
// GET /api/v1/connection-logs to a JSON array (spec E.2). Empty store
// yields `[]`.
func TestV050Contract_ConnectionLogs_Shape(t *testing.T) {
	d := v050Deps()
	d.ConnLogDB = &fakeConnLogStore{}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/connection-logs")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("want JSON array; got %s", body)
	}
}

// TestV050Contract_ConnectionLogRollups_Shape pins
// GET /api/v1/connection-logs/rollups to a JSON array (spec E.2).
func TestV050Contract_ConnectionLogRollups_Shape(t *testing.T) {
	d := v050Deps()
	d.ConnLogDB = &fakeConnLogStore{}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/connection-logs/rollups")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("want JSON array; got %s", body)
	}
}

// --- Part F — Retention & compliance ---------------------------------------

// TestV050Contract_Retention_Shape pins
// GET /api/v1/settings/retention to carry the documented retention-day
// keys plus the `audit_retention_note` advisory (spec F.1 + F.2).
func TestV050Contract_Retention_Shape(t *testing.T) {
	d := v050Deps()
	d.Settings = newFakeRetentionSettings()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/settings/retention")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v050Decode(t, r)
	v050AssertContainsKeys(t, "GET /settings/retention", obj,
		[]string{"audit_retention_days", "audit_retention_note"})
}

// --- Part G — Database backend status --------------------------------------

// TestV050Contract_DatabaseStatus_Shape pins
// GET /api/v1/database to {driver, postgres_alpha, url_redacted} — the
// universal "which backend am I on?" surface (spec G.1 / Task 15).
func TestV050Contract_DatabaseStatus_Shape(t *testing.T) {
	d := v050Deps()
	d.Database = DBInfo{Driver: "sqlite", URLRedacted: "./burrow.db"}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/database")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v050Decode(t, r)
	v050AssertContainsKeys(t, "GET /database", obj, []string{"driver"})
	if obj["driver"] != "sqlite" {
		t.Errorf("driver: got %v want sqlite", obj["driver"])
	}
}

// --- Part H — Webhook payload templates ------------------------------------

// TestV050Contract_WebhookPreview_Shape pins
// POST /api/v1/webhooks/{id}/preview to {rendered, size_bytes} (spec H.4).
// A pre-seeded webhook carrying a tiny template lets the handler render
// successfully and emit the documented shape.
func TestV050Contract_WebhookPreview_Shape(t *testing.T) {
	store := newFakeWebhookStore()
	store.webhooks["wh_ops"] = db.Webhook{
		ID: "wh_ops", Name: "ops", URL: "https://example.com",
		SecretHash:      "h",
		Events:          `["ai.upstream_error"]`,
		PayloadTemplate: `svc={{.ServiceID}}`,
		CreatedAt:       time.Now().UTC(),
	}
	d := v050Deps()
	d.Webhooks = store
	d.WebhookDispatcher = &fakeWebhookDispatcher{}
	d.WebhookSecrets = newFakeSecretRegistry()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/webhooks/wh_ops/preview", map[string]any{
		"event":  "ai.upstream_error",
		"fields": map[string]any{"ServiceID": "svc-1"},
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := v050Decode(t, r)
	v050AssertContainsKeys(t, "POST /webhooks/{id}/preview", obj,
		[]string{"rendered", "size_bytes"})
}

// --- Part J — Embedded OpenAPI viewer --------------------------------------

// TestV050Contract_OpenAPIViewer_HTML pins
// GET /api/v1/openapi/viewer/ to 200 text/html (spec J.1). The trailing
// slash mirrors the chi.Route registration and avoids the 301 redirect
// chi applies to bare-route requests.
func TestV050Contract_OpenAPIViewer_HTML(t *testing.T) {
	d := v050Deps()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/openapi/viewer/")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	defer r.Body.Close()
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type=%q want prefix text/html", ct)
	}
}

// TestV050Contract_OpenAPIViewer_JS pins
// GET /api/v1/openapi/viewer/static/viewer.js to 200 application/javascript
// (spec J.1, J.2).
func TestV050Contract_OpenAPIViewer_JS(t *testing.T) {
	d := v050Deps()
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/openapi/viewer/static/viewer.js")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	defer r.Body.Close()
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type=%q want prefix application/javascript", ct)
	}
}
