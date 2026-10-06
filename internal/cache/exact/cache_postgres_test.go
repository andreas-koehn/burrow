//go:build postgres

package exact

import (
	"os"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

// TestCacheLifecycle_Postgres runs the exact cache against a live Postgres:
// the same check as on SQLite. An entry's age is compared in Go, so no date
// function of either database is involved.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestCacheLifecycle_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres exact cache check")
	}
	b, err := db.OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	d := db.Wrap(b.DB())
	t.Cleanup(func() { _ = d.Close() })
	checkCacheLifecycle(t, New(d, nil), "pg")
}
