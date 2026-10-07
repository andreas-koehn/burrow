//go:build postgres

package aimeter_test

import (
	"os"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

// TestSQLSink_Postgres runs the sink's insert against a live Postgres, where
// streamed and cache_hit are BOOLEAN and the translation columns are TEXT.
// The checks are the ones the SQLite test runs.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestSQLSink_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres sink check")
	}
	b, err := db.OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := db.Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	t.Run("translation", func(t *testing.T) { checkSinkTranslation(t, x, "u-sink-tr-pg") })
}
