//go:build postgres

package store

import (
	"os"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

// TestUpdateGatewayKey_Concurrent_Postgres runs the concurrent partial
// updates against a live Postgres, where each update runs on a connection of
// its own.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestUpdateGatewayKey_Concurrent_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres gateway key update check")
	}
	b, err := db.OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = b.DB().Close() })
	checkConcurrentKeyUpdates(t, New(b.DB()))
}
