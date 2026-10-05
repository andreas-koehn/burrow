//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestGetAliasesByPriority_LookupPostgres runs the alias lookup against a
// live Postgres: the query must not lean on SQLite-only columns.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestGetAliasesByPriority_LookupPostgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres alias lookup")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	checkAliasLookup(t, x, "u-alias-pg")
}
