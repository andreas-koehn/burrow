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

// TestListUsageForWindow_GroupsByRoute and the daily per-subject queries: see
// checkUsageAccounting, which the Postgres test runs too.
func TestListUsageForWindow_GroupsByRoute(t *testing.T) {
	checkUsageAccounting(t, testDB(t), "u-acct")
}

// checkUsageAccounting runs every usage query the cost engine, the budget
// guard and the day quotas read, on whatever database x is. It may run more
// than once against the same database: everything it writes hangs on userID.
func checkUsageAccounting(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	svc := seedSvc(t, x, userID, "svc-acct")

	gk1, gk2 := "gk1-"+userID, "gk2-"+userID
	smart := "burrow-smart-" + userID
	key := "k-" + userID
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	quarter := 0.25
	type ev struct {
		id, apiKey, gwKey, dialect, provider, requested, target string
		ts                                                      time.Time
		in, out, bytesIn, bytesOut                              int64
		cost                                                    *float64
	}
	events := []ev{
		{id: "A", gwKey: gk1, dialect: "openai", provider: "zai", requested: smart, target: "glm-5.1", ts: now, in: 100, out: 50, bytesIn: 400, bytesOut: 400},
		// B sits on the first instant of the UTC day: it belongs to today.
		{id: "B", gwKey: gk1, dialect: "openai", provider: "zai", requested: smart, target: "glm-5.1", ts: midnight, in: 10, out: 5, bytesIn: 40, bytesOut: 40},
		{id: "C", gwKey: gk1, dialect: "openai", provider: "openrouter", requested: smart, target: "google/gemini-x", ts: now, in: 7, out: 3, bytesIn: 8, bytesOut: 8, cost: &quarter},
		{id: "D", apiKey: key, provider: "ollama", ts: now, in: 1, out: 1, bytesIn: 4, bytesOut: 4},
		{id: "E", gwKey: gk2, dialect: "anthropic", provider: "zai", requested: "other-" + userID, target: "glm-5.1", ts: now, in: 3_000_000_000, out: 3_000_000_000, bytesIn: 3_000_000_000, bytesOut: 3_000_000_000},
		// The last second of yesterday, and rows older than a week and a month.
		{id: "Y", gwKey: gk1, dialect: "openai", provider: "zai", requested: smart, target: "glm-5.1", ts: midnight.Add(-time.Second), in: 1000, out: 1000, bytesIn: 4000, bytesOut: 4000},
		{id: "Yk", apiKey: key, provider: "ollama", ts: midnight.Add(-time.Second), in: 1000, out: 1000, bytesIn: 4000, bytesOut: 4000},
		{id: "W", gwKey: gk1, dialect: "openai", provider: "zai", requested: smart, target: "glm-5.1", ts: now.Add(-8 * 24 * time.Hour), in: 50000, out: 50000},
		{id: "M", gwKey: gk1, dialect: "openai", provider: "zai", requested: smart, target: "glm-5.1", ts: now.Add(-31 * 24 * time.Hour), in: 700000, out: 700000},
	}
	for _, e := range events {
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, bytes_in, bytes_out, cost_usd,
			                          gateway_key_id, dialect, provider_slug, requested_model, target_model)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			"acct-"+userID+"-"+e.id, svc, e.apiKey, e.ts, "openai", e.in, e.out, e.bytesIn, e.bytesOut, e.cost,
			e.gwKey, e.dialect, e.provider, e.requested, e.target); err != nil {
			t.Fatalf("insert %s: %v", e.id, err)
		}
	}

	// Budgets for the two gateway scopes, with a token cap beyond 32 bits.
	bid := "bud-" + userID
	_ = x.DeleteBudget(ctx, bid)
	t.Cleanup(func() { _ = x.DeleteBudget(ctx, bid) })
	if err := x.CreateBudget(ctx, Budget{ID: bid, Scope: "gateway_key", SubjectID: gk1, DailyTokens: 5_000_000_000, ActionOnExceed: "throttle_zero"}); err != nil {
		t.Fatal(err)
	}
	if b, err := x.GetBudget(ctx, bid); err != nil || b.Scope != "gateway_key" || b.SubjectID != gk1 || b.DailyUSD != 0 || b.DailyTokens != 5_000_000_000 {
		t.Fatalf("budget: %v %+v", err, b)
	}
	if err := x.UpdateBudget(ctx, Budget{ID: bid, Scope: "model", SubjectID: smart, DailyUSD: 1.5, DailyTokens: 7, ActionOnExceed: "disable_key"}); err != nil {
		t.Fatal(err)
	}
	listed := false
	budgets, err := x.ListBudgets(ctx)
	for _, b := range budgets {
		listed = listed || (b.ID == bid && b.Scope == "model" && b.SubjectID == smart && b.DailyUSD == 1.5 && b.DailyTokens == 7)
	}
	if err != nil || !listed {
		t.Fatalf("list budgets: %v %+v", err, budgets)
	}

	mine := func(window string) []UsageRow {
		t.Helper()
		all, err := x.ListUsageForWindow(ctx, window)
		if err != nil {
			t.Fatalf("ListUsageForWindow(%s): %v", window, err)
		}
		var out []UsageRow
		for _, r := range all {
			if r.ServiceID == svc && r.GatewayKeyID != gk2 {
				out = append(out, r)
			}
		}
		return out
	}
	today := mine("today")
	if len(today) != 3 {
		t.Fatalf("today: %d rows, want 3: %+v", len(today), today)
	}
	for _, r := range today {
		switch {
		case r.ProviderSlug == "zai":
			if r.Requests != 2 || r.TokensIn != 110 || r.TokensOut != 55 || r.PricedTokensIn != 110 || r.PricedTokensOut != 55 ||
				r.ReportedUSD != 0 || r.GatewayKeyID != gk1 || r.Dialect != "openai" || r.RequestedModel != smart ||
				r.TargetModel != "glm-5.1" || r.APIKeyID != "" || r.BytesIn != 440 || r.Kind != "openai" {
				t.Errorf("A+B row = %+v", r)
			}
		case r.ProviderSlug == "openrouter":
			if r.Requests != 1 || r.ReportedUSD != 0.25 || r.PricedTokensIn != 0 || r.PricedTokensOut != 0 || r.TokensIn != 7 ||
				r.TargetModel != "google/gemini-x" {
				t.Errorf("C row = %+v", r)
			}
		case r.ProviderSlug == "ollama":
			if r.Requests != 1 || r.APIKeyID != key || r.GatewayKeyID != "" || r.Dialect != "" || r.RequestedModel != "" || r.TargetModel != "" {
				t.Errorf("D row = %+v", r)
			}
		default:
			t.Errorf("unexpected row %+v", r)
		}
	}
	// Sums beyond 32 bits come back whole.
	all, _ := x.ListUsageForWindow(ctx, "today")
	big := false
	for _, r := range all {
		if r.GatewayKeyID == gk2 {
			big = r.TokensIn == 3_000_000_000 && r.PricedTokensOut == 3_000_000_000 && r.BytesOut == 3_000_000_000 && r.Dialect == "anthropic"
		}
	}
	if !big {
		t.Errorf("the gk2 row lost its 64-bit sums: %+v", all)
	}
	zaiIn := func(window string) int64 {
		for _, r := range mine(window) {
			if r.ProviderSlug == "zai" {
				return r.TokensIn
			}
		}
		return -1
	}
	// week takes yesterday in, month the row of 8 days ago, year the rest.
	if got := zaiIn("week"); got != 1110 {
		t.Errorf("week zai tokens_in = %d, want 1110", got)
	}
	if got := zaiIn("month"); got != 51110 {
		t.Errorf("month zai tokens_in = %d, want 51110", got)
	}
	if got := zaiIn("year"); got != 751110 {
		t.Errorf("year zai tokens_in = %d, want 751110", got)
	}
	if got := zaiIn("no-such-window"); got != 110 {
		t.Errorf("unknown window zai tokens_in = %d, want today's 110", got)
	}

	// Budget sums: today's tokens of a service key and of the service.
	if in, out, err := x.SumDailyTokensByAPIKey(ctx, key); err != nil || in != 1 || out != 1 {
		t.Errorf("SumDailyTokensByAPIKey = %d/%d (%v), want 1/1", in, out, err)
	}
	if in, out, err := x.SumDailyTokensByService(ctx, svc); err != nil || in != 3_000_000_118 || out != 3_000_000_059 {
		t.Errorf("SumDailyTokensByService = %d/%d (%v)", in, out, err)
	}

	// Day quotas: the byte estimate (bytes/4) and the request count, for a
	// service key id, a "gw:<id>" subject, a gateway key, a model, a service.
	type q struct {
		name string
		got  func() (int64, error)
		want int64
	}
	for _, c := range []q{
		{"sum api key", func() (int64, error) { return x.SumDailyUsageEventsByAPIKey(ctx, key) }, 2},
		{"count api key", func() (int64, error) { return x.CountDailyUsageEventsByAPIKey(ctx, key) }, 1},
		{"sum gw subject", func() (int64, error) { return x.SumDailyUsageEventsByAPIKey(ctx, "gw:"+gk1) }, 224},
		{"count gw subject", func() (int64, error) { return x.CountDailyUsageEventsByAPIKey(ctx, "gw:"+gk1) }, 3},
		{"sum gateway key", func() (int64, error) { return x.SumDailyUsageEventsByGatewayKey(ctx, gk1) }, 224},
		{"count gateway key", func() (int64, error) { return x.CountDailyUsageEventsByGatewayKey(ctx, gk1) }, 3},
		{"sum big gateway key", func() (int64, error) { return x.SumDailyUsageEventsByGatewayKey(ctx, gk2) }, 1_500_000_000},
		{"sum model", func() (int64, error) { return x.SumDailyUsageEventsByModel(ctx, smart) }, 224},
		{"count model", func() (int64, error) { return x.CountDailyUsageEventsByModel(ctx, smart) }, 3},
		{"sum service", func() (int64, error) { return x.SumDailyUsageEventsByService(ctx, svc) }, 1_500_000_226},
		{"count service", func() (int64, error) { return x.CountDailyUsageEventsByService(ctx, svc) }, 5},
		{"sum unknown key", func() (int64, error) { return x.SumDailyUsageEventsByAPIKey(ctx, "nobody-"+userID) }, 0},
		{"count unknown model", func() (int64, error) { return x.CountDailyUsageEventsByModel(ctx, "nothing-"+userID) }, 0},
		{"empty gw subject", func() (int64, error) { return x.CountDailyUsageEventsByAPIKey(ctx, "gw:") }, 0},
		{"empty gateway key", func() (int64, error) { return x.SumDailyUsageEventsByGatewayKey(ctx, "") }, 0},
		{"empty model", func() (int64, error) { return x.CountDailyUsageEventsByModel(ctx, "") }, 0},
	} {
		if got, err := c.got(); err != nil || got != c.want {
			t.Errorf("%s = %d (%v), want %d", c.name, got, err, c.want)
		}
	}
}

func TestUsageWindowStart(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 30, 0, 0, time.FixedZone("x", 2*3600)) // 2026-02-28 22:30 UTC
	for window, want := range map[string]time.Time{
		"today": time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC),
		"week":  time.Date(2026, 2, 21, 22, 30, 0, 0, time.UTC),
		"month": time.Date(2026, 1, 29, 22, 30, 0, 0, time.UTC),
		"year":  time.Date(2025, 2, 28, 22, 30, 0, 0, time.UTC),
		"bogus": time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC),
	} {
		got := UsageWindowStart(window, now)
		if !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("%s: %v, want %v", window, got, want)
		}
	}
}
