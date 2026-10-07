//go:build postgres

package exact

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

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

// Concurrent Stores of one key must all succeed whatever isolation level the
// database defaults to: the first writer wins and the others see that. Under
// REPEATABLE READ or SERIALIZABLE Postgres used to end the later writers with
// SQLSTATE 40001 (a warning in the log and a skipped cache write). Both a
// fresh key and a key held by an expired entry are raced.
func TestCacheStore_ConcurrentWritersUnderStrictIsolation_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres exact cache check")
	}
	for _, level := range []string{"read committed", "repeatable read", "serializable"} {
		t.Run(level, func(t *testing.T) {
			u, err := url.Parse(pgURL)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			q.Set("default_transaction_isolation", level)
			u.RawQuery = q.Encode()
			b, err := db.OpenPostgres(u.String())
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			d := db.Wrap(b.DB())
			t.Cleanup(func() { _ = d.Close() })
			var got string
			if err := d.DB().QueryRow(`SHOW default_transaction_isolation`).Scan(&got); err != nil || got != level {
				t.Fatalf("default isolation = %q (%v), want %q", got, err, level)
			}
			c := New(d, nil)
			ctx := context.Background()
			scope := "endpoint:pg-iso:/v1/chat/completions"
			_ = c.Clear(ctx, scope)
			t.Cleanup(func() { _ = c.Clear(ctx, scope) })

			const writers, rounds = 8, 25
			for round := 0; round < rounds; round++ {
				key := fmt.Sprintf("%s:%d", scope, round)
				if round%2 == 1 {
					// The key is held by an entry that has outlived its ttl:
					// every writer finds it and wants it gone.
					old := Entry{Status: 200, Body: []byte("old"), CreatedAt: time.Now().Add(-time.Hour), TTLSeconds: 1}
					if err := c.Store(ctx, key, old); err != nil {
						t.Fatalf("round %d: seed expired entry: %v", round, err)
					}
				}
				errs := make(chan error, writers)
				start := make(chan struct{})
				var wg sync.WaitGroup
				for w := 0; w < writers; w++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						errs <- c.Store(ctx, key, Entry{Status: 200, Body: []byte("new"), TTLSeconds: 3600})
					}()
				}
				close(start)
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatalf("round %d: a concurrent Store failed: %v", round, err)
					}
				}
				e, hit, err := c.Lookup(ctx, key)
				if err != nil || !hit || string(e.Body) != "new" {
					t.Fatalf("round %d: Lookup after the race: hit=%v body=%q err=%v", round, hit, e.Body, err)
				}
			}
		})
	}
}
