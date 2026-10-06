//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestListModelAliases_Postgres runs the alias list, which the one-time
// import into synthetic models reads, against a live Postgres.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestListModelAliases_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres alias list")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	checkListModelAliases(t, x, "u-alias-pg")
}
