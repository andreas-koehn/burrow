package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAIEndpoint_JudgeCallsLeftOut(t *testing.T) {
	checkAIEndpointJudgeCallsLeftOut(t, testDB(t), "u-judge-rows")
}

// checkAIEndpointJudgeCallsLeftOut: a judge call is metered under the judged
// request's service, but it is the judge model's time and request, not the
// provider's: the providers' request counts, the per-minute series and the
// p95 that fastest routing reads leave the rows of purpose
// "guardrail-judge" out. Tokens and cost keep them: the judge's spend is
// real.
func checkAIEndpointJudgeCallsLeftOut(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	svc := seedSvc(t, x, userID, "svc-judged")
	now := time.Now().UTC()
	insert := func(id string, latency int64, purpose string) {
		t.Helper()
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, streamed, cache_hit, latency_ms, upstream_status, purpose)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			userID+"-"+id, svc, "", now.Add(-time.Minute), "openai", 10, 10, false, false, latency, 200, purpose); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 10; i++ {
		insert(fmt.Sprintf("app-%d", i), 100, "")
	}
	for i := 1; i <= 10; i++ {
		insert(fmt.Sprintf("judge-%d", i), 5000, "guardrail-judge")
	}
	p95, err := x.AIEndpointLatencyP95(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p95[svc] != 100 {
		t.Errorf("p95 %d, want 100 (the judge calls' 5000 ms left out)", p95[svc])
	}
	counts, err := x.AIEndpointRequests24h(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts[svc].Requests; got != 10 {
		t.Errorf("requests %d, want 10", got)
	}
	agg, err := x.AIEndpointMetrics24h(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	perMinute := 0
	for _, n := range agg.PerMinute {
		perMinute += n
	}
	if agg.Requests != 10 || perMinute != 10 || agg.TokensIn != 200 {
		t.Errorf("detail: %d requests, %d in the hour, %d tokens in; want 10, 10 and 200 (tokens keep the judge's)", agg.Requests, perMinute, agg.TokensIn)
	}
}

// Migration 0029's Down block removes what its Up block adds.
func TestMigration0029_DownAndUp(t *testing.T) {
	checkMigration0029DownAndUp(t, testDB(t), "0029_v0.11.0_usage_purpose.sql")
}

func checkMigration0029DownAndUp(t *testing.T, x *DB, file string) {
	t.Helper()
	ctx := context.Background()
	raw, err := migrationFS.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	_, down, found := strings.Cut(string(raw), "-- +goose Down")
	if !found || strings.TrimSpace(down) == "" {
		t.Fatalf("%s has no Down block", file)
	}
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, down); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := tx.ExecContext(ctx, upBlock(string(raw))); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}
