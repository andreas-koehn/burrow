package db

import (
	"context"
	"testing"
	"time"
)

func TestUsageAttempts(t *testing.T) { checkUsageAttempts(t, testDB(t)) }

// checkUsageAttempts exercises the usage_attempts statements. It runs against
// SQLite here and against a live Postgres in the postgres-tagged test.
func checkUsageAttempts(t *testing.T, x *DB) {
	t.Helper()
	ctx := context.Background()
	reset := func() {
		_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM usage_attempts WHERE request_id IN ('req-1','req-2')`)
	}
	reset()
	t.Cleanup(reset)
	now := time.Now().UTC()
	err := x.InsertUsageAttempts(ctx, []UsageAttempt{
		{RequestID: "req-1", Position: 1, Ts: now, ProviderSlug: "b", TargetModel: "m2", Status: 200, DurationMs: 40},
		{RequestID: "req-1", Position: 0, Ts: now, ProviderSlug: "a", TargetModel: "m1", Status: 503, ErrorCode: "upstream_error", DurationMs: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := x.ListUsageAttempts(ctx, "req-1")
	if err != nil || len(got) != 2 || got[0].Position != 0 || got[0].ErrorCode != "upstream_error" || got[1].ProviderSlug != "b" || got[1].DurationMs != 40 {
		t.Fatalf("list: %v %+v", err, got)
	}
	if err := x.InsertUsageAttempts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := x.InsertUsageAttempts(ctx, []UsageAttempt{}); err != nil {
		t.Fatal(err)
	}
	err = x.InsertUsageAttempts(ctx, []UsageAttempt{
		{RequestID: "req-2", Position: 0, ProviderSlug: "a", TargetModel: "m"},
		{RequestID: "req-1", Position: 0, ProviderSlug: "a", TargetModel: "m"},
	})
	if err == nil {
		t.Fatal("duplicate (request_id, position) must fail")
	}
	if l, _ := x.ListUsageAttempts(ctx, "req-2"); len(l) != 0 {
		t.Fatalf("batch not atomic: %+v", l)
	}
	l, err := x.ListUsageAttempts(ctx, "unknown")
	if err != nil || l == nil || len(l) != 0 {
		t.Fatalf("unknown: %v %#v", err, l)
	}
}
