package retention

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// testDB opens a fresh migrated in-memory-style SQLite database for tests.
func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	x := db.Wrap(d)
	t.Cleanup(func() { _ = x.Close() })
	return x
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// staticLoader implements Loader with a fixed Settings value.
type staticLoader struct{ s Settings }

func (l *staticLoader) Load(context.Context) (Settings, error) { return l.s, nil }

// noopAuditLogger implements AuditLogger as a no-op.
type noopAuditLogger struct{ calls []string }

func (n *noopAuditLogger) AppendRetentionCompact(_ context.Context, table string, _ int) error {
	n.calls = append(n.calls, table)
	return nil
}

// seedAuditRow inserts a minimal audit_events row with the given action and timestamp.
// It bypasses the hash-chain logic deliberately (direct SQL) to set up test fixtures
// without needing a full audit.Logger with a signing key.
func seedAuditRow(t *testing.T, x *db.DB, id, action string, ts time.Time) {
	t.Helper()
	_, err := x.DB().ExecContext(context.Background(),
		`INSERT INTO audit_events(id, ts, actor_id, actor_email, action,
			subject_id, subject_label, result, source_ip, user_agent, request_id,
			payload, prev_hash, hash)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, ts.UTC(), "", "", action, "", "", "ok", "", "", "", "{}", "0000000000000000000000000000000000000000000000000000000000000000", id,
	)
	if err != nil {
		t.Fatalf("seedAuditRow %s: %v", id, err)
	}
}

// TestCompactorDeletesEligibleAuditRowsOnlyAndIsIdempotent verifies that:
//  1. Eligible audit actions (e.g. redaction.applied) older than the cutoff
//     are deleted.
//  2. Ineligible audit actions (e.g. user.create) are NOT deleted even when
//     they are also older than the cutoff.
//  3. A second call (same window) deletes 0 rows (idempotent).
func TestCompactorDeletesEligibleAuditRowsOnlyAndIsIdempotent(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()

	// Cutoff = now - 30 days; seed rows clearly before the cutoff.
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)

	// 5 eligible rows (redaction.applied)
	for i := 0; i < 5; i++ {
		seedAuditRow(t, x, "r"+string(rune('0'+i)), "redaction.applied", old)
	}
	// 5 ineligible rows (user.create) — must survive compaction
	for i := 0; i < 5; i++ {
		seedAuditRow(t, x, "u"+string(rune('0'+i)), "user.create", old)
	}

	al := &noopAuditLogger{}
	c := New(x, &staticLoader{s: Settings{AuditDays: 30}}, al, discardLog())

	// First run: must delete exactly 5 redaction.applied rows.
	counts, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := counts["audit_events"]; got != 5 {
		t.Errorf("first run: want 5 audit_events deleted, got %d", got)
	}

	// Verify user.create rows are still present.
	var remaining int
	if err := x.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action='user.create'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 5 {
		t.Errorf("user.create rows: want 5 remaining, got %d", remaining)
	}

	// audit appender should have been called once (for audit_events table).
	if len(al.calls) != 1 || al.calls[0] != "audit_events" {
		t.Errorf("audit calls: want [audit_events], got %v", al.calls)
	}

	// Second run (same window): idempotent — 0 additional rows deleted.
	counts2, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce idempotent: %v", err)
	}
	if got := counts2["audit_events"]; got != 0 {
		t.Errorf("second run (idempotent): want 0, got %d", got)
	}
}

// TestCompactorAuditDaysZeroSkipsAuditDeletion verifies that setting
// audit.retention_days = 0 (the spec default, meaning "keep forever")
// results in zero audit rows being deleted, regardless of their age.
func TestCompactorAuditDaysZeroSkipsAuditDeletion(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()

	old := time.Now().UTC().Add(-365 * 24 * time.Hour) // very old
	seedAuditRow(t, x, "e1", "redaction.applied", old)
	seedAuditRow(t, x, "e2", "user.create", old)

	c := New(x, &staticLoader{s: Settings{AuditDays: 0}}, &noopAuditLogger{}, discardLog())
	counts, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n, ok := counts["audit_events"]; ok && n > 0 {
		t.Errorf("audit_days=0 should skip deletion; got %d rows deleted", n)
	}

	var total int
	if err := x.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("want 2 rows preserved, got %d", total)
	}
}

// TestRunOnceLogsAndContinuesOnPerTableError verifies spec F.2: when one table's
// DELETE fails (e.g. because the table has been dropped), RunOnce returns nil
// (not an error) and still processes the surviving tables.
func TestRunOnceLogsAndContinuesOnPerTableError(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()

	// Seed 5 eligible audit_events rows older than the 30-day cutoff.
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		seedAuditRow(t, x, "fa"+string(rune('0'+i)), "redaction.applied", old)
	}

	// Corrupt usage_events (the second table processed) so its DELETE will fail.
	// audit_events must still be compacted despite usage_events failing.
	if _, err := x.DB().ExecContext(ctx, `DROP TABLE usage_events`); err != nil {
		t.Fatalf("drop usage_events: %v", err)
	}

	c := New(x, &staticLoader{s: Settings{
		AuditDays: 30,
		UsageDays: 90, // non-zero so RunOnce attempts the deleted table
	}}, &noopAuditLogger{}, discardLog())

	counts, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce returned err=%v; want nil (log-and-continue per spec F.2)", err)
	}
	if counts["audit_events"] != 5 {
		t.Errorf("audit_events: want 5 deleted, got %d", counts["audit_events"])
	}
	// usage_events should be absent (or zero) because its DELETE failed.
	if n, ok := counts["usage_events"]; ok && n > 0 {
		t.Errorf("usage_events: want absent/0 after table drop, got %d", n)
	}
}

// TestParseHHMM verifies the internal HH:MM parser.
func TestParseHHMM(t *testing.T) {
	cases := []struct {
		in   string
		h, m int
	}{
		{"00:30", 0, 30},
		{"23:59", 23, 59},
		{"invalid", 0, 30}, // fallback
		{"25:00", 0, 30},   // out-of-range hour
		{"12:60", 0, 30},   // out-of-range minute
	}
	for _, tc := range cases {
		h, m := parseHHMM(tc.in)
		if h != tc.h || m != tc.m {
			t.Errorf("parseHHMM(%q) = %d:%d; want %d:%d", tc.in, h, m, tc.h, tc.m)
		}
	}
}

// TestNextFiring verifies nextFiring scheduling logic.
func TestNextFiring(t *testing.T) {
	// When now is before the target HH:MM today, nextFiring returns today's target.
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC) // midnight
	got := nextFiring(now, 0, 30)
	want := time.Date(2026, 1, 15, 0, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextFiring before target: got %v, want %v", got, want)
	}

	// When now is exactly at the target, nextFiring returns tomorrow's target.
	now = time.Date(2026, 1, 15, 0, 30, 0, 0, time.UTC)
	got = nextFiring(now, 0, 30)
	want = time.Date(2026, 1, 16, 0, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextFiring at target: got %v, want %v", got, want)
	}

	// When now is after the target HH:MM today, nextFiring returns tomorrow's target.
	now = time.Date(2026, 1, 15, 1, 0, 0, 0, time.UTC)
	got = nextFiring(now, 0, 30)
	want = time.Date(2026, 1, 16, 0, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextFiring after target: got %v, want %v", got, want)
	}
}

// TestCompactorSweepsExpiredClientLogins: expired sign-in requests go with
// every run, whatever the retention settings say; live ones stay.
func TestCompactorSweepsExpiredClientLogins(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i, expires := range []time.Time{now.Add(-time.Hour), now.Add(-time.Second), now.Add(5 * time.Minute)} {
		ok, err := x.InsertClientLogin(ctx, db.ClientLoginRequest{
			DeviceCodeHash: "hash" + string(rune('0'+i)), UserCode: "CODE000" + string(rune('0'+i)),
			CreatedAt: expires.Add(-10 * time.Minute), ExpiresAt: expires,
		}, 20)
		if err != nil || !ok {
			t.Fatalf("seed %d: ok=%v err=%v", i, ok, err)
		}
	}
	al := &noopAuditLogger{}
	// All retention settings zero: "keep forever" for every other table.
	c := New(x, &staticLoader{}, al, discardLog())
	counts, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["client_login_requests"] != 2 {
		t.Fatalf("client_login_requests: want 2 deleted, got %d", counts["client_login_requests"])
	}
	if _, err := x.GetClientLoginByDeviceHash(ctx, "hash2"); err != nil {
		t.Fatalf("the live request was swept: %v", err)
	}
	if len(al.calls) != 1 || al.calls[0] != "client_login_requests" {
		t.Fatalf("audit calls = %v", al.calls)
	}
}
