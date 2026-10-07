//go:build postgres

package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUsageAccounting_Postgres runs the usage queries of the cost engine, the
// budget guard and the day quotas against a live Postgres: the grouped window
// query, the daily token sums and the daily per-subject sums and counts, for a
// service key id and for a "gw:<id>" subject. The checks are the ones the
// SQLite test runs. Every time boundary is computed in Go and bound.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestUsageAccounting_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres usage accounting check")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	t.Run("usage accounting", func(t *testing.T) { checkUsageAccounting(t, x, "u-acct-pg") })
	t.Run("ai endpoint metrics", func(t *testing.T) { checkAIEndpointMetrics(t, x, "u-metrics-pg") })
	t.Run("expired sessions", func(t *testing.T) { checkDeleteExpiredSessions(t, x, "u-sess-pg") })
	t.Run("revoke if active", func(t *testing.T) { checkRevokeAIGatewayKeyIfActive(t, x, "u-revoke-pg") })
	// Bound times are instants: nothing changes when the session's time zone
	// is west or east of UTC.
	for _, zone := range []string{"America/New_York", "Asia/Tokyo"} {
		t.Run("timezone "+zone, func(t *testing.T) {
			u, err := url.Parse(pgURL)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			q.Set("timezone", zone)
			u.RawQuery = q.Encode()
			zb, err := OpenPostgres(u.String())
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			z := Wrap(zb.DB())
			t.Cleanup(func() { _ = z.Close() })
			var got string
			if err := z.DB().QueryRow(`SHOW timezone`).Scan(&got); err != nil || got != zone {
				t.Fatalf("session time zone = %q (%v), want %q", got, err, zone)
			}
			checkUsageAccounting(t, z, "u-acct-pg-tz")
			checkAIEndpointMetrics(t, z, "u-metrics-pg-tz")
			checkDeleteExpiredSessions(t, z, "u-sess-pg-tz")
		})
	}
	// The two reads migration 0024 indexes do not read the table whole. The
	// planner decides by the table's statistics, so the table is given some
	// thousand rows over a month and forty models and analysed first; which
	// of the usable indexes it then takes is its business.
	t.Run("usage indexes", func(t *testing.T) {
		ctx := context.Background()
		const userID = "u-idx-pg"
		_ = x.DeleteUser(ctx, userID)
		mustUser(t, x, userID)
		t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
		svc := seedSvc(t, x, userID, "svc-idx")
		if _, err := x.sqlDB.ExecContext(ctx, `
			INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, requested_model, gateway_key_id, tokens_in, bytes_in)
			SELECT 'idx-' || g, ?, '', now() - (g % 4320) * interval '10 minutes', 'openai',
			       'model-' || (g % 40), 'gk-' || (g % 25), g, g
			  FROM generate_series(1, 8000) g`, svc); err != nil {
			t.Fatal(err)
		}
		if _, err := x.sqlDB.ExecContext(ctx, `ANALYZE usage_events`); err != nil {
			t.Fatal(err)
		}
		for _, idx := range migration0024Indexes {
			var n string
			if err := x.DB().QueryRow(`SELECT indexname FROM pg_indexes WHERE schemaname='public' AND indexname=$1`, idx).Scan(&n); err != nil {
				t.Errorf("index %s missing: %v", idx, err)
			}
		}
		since := time.Now().UTC().Add(-time.Hour)
		for name, q := range map[string]struct {
			sql  string
			args []any
		}{
			"day sum of a model": {sumDailyUsageSQL("requested_model"), []any{"model-7", since}},
			"window aggregation": {listUsageForWindowSQL, []any{since}},
		} {
			rows, err := x.sqlDB.QueryContext(ctx, `EXPLAIN `+q.sql, q.args...)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			plan := ""
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				plan += line + "\n"
			}
			rows.Close()
			if strings.Contains(plan, "Seq Scan") || !strings.Contains(plan, "Index") {
				t.Errorf("%s reads the table whole or uses no index:\n%s", name, plan)
			}
		}
	})
}

// freshDatabase creates an empty database next to the one pgURL names and
// returns its URL; it is dropped when the test ends.
func freshDatabase(t *testing.T, pgURL string) string {
	t.Helper()
	admin, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer admin.Close()
	name := fmt.Sprintf("burrow_fresh_%d", time.Now().UnixNano())
	if _, err := admin.DB().Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a, err := OpenPostgres(pgURL); err == nil {
			_, _ = a.DB().Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
			_ = a.Close()
		}
	})
	u, err := url.Parse(pgURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// holdMigrationLock takes the migration lock of the database at url in a
// session of its own, as an instance that is migrating (or hangs) would, and
// returns what releases it.
func holdMigrationLock(t *testing.T, url string) (release func()) {
	t.Helper()
	d, err := sql.Open("pgx-rewrite", url)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, pgMigrationLockKey); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, pgMigrationLockKey)
			_ = conn.Close()
			_ = d.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// While another instance holds the migration lock, opening waits and says
// so; once the lock is free it migrates and succeeds.
func TestOpenPostgres_WaitsForTheMigrationLock(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres migration lock check")
	}
	fresh := freshDatabase(t, pgURL)
	release := holdMigrationLock(t, fresh)
	var logs bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil))
	done := make(chan error, 1)
	go func() {
		b, err := openPostgres(fresh, 30*time.Second, log)
		if err == nil {
			err = b.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("opened while the lock was held (err %v)", err)
	case <-time.After(700 * time.Millisecond):
	}
	// Nothing was migrated meanwhile.
	probe, err := sql.Open("pgx-rewrite", fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	var tables int
	if err := probe.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("%d tables exist while the lock is held (%v)", tables, err)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("open after the lock was released: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("still waiting after the lock was released")
	}
	mu.Lock()
	out := logs.String()
	mu.Unlock()
	if strings.Count(out, "migration lock") != 1 || !strings.Contains(out, "level=INFO") {
		t.Fatalf("the wait was not logged exactly once at Info: %q", out)
	}
}

// A lock that is not released in time fails the start with an error that
// names the lock, instead of waiting for ever.
func TestOpenPostgres_MigrationLockDeadline(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres migration lock check")
	}
	fresh := freshDatabase(t, pgURL)
	holdMigrationLock(t, fresh)
	start := time.Now()
	b, err := openPostgres(fresh, 600*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		_ = b.Close()
		t.Fatal("opened although the lock was held past the deadline")
	}
	if d := time.Since(start); d < 500*time.Millisecond || d > 20*time.Second {
		t.Fatalf("gave up after %v, want about the deadline", d)
	}
	if msg := err.Error(); !strings.Contains(msg, "migration lock") || !strings.Contains(msg, fmt.Sprint(pgMigrationLockKey)) {
		t.Fatalf("error does not name the lock: %v", err)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// Two relays that start at the same time against an empty database both run
// the migrations: one must wait for the other instead of colliding with it.
func TestOpenPostgres_ConcurrentOnFreshDatabase(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres concurrent migration check")
	}
	fresh := freshDatabase(t, pgURL)
	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			b, err := OpenPostgres(fresh)
			if err == nil {
				err = b.Close()
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("open %d: %v", i, err)
		}
	}
	// Every migration was applied exactly once.
	b, err := OpenPostgres(fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var applied, distinct int
	if err := b.DB().QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version) FROM schema_migrations`).Scan(&applied, &distinct); err != nil || applied == 0 || applied != distinct {
		t.Fatalf("schema_migrations: %d rows, %d versions (%v)", applied, distinct, err)
	}
}
