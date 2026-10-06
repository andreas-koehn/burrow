//go:build postgres

package db

import (
	"context"
	"fmt"
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
	// The two reads migration 0024 indexes can be answered from the index.
	// The table is small, so the planner is told not to prefer reading it
	// whole: what is checked is that the index fits the query.
	t.Run("usage indexes", func(t *testing.T) {
		ctx := context.Background()
		conn, err := x.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, `SET enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = conn.ExecContext(ctx, `RESET enable_seqscan`) }()
		for idx, q := range usageIndexQueries() {
			rows, err := conn.QueryContext(ctx, `EXPLAIN `+q.sql, q.args...)
			if err != nil {
				t.Fatalf("%s: %v", idx, err)
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
			if !strings.Contains(plan, idx) {
				t.Errorf("the plan does not use %s:\n%s", idx, plan)
			}
		}
	})
}

// Two relays that start at the same time against an empty database both run
// the migrations: one must wait for the other instead of colliding with it.
func TestOpenPostgres_ConcurrentOnFreshDatabase(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres concurrent migration check")
	}
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
	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			b, err := OpenPostgres(u.String())
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
	b, err := OpenPostgres(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var applied, distinct int
	if err := b.DB().QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version) FROM schema_migrations`).Scan(&applied, &distinct); err != nil || applied == 0 || applied != distinct {
		t.Fatalf("schema_migrations: %d rows, %d versions (%v)", applied, distinct, err)
	}
}
