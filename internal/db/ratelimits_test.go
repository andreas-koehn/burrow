package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRateLimitsCRUD(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()

	// Empty list returns a non-nil empty slice.
	rows, err := x.ListRateLimits(ctx)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("want empty non-nil slice, got %v", rows)
	}

	// Create two rows spanning both window kinds.
	if err := x.CreateRateLimit(ctx, RateLimit{
		ID: "rl1", Scope: "api_key", Subject: "k1", Dimension: "rpm",
		Lim: 5, Burst: 5, Window: "minute",
	}); err != nil {
		t.Fatalf("create rl1: %v", err)
	}
	if err := x.CreateRateLimit(ctx, RateLimit{
		ID: "rl2", Scope: "role", Subject: "user", Dimension: "bpm",
		Lim: 100000, Burst: 100000, Window: "day",
	}); err != nil {
		t.Fatalf("create rl2: %v", err)
	}

	// Read-back via Get.
	got, err := x.GetRateLimit(ctx, "rl1")
	if err != nil {
		t.Fatalf("get rl1: %v", err)
	}
	if got.Scope != "api_key" || got.Subject != "k1" || got.Dimension != "rpm" ||
		got.Lim != 5 || got.Burst != 5 || got.Window != "minute" {
		t.Fatalf("rl1 round-trip: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("created_at zero (sqlite default not applied?)")
	}

	// List returns both ordered by (scope, subject, dimension).
	rows, err = x.ListRateLimits(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("list len = %d, want 2", len(rows))
	}
	// api_key < role alphabetically.
	if rows[0].ID != "rl1" || rows[1].ID != "rl2" {
		t.Fatalf("list order: %+v", rows)
	}

	// Update replaces the mutable columns; window column quoting works for
	// UPDATE too.
	upd := RateLimit{
		ID: "rl1", Scope: "api_key", Subject: "k1", Dimension: "bpm",
		Lim: 50000, Burst: 75000, Window: "day",
	}
	if err := x.UpdateRateLimit(ctx, upd); err != nil {
		t.Fatalf("update rl1: %v", err)
	}
	got, _ = x.GetRateLimit(ctx, "rl1")
	if got.Dimension != "bpm" || got.Lim != 50000 || got.Burst != 75000 || got.Window != "day" {
		t.Fatalf("update did not persist: %+v", got)
	}

	// Delete removes; subsequent get → ErrNotFound.
	if err := x.DeleteRateLimit(ctx, "rl1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := x.GetRateLimit(ctx, "rl1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}

	// Delete unknown id → ErrNotFound.
	if err := x.DeleteRateLimit(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown: %v, want ErrNotFound", err)
	}

	// Update unknown id → ErrNotFound.
	if err := x.UpdateRateLimit(ctx, RateLimit{
		ID: "nope", Scope: "global", Subject: "", Dimension: "rpm",
		Lim: 1, Burst: 1, Window: "minute",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: %v, want ErrNotFound", err)
	}
}

// TestRateLimitsGetUnknown asserts a missing id surfaces ErrNotFound.
func TestRateLimitsGetUnknown(t *testing.T) {
	x := testDB(t)
	if _, err := x.GetRateLimit(context.Background(), "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown: %v, want ErrNotFound", err)
	}
}

// TestDailyUsageQueries exercises the on-demand quota aggregation queries
// (SumDailyUsageEvents* / CountDailyUsageEvents*). They MUST return only
// rows from the current UTC day — yesterday's row is ignored.
func TestDailyUsageQueries(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc-quota")

	// Two rows today (different api_keys), one row yesterday.
	now := time.Now().UTC()
	yesterday := now.Add(-25 * time.Hour)
	rows := []UsageEvent{
		{ID: "u-today-1", ServiceID: svc, APIKeyID: "k1", Ts: now,
			Kind: "openai", BytesIn: 200, BytesOut: 600},
		{ID: "u-today-2", ServiceID: svc, APIKeyID: "k1", Ts: now,
			Kind: "openai", BytesIn: 100, BytesOut: 300},
		{ID: "u-today-3", ServiceID: svc, APIKeyID: "k2", Ts: now,
			Kind: "openai", BytesIn: 50, BytesOut: 150},
		{ID: "u-yesterday", ServiceID: svc, APIKeyID: "k1", Ts: yesterday,
			Kind: "openai", BytesIn: 99999, BytesOut: 99999},
	}
	for _, ue := range rows {
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, bytes_in, bytes_out)
			 VALUES(?,?,?,?,?,?,?)`,
			ue.ID, ue.ServiceID, ue.APIKeyID, ue.Ts, ue.Kind, ue.BytesIn, ue.BytesOut,
		); err != nil {
			t.Fatalf("insert %s: %v", ue.ID, err)
		}
	}

	// Bytes/4 per spec D: api_key=k1 today = (200+600+100+300)/4 = 300.
	bytesK1, err := x.SumDailyUsageEventsByAPIKey(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if bytesK1 != 300 {
		t.Errorf("k1 daily byte-estimate = %d, want 300", bytesK1)
	}

	// Count: api_key=k1 today = 2 rows (yesterday excluded).
	countK1, err := x.CountDailyUsageEventsByAPIKey(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if countK1 != 2 {
		t.Errorf("k1 daily request count = %d, want 2", countK1)
	}

	// Service-scoped: 3 rows today → bytes (200+600+100+300+50+150)/4 = 350.
	bytesSvc, err := x.SumDailyUsageEventsByService(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	if bytesSvc != 350 {
		t.Errorf("service daily byte-estimate = %d, want 350", bytesSvc)
	}
	countSvc, err := x.CountDailyUsageEventsByService(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	if countSvc != 3 {
		t.Errorf("service daily request count = %d, want 3", countSvc)
	}

	// A gateway key is charged as "gw:<key id>"; its rows carry the id in
	// gateway_key_id and no api_key_id.
	for i, gk := range []string{"gk1", "gk1", "gk2"} {
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, gateway_key_id, ts, kind, bytes_in, bytes_out)
			 VALUES(?,?,?,?,?,?,?,?)`,
			"u-gw-"+string(rune('a'+i)), svc, "", gk, now, "openai", 40, 40,
		); err != nil {
			t.Fatalf("insert gateway row: %v", err)
		}
	}
	if n, err := x.CountDailyUsageEventsByAPIKey(ctx, "gw:gk1"); err != nil || n != 2 {
		t.Errorf("gw:gk1 daily request count = %d (%v), want 2", n, err)
	}
	if n, err := x.SumDailyUsageEventsByAPIKey(ctx, "gw:gk1"); err != nil || n != 40 {
		t.Errorf("gw:gk1 daily byte-estimate = %d (%v), want 40", n, err)
	}
	if n, _ := x.CountDailyUsageEventsByAPIKey(ctx, "gw:"); n != 0 {
		t.Errorf("empty gateway key id → %d, want 0", n)
	}
	if n, _ := x.CountDailyUsageEventsByAPIKey(ctx, "k1"); n != 2 {
		t.Errorf("k1 after gateway rows = %d, want 2", n)
	}

	// Empty subject short-circuits to 0 (engine calls these for unknown
	// subjects on non-applicable scopes).
	if n, _ := x.SumDailyUsageEventsByAPIKey(ctx, ""); n != 0 {
		t.Errorf("empty api_key → %d, want 0", n)
	}
	if n, _ := x.CountDailyUsageEventsByService(ctx, ""); n != 0 {
		t.Errorf("empty service → %d, want 0", n)
	}
}
