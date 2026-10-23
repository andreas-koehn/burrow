//go:build integration

// test-only — never deploy this shape.
//
// Build-tagged registration of the /api/v1/internal/test-reset endpoint
// used by the test/e2e e2e harness (Playwright + manual
// runbook). Compiled into the binary ONLY when `go build -tags=integration`
// is set. The default build picks up the no-op stub in
// router_integration_stub.go instead, so production binaries never expose
// this route — verified by `go tool nm` post-build.
package api

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// registerIntegrationRoutes mounts test-only routes onto the /api/v1 sub-
// router. Called unconditionally from router.go inside the /api/v1 closure;
// production builds use the stub variant which is a no-op.
//
// The route is intentionally UNAUTHENTICATED: it is the very thing test
// runners hit to clear state BEFORE they have credentials. The build tag
// is the security boundary — production binaries never compile this in.
func registerIntegrationRoutes(r chi.Router, d Deps) {
	r.Method(http.MethodPost, "/internal/test-reset", testResetHandler(d))
}

// testResetHandler truncates all per-test mutable tables in a single
// transaction, preserving the schema_migrations table and the seeded admin
// user. Tables that don't exist (e.g. older databases without v0.5 schema)
// are skipped via best-effort error tolerance.
//
// d.DB must be *sql.DB (cmd/server wires *sql.DB into Deps.DB via the
// Pinger interface); the assertion fails closed with 500 if not.
func testResetHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		raw, ok := d.DB.(*sql.DB)
		if !ok || raw == nil {
			http.Error(w, "test-reset: Deps.DB is not *sql.DB", http.StatusInternalServerError)
			return
		}
		// Mutable tables to truncate. Order matters when FKs are in play:
		// children before parents. SQLite + Postgres both accept DELETE
		// without WHERE for full-table wipes; TRUNCATE is Postgres-only so
		// DELETE is the portable choice. Missing tables are tolerated so
		// older schemas still reset cleanly.
		tables := []string{
			"audit_events",
			"webhook_deliveries",
			"webhooks",
			"connection_log_rollup_top_ips",
			"connection_log_rollups",
			"connection_logs",
			"service_api_keys",
			"service_access_policy",
			"service_custom_domains",
			"service_ai_config",
			"service_upstream_credentials",
			"service_ip_geo",
			"service_ip_grants",
			"rate_limits",
			"model_aliases",
			"budget_alerts",
			"budgets",
			"cost_pricing",
			"automation_tokens",
			"client_tokens",
			"sessions",
			"services",
		}
		tx, err := raw.BeginTx(r.Context(), nil)
		if err != nil {
			http.Error(w, "test-reset: begin tx: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for _, t := range tables {
			if _, err := tx.ExecContext(r.Context(), "DELETE FROM "+t); err != nil {
				// Tolerate missing tables (older schemas) by checking the
				// error text portably — no driver-specific code branches.
				msg := err.Error()
				if !containsAny(msg, "no such table", "does not exist", "undefined table") {
					_ = tx.Rollback()
					http.Error(w, "test-reset: "+t+": "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
		// Preserve seeded admin user: keep the FIRST user (lowest rowid),
		// drop all others. The seeded admin is created at boot and is
		// guaranteed to be the first row.
		if _, err := tx.ExecContext(r.Context(),
			"DELETE FROM users WHERE id NOT IN (SELECT id FROM users ORDER BY created_at ASC LIMIT 1)"); err != nil {
			msg := err.Error()
			if !containsAny(msg, "no such table", "does not exist", "undefined table") {
				_ = tx.Rollback()
				http.Error(w, "test-reset: users: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "test-reset: commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		for i := 0; i+len(n) <= len(s); i++ {
			if s[i:i+len(n)] == n {
				return true
			}
		}
	}
	return false
}
