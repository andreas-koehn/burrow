package cost_test

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/db"
)

// --- LoadEmbedded ------------------------------------------------------------

// TestLoadEmbedded asserts the bundled pricing.yaml loads, has a non-empty
// version, and ships at least one entry per major provider.
func TestLoadEmbedded(t *testing.T) {
	p, err := cost.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	if p.Version == "" {
		t.Fatal("bundled pricing must have a non-empty version")
	}
	if len(p.Entries) == 0 {
		t.Fatal("bundled pricing must have entries")
	}
	// Spot-check the canonical key shape and a few popular models.
	for _, key := range []string{
		"openai/gpt-4o",
		"openai/gpt-4o-mini",
		"anthropic/claude-3-5-sonnet",
		"google/gemini-1.5-pro",
		"ollama/llama3",
	} {
		if _, ok := p.Lookup(key); !ok {
			t.Errorf("missing canonical pricing key %q", key)
		}
	}
}

// TestLoadEmbedded_BareModelFallback asserts that a bare model name (without
// the provider prefix) also resolves, so callers that haven't plumbed the
// provider still get a price.
func TestLoadEmbedded_BareModelFallback(t *testing.T) {
	p, err := cost.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	if _, ok := p.Lookup("gpt-4o"); !ok {
		t.Error("bare 'gpt-4o' should resolve via fallback")
	}
}

// --- UsdFor ------------------------------------------------------------------

// TestEngine_UsdForOpenAIGPT4o pins the spec example:
//
//	UsdFor("openai/gpt-4o", 1000, 500)
//	= 2.50/1e6*1000 + 10.0/1e6*500
//	= 0.0075
func TestEngine_UsdForOpenAIGPT4o(t *testing.T) {
	p, err := cost.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	e := cost.New(nil, p)
	got := e.UsdFor("openai/gpt-4o", 1_000, 500)
	want := 2.50/1e6*1_000 + 10.0/1e6*500
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("UsdFor = %v, want %v (delta %v)", got, want, got-want)
	}
}

// TestEngine_UsdForUnknownModelIsZero asserts unknown models return 0 (not
// an error), so callers don't need to guard the call.
func TestEngine_UsdForUnknownModelIsZero(t *testing.T) {
	p, _ := cost.LoadEmbedded()
	e := cost.New(nil, p)
	if got := e.UsdFor("not-a-real-model", 10_000, 5_000); got != 0 {
		t.Fatalf("unknown model should be 0, got %v", got)
	}
}

// --- CheckBudgets: alert_webhook transition ----------------------------------

// fakeBudgetStore is an in-memory BudgetStore.
type fakeBudgetStore struct {
	mu      sync.Mutex
	budgets []db.Budget
}

func (f *fakeBudgetStore) ListBudgets(_ context.Context) ([]db.Budget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.Budget, len(f.budgets))
	copy(out, f.budgets)
	return out, nil
}

// fakeUsageReader returns canned usage rows.
type fakeUsageReader struct {
	mu   sync.Mutex
	rows []db.UsageRow
}

func (f *fakeUsageReader) ListUsageForWindow(_ context.Context, _ string) ([]db.UsageRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.UsageRow, len(f.rows))
	copy(out, f.rows)
	// Like the real query: a row without a reported cost has all its tokens
	// priced from the table.
	for i := range out {
		if r := &out[i]; r.ReportedUSD == 0 && r.PricedTokensIn == 0 && r.PricedTokensOut == 0 {
			r.PricedTokensIn, r.PricedTokensOut = r.TokensIn, r.TokensOut
		}
	}
	return out, nil
}

func (f *fakeUsageReader) addRow(r db.UsageRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, r)
}

// fakeDailyReader returns canned per-subject totals.
type fakeDailyReader struct{}

func (fakeDailyReader) SumDailyTokensByAPIKey(_ context.Context, _ string) (int64, int64, error) {
	return 0, 0, nil
}
func (fakeDailyReader) SumDailyTokensByService(_ context.Context, _ string) (int64, int64, error) {
	return 0, 0, nil
}

// fakeDispatcher captures Publish calls.
type fakeDispatcher struct {
	mu     sync.Mutex
	events []dispatchedEvent
}
type dispatchedEvent struct {
	event   string
	payload any
}

func (f *fakeDispatcher) Publish(_ context.Context, event string, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, dispatchedEvent{event, payload})
}
func (f *fakeDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

// fakeKeyLocator + fakeRevoker for the disable_key path.
type fakeKeyLocator struct {
	serviceID string
	err       error
}

func (f fakeKeyLocator) LookupServiceAPIKey(_ context.Context, apiKeyID string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return apiKeyID, f.serviceID, nil
}

type fakeRevoker struct {
	mu      sync.Mutex
	revoked []string
}

func (f *fakeRevoker) DeleteServiceAPIKey(_ context.Context, id, serviceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, id+"@"+serviceID)
	return nil
}

// TestCheckBudgets_AlertWebhookTransitionFiresOnce feeds 100 usage events
// whose total cost exceeds the daily budget; the second CheckBudgets call
// (after the transition into "exceeded") MUST report action=alert_webhook
// and dispatch exactly one budget.exceeded event. A third call MUST NOT
// re-fire (the once-per-day gate).
func TestCheckBudgets_AlertWebhookTransitionFiresOnce(t *testing.T) {
	p, err := cost.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	usage := &fakeUsageReader{}
	budgets := &fakeBudgetStore{budgets: []db.Budget{{
		ID:             "b-alert",
		Scope:          "api_key",
		SubjectID:      "k1",
		DailyUSD:       0.50, // very low cap so we exceed it
		ActionOnExceed: "alert_webhook",
	}}}
	disp := &fakeDispatcher{}
	e := cost.NewWithDeps(p, budgets, usage, fakeDailyReader{}, nil, nil, disp, nil)

	subj := cost.Subjects{APIKeyID: "k1", ServiceID: "svc-A"}

	// Phase 1: a single low-volume event keeps us under budget. We set
	// Kind to a real pricing key ("openai/gpt-4o") because the engine
	// looks up prices by the kind column — see chain.go's TODO at the
	// recordMeter call site for the eventual model plumbing.
	usage.addRow(db.UsageRow{
		ServiceID: "svc-A", APIKeyID: "k1", Kind: "openai/gpt-4o",
		TokensIn: 10, TokensOut: 10, // < 1 cent
	})
	action, _, err := e.CheckBudgets(context.Background(), subj)
	if err != nil {
		t.Fatalf("CheckBudgets (under): %v", err)
	}
	if action != "" {
		t.Fatalf("under-budget call should not trigger, got action=%q", action)
	}
	if disp.count() != 0 {
		t.Fatalf("under-budget call must not dispatch, got %d", disp.count())
	}

	// Phase 2: 100 more events bring us well over $0.50. (openai/gpt-4o
	// pricing: 2.50/M in + 10/M out → 100 events × (100 in + 50 out) →
	// 0.0125 USD total, still under. Crank up the tokens so we exceed.)
	for i := 0; i < 100; i++ {
		usage.addRow(db.UsageRow{
			ServiceID: "svc-A", APIKeyID: "k1", Kind: "openai/gpt-4o",
			TokensIn: 10_000, TokensOut: 5_000,
		})
	}
	action, b, err := e.CheckBudgets(context.Background(), subj)
	if err != nil {
		t.Fatalf("CheckBudgets (transition): %v", err)
	}
	if action != "alert_webhook" {
		t.Fatalf("transition call should fire alert_webhook, got action=%q", action)
	}
	if b.ID != "b-alert" {
		t.Fatalf("returned budget id = %q, want b-alert", b.ID)
	}
	if disp.count() != 1 {
		t.Fatalf("transition should dispatch exactly once, got %d", disp.count())
	}

	// Phase 3: another charge keeps us over budget — but the once-per-day
	// gate MUST suppress a re-fire.
	for i := 0; i < 10; i++ {
		usage.addRow(db.UsageRow{
			ServiceID: "svc-A", APIKeyID: "k1", Kind: "openai/gpt-4o",
			TokensIn: 1_000, TokensOut: 500,
		})
	}
	action, _, err = e.CheckBudgets(context.Background(), subj)
	if err != nil {
		t.Fatalf("CheckBudgets (post-exceed): %v", err)
	}
	if action != "" {
		t.Fatalf("post-exceed call must not re-fire, got action=%q", action)
	}
	if disp.count() != 1 {
		t.Fatalf("dispatcher count must stay at 1, got %d", disp.count())
	}
}

// TestCheckBudgets_DisableKeyRevokesViaStore asserts the disable_key action
// looks up the api_key's service and calls DeleteServiceAPIKey via the
// revoker exactly once per exceed transition.
func TestCheckBudgets_DisableKeyRevokesViaStore(t *testing.T) {
	p, err := cost.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	usage := &fakeUsageReader{}
	budgets := &fakeBudgetStore{budgets: []db.Budget{{
		ID:             "b-disable",
		Scope:          "api_key",
		SubjectID:      "k1",
		DailyUSD:       0.10,
		ActionOnExceed: "disable_key",
	}}}
	rev := &fakeRevoker{}
	loc := fakeKeyLocator{serviceID: "svc-Z"}
	disp := &fakeDispatcher{}
	e := cost.NewWithDeps(p, budgets, usage, fakeDailyReader{}, loc, rev, disp, nil)

	// Crank usage above $0.10.
	for i := 0; i < 50; i++ {
		usage.addRow(db.UsageRow{
			ServiceID: "svc-Z", APIKeyID: "k1", Kind: "openai/gpt-4o",
			TokensIn: 5_000, TokensOut: 5_000,
		})
	}
	subj := cost.Subjects{APIKeyID: "k1", ServiceID: "svc-Z"}
	action, _, err := e.CheckBudgets(context.Background(), subj)
	if err != nil {
		t.Fatalf("CheckBudgets: %v", err)
	}
	if action != "disable_key" {
		t.Fatalf("want action=disable_key, got %q", action)
	}
	rev.mu.Lock()
	defer rev.mu.Unlock()
	if len(rev.revoked) != 1 || rev.revoked[0] != "k1@svc-Z" {
		t.Fatalf("revoker should be called once with k1@svc-Z, got %v", rev.revoked)
	}
}

// TestCheckBudgets_DispatcherNilDoesNotPanic asserts that an engine with a
// nil dispatcher logs + swallows the alert (defensive — production wiring
// supplies a real dispatcher, but tests sometimes don't).
func TestCheckBudgets_DispatcherNilDoesNotPanic(t *testing.T) {
	p, _ := cost.LoadEmbedded()
	usage := &fakeUsageReader{}
	budgets := &fakeBudgetStore{budgets: []db.Budget{{
		ID:             "b-nil-disp",
		Scope:          "api_key",
		SubjectID:      "k1",
		DailyUSD:       0.01,
		ActionOnExceed: "alert_webhook",
	}}}
	e := cost.NewWithDeps(p, budgets, usage, fakeDailyReader{}, nil, nil, nil, nil)
	usage.addRow(db.UsageRow{
		ServiceID: "svc", APIKeyID: "k1", Kind: "openai/gpt-4o",
		TokensIn: 100_000, TokensOut: 100_000,
	})
	action, _, err := e.CheckBudgets(context.Background(), cost.Subjects{APIKeyID: "k1"})
	if err != nil {
		t.Fatalf("CheckBudgets: %v", err)
	}
	if action != "alert_webhook" {
		t.Fatalf("want action=alert_webhook, got %q", action)
	}
}

// TestCheckBudgets_NewDayResetsTriggered uses the SetClock hook to fast-
// forward past UTC midnight and asserts the second day's first exceed
// fires again.
func TestCheckBudgets_NewDayResetsTriggered(t *testing.T) {
	p, _ := cost.LoadEmbedded()
	usage := &fakeUsageReader{}
	budgets := &fakeBudgetStore{budgets: []db.Budget{{
		ID:             "b-daily",
		Scope:          "api_key",
		SubjectID:      "k1",
		DailyUSD:       0.01,
		ActionOnExceed: "alert_webhook",
	}}}
	disp := &fakeDispatcher{}
	e := cost.NewWithDeps(p, budgets, usage, fakeDailyReader{}, nil, nil, disp, nil)

	day1 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	clock := day1
	e.SetClock(func() time.Time { return clock })

	usage.addRow(db.UsageRow{
		ServiceID: "svc", APIKeyID: "k1", Kind: "openai/gpt-4o",
		TokensIn: 100_000, TokensOut: 100_000,
	})
	if action, _, _ := e.CheckBudgets(context.Background(), cost.Subjects{APIKeyID: "k1"}); action != "alert_webhook" {
		t.Fatalf("day1 first call: want alert_webhook, got %q", action)
	}
	if action, _, _ := e.CheckBudgets(context.Background(), cost.Subjects{APIKeyID: "k1"}); action != "" {
		t.Fatalf("day1 second call: want no-op, got %q", action)
	}
	// Advance the clock past UTC midnight.
	clock = day2
	if action, _, _ := e.CheckBudgets(context.Background(), cost.Subjects{APIKeyID: "k1"}); action != "alert_webhook" {
		t.Fatalf("day2 first call: want alert_webhook (reset), got %q", action)
	}
	if disp.count() != 2 {
		t.Fatalf("dispatcher count = %d, want 2 (one per UTC day)", disp.count())
	}
}

// TestSummary_BasicAggregation feeds a few usage rows and asserts the
// resulting Summary aggregates tokens + USD correctly and produces a
// sorted top_consumers list.
func TestSummary_BasicAggregation(t *testing.T) {
	p, _ := cost.LoadEmbedded()
	usage := &fakeUsageReader{rows: []db.UsageRow{
		{ServiceID: "svc-A", APIKeyID: "kA", Kind: "openai/gpt-4o",
			TokensIn: 1_000, TokensOut: 500}, // 0.0075
		{ServiceID: "svc-B", APIKeyID: "kB", Kind: "openai/gpt-4o-mini",
			TokensIn: 10_000, TokensOut: 10_000}, // 0.15/M*10k + 0.6/M*10k = 0.0015+0.006 = 0.0075
	}}
	e := cost.NewWithDeps(p, nil, usage, nil, nil, nil, nil, nil)
	s, err := e.Summary(context.Background(), "today")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if s.Window != "today" {
		t.Errorf("window = %q, want today", s.Window)
	}
	if s.TokensIn != 11_000 || s.TokensOut != 10_500 {
		t.Errorf("tokens = (in=%d,out=%d), want (11000,10500)", s.TokensIn, s.TokensOut)
	}
	wantTotal := 0.0075 + 0.0075
	if math.Abs(s.TotalUSD-wantTotal) > 1e-9 {
		t.Errorf("total_usd = %v, want %v", s.TotalUSD, wantTotal)
	}
	if len(s.TopConsumers) != 2 {
		t.Fatalf("top_consumers len = %d, want 2", len(s.TopConsumers))
	}
}

// reportedPricing prices kind "openai" at 1 USD per 1M input tokens and
// 2 USD per 1M output tokens.
func reportedPricing() cost.Pricing {
	return cost.Pricing{Version: "test", Entries: map[string]cost.Entry{
		"openai": {InputPerMillion: 1, OutputPerMillion: 2},
	}}
}

func TestSummary_UsesReportedCostAndPricesTheRest(t *testing.T) {
	usage := &fakeUsageReader{rows: []db.UsageRow{
		{ServiceID: "svc-A", APIKeyID: "kA", Kind: "openai", TokensIn: 1_000_100, TokensOut: 500_050,
			ReportedUSD: 0.25, PricedTokensIn: 1_000_000, PricedTokensOut: 500_000},
	}}
	e := cost.NewWithDeps(reportedPricing(), nil, usage, nil, nil, nil, nil, nil)
	s, err := e.Summary(context.Background(), "today")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if math.Abs(s.TotalUSD-2.25) > 1e-9 {
		t.Errorf("total_usd = %v, want 2.25", s.TotalUSD)
	}
	if s.TokensIn != 1_000_100 || s.TokensOut != 500_050 {
		t.Errorf("tokens = (%d, %d), want all tokens (1000100, 500050)", s.TokensIn, s.TokensOut)
	}
	if len(s.TopConsumers) != 1 || math.Abs(s.TopConsumers[0].USD-2.25) > 1e-9 {
		t.Errorf("top_consumers = %+v", s.TopConsumers)
	}
}

// A kind without a price entry used to count as 0 USD; the reported cost
// makes it count, for every budget scope.
func TestCurrentUsdFor_CountsReportedCost(t *testing.T) {
	usage := &fakeUsageReader{rows: []db.UsageRow{
		{ServiceID: "svc-A", APIKeyID: "kA", Kind: "unknown", TokensIn: 10, TokensOut: 10, ReportedUSD: 1.5},
		{ServiceID: "svc-B", APIKeyID: "kB", Kind: "openai", TokensIn: 2_000_000, TokensOut: 0,
			ReportedUSD: 0.5, PricedTokensIn: 1_000_000},
	}}
	e := cost.NewWithDeps(reportedPricing(), nil, usage, fakeDailyReader{}, nil, nil, nil, nil)
	for _, c := range []struct {
		b    db.Budget
		want float64
	}{
		{db.Budget{Scope: "api_key", SubjectID: "kA"}, 1.5},
		{db.Budget{Scope: "service", SubjectID: "svc-B"}, 1.5},
		{db.Budget{Scope: "global"}, 3.0},
	} {
		got, err := e.CurrentUsdFor(context.Background(), c.b)
		if err != nil {
			t.Fatalf("%s: %v", c.b.Scope, err)
		}
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: current = %v, want %v", c.b.Scope, got, c.want)
		}
	}
}
