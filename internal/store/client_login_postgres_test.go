//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// TestClientLogin_Postgres runs a sign-in from start to collection against a
// live Postgres, with twenty clients polling at once on separate
// connections: exactly one of them gets the token.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestClientLogin_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres client sign-in check")
	}
	b, err := db.OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = b.DB().Close() })
	ctx := context.Background()
	// The client sign-in tests of internal/db empty the same table and count
	// its rows; against one database they and this test take turns. The key
	// is internal/db's clientLoginTestLockKey.
	lock, err := b.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, int64(0x627572726f777463)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = lock.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(0x627572726f777463))
		_ = lock.Close()
	})
	s := New(b.DB())
	clk := newLoginClock()
	s.SetClientLoginClock(clk.now)
	if _, err := b.DB().ExecContext(ctx, `DELETE FROM client_login_requests`); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser(ctx, fmt.Sprintf("login-pg-%d@x", time.Now().UnixNano()), "password1", "user")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteUser(context.Background(), u.ID) })

	for round := 0; round < 5; round++ {
		st, err := s.StartClientLogin(ctx, loginMeta)
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if v, err := s.GetClientLogin(ctx, st.UserCode); err != nil || v.Status != "pending" || v.Hostname != "laptop.local" {
			t.Fatalf("get: %+v %v", v, err)
		}
		if _, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginPending) {
			t.Fatalf("poll while pending: %v", err)
		}
		if _, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginSlowDown) {
			t.Fatalf("poll at once: %v", err)
		}
		if _, err := s.ApproveClientLogin(ctx, st.UserCode, u.ID, "laptop"); err != nil {
			t.Fatalf("approve: %v", err)
		}
		if _, err := s.ApproveClientLogin(ctx, st.UserCode, u.ID, "again"); !errors.Is(err, ErrLoginConflict) {
			t.Fatalf("second approve: %v", err)
		}
		clk.add(3 * time.Second)

		const n = 20
		var wg sync.WaitGroup
		errs := make([]error, n)
		res := make([]LoginResult, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				res[i], errs[i] = s.PollClientLogin(ctx, st.DeviceCode)
			}(i)
		}
		close(start)
		wg.Wait()
		got := 0
		for i := 0; i < n; i++ {
			switch {
			case errs[i] == nil:
				got++
				if uid, name, err := s.AuthenticateNamed(ctx, res[i].Token); err != nil || uid != u.ID || name != "laptop" {
					t.Fatalf("token authenticates as %q/%q (%v)", uid, name, err)
				}
			case errors.Is(errs[i], ErrLoginNotFound), errors.Is(errs[i], ErrLoginSlowDown):
			default:
				t.Fatalf("round %d poll %d: %v", round, i, errs[i])
			}
		}
		if got != 1 {
			t.Fatalf("round %d: %d polls received a token, want exactly 1", round, got)
		}
		ts, err := s.ListClientTokens(ctx, u.ID)
		if err != nil || len(ts) != round+1 {
			t.Fatalf("round %d: %d tokens (%v), want %d", round, len(ts), err, round+1)
		}
		clk.add(3 * time.Second)
		if _, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginNotFound) {
			t.Fatalf("poll after collection: %v", err)
		}
	}

	// Expiry and the sweep, on Postgres timestamps.
	st, err := s.StartClientLogin(ctx, loginMeta)
	if err != nil {
		t.Fatal(err)
	}
	clk.add(10*time.Minute + time.Second)
	if _, err := s.GetClientLogin(ctx, st.UserCode); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("get after expiry: %v", err)
	}
	if n, err := s.SweepClientLogins(ctx); err != nil || n != 1 {
		t.Fatalf("sweep removed %d (%v), want 1", n, err)
	}
}
