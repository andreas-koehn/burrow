//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestServiceLastSeen_Postgres runs the statements of migration 0027 against
// a live Postgres: the stamp a client leaves on the service it serves and the
// Down block of the migration. The checks are the ones the SQLite tests run.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestServiceLastSeen_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres service last seen check")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	t.Run("never seen", func(t *testing.T) { checkServiceNeverSeen(t, x, "u-never-seen-pg") })
	t.Run("touch", func(t *testing.T) { checkTouchServiceLastSeen(t, x, "u-seen-pg") })
	t.Run("unknown service", func(t *testing.T) { checkTouchServiceLastSeenUnknown(t, x, "u-seen-unknown-pg") })
	t.Run("migration 0027 down and up", func(t *testing.T) {
		checkMigration0027DownAndUp(t, x, "0027_v0.10.0_service_last_seen.postgres.sql", "u-mig27-pg")
	})
}

// TestServiceDeleteKeepsUsage_Postgres runs the delete checks of SQLite and
// the Down and Up blocks of migration 0031 against a live Postgres.
func TestServiceDeleteKeepsUsage_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres service delete check")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	t.Run("delete keeps usage", func(t *testing.T) { checkDeleteServiceKeepsUsage(t, x, "u-del-usage-pg") })
	t.Run("migration 0031 down and up", func(t *testing.T) {
		checkMigration0031DownAndUp(t, x, "0031_v0.11.1_usage_keep_history.postgres.sql", "u-mig31-pg")
	})
}
