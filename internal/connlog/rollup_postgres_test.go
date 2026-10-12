//go:build postgres

package connlog

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// TestRollup_Postgres runs the three statements of the rollup (the groups,
// the top source IPs, the p95) against a live Postgres, with rows around
// midnight.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestRollup_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres rollup check")
	}
	b, err := db.OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := db.Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	ctx := context.Background()

	const user, svc = "u-rollup-pg", "s-rollup-pg"
	_, _ = x.DB().Exec(`DELETE FROM connection_log_rollup_top_ips WHERE service_id=?`, svc)
	_, _ = x.DB().Exec(`DELETE FROM connection_log_rollups WHERE service_id=?`, svc)
	_, _ = x.DB().Exec(`DELETE FROM connection_logs WHERE service_id=?`, svc)
	_, _ = x.DB().Exec(`DELETE FROM services WHERE id=?`, svc)
	_, _ = x.DB().Exec(`DELETE FROM users WHERE id=?`, user)
	seedUser(t, x, user)
	seedService(t, x, svc, user)
	t.Cleanup(func() {
		_, _ = x.DB().Exec(`DELETE FROM connection_log_rollup_top_ips WHERE service_id=?`, svc)
		_, _ = x.DB().Exec(`DELETE FROM connection_log_rollups WHERE service_id=?`, svc)
		_, _ = x.DB().Exec(`DELETE FROM connection_logs WHERE service_id=?`, svc)
		_, _ = x.DB().Exec(`DELETE FROM services WHERE id=?`, svc)
		_, _ = x.DB().Exec(`DELETE FROM users WHERE id=?`, user)
	})

	d1 := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
	for i, r := range []struct {
		ts  time.Time
		ip  string
		dur int
	}{
		{d1.Add(-time.Second), "198.51.100.1", 1},
		{d1, "198.51.100.2", 10},
		{d1.Add(time.Hour), "198.51.100.2", 20},
		{d1.Add(24*time.Hour - time.Second), "198.51.100.3", 30},
		{d1.Add(24 * time.Hour), "198.51.100.4", 1000},
	} {
		_, err := x.DB().Exec(
			`INSERT INTO connection_logs
			  (id, kind, service_id, source_ip, started_at, ended_at,
			   duration_ms, bytes_in, bytes_out, status)
			 VALUES (?,?,?,?,?,?,?,0,0,'closed_clean')`,
			"pg-rollup-"+string(rune('a'+i)), "http_proxy", svc, r.ip, r.ts, r.ts.Add(time.Second), r.dur)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	sink := NewSQLSink(x, nil)
	if err := sink.Rollup(ctx, d1); err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	var sessions int
	if err := x.DB().QueryRow(`SELECT sessions FROM connection_log_rollups WHERE service_id=? AND day=?`, svc, "2026-03-16").Scan(&sessions); err != nil || sessions != 3 {
		t.Fatalf("sessions = %d, %v; want 3", sessions, err)
	}
	var ips int
	if err := x.DB().QueryRow(`SELECT COUNT(*) FROM connection_log_rollup_top_ips WHERE service_id=? AND day=?`, svc, "2026-03-16").Scan(&ips); err != nil || ips != 2 {
		t.Fatalf("top ips = %d, %v; want 2", ips, err)
	}
	// The listed rows carry the short day, and the lookup the API handler
	// makes (rows keyed by Day against the batch) finds the top IPs.
	listed, err := ListConnectionLogRollups(ctx, x, RollupQuery{ServiceID: svc})
	if err != nil || len(listed) != 1 || listed[0].Day != "2026-03-16" {
		t.Fatalf("listed rollups: %+v, %v; want one with Day 2026-03-16", listed, err)
	}
	hg := TopIPsGroup{Day: listed[0].Day, ServiceID: listed[0].ServiceID, Kind: listed[0].Kind}
	byRow, err := ListConnectionLogRollupTopIPsBatch(ctx, x, []TopIPsGroup{hg})
	if err != nil || len(byRow[hg]) != 2 {
		t.Fatalf("top ips by the listed row's day: %v, %v; want 2", byRow, err)
	}
	// The announcement looks the top IPs up by the short day: Postgres hands
	// the DATE column back as a timestamp, the batch must key it as 2006-01-02.
	g := TopIPsGroup{Day: "2026-03-16", ServiceID: svc, Kind: "http_proxy"}
	top, err := ListConnectionLogRollupTopIPsBatch(ctx, x, []TopIPsGroup{g})
	if err != nil || len(top[g]) != 2 {
		t.Fatalf("top ips by the short day: %v, %v; want 2", top, err)
	}
	if p95, err := sink.computeP95(ctx, "2026-03-16", svc, "http_proxy"); err != nil || p95 != 30 {
		t.Fatalf("p95 = %d, %v; want 30", p95, err)
	}
}
