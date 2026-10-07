//go:build postgres

package exact

import (
	"net/url"
	"os"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

// TestCacheLifecycle_Postgres runs the exact cache against a live Postgres:
// the same check as on SQLite. An entry's age is compared in Go, so no date
// function of either database is involved. The check runs once in the
// server's own time zone and once each with the session time zone set west
// and east of UTC: when an entry was stored must not depend on it.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestCacheLifecycle_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres exact cache check")
	}
	for _, zone := range []string{"", "America/New_York", "Asia/Tokyo"} {
		t.Run("timezone "+zone, func(t *testing.T) {
			u, err := url.Parse(pgURL)
			if err != nil {
				t.Fatal(err)
			}
			if zone != "" {
				q := u.Query()
				q.Set("timezone", zone)
				u.RawQuery = q.Encode()
			}
			b, err := db.OpenPostgres(u.String())
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			d := db.Wrap(b.DB())
			t.Cleanup(func() { _ = d.Close() })
			if zone != "" {
				var got string
				if err := d.DB().QueryRow(`SHOW timezone`).Scan(&got); err != nil || got != zone {
					t.Fatalf("session time zone = %q (%v), want %q", got, err, zone)
				}
			}
			checkCacheLifecycle(t, New(d, nil), "pg")
		})
	}
}
