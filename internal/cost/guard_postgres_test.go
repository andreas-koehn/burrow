//go:build postgres

package cost_test

import (
	"os"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

// TestGuard_WithSinkAndDatabase_Postgres runs the usage sink, the cost engine
// and the budget guard against a live Postgres: the same check as on SQLite.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestGuard_WithSinkAndDatabase_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres budget guard check")
	}
	b, err := db.OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := db.Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	checkGuardWithSink(t, x, "u-guard-pg")
}
