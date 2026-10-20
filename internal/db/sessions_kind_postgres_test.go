//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestSessionKind_Postgres runs migration 0038's checks against a live
// Postgres: the kind and parent of a session, the services sessions that end
// with their dashboard session, and the Down block.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestSessionKind_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres session kind check")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	t.Run("kind and parent", func(t *testing.T) { checkSessionKindAndParent(t, x, "u-kind-pg") })
	t.Run("migration 0038 down and up", func(t *testing.T) {
		checkMigration0029DownAndUp(t, x, "0038_v0.12.0_session_kind.postgres.sql")
	})
}
