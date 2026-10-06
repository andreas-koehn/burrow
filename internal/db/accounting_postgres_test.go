//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestUsageAccounting_Postgres runs the usage queries of the cost engine, the
// budget guard and the day quotas against a live Postgres: the grouped window
// query, the daily token sums and the daily per-subject sums and counts, for a
// service key id and for a "gw:<id>" subject. The checks are the ones the
// SQLite test runs. Every time boundary is computed in Go and bound.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestUsageAccounting_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres usage accounting check")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	t.Run("usage accounting", func(t *testing.T) { checkUsageAccounting(t, x, "u-acct-pg") })
}
