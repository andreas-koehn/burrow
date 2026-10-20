//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestCleanup_Postgres runs the cleanup round's checks of internal/db
// against a live Postgres.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestCleanup_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres cleanup checks")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	t.Run("delete session, parent gone", func(t *testing.T) { checkDeleteSessionParentGone(t, x, "u-gone-pg") })
	t.Run("migration 0039 down and up", func(t *testing.T) {
		checkMigration0029DownAndUp(t, x, "0039_v0.12.1_cache_guard_gen.postgres.sql")
	})
	t.Run("list service ai configs", func(t *testing.T) { checkListServiceAIConfigs(t, x, "u-aicfg-pg") })
	t.Run("tools of several sources", func(t *testing.T) { checkListMCPToolsForSlugs(t, x, "u-mcpslugs-pg") })
}
