//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestCustomRoleLifecycle_Postgres runs the custom-role check against a live
// Postgres: the builtin and default_for_new_users columns are BOOLEAN there.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestCustomRoleLifecycle_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres custom role check")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	checkCustomRoleLifecycle(t, x)
}
