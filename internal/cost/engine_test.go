package cost_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
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
func TestBudgetUsages_CountsReportedCost(t *testing.T) {
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
		u, err := e.BudgetUsages(context.Background(), []db.Budget{c.b})
		if err != nil {
			t.Fatalf("%s: %v", c.b.Scope, err)
		}
		if got := u[0].USD; math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: current = %v, want %v", c.b.Scope, got, c.want)
		}
	}
}

// --- usage by key and model, token budgets ------------------------------------

func routePricing() cost.Pricing {
	return cost.Pricing{Version: "test", Entries: map[string]cost.Entry{
		"zai/glm-5.1": {InputPerMillion: 1, OutputPerMillion: 2},
		"gemini":      {InputPerMillion: 4, OutputPerMillion: 4},
		"openai":      {InputPerMillion: 9, OutputPerMillion: 9},
	}}
}

// A usage row is priced by "<provider>/<target model>", then the target
// model, then the kind; a reported cost wins over all of them.
func TestRowUSD_PricesByTargetModel(t *testing.T) {
	for name, c := range map[string]struct {
		row  db.UsageRow
		want float64
	}{
		"provider/model wins over kind": {db.UsageRow{Kind: "openai", ProviderSlug: "zai", TargetModel: "glm-5.1",
			TokensIn: 1_000_000, TokensOut: 500_000, PricedTokensIn: 1_000_000, PricedTokensOut: 500_000}, 2.0},
		"bare target model": {db.UsageRow{Kind: "openai", ProviderSlug: "openrouter", TargetModel: "gemini",
			TokensIn: 1_000_000, PricedTokensIn: 1_000_000}, 4.0},
		"unknown model falls back to kind": {db.UsageRow{Kind: "openai", ProviderSlug: "zai", TargetModel: "unknown-model",
			TokensIn: 1_000_000, PricedTokensIn: 1_000_000}, 9.0},
		"reported cost only": {db.UsageRow{Kind: "openai", ProviderSlug: "zai", TargetModel: "glm-5.1",
			TokensIn: 1_000_000, ReportedUSD: 0.25}, 0.25},
		"nothing known": {db.UsageRow{Kind: "mcp", TokensIn: 1_000_000, PricedTokensIn: 1_000_000}, 0},
	} {
		usage := &rawUsageReader{rows: []db.UsageRow{c.row}}
		e := cost.NewWithDeps(routePricing(), nil, usage, nil, nil, nil, nil, nil)
		s, err := e.Summary(context.Background(), "today")
		if err != nil || math.Abs(s.TotalUSD-c.want) > 1e-9 {
			t.Errorf("%s: total %v (%v), want %v", name, s.TotalUSD, err, c.want)
		}
		// The global budget prices rows the same way.
		u, err := e.BudgetUsages(context.Background(), []db.Budget{{Scope: "global"}})
		if err != nil || math.Abs(u[0].USD-c.want) > 1e-9 {
			t.Errorf("%s: global current %+v (%v), want %v", name, u, err, c.want)
		}
	}
}

// rawUsageReader returns its rows as they are and counts the reads. Like a
// database it gives up when its context ends. A test can hold a read at the
// gate (it announces itself on entered first) to see what others do meanwhile.
type rawUsageReader struct {
	mu      sync.Mutex
	rows    []db.UsageRow
	err     error
	calls   int
	gate    chan struct{}
	entered chan struct{}
}

func (f *rawUsageReader) ListUsageForWindow(ctx context.Context, _ string) ([]db.UsageRow, error) {
	f.mu.Lock()
	f.calls++
	gate, entered := f.gate, f.entered
	rows, err := append([]db.UsageRow(nil), f.rows...), f.err
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (f *rawUsageReader) hold() (entered chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate, f.entered = make(chan struct{}), make(chan struct{}, 16)
	gate := f.gate
	return f.entered, func() {
		f.mu.Lock()
		f.gate, f.entered = nil, nil
		f.mu.Unlock()
		close(gate)
	}
}

func (f *rawUsageReader) set(rows []db.UsageRow, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.err = rows, err
}

func (f *rawUsageReader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func routeRows() []db.UsageRow {
	return []db.UsageRow{
		{GatewayKeyID: "gk1", Dialect: "openai", RequestedModel: "burrow-smart", ProviderSlug: "zai", TargetModel: "glm-5.1",
			Kind: "openai", Requests: 2, TokensIn: 1_000_000, TokensOut: 500_000, PricedTokensIn: 1_000_000, PricedTokensOut: 500_000}, // 2.0
		{GatewayKeyID: "gk1", Dialect: "openai", RequestedModel: "burrow-smart", ProviderSlug: "openrouter", TargetModel: "gemini",
			Kind: "openai", Requests: 1, TokensIn: 10, TokensOut: 10, ReportedUSD: 0.25},
		{GatewayKeyID: "gk2", Dialect: "anthropic", RequestedModel: "burrow-simple", ProviderSlug: "ollama", TargetModel: "mistral",
			Kind: "anthropic", Requests: 4, TokensIn: 40, TokensOut: 4, PricedTokensIn: 40, PricedTokensOut: 4}, // 0
		{ServiceID: "svc", APIKeyID: "k1", Kind: "openai", Requests: 3, TokensIn: 1_000_000, PricedTokensIn: 1_000_000}, // 9.0
	}
}

func TestSummaryBy(t *testing.T) {
	e := cost.NewWithDeps(routePricing(), nil, &rawUsageReader{rows: routeRows()}, nil, nil, nil, nil, nil)
	ctx := context.Background()
	keys := func(rows []cost.GroupRow) []string {
		out := []string{}
		for _, r := range rows {
			out = append(out, r.Key)
		}
		return out
	}
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	byKey, err := e.SummaryBy(ctx, "today", "gateway_key")
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by USD descending, then key; the rows without a gateway key
	// (key "") come last although they cost the most.
	if !eq(keys(byKey), []string{"gk1", "gk2", ""}) {
		t.Fatalf("gateway_key groups = %+v", byKey)
	}
	if g := byKey[0]; g.Requests != 3 || g.TokensIn != 1_000_010 || g.TokensOut != 500_010 || math.Abs(g.USD-2.25) > 1e-9 {
		t.Errorf("gk1 = %+v", g)
	}
	if g := byKey[1]; g.Requests != 4 || g.TokensIn != 40 || g.USD != 0 {
		t.Errorf("gk2 = %+v", g)
	}
	if g := byKey[2]; g.Requests != 3 || math.Abs(g.USD-9) > 1e-9 {
		t.Errorf("no key = %+v", g)
	}
	for dim, want := range map[string][]string{
		"model":        {"burrow-smart", "burrow-simple", ""},
		"provider":     {"zai", "openrouter", "ollama", ""},
		"dialect":      {"openai", "anthropic", ""},
		"target_model": {"zai/glm-5.1", "openrouter/gemini", "ollama/mistral", ""},
	} {
		got, err := e.SummaryBy(ctx, "today", dim)
		if err != nil || !eq(keys(got), want) {
			t.Errorf("%s: %v (%v), want %v", dim, keys(got), err, want)
		}
	}
	for _, bad := range []string{"service; DROP TABLE", "", "api_key", "GATEWAY_KEY"} {
		if _, err := e.SummaryBy(ctx, "today", bad); !errors.Is(err, cost.ErrBadDimension) {
			t.Errorf("dimension %q: err = %v, want ErrBadDimension", bad, err)
		}
	}
	// No usage: an empty list, not nil.
	empty := cost.NewWithDeps(routePricing(), nil, &rawUsageReader{}, nil, nil, nil, nil, nil)
	if got, err := empty.SummaryBy(ctx, "today", "model"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty: %#v %v", got, err)
	}
}

// A client can send any model name on a provider path: the groups are capped,
// and what is cut is added up under the empty key so the totals stay whole.
func TestSummaryBy_CapsGroups(t *testing.T) {
	var rows []db.UsageRow
	for i := 0; i < cost.MaxGroups+50; i++ {
		rows = append(rows, db.UsageRow{RequestedModel: fmt.Sprintf("m-%04d", i), Requests: 1, TokensIn: 1})
	}
	e := cost.NewWithDeps(routePricing(), nil, &rawUsageReader{rows: rows}, nil, nil, nil, nil, nil)
	got, err := e.SummaryBy(context.Background(), "today", "model")
	if err != nil || len(got) != cost.MaxGroups+1 {
		t.Fatalf("len = %d (%v), want %d", len(got), err, cost.MaxGroups+1)
	}
	if last := got[len(got)-1]; last.Key != "" || last.Requests != 50 || last.TokensIn != 50 {
		t.Fatalf("rest = %+v", last)
	}
}

func TestCheckBudgets_GatewayKeyAndModelScopes(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		b    db.Budget
		subj cost.Subjects
		want bool
	}{
		{"gateway key", db.Budget{ID: "b", Scope: "gateway_key", SubjectID: "gk1", DailyUSD: 1, ActionOnExceed: "alert_webhook"},
			cost.Subjects{GatewayKeyID: "gk1", Model: "burrow-smart"}, true},
		{"model", db.Budget{ID: "b", Scope: "model", SubjectID: "burrow-smart", DailyUSD: 1, ActionOnExceed: "alert_webhook"},
			cost.Subjects{GatewayKeyID: "gk1", Model: "burrow-smart"}, true},
		{"other key's budget", db.Budget{ID: "b", Scope: "gateway_key", SubjectID: "gk2", DailyUSD: 1, ActionOnExceed: "alert_webhook"},
			cost.Subjects{GatewayKeyID: "gk1", Model: "burrow-smart"}, false},
		{"other model's budget", db.Budget{ID: "b", Scope: "model", SubjectID: "burrow-simple", DailyUSD: 1, ActionOnExceed: "alert_webhook"},
			cost.Subjects{GatewayKeyID: "gk1", Model: "burrow-smart"}, false},
		{"no gateway key", db.Budget{ID: "b", Scope: "gateway_key", SubjectID: "", DailyUSD: 0.01, ActionOnExceed: "alert_webhook"},
			cost.Subjects{APIKeyID: "k1", ServiceID: "svc"}, false},
	} {
		disp := &fakeDispatcher{}
		e := cost.NewWithDeps(routePricing(), &fakeBudgetStore{budgets: []db.Budget{c.b}},
			&rawUsageReader{rows: routeRows()}, fakeDailyReader{}, nil, nil, disp, nil)
		action, _, err := e.CheckBudgets(ctx, c.subj)
		if err != nil || (action != "") != c.want || (disp.count() == 1) != c.want {
			t.Errorf("%s: action %q, %d events (%v), want fired=%v", c.name, action, disp.count(), err, c.want)
		}
		if again, _, _ := e.CheckBudgets(ctx, c.subj); again != "" {
			t.Errorf("%s: fired twice", c.name)
		}
	}
	// What a budget has used does not depend on who asks.
	e := cost.NewWithDeps(routePricing(), nil, &rawUsageReader{rows: routeRows()}, fakeDailyReader{}, nil, nil, nil, nil)
	for _, c := range []struct {
		b      db.Budget
		usd    float64
		tokens int64
	}{
		{db.Budget{Scope: "gateway_key", SubjectID: "gk1"}, 2.25, 1_500_020},
		{db.Budget{Scope: "gateway_key", SubjectID: "gk2"}, 0, 44},
		{db.Budget{Scope: "model", SubjectID: "burrow-smart"}, 2.25, 1_500_020},
		{db.Budget{Scope: "model", SubjectID: "nothing"}, 0, 0},
		{db.Budget{Scope: "api_key", SubjectID: "k1"}, 9, 1_000_000},
		{db.Budget{Scope: "service", SubjectID: "svc"}, 9, 1_000_000},
		{db.Budget{Scope: "global"}, 11.25, 2_500_064},
		{db.Budget{Scope: "user", SubjectID: "u"}, 0, 0},
	} {
		u, err := e.BudgetUsages(ctx, []db.Budget{c.b})
		if err != nil || math.Abs(u[0].USD-c.usd) > 1e-9 || u[0].Tokens != c.tokens {
			t.Errorf("%s/%s: %+v (%v), want %v / %d", c.b.Scope, c.b.SubjectID, u, err, c.usd, c.tokens)
		}
	}
}

// A model budget goes by the name the client asked for, exactly. On a
// provider's own path the gateway records that name as "<provider>/<id>", the
// model's direct address, so both doors count for one budget. Nothing else
// does: not a synthetic model that happens to share the id or to target the
// model, and not another provider's model whose id looks like the address.
func TestModelBudget_MatchesTheRequestedNameExactly(t *testing.T) {
	rows := []db.UsageRow{
		// gateway endpoint with the direct address, and the provider path
		{GatewayKeyID: "gk1", RequestedModel: "openai/gpt-4o", ProviderSlug: "openai", TargetModel: "gpt-4o", TokensIn: 100, TokensOut: 1},
		{GatewayKeyID: "gk2", RequestedModel: "openai/gpt-4o", ProviderSlug: "openai", TargetModel: "gpt-4o", TokensIn: 10, TokensOut: 1},
		// openrouter's native id "openai/gpt-4o": another model
		{GatewayKeyID: "gk1", RequestedModel: "openrouter/openai/gpt-4o", ProviderSlug: "openrouter", TargetModel: "openai/gpt-4o", TokensIn: 5000, TokensOut: 1},
		// a synthetic model "gpt-4o" that targets openai/gpt-4o
		{GatewayKeyID: "gk1", RequestedModel: "gpt-4o", ProviderSlug: "openai", TargetModel: "gpt-4o", TokensIn: 7000, TokensOut: 1},
		// a request that named no model
		{GatewayKeyID: "gk1", ProviderSlug: "openai", TokensIn: 90000, TokensOut: 1},
	}
	e := cost.NewWithDeps(routePricing(), nil, &rawUsageReader{rows: rows}, fakeDailyReader{}, nil, nil, nil, nil)
	for subject, want := range map[string]int64{
		"openai/gpt-4o":            112,
		"openrouter/openai/gpt-4o": 5001,
		"gpt-4o":                   7001,
		"openai/gpt":               0,
	} {
		got, err := e.BudgetUsages(context.Background(), []db.Budget{{Scope: "model", SubjectID: subject}})
		if err != nil || got[0].Tokens != want {
			t.Errorf("%s: tokens = %+v (%v), want %d", subject, got, err, want)
		}
	}
	// Only a request that asked for the model reaches its budget.
	for model, fires := range map[string]bool{"openai/gpt-4o": true, "gpt-4o": false, "openrouter/openai/gpt-4o": false, "": false} {
		disp := &fakeDispatcher{}
		e = cost.NewWithDeps(routePricing(), &fakeBudgetStore{budgets: []db.Budget{
			{ID: "b", Scope: "model", SubjectID: "openai/gpt-4o", DailyTokens: 100, ActionOnExceed: "alert_webhook"},
		}}, &rawUsageReader{rows: rows}, fakeDailyReader{}, nil, nil, disp, nil)
		e.CheckBudgetsForSample(context.Background(), "svc", "", "gk1", model)
		if (disp.count() == 1) != fires {
			t.Errorf("sample for %q: %d events, want fired=%v", model, disp.count(), fires)
		}
	}
}

func TestCheckBudgets_TokenCap(t *testing.T) {
	ctx := context.Background()
	usage := func(in, out int64) *rawUsageReader {
		return &rawUsageReader{rows: []db.UsageRow{{GatewayKeyID: "gk1", RequestedModel: "m", Kind: "openai",
			ProviderSlug: "zai", TargetModel: "glm-5.1", TokensIn: in, TokensOut: out, PricedTokensIn: in, PricedTokensOut: out}}}
	}
	for _, c := range []struct {
		name    string
		b       db.Budget
		in, out int64
		want    bool
	}{
		{"tokens over", db.Budget{DailyTokens: 1000}, 600, 500, true},
		{"tokens under", db.Budget{DailyTokens: 1000}, 400, 500, false},
		{"tokens exactly at the cap", db.Budget{DailyTokens: 1000}, 500, 500, false},
		// A token budget has no USD cap: spend alone does not exceed it.
		{"token budget, expensive", db.Budget{DailyTokens: 10_000_000}, 2_000_000, 0, false},
		{"both, usd over", db.Budget{DailyUSD: 1, DailyTokens: 10_000_000}, 2_000_000, 0, true},
		{"both, tokens over", db.Budget{DailyUSD: 100, DailyTokens: 1000}, 2000, 0, true},
		{"both, neither", db.Budget{DailyUSD: 100, DailyTokens: 10_000_000}, 2000, 0, false},
		// Without a token cap the USD cap decides as it always did, zero included.
		{"usd only, over", db.Budget{DailyUSD: 1}, 2_000_000, 0, true},
		{"usd only, under", db.Budget{DailyUSD: 3}, 2_000_000, 0, false},
		{"usd zero, any spend", db.Budget{}, 1000, 0, true},
	} {
		b := c.b
		b.ID, b.Scope, b.SubjectID, b.ActionOnExceed = "b", "gateway_key", "gk1", "alert_webhook"
		disp := &fakeDispatcher{}
		e := cost.NewWithDeps(routePricing(), &fakeBudgetStore{budgets: []db.Budget{b}}, usage(c.in, c.out), fakeDailyReader{}, nil, nil, disp, nil)
		action, _, err := e.CheckBudgets(ctx, cost.Subjects{GatewayKeyID: "gk1"})
		if err != nil || (action == "alert_webhook") != c.want {
			t.Errorf("%s: action %q (%v), want fired=%v", c.name, action, err, c.want)
		}
		u, err := e.BudgetUsages(ctx, []db.Budget{b})
		if err != nil || u[0].Exceeded != c.want || cost.BudgetExceeded(b, u[0].USD, u[0].Tokens) != c.want {
			t.Errorf("%s: usage %+v (%v), want exceeded=%v", c.name, u, err, c.want)
		}
		if c.want && disp.count() == 1 {
			p := disp.events[0].payload.(map[string]any)
			if p["current_tokens"] != c.in+c.out || p["daily_tokens"] != b.DailyTokens {
				t.Errorf("%s: payload %+v", c.name, p)
			}
		}
	}
}

type fakeGatewayRevoker struct {
	mu  sync.Mutex
	ids []string
	err error
}

func (f *fakeGatewayRevoker) RevokeGatewayKeyByID(_ context.Context, id, budgetID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = append(f.ids, id+" by "+budgetID)
	return f.err
}

func TestDisableKey_GatewayKey(t *testing.T) {
	ctx := context.Background()
	budgets := &fakeBudgetStore{budgets: []db.Budget{
		{ID: "b", Scope: "gateway_key", SubjectID: "gk1", DailyUSD: 1, ActionOnExceed: "disable_key"},
	}}
	rev := &fakeGatewayRevoker{}
	disp := &fakeDispatcher{}
	var logs bytes.Buffer
	e := cost.NewWithDeps(routePricing(), budgets, &rawUsageReader{rows: routeRows()}, fakeDailyReader{}, nil, nil, disp,
		slog.New(slog.NewTextHandler(&logs, nil)))
	e.SetGatewayKeyRevoker(rev)
	e.CheckBudgetsForSample(ctx, "svc", "", "gk1", "burrow-smart")
	e.CheckBudgetsForSample(ctx, "svc", "", "gk1", "burrow-smart")
	if len(rev.ids) != 1 || rev.ids[0] != "gk1 by b" {
		t.Fatalf("revoked = %v, want gk1 once, with the budget's id", rev.ids)
	}
	// The revoke leaves a line in the log, at Info.
	if l := logs.String(); !strings.Contains(l, "level=INFO") || !strings.Contains(l, "gateway_key_id=gk1") || !strings.Contains(l, "budget_id=b") {
		t.Fatalf("log = %q", l)
	}
	if disp.count() != 1 {
		t.Fatalf("events = %d, want 1", disp.count())
	}
	// Without a revoker nothing panics; a model budget revokes no key.
	e = cost.NewWithDeps(routePricing(), budgets, &rawUsageReader{rows: routeRows()}, fakeDailyReader{}, nil, nil, nil, nil)
	e.CheckBudgetsForSample(ctx, "svc", "", "gk1", "burrow-smart")
	rev = &fakeGatewayRevoker{}
	e = cost.NewWithDeps(routePricing(), &fakeBudgetStore{budgets: []db.Budget{
		{ID: "m", Scope: "model", SubjectID: "burrow-smart", DailyUSD: 1, ActionOnExceed: "disable_key"},
	}}, &rawUsageReader{rows: routeRows()}, fakeDailyReader{}, nil, nil, nil, nil)
	e.SetGatewayKeyRevoker(rev)
	e.CheckBudgetsForSample(ctx, "svc", "", "gk1", "burrow-smart")
	if len(rev.ids) != 0 {
		t.Fatalf("a model budget revoked %v", rev.ids)
	}
}

// One check reads today's usage once, however many budgets match.
func TestCheckBudgets_ReadsUsageOnce(t *testing.T) {
	usage := &rawUsageReader{rows: routeRows()}
	e := cost.NewWithDeps(routePricing(), &fakeBudgetStore{budgets: []db.Budget{
		{ID: "a", Scope: "gateway_key", SubjectID: "gk1", DailyUSD: 100, ActionOnExceed: "alert_webhook"},
		{ID: "b", Scope: "model", SubjectID: "burrow-smart", DailyUSD: 100, ActionOnExceed: "alert_webhook"},
		{ID: "c", Scope: "global", DailyUSD: 100, ActionOnExceed: "alert_webhook"},
	}}, usage, fakeDailyReader{}, nil, nil, nil, nil)
	if _, _, err := e.CheckBudgets(context.Background(), cost.Subjects{GatewayKeyID: "gk1", Model: "burrow-smart"}); err != nil {
		t.Fatal(err)
	}
	if usage.count() != 1 {
		t.Fatalf("usage read %d times, want 1", usage.count())
	}
}
