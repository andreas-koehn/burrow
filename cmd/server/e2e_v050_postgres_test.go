//go:build postgres

package main

// e2e_v050_postgres_test.go — Integration Task 4 (v0.5.0).
//
// Opt-in real-stack e2e smoke for the `postgres` build flavour.
//
// Mirrors TestV050DefaultBuildE2E (the SQLite-backed seam test in
// e2e_v050_default_test.go) but boots the v0.5.0 API surface against a
// real Postgres database opened via db.OpenPostgres, so every v0.5.0
// endpoint that touches the DB is exercised against the production
// Postgres migration ladder + the production query layer.
//
// Test database isolation: we use a "DROP SCHEMA public CASCADE;
// CREATE SCHEMA public;" reset BEFORE OpenPostgres applies the
// migration ladder. The schema-drop is a two-statement Exec; the
// subsequent OpenPostgres call runs the embedded postgres migration
// ladder from scratch. This guarantees per-test-run isolation against
// a long-lived test DB without requiring CREATE DATABASE privileges.
//
// Requires BURROW_TEST_POSTGRES_URL pointing at a reachable Postgres
// instance with permissions to drop+recreate the `public` schema and
// run DDL. CI's e2e compose job provides this var via a sidecar pg
// container. The test is silently skipped when the variable is unset.
//
// Part E note: unlike the default-build counterpart in
// e2e_v050_default_test.go (which drives a visitor request through
// bootE2EStack's real proxy + tunnel + upstream to prove F-13's
// proxy hot-path SQLSink invocation), this Postgres test plants a row
// via the connlog.SQLSink directly and then asserts GET
// /api/v1/connection-logs returns >=1 row. The Postgres goal is to
// exercise the JSON API + DB query path under the postgres driver —
// the proxy hot-path is identical between SQLite and Postgres builds
// (no driver-specific code in proxy.go) and is already covered by the
// default-build E subtest.

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/db"
)

// TestV050PostgresBackendE2E exercises one request per v0.5.0 spec family
// (A–H, J — matching TestV050DefaultBuildE2E's matrix minus the proxy-hot-path
// D2 subtest) against the full real API binary built with -tags=postgres
// and pointed at a live Postgres instance. Skipped when BURROW_TEST_POSTGRES_URL
// is unset.
//
// Part G asserts driver=="postgres"; the surrounding subtests assert each
// v0.5.0 endpoint returns 2xx + the expected shape. Any genuine SQLite-only
// query in the v0.5.0 code path will manifest here as a 5xx + a clear
// pq/pgx error in the test output.
func TestV050PostgresBackendE2E(t *testing.T) {
	// v0.5.1 P1.1: portable placeholders are now wired via the pgx-rewrite
	// driver in internal/db/postgres.go — SQLite-style "?" placeholders are
	// rewritten to "$N" at PrepareContext/QueryContext/ExecContext time, so
	// the unchanged v0.5.0 query strings execute under Postgres. The corpus
	// gate is internal/db/postgres_rewriter_test.go::TestPostgresRewriteCorpus.

	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping Postgres e2e")
	}

	// --- Schema reset + open ----------------------------------------------
	//
	// We open Postgres twice: first to DROP+CREATE the public schema
	// (clearing any state from a previous run), then again so OpenPostgres
	// applies the embedded migration ladder against the empty schema.
	resetPostgresSchema(t, pgURL)
	backend, err := db.OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	sqldb := backend.DB()

	// urlRedacted is what Part G's GET /api/v1/database surfaces. The
	// production redactor (api.RedactDatabaseURL) replaces user:pass; here
	// the test uses a fixed sentinel so the assertion is byte-stable.
	const urlRedacted = "postgres://****:****@e2e/burrow_test"

	env := buildV050EnvOnDB(t, sqldb, "postgres", urlRedacted)

	// Helpers reused from e2e_v050_default_test.go: env.do, env.rawGet,
	// genTestLeaf, mustReuseTestCA. The CA root pool is the one that
	// buildV050EnvOnDB plumbed into Deps.CertValidationRoots — same path
	// as the SQLite test, no driver-specific behaviour.

	// --- Part A — semantic cache (NoopCache: enabled=false) ----------------
	t.Run("A_semantic_cache_settings", func(t *testing.T) {
		code, body := env.do(t, http.MethodGet, "/api/v1/cache/settings", nil)
		if code != http.StatusOK {
			t.Fatalf("status=%d body=%s", code, body)
		}
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("decode: %v body=%s", err, body)
		}
		sem, ok := obj["semantic"].(map[string]any)
		if !ok {
			t.Fatalf("response missing semantic block: %s", body)
		}
		enabled, _ := sem["enabled"].(bool)
		if enabled {
			t.Errorf("postgres build (no semantic_cache tag) must report semantic.enabled=false; got %s", body)
		}
	})

	// --- Part B — upstream credential PUT ---------------------------------
	t.Run("B_upstream_credential_put", func(t *testing.T) {
		code, body := env.do(t, http.MethodPut,
			"/api/v1/services/"+env.serviceID+"/upstream-credential",
			map[string]string{
				"slot":          "OPENAI",
				"header_name":   "Authorization",
				"header_format": "Bearer {key}",
			})
		if code != http.StatusNoContent {
			t.Fatalf("status=%d body=%s", code, body)
		}
		code2, body2 := env.do(t, http.MethodGet,
			"/api/v1/services/"+env.serviceID+"/upstream-credential", nil)
		if code2 != http.StatusOK {
			t.Fatalf("readback status=%d body=%s", code2, body2)
		}
		var got map[string]any
		_ = json.Unmarshal(body2, &got)
		if got["slot"] != "OPENAI" {
			t.Errorf("readback slot=%v want OPENAI (body=%s)", got["slot"], body2)
		}
	})

	// --- Part C — multi-provider: a synthetic model and a gateway key ----
	t.Run("C_synthetic_model_and_gateway_key", func(t *testing.T) { v050CheckSyntheticModel(t, env) })

	// --- Part D — custom domain POST --------------------------------------
	t.Run("D_custom_domain_post", func(t *testing.T) {
		ca, caKey := mustReuseTestCA(t, env)
		certPEM, keyPEM := genTestLeaf(t, ca, caKey, []string{"foo.example.com"})
		code, body := env.do(t, http.MethodPost,
			"/api/v1/services/"+env.serviceID+"/domains",
			map[string]any{
				"hostname": "foo.example.com",
				"cert_pem": certPEM,
				"key_pem":  keyPEM,
			})
		if code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", code, body)
		}
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("decode: %v body=%s", err, body)
		}
		if obj["hostname"] != "foo.example.com" {
			t.Errorf("hostname=%v want foo.example.com (body=%s)", obj["hostname"], body)
		}
	})

	// --- Part E — connection log read against postgres --------------------
	//
	// Plants a row via connlog.SQLSink.Record (the same write path the
	// proxy hot-path uses) and then asserts GET /api/v1/connection-logs
	// returns >=1 row. This exercises the read-side SQL query against
	// the postgres connection_logs table, which is what Task 4 cares
	// about (driver compatibility) — the proxy hot-path is already
	// covered by the default-build smoke and is driver-agnostic.
	t.Run("E_connection_logs", func(t *testing.T) {
		sink := connlog.NewSQLSink(env.sqldb, slog.New(slog.NewTextHandler(io.Discard, nil)))
		now := time.Now().UTC()
		err := sink.Record(context.Background(), connlog.Entry{
			Kind:      connlog.KindHTTPProxy,
			ServiceID: env.serviceID,
			UserID:    env.adminID,
			SourceIP:  "127.0.0.1",
			StartedAt: now.Add(-100 * time.Millisecond),
			EndedAt:   now,
			BytesIn:   10,
			BytesOut:  42,
			Status:    connlog.StatusClosedClean,
		})
		if err != nil {
			t.Fatalf("sink.Record: %v", err)
		}
		// sink.Record is async (goroutine); poll until the row lands.
		// GET /api/v1/connection-logs returns a flat JSON array per the
		// v0.5.0 contract (writeJSON of []connectionLogResp), not an
		// {"items": [...]} envelope.
		deadline := time.Now().Add(5 * time.Second)
		var count int
		for time.Now().Before(deadline) {
			code, body := env.do(t, http.MethodGet,
				"/api/v1/connection-logs?service_id="+env.serviceID, nil)
			if code != http.StatusOK {
				t.Fatalf("status=%d body=%s", code, body)
			}
			var items []json.RawMessage
			if err := json.Unmarshal(body, &items); err != nil {
				t.Fatalf("decode: %v body=%s", err, body)
			}
			count = len(items)
			if count >= 1 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if count < 1 {
			t.Fatalf("connection_logs did not return any rows for service_id=%s after 5s", env.serviceID)
		}
	})

	// --- Part F — retention settings PUT then GET --------------------------
	t.Run("F_retention_put_get", func(t *testing.T) {
		code, body := env.do(t, http.MethodPut, "/api/v1/settings/retention",
			map[string]any{"usage_retention_days": 60})
		if code != http.StatusNoContent {
			t.Fatalf("PUT status=%d body=%s", code, body)
		}
		code2, body2 := env.do(t, http.MethodGet, "/api/v1/settings/retention", nil)
		if code2 != http.StatusOK {
			t.Fatalf("GET status=%d body=%s", code2, body2)
		}
		var obj map[string]any
		if err := json.Unmarshal(body2, &obj); err != nil {
			t.Fatalf("decode: %v body=%s", err, body2)
		}
		if v, _ := obj["usage_retention_days"].(float64); int(v) != 60 {
			t.Errorf("usage_retention_days=%v want 60 (body=%s)", obj["usage_retention_days"], body2)
		}
	})

	// --- Part G — database backend status (driver=="postgres") ------------
	t.Run("G_database_status", func(t *testing.T) {
		code, body := env.do(t, http.MethodGet, "/api/v1/database", nil)
		if code != http.StatusOK {
			t.Fatalf("status=%d body=%s", code, body)
		}
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("decode: %v body=%s", err, body)
		}
		if obj["driver"] != "postgres" {
			t.Errorf("driver=%v want postgres (body=%s)", obj["driver"], body)
		}
	})

	// --- Part H — webhook payload template preview ------------------------
	t.Run("H_webhook_template_preview", func(t *testing.T) {
		code, body := env.do(t, http.MethodPost, "/api/v1/webhooks", map[string]any{
			"name":             "ops",
			"url":              "https://example.com/hook",
			"events":           []string{"ai.upstream_error"},
			"payload_template": `svc={{.service_id}}`,
		})
		if code != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", code, body)
		}
		var created map[string]any
		if err := json.Unmarshal(body, &created); err != nil {
			t.Fatalf("decode created: %v body=%s", err, body)
		}
		whID, _ := created["id"].(string)
		if whID == "" {
			t.Fatalf("created webhook missing id: %s", body)
		}
		code2, body2 := env.do(t, http.MethodPost,
			"/api/v1/webhooks/"+whID+"/preview",
			map[string]any{
				"event":  "ai.upstream_error",
				"fields": map[string]any{"service_id": env.serviceID},
			})
		if code2 != http.StatusOK {
			t.Fatalf("preview status=%d body=%s", code2, body2)
		}
		var prev map[string]any
		if err := json.Unmarshal(body2, &prev); err != nil {
			t.Fatalf("decode preview: %v body=%s", err, body2)
		}
		if _, ok := prev["rendered"]; !ok {
			t.Errorf("preview missing rendered key (body=%s)", body2)
		}
	})

	// --- Part J — embedded OpenAPI viewer ---------------------------------
	t.Run("J_openapi_viewer", func(t *testing.T) {
		resp := env.rawGet(t, "/api/v1/openapi/viewer/")
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type=%q want prefix text/html", ct)
		}
	})
}

// resetPostgresSchema drops + recreates the `public` schema on the Postgres
// instance at pgURL. Called BEFORE OpenPostgres so the migration ladder runs
// against an empty schema and each test starts deterministically.
//
// Requires the connecting role to own the `public` schema (the default for
// the `postgres` superuser the CI compose harness provides). On failure the
// test is fatal — there's no graceful degradation for a stuck schema.
func resetPostgresSchema(t *testing.T, pgURL string) {
	t.Helper()
	b, err := db.OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("resetPostgresSchema open: %v", err)
	}
	defer b.Close()
	_, err = b.DB().ExecContext(context.Background(),
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`)
	if err != nil {
		t.Fatalf("resetPostgresSchema DROP/CREATE: %v", err)
	}
}

// _ keeps the x509 import live for future certPool integration without
// re-shuffling the import block on every revision.
var _ = x509.NewCertPool
