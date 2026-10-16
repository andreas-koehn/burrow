//go:build postgres

package db

import (
	"os"
	"testing"
)

// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestTokenlessRequests_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres tokenless check")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	checkTokenlessRequests(t, x, "u-tokenless-pg")
}
