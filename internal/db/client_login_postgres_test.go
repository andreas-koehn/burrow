//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestClientLogin_Postgres runs the client sign-in queries against a live
// Postgres: the same statements, the same state checks, real concurrency.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestClientLogin_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres client sign-in check")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	checkClientLogin(t, x, "u-login-pg")
}
