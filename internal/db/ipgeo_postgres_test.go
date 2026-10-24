//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestCountCountryRules_Postgres runs the count against a live Postgres,
// where the country lists are JSONB columns.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestCountCountryRules_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres country rule count")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	checkCountCountryRules(t, x)
}
