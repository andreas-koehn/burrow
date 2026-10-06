//go:build postgres

package db

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	checkClientLoginPerIPCap(t, x)
	checkClientLoginCapUnderConcurrency(t, x)
}

// checkClientLoginCapUnderConcurrency: sixty clients start at the same moment
// on separate connections; exactly as many as the cap allows get a request.
// Postgres checks the count of one statement against a snapshot, so without
// the advisory lock of the start transaction several of them see room.
func checkClientLoginCapUnderConcurrency(t *testing.T, x *DB) {
	t.Helper()
	ctx := context.Background()
	clear := func() { _, _ = x.DB().ExecContext(context.Background(), `DELETE FROM client_login_requests`) }
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for round := 0; round < 5; round++ {
		clear()
		const n = 60
		var wg sync.WaitGroup
		var won atomic.Int64
		errs := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				ok, err := x.InsertClientLogin(ctx, ClientLoginRequest{
					DeviceCodeHash: fmt.Sprintf("cc-hash-%d-%d", round, i), UserCode: fmt.Sprintf("C%d%06d", round, i),
					SourceIP: fmt.Sprintf("198.51.100.%d", i), CreatedAt: t0, ExpiresAt: t0.Add(10 * time.Minute),
				}, 20, 5)
				errs[i] = err
				if ok {
					won.Add(1)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d start %d: %v", round, i, err)
			}
		}
		var rows int
		if err := x.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM client_login_requests`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if won.Load() != 20 || rows != 20 {
			t.Fatalf("round %d: %d starts succeeded, %d rows, want exactly 20", round, won.Load(), rows)
		}
	}
	clear()
}
