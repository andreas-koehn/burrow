package connlog

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// testDB opens a temp SQLite database, runs all migrations, and returns a
// *db.DB. The database is closed when t finishes.
func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "connlog_test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	x := db.Wrap(d)
	t.Cleanup(func() { _ = x.Close() })
	return x
}

// seedUser inserts a minimal users row so FK constraints are satisfied.
func seedUser(t *testing.T, x *db.DB, id string) {
	t.Helper()
	_, err := x.DB().Exec(
		`INSERT INTO users(id,email,password_hash,role) VALUES(?,?,?,'user')`,
		id, id+"@test.invalid", "hash",
	)
	if err != nil {
		t.Fatalf("seedUser: %v", err)
	}
}

// seedService inserts a minimal services row.
func seedService(t *testing.T, x *db.DB, id, userID string) {
	t.Helper()
	_, err := x.DB().Exec(
		`INSERT INTO services(id,user_id,name,type,subdomain,access_mode,api_key_header)
		 VALUES(?,?,'svc','http','sub','open','Authorization')`,
		id, userID,
	)
	if err != nil {
		t.Fatalf("seedService: %v", err)
	}
}

// waitForRows polls until the connection_logs table has at least n rows or
// times out after 2 s. Required because Record spawns a goroutine.
func waitForRows(t *testing.T, x *db.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int
		_ = x.DB().QueryRow(`SELECT COUNT(*) FROM connection_logs`).Scan(&count)
		if count >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: want %d connection_log rows, got %d", n, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestConnLogSinkRecordsAndQueryReturnsDescendingByStartedAt inserts three
// entries with started_at staggered by 1 s and asserts ListConnectionLogs
// returns them newest-first.
func TestConnLogSinkRecordsAndQueryReturnsDescendingByStartedAt(t *testing.T) {
	x := testDB(t)
	sink := NewSQLSink(x, nil)
	ctx := context.Background()

	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		start := base.Add(time.Duration(i) * time.Second)
		end := start.Add(100 * time.Millisecond)
		if err := sink.Record(ctx, Entry{
			Kind:      KindHTTPProxy,
			SourceIP:  "10.0.0.1",
			StartedAt: start,
			EndedAt:   end,
			Status:    StatusClosedClean,
		}); err != nil {
			t.Fatalf("Record[%d]: %v", i, err)
		}
	}

	waitForRows(t, x, 3)

	rows, err := ListConnectionLogs(ctx, x, ConnLogQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListConnectionLogs: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	// Newest first.
	if !rows[0].StartedAt.After(rows[1].StartedAt) {
		t.Errorf("rows[0].StartedAt=%v should be after rows[1].StartedAt=%v",
			rows[0].StartedAt, rows[1].StartedAt)
	}
	if !rows[1].StartedAt.After(rows[2].StartedAt) {
		t.Errorf("rows[1].StartedAt=%v should be after rows[2].StartedAt=%v",
			rows[1].StartedAt, rows[2].StartedAt)
	}
}

// TestConnLogSinkFiltersOnKind asserts that the kind= filter is applied.
func TestConnLogSinkFiltersOnKind(t *testing.T) {
	x := testDB(t)
	sink := NewSQLSink(x, nil)
	ctx := context.Background()

	now := time.Now().UTC()
	for _, kind := range []Kind{KindHTTPProxy, KindTCPProxy, KindControl} {
		_ = sink.Record(ctx, Entry{
			Kind:      kind,
			SourceIP:  "10.0.0.1",
			StartedAt: now,
			EndedAt:   now.Add(50 * time.Millisecond),
			Status:    StatusClosedClean,
		})
	}
	waitForRows(t, x, 3)

	rows, err := ListConnectionLogs(ctx, x, ConnLogQuery{Kind: "http_proxy", Limit: 10})
	if err != nil {
		t.Fatalf("ListConnectionLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 http_proxy row, got %d", len(rows))
	}
	if rows[0].Kind != "http_proxy" {
		t.Errorf("unexpected kind %q", rows[0].Kind)
	}
}

// TestConnLogSinkCursorPagination verifies that ?before_id= returns older rows.
func TestConnLogSinkCursorPagination(t *testing.T) {
	x := testDB(t)
	sink := NewSQLSink(x, nil)
	ctx := context.Background()

	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		start := base.Add(time.Duration(i) * time.Second)
		_ = sink.Record(ctx, Entry{
			Kind:      KindHTTPProxy,
			SourceIP:  "10.0.0.1",
			StartedAt: start,
			EndedAt:   start.Add(10 * time.Millisecond),
			Status:    StatusClosedClean,
		})
	}
	waitForRows(t, x, 5)

	// First page: 2 newest.
	page1, err := ListConnectionLogs(ctx, x, ConnLogQuery{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("want 2 rows on page1, got %d", len(page1))
	}
	cursorID := page1[len(page1)-1].ID

	// Second page: rows older than cursorID.
	page2, err := ListConnectionLogs(ctx, x, ConnLogQuery{BeforeID: cursorID, Limit: 10})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 3 {
		t.Fatalf("want 3 rows on page2, got %d", len(page2))
	}
	// All page2 rows must be older than the cursor.
	cursorTime := page1[len(page1)-1].StartedAt
	for _, r := range page2 {
		if !r.StartedAt.Before(cursorTime) {
			t.Errorf("page2 row %s started_at=%v not before cursor=%v", r.ID, r.StartedAt, cursorTime)
		}
	}
}

// TestRecordHonoursDetachedContext verifies that Record still lands the row in
// the DB even when the caller's context is already cancelled at call time.
// This covers the proxy hot-path pattern:
//
//	defer sink.Record(r.Context(), entry)
//
// where r.Context() is cancelled the moment the handler returns.
func TestRecordHonoursDetachedContext(t *testing.T) {
	x := testDB(t)
	sink := NewSQLSink(x, nil)

	// Pre-cancel the context before calling Record.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	now := time.Now().UTC()
	if err := sink.Record(ctx, Entry{
		Kind:      KindHTTPProxy,
		SourceIP:  "192.0.2.1",
		StartedAt: now,
		EndedAt:   now.Add(50 * time.Millisecond),
		Status:    StatusClosedClean,
	}); err != nil {
		t.Fatalf("Record returned unexpected error: %v", err)
	}

	// Poll until the row lands or we time out.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := ListConnectionLogs(context.Background(), x, ConnLogQuery{Limit: 10})
		if len(entries) >= 1 {
			return // success
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("entry never landed in DB despite pre-cancelled caller context")
}

// fakeSettingsReader is a minimal in-memory settingsReader for tests.
type fakeSettingsReader struct {
	m map[string]string
}

func (f *fakeSettingsReader) GetSettings(_ context.Context) (map[string]string, error) {
	if f.m == nil {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(f.m))
	for k, v := range f.m {
		out[k] = v
	}
	return out, nil
}

// TestRollup_TopIPsToggle verifies the Q12 toggle behaviour:
//   - When the setting is unset (default) or "true", Rollup populates the top
//     10 source IPs per (day, service_id, kind) group into
//     connection_log_rollup_top_ips.
//   - When the setting flips to "false", the next Rollup deletes any existing
//     rows for the affected (day, service_id, kind) groups AND skips the
//     aggregation step.
func TestRollup_TopIPsToggle(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()

	seedUser(t, x, "u1")
	seedService(t, x, "s1", "u1")

	settings := &fakeSettingsReader{m: map[string]string{}}
	sink := NewSQLSink(x, nil).WithSettings(settings)

	day := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
	base := day.Add(1 * time.Hour)

	// Seed 15 distinct source IPs with varied session counts so the top-10
	// truncation is observable. IP "1.2.3.<n>" has n+1 sessions (n=0..14).
	id := 0
	for n := 0; n < 15; n++ {
		ip := fmt.Sprintf("1.2.3.%d", n)
		for s := 0; s <= n; s++ {
			start := base.Add(time.Duration(id) * time.Millisecond)
			end := start.Add(50 * time.Millisecond)
			_, err := x.DB().ExecContext(ctx,
				`INSERT INTO connection_logs
				  (id, kind, service_id, source_ip, started_at, ended_at,
				   duration_ms, bytes_in, bytes_out, status)
				 VALUES (?,?,?,?,?,?,?,0,0,'closed_clean')`,
				fmt.Sprintf("toggle-%03d", id),
				"http_proxy", "s1", ip,
				start.UTC(), end.UTC(), 50,
			)
			if err != nil {
				t.Fatalf("insert[%d]: %v", id, err)
			}
			id++
		}
	}

	// Default (unset setting) → toggle is ON.
	if err := sink.Rollup(ctx, day); err != nil {
		t.Fatalf("Rollup(default): %v", err)
	}
	var topCount int
	_ = x.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM connection_log_rollup_top_ips
		  WHERE day=? AND service_id='s1' AND kind='http_proxy'`,
		day.Format("2006-01-02"),
	).Scan(&topCount)
	if topCount != 10 {
		t.Fatalf("default toggle ON: want 10 top-ip rows, got %d", topCount)
	}

	// Top row should be 1.2.3.14 with 15 sessions (largest group).
	var topIP string
	var topSessions int64
	if err := x.DB().QueryRowContext(ctx,
		`SELECT ip, sessions FROM connection_log_rollup_top_ips
		  WHERE day=? AND service_id='s1' AND kind='http_proxy'
		  ORDER BY sessions DESC LIMIT 1`,
		day.Format("2006-01-02"),
	).Scan(&topIP, &topSessions); err != nil {
		t.Fatalf("query top row: %v", err)
	}
	if topIP != "1.2.3.14" || topSessions != 15 {
		t.Errorf("want top=1.2.3.14/15, got %s/%d", topIP, topSessions)
	}

	// Flip OFF and re-run.
	settings.m["connection_logs.rollup_include_top_ips"] = "false"
	if err := sink.Rollup(ctx, day); err != nil {
		t.Fatalf("Rollup(off): %v", err)
	}
	_ = x.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM connection_log_rollup_top_ips
		  WHERE day=? AND service_id='s1' AND kind='http_proxy'`,
		day.Format("2006-01-02"),
	).Scan(&topCount)
	if topCount != 0 {
		t.Fatalf("toggle OFF: want 0 top-ip rows (deleted), got %d", topCount)
	}

	// Flip back ON via explicit "true" and re-run.
	settings.m["connection_logs.rollup_include_top_ips"] = "true"
	if err := sink.Rollup(ctx, day); err != nil {
		t.Fatalf("Rollup(on): %v", err)
	}
	_ = x.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM connection_log_rollup_top_ips
		  WHERE day=? AND service_id='s1' AND kind='http_proxy'`,
		day.Format("2006-01-02"),
	).Scan(&topCount)
	if topCount != 10 {
		t.Fatalf("toggle ON re-enable: want 10 top-ip rows, got %d", topCount)
	}

	// The base connection_log_rollups row count must remain 1 across all
	// toggles (the toggle only affects the aux table, not the per-group
	// rollup).
	var rollupCount int
	_ = x.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM connection_log_rollups
		  WHERE day=? AND service_id='s1' AND kind='http_proxy'`,
		day.Format("2006-01-02"),
	).Scan(&rollupCount)
	if rollupCount != 1 {
		t.Errorf("base rollup row count want 1, got %d", rollupCount)
	}
}

// TestRollupIdempotentAndComputesP95 inserts 100 entries with durations
// 100..199 ms for one service/day, runs Rollup twice, and asserts:
//   - exactly 1 rollup row exists (idempotent),
//   - p95_duration_ms == 195 (index 95 of 100 sorted values 100..199).
func TestRollupIdempotentAndComputesP95(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()

	seedUser(t, x, "u1")
	seedService(t, x, "s1", "u1")

	sink := NewSQLSink(x, nil)

	day := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	base := day.Add(1 * time.Hour)

	// Insert 100 rows with durations 100..199 ms directly (synchronous Exec)
	// so we don't need to poll.
	for i := 0; i < 100; i++ {
		dur := 100 + i // ms
		start := base.Add(time.Duration(i) * time.Second)
		end := start.Add(time.Duration(dur) * time.Millisecond)
		_, err := x.DB().ExecContext(ctx,
			`INSERT INTO connection_logs
			  (id, kind, service_id, source_ip, started_at, ended_at,
			   duration_ms, bytes_in, bytes_out, status)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("cl-%03d", i), // unique ID
			"http_proxy", "s1", "1.2.3.4",
			start.UTC(), end.UTC(),
			dur, 0, 0, "closed_clean",
		)
		if err != nil {
			t.Fatalf("insert[%d]: %v", i, err)
		}
	}

	// First rollup.
	if err := sink.Rollup(ctx, day); err != nil {
		t.Fatalf("Rollup(1st): %v", err)
	}

	// Verify row count and p95.
	var rowCount int
	_ = x.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM connection_log_rollups`).Scan(&rowCount)
	if rowCount != 1 {
		t.Fatalf("want 1 rollup row, got %d", rowCount)
	}

	var p95 int64
	var sessions int64
	_ = x.DB().QueryRowContext(ctx,
		`SELECT p95_duration_ms, sessions FROM connection_log_rollups WHERE day=? AND service_id='s1' AND kind='http_proxy'`,
		day.Format("2006-01-02"),
	).Scan(&p95, &sessions)

	if sessions != 100 {
		t.Errorf("want sessions=100, got %d", sessions)
	}
	// p95 of [100..199] (n=100, sorted): index floor(100 * 0.95) = 95 → value 195.
	if p95 != 195 {
		t.Errorf("want p95=195, got %d", p95)
	}

	// Second rollup (idempotent).
	if err := sink.Rollup(ctx, day); err != nil {
		t.Fatalf("Rollup(2nd): %v", err)
	}

	// Still only 1 row.
	_ = x.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM connection_log_rollups`).Scan(&rowCount)
	if rowCount != 1 {
		t.Fatalf("after 2nd rollup: want 1 row, got %d", rowCount)
	}
}

// TestUpsertTopIPsIsTransactional pins the v0.5.2 BACKLOG #3 fix: upsertTopIPs
// must be atomic so a concurrent reader cannot observe an empty top-IPs window
// during the DELETE-then-INSERT sequence.
//
// Setup: seed enough source IPs to fill the top-10 window, then call
// upsertTopIPs once to populate the aux table. A goroutine polls reads of the
// aux table while the main goroutine calls upsertTopIPs again. With the
// pre-fix non-transactional implementation, the reader observes 0 rows during
// the gap; with the transactional fix, the reader always observes the full
// window.
func TestUpsertTopIPsIsTransactional(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()

	seedUser(t, x, "u1")
	seedService(t, x, "s1", "u1")

	sink := NewSQLSink(x, nil)

	day := time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC)
	dayStr := day.Format("2006-01-02")
	base := day.Add(1 * time.Hour)

	// Seed 12 distinct source IPs with varied session counts so the top-10
	// truncation triggers a deterministic 10-row window. IP "10.0.0.<n>" has
	// n+1 sessions (n=0..11).
	id := 0
	for n := 0; n < 12; n++ {
		ip := fmt.Sprintf("10.0.0.%d", n)
		for s := 0; s <= n; s++ {
			start := base.Add(time.Duration(id) * time.Millisecond)
			end := start.Add(50 * time.Millisecond)
			_, err := x.DB().ExecContext(ctx,
				`INSERT INTO connection_logs
				  (id, kind, service_id, source_ip, started_at, ended_at,
				   duration_ms, bytes_in, bytes_out, status)
				 VALUES (?,?,?,?,?,?,?,0,0,'closed_clean')`,
				fmt.Sprintf("tx-%03d", id),
				"http_proxy", "s1", ip,
				start.UTC(), end.UTC(), 50,
			)
			if err != nil {
				t.Fatalf("insert[%d]: %v", id, err)
			}
			id++
		}
	}

	// Initial population of the aux table.
	if err := sink.upsertTopIPs(ctx, dayStr, "s1", "http_proxy"); err != nil {
		t.Fatalf("upsertTopIPs (initial): %v", err)
	}

	// Confirm we have 10 rows up front.
	var pre int
	_ = x.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM connection_log_rollup_top_ips
		  WHERE day=? AND service_id='s1' AND kind='http_proxy'`,
		dayStr,
	).Scan(&pre)
	if pre != 10 {
		t.Fatalf("setup: want 10 aux rows, got %d", pre)
	}

	// Concurrent reader: polls the aux table while the writer re-upserts.
	stop := make(chan struct{})
	done := make(chan struct{})
	var sawEmpty int
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var cnt int
			err := x.DB().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM connection_log_rollup_top_ips
				  WHERE day=? AND service_id='s1' AND kind='http_proxy'`,
				dayStr,
			).Scan(&cnt)
			if err != nil {
				continue
			}
			if cnt == 0 {
				sawEmpty++
			}
		}
	}()

	// Run the writer several times to widen the race window.
	for i := 0; i < 25; i++ {
		if err := sink.upsertTopIPs(ctx, dayStr, "s1", "http_proxy"); err != nil {
			t.Fatalf("upsertTopIPs (loop %d): %v", i, err)
		}
	}
	close(stop)
	<-done

	if sawEmpty > 0 {
		t.Errorf("reader observed empty top-IPs window %d times — upsertTopIPs is not transactional", sawEmpty)
	}

	// Final assertion: aux table still has the full 10-row window.
	var post int
	_ = x.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM connection_log_rollup_top_ips
		  WHERE day=? AND service_id='s1' AND kind='http_proxy'`,
		dayStr,
	).Scan(&post)
	if post != 10 {
		t.Errorf("post-writes: want 10 aux rows, got %d", post)
	}
}

// insertLogAt inserts one connection log row that started at ts.
func insertLogAt(t *testing.T, x *db.DB, id, ip string, ts time.Time, durationMs int) {
	t.Helper()
	_, err := x.DB().Exec(
		`INSERT INTO connection_logs
		  (id, kind, service_id, source_ip, started_at, ended_at,
		   duration_ms, bytes_in, bytes_out, status)
		 VALUES (?,?,?,?,?,?,?,0,0,'closed_clean')`,
		id, "http_proxy", "s1", ip, ts.UTC(), ts.Add(time.Second).UTC(), durationMs,
	)
	if err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

// A day is the half-open interval [00:00, 24:00) UTC: a row a nanosecond
// before midnight belongs to the day before, one at midnight to the day that
// starts, one just before the next midnight to the same day. The rollup, the
// top source IPs and the p95 all use the same range.
func TestRollup_DayBoundaries(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	seedUser(t, x, "u1")
	seedService(t, x, "s1", "u1")

	d1 := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
	d2 := d1.Add(24 * time.Hour)
	insertLogAt(t, x, "a", "198.51.100.1", d1.Add(-time.Nanosecond), 1)      // d0
	insertLogAt(t, x, "b", "198.51.100.2", d1, 10)                           // d1, at midnight
	insertLogAt(t, x, "c", "198.51.100.2", d1.Add(500*time.Millisecond), 20) // d1, fraction
	insertLogAt(t, x, "d", "198.51.100.3", d2.Add(-time.Nanosecond), 30)     // d1, last instant
	insertLogAt(t, x, "e", "198.51.100.4", d2, 1000)                         // d2

	sink := NewSQLSink(x, nil)
	if err := sink.Rollup(ctx, d1.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var sessions int
	if err := x.DB().QueryRow(`SELECT sessions FROM connection_log_rollups WHERE day=? AND service_id='s1'`, "2026-03-16").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 3 {
		t.Fatalf("day sessions = %d, want 3 (midnight, midnight+500ms, last nanosecond)", sessions)
	}
	var n int
	if err := x.DB().QueryRow(`SELECT COUNT(*) FROM connection_log_rollups WHERE day<>?`, "2026-03-16").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rollup rows for other days: %d, %v", n, err)
	}
	var ips int
	if err := x.DB().QueryRow(`SELECT COUNT(*) FROM connection_log_rollup_top_ips WHERE day=?`, "2026-03-16").Scan(&ips); err != nil || ips != 2 {
		t.Fatalf("top ips for the day: %d (%v), want the 2 addresses of b, c and d", ips, err)
	}
	p95, err := sink.computeP95(ctx, "2026-03-16", "s1", "http_proxy")
	if err != nil || p95 != 30 {
		t.Fatalf("p95 = %d, %v; want 30 (rows 10, 20, 30 only)", p95, err)
	}
}

// A day comes back from a DATE column as a timestamp on Postgres and as text
// on SQLite; the batch of top source IPs is keyed by the short form either way.
func TestDayKey(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-10":                    "2026-10-10",
		"2026-10-10T00:00:00Z":          "2026-10-10",
		"2026-10-10 00:00:00 +0000 UTC": "2026-10-10",
		"":                              "",
	} {
		if got := dayKey(in); got != want {
			t.Errorf("dayKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// The batch finds the top IPs of a rolled day by the day's short form.
func TestListTopIPsBatch_KeyedByShortDay(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	seedUser(t, x, "u1")
	seedService(t, x, "s1", "u1")
	d := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
	insertLogAt(t, x, "a", "198.51.100.2", d.Add(time.Hour), 1)
	if err := NewSQLSink(x, nil).Rollup(ctx, d); err != nil {
		t.Fatal(err)
	}
	g := TopIPsGroup{Day: "2026-03-16", ServiceID: "s1", Kind: "http_proxy"}
	got, err := ListConnectionLogRollupTopIPsBatch(ctx, x, []TopIPsGroup{g})
	if err != nil || len(got[g]) != 1 || got[g][0].IP != "198.51.100.2" {
		t.Fatalf("batch: %v, %v", got, err)
	}
}

// A rollup row whose day comes back as a timestamp (Postgres hands a DATE
// column back that way) is listed with the short date, so the API's day is the
// same on both databases and matches the keys of the top-IP batch.
func TestListConnectionLogRollups_DayIsADate(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	seedUser(t, x, "u1")
	seedService(t, x, "s1", "u1")
	if _, err := x.DB().Exec(`INSERT INTO connection_log_rollups
		(day, service_id, kind, sessions, bytes_in, bytes_out, avg_duration_ms, p95_duration_ms)
		VALUES ('2026-03-16T00:00:00Z','s1','http_proxy',1,1,1,1,1)`); err != nil {
		t.Fatal(err)
	}
	rows, err := ListConnectionLogRollups(ctx, x, RollupQuery{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v, %v", rows, err)
	}
	if rows[0].Day != "2026-03-16" {
		t.Fatalf("Day = %q, want 2026-03-16", rows[0].Day)
	}
}

// RollupDay returns what it wrote.
func TestRollupDay_ReturnsWhatItWrote(t *testing.T) {
	x := testDB(t)
	seedUser(t, x, "u1")
	seedService(t, x, "s1", "u1")
	d := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
	insertLogAt(t, x, "a", "198.51.100.2", d.Add(time.Hour), 10)
	insertLogAt(t, x, "b", "198.51.100.2", d.Add(2*time.Hour), 30)
	got, err := NewSQLSink(x, nil).RollupDay(context.Background(), d)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v, %v", got, err)
	}
	g := got[0]
	if g.ServiceID != "s1" || g.Kind != "http_proxy" || g.Sessions != 2 || g.AvgDurationMs != 20 || g.P95DurationMs != 30 || len(g.TopIPs) != 1 || g.TopIPs[0] != (TopIP{IP: "198.51.100.2", Sessions: 2}) {
		t.Fatalf("%+v", g)
	}
}
