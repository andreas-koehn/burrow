//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestClientLastSession_Postgres runs the statements of migration 0026
// against a live Postgres: the record a client session leaves on its token,
// the list of every user's tokens and the Down block of the migration. The
// checks are the ones the SQLite tests run.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestClientLastSession_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres client last session check")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	t.Run("record", func(t *testing.T) { checkRecordClientSession(t, x, "u-rec-pg") })
	t.Run("unknown token", func(t *testing.T) { checkRecordClientSessionUnknownToken(t, x, "u-rec-unknown-pg") })
	t.Run("caps", func(t *testing.T) { checkRecordClientSessionCapsFields(t, x, "u-rec-caps-pg") })
	t.Run("order", func(t *testing.T) { checkListClientTokensOrder(t, x, "u-order-pg-1", "u-order-pg-2") })
	t.Run("scope", func(t *testing.T) { checkClientTokenScope(t, x, "u-scope-pg") })
	t.Run("migration 0043 down and up", func(t *testing.T) {
		checkMigration0029DownAndUp(t, x, "0043_v0.13.0_client_token_scope.postgres.sql")
	})
	t.Run("migration 0026 down and up", func(t *testing.T) {
		checkMigration0026DownAndUp(t, x, "0026_v0.10.0_client_last_session.postgres.sql", "u-mig26-pg")
	})
}
