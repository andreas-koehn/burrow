package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBudgetsCRUD(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()

	// Empty list returns a non-nil empty slice.
	rows, err := x.ListBudgets(ctx)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("want empty non-nil slice, got %v", rows)
	}

	// Create two rows, one with an alert_webhook_id and one without.
	whID := "wh1"
	if err := x.CreateBudget(ctx, Budget{
		ID: "b1", Scope: "api_key", SubjectID: "k1",
		DailyUSD: 10.0, ActionOnExceed: "alert_webhook",
		AlertWebhookID: &whID,
	}); err != nil {
		t.Fatalf("create b1: %v", err)
	}
	if err := x.CreateBudget(ctx, Budget{
		ID: "b2", Scope: "service", SubjectID: "svc-a",
		DailyUSD: 100.0, ActionOnExceed: "throttle_zero",
	}); err != nil {
		t.Fatalf("create b2: %v", err)
	}

	// Get round-trip preserves the nullable pointer.
	got, err := x.GetBudget(ctx, "b1")
	if err != nil {
		t.Fatalf("get b1: %v", err)
	}
	if got.Scope != "api_key" || got.SubjectID != "k1" ||
		got.DailyUSD != 10.0 || got.ActionOnExceed != "alert_webhook" {
		t.Fatalf("b1 round-trip: %+v", got)
	}
	if got.AlertWebhookID == nil || *got.AlertWebhookID != "wh1" {
		t.Fatalf("b1 alert_webhook_id: got %v, want wh1", got.AlertWebhookID)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at zero (sqlite default not applied?)")
	}

	// Second row's webhook should be nil.
	got2, _ := x.GetBudget(ctx, "b2")
	if got2.AlertWebhookID != nil {
		t.Fatalf("b2 alert_webhook_id should be nil, got %v", *got2.AlertWebhookID)
	}

	// List returns both ordered by (scope, subject_id).
	rows, err = x.ListBudgets(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("list len = %d, want 2", len(rows))
	}
	// api_key < service alphabetically.
	if rows[0].ID != "b1" || rows[1].ID != "b2" {
		t.Fatalf("list order: %+v", rows)
	}

	// Update replaces the mutable columns; nullable alert_webhook_id can be
	// cleared by passing nil.
	upd := Budget{
		ID: "b1", Scope: "api_key", SubjectID: "k1-changed",
		DailyUSD: 25.5, ActionOnExceed: "disable_key",
		AlertWebhookID: nil,
	}
	if err := x.UpdateBudget(ctx, upd); err != nil {
		t.Fatalf("update b1: %v", err)
	}
	got, _ = x.GetBudget(ctx, "b1")
	if got.SubjectID != "k1-changed" || got.DailyUSD != 25.5 ||
		got.ActionOnExceed != "disable_key" {
		t.Fatalf("update did not persist: %+v", got)
	}
	if got.AlertWebhookID != nil {
		t.Errorf("alert_webhook_id should be cleared, got %v", *got.AlertWebhookID)
	}

	// Delete removes; subsequent get → ErrNotFound.
	if err := x.DeleteBudget(ctx, "b1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := x.GetBudget(ctx, "b1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}

	// Delete unknown id → ErrNotFound.
	if err := x.DeleteBudget(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown: %v, want ErrNotFound", err)
	}

	// Update unknown id → ErrNotFound.
	if err := x.UpdateBudget(ctx, Budget{
		ID: "nope", Scope: "global", SubjectID: "",
		DailyUSD: 1, ActionOnExceed: "alert_webhook",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: %v, want ErrNotFound", err)
	}
}

// TestBudgetsGetUnknown asserts a missing id surfaces ErrNotFound.
func TestBudgetsGetUnknown(t *testing.T) {
	x := testDB(t)
	if _, err := x.GetBudget(context.Background(), "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown: %v, want ErrNotFound", err)
	}
}

// TestListUsageForWindow exercises the usage aggregation used by the cost
// engine. The query MUST exclude rows older than the window boundary; for
// "today" that means anything from the previous UTC day.
func TestListUsageForWindow(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc-cost")

	now := time.Now().UTC()
	yesterday := now.Add(-25 * time.Hour)
	rows := []UsageEvent{
		{ID: "u-t-1", ServiceID: svc, APIKeyID: "k1", Ts: now,
			Kind: "openai", TokensIn: 1000, TokensOut: 500, BytesIn: 100, BytesOut: 200},
		{ID: "u-t-2", ServiceID: svc, APIKeyID: "k1", Ts: now,
			Kind: "openai", TokensIn: 2000, TokensOut: 1000, BytesIn: 200, BytesOut: 400},
		{ID: "u-t-3", ServiceID: svc, APIKeyID: "k2", Ts: now,
			Kind: "anthropic", TokensIn: 500, TokensOut: 250, BytesIn: 50, BytesOut: 100},
		{ID: "u-y-1", ServiceID: svc, APIKeyID: "k1", Ts: yesterday,
			Kind: "openai", TokensIn: 999999, TokensOut: 999999, BytesIn: 9999, BytesOut: 9999},
	}
	for _, ue := range rows {
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, bytes_in, bytes_out)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			ue.ID, ue.ServiceID, ue.APIKeyID, ue.Ts, ue.Kind,
			ue.TokensIn, ue.TokensOut, ue.BytesIn, ue.BytesOut); err != nil {
			t.Fatalf("insert %s: %v", ue.ID, err)
		}
	}

	got, err := x.ListUsageForWindow(ctx, "today")
	if err != nil {
		t.Fatalf("list usage today: %v", err)
	}
	// Two (service, api_key, kind) groups today: (svc, k1, openai) and (svc, k2, anthropic).
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2; got %+v", len(got), got)
	}

	// Find k1+openai and assert merged totals.
	var k1Openai *UsageRow
	for i := range got {
		if got[i].APIKeyID == "k1" && got[i].Kind == "openai" {
			k1Openai = &got[i]
			break
		}
	}
	if k1Openai == nil {
		t.Fatal("missing k1/openai aggregate")
	}
	if k1Openai.TokensIn != 3000 || k1Openai.TokensOut != 1500 {
		t.Errorf("k1 today merged: in=%d out=%d, want 3000/1500", k1Openai.TokensIn, k1Openai.TokensOut)
	}
}

// TestSumDailyTokensQueries exercises the per-subject daily aggregations
// used by Engine.CheckBudgets. Yesterday's row is ignored.
func TestSumDailyTokensQueries(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc-bud")

	now := time.Now().UTC()
	yesterday := now.Add(-25 * time.Hour)
	rows := []UsageEvent{
		{ID: "u-t-1", ServiceID: svc, APIKeyID: "k1", Ts: now,
			Kind: "openai", TokensIn: 1000, TokensOut: 500},
		{ID: "u-t-2", ServiceID: svc, APIKeyID: "k1", Ts: now,
			Kind: "openai", TokensIn: 500, TokensOut: 100},
		{ID: "u-y", ServiceID: svc, APIKeyID: "k1", Ts: yesterday,
			Kind: "openai", TokensIn: 999, TokensOut: 999},
	}
	for _, ue := range rows {
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out)
			 VALUES(?,?,?,?,?,?,?)`,
			ue.ID, ue.ServiceID, ue.APIKeyID, ue.Ts, ue.Kind,
			ue.TokensIn, ue.TokensOut); err != nil {
			t.Fatalf("insert %s: %v", ue.ID, err)
		}
	}

	in, out, err := x.SumDailyTokensByAPIKey(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if in != 1500 || out != 600 {
		t.Errorf("k1 daily tokens: in=%d out=%d, want 1500/600", in, out)
	}

	inS, outS, err := x.SumDailyTokensByService(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	if inS != 1500 || outS != 600 {
		t.Errorf("service daily tokens: in=%d out=%d, want 1500/600", inS, outS)
	}

	// Empty subject short-circuits to 0.
	if in, out, _ := x.SumDailyTokensByAPIKey(ctx, ""); in != 0 || out != 0 {
		t.Errorf("empty api_key → in=%d out=%d, want 0/0", in, out)
	}
}

// TestListUsageForWindow_ReportedCost: a row with a reported cost adds to
// ReportedUSD and its tokens are not priced again; a row without one is left
// to the price table. A reported cost of 0 is a reported cost.
func TestListUsageForWindow_ReportedCost(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc-reported")

	now := time.Now().UTC()
	quarter, zero := 0.25, 0.0
	events := []UsageEvent{
		{ID: "r-1", TokensIn: 100, TokensOut: 50, CostUSD: &quarter},
		{ID: "r-2", TokensIn: 10, TokensOut: 5},
		{ID: "r-3", TokensIn: 7, TokensOut: 3, CostUSD: &zero},
	}
	for _, ue := range events {
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, cost_usd)
			 VALUES(?,?,?,?,?,?,?,?)`,
			ue.ID, svc, "k1", now, "openai", ue.TokensIn, ue.TokensOut, ue.CostUSD); err != nil {
			t.Fatalf("insert %s: %v", ue.ID, err)
		}
	}

	got, err := x.ListUsageForWindow(ctx, "today")
	if err != nil {
		t.Fatalf("list usage today: %v", err)
	}
	var sum UsageRow
	for _, r := range got {
		sum.ReportedUSD += r.ReportedUSD
		sum.PricedTokensIn += r.PricedTokensIn
		sum.PricedTokensOut += r.PricedTokensOut
		sum.TokensIn += r.TokensIn
		sum.TokensOut += r.TokensOut
	}
	if sum.ReportedUSD != 0.25 || sum.PricedTokensIn != 10 || sum.PricedTokensOut != 5 || sum.TokensIn != 117 || sum.TokensOut != 58 {
		t.Fatalf("sum = %+v, want reported 0.25, priced 10/5, tokens 117/58", sum)
	}
}

func TestBudgetDailyTokens(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	if err := x.CreateBudget(ctx, Budget{ID: "bt", Scope: "global", DailyUSD: 1, DailyTokens: 500000, ActionOnExceed: "throttle_zero"}); err != nil {
		t.Fatal(err)
	}
	got, err := x.GetBudget(ctx, "bt")
	if err != nil || got.DailyTokens != 500000 {
		t.Fatalf("get: %v %+v", err, got)
	}
	got.DailyTokens = 7
	if err := x.UpdateBudget(ctx, got); err != nil {
		t.Fatal(err)
	}
	list, _ := x.ListBudgets(ctx)
	if len(list) != 1 || list[0].DailyTokens != 7 {
		t.Fatalf("list: %+v", list)
	}
}
