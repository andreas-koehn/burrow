package cost_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aimeter"
	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/db"
)

func guardBudgets() []db.Budget {
	return []db.Budget{
		{ID: "b1", Scope: "gateway_key", SubjectID: "gk1", DailyUSD: 1, ActionOnExceed: "throttle_zero"},
		{ID: "b2", Scope: "gateway_key", SubjectID: "gk2", DailyUSD: 1, ActionOnExceed: "alert_webhook"},
		{ID: "b3", Scope: "model", SubjectID: "burrow-smart", DailyUSD: 1, ActionOnExceed: "disable_key"},
		{ID: "b4", Scope: "gateway_key", SubjectID: "gk3", DailyUSD: 1, ActionOnExceed: "throttle_zero"},
		{ID: "b5", Scope: "gateway_key", SubjectID: "gk5", DailyTokens: 100, ActionOnExceed: "throttle_zero"},
		// Scopes the guard does not enforce, exceeded or not.
		{ID: "b6", Scope: "global", DailyUSD: 0.01, ActionOnExceed: "throttle_zero"},
		{ID: "b7", Scope: "api_key", SubjectID: "gk3", DailyUSD: 0.01, ActionOnExceed: "throttle_zero"},
	}
}

func guardRows() []db.UsageRow {
	row := func(key, model string, usd float64, tokens int64) db.UsageRow {
		return db.UsageRow{GatewayKeyID: key, RequestedModel: model, ReportedUSD: usd, TokensIn: tokens}
	}
	return []db.UsageRow{
		row("gk1", "x", 2, 0),
		row("gk2", "x", 2, 0),
		row("gk8", "burrow-smart", 2, 0),
		row("gk3", "burrow-simple", 0.5, 0),
		row("gk5", "y", 0, 101),
	}
}

type guardFixture struct {
	g     *cost.Guard
	usage *rawUsageReader
	store *fakeBudgetStore
	now   *time.Time
	logs  *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newGuardFixture(ttl time.Duration) guardFixture {
	usage := &rawUsageReader{rows: guardRows()}
	store := &fakeBudgetStore{budgets: guardBudgets()}
	logs := &syncBuffer{}
	e := cost.NewWithDeps(routePricing(), store, usage, fakeDailyReader{}, nil, nil, nil, slog.New(slog.NewTextHandler(logs, nil)))
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	g := cost.NewGuard(e, ttl)
	g.SetClock(func() time.Time { return now })
	return guardFixture{g: g, usage: usage, store: store, now: &now, logs: logs}
}

func TestGuard_Blocked(t *testing.T) {
	f := newGuardFixture(time.Minute)
	ctx := context.Background()
	for _, c := range []struct {
		key, model, reason string
		blocked            bool
	}{
		{"gk1", "any", "the daily budget for this key is used up", true},
		{"gk2", "any", "", false}, // alert only
		{"gk9", "burrow-smart", "the daily budget for model burrow-smart is used up", true},
		{"gk3", "burrow-simple", "", false},
		{"gk5", "any", "the daily budget for this key is used up", true}, // token cap
		{"", "", "", false},
		{"", "burrow-smart", "the daily budget for model burrow-smart is used up", true},
		{"gk1", "burrow-smart", "the daily budget for this key is used up", true},
	} {
		reason, blocked := f.g.Blocked(ctx, c.key, c.model)
		if blocked != c.blocked || reason != c.reason {
			t.Errorf("Blocked(%q, %q) = %q, %v; want %q, %v", c.key, c.model, reason, blocked, c.reason, c.blocked)
		}
	}
	// A reason names nothing but what the caller sent: no amounts, no ids.
	if reason, _ := f.g.Blocked(ctx, "gk1", "any"); strings.ContainsAny(reason, "0123456789$") {
		t.Errorf("reason %q carries a number", reason)
	}
}

func TestGuard_CachesForTTL(t *testing.T) {
	f := newGuardFixture(time.Minute)
	ctx := context.Background()
	f.g.Blocked(ctx, "gk1", "m")
	f.g.Blocked(ctx, "gk3", "m")
	if n := f.usage.count(); n != 1 {
		t.Fatalf("usage read %d times within the ttl, want 1", n)
	}
	// gk3 goes over; within the ttl the guard does not see it yet.
	f.usage.set(append(guardRows(), db.UsageRow{GatewayKeyID: "gk3", ReportedUSD: 5}), nil)
	*f.now = f.now.Add(59 * time.Second)
	if _, blocked := f.g.Blocked(ctx, "gk3", "m"); blocked || f.usage.count() != 1 {
		t.Fatalf("blocked %v after %d reads", blocked, f.usage.count())
	}
	*f.now = f.now.Add(time.Second)
	if _, blocked := f.g.Blocked(ctx, "gk3", "m"); !blocked || f.usage.count() != 2 {
		t.Fatalf("after the ttl: blocked %v, %d reads", blocked, f.usage.count())
	}
}

// The usage row that takes a hard budget over its cap makes the guard read
// again at once: it does not wait for the ttl.
func TestGuard_RefreshesWhenABudgetIsCrossed(t *testing.T) {
	usage := &rawUsageReader{}
	store := &fakeBudgetStore{budgets: []db.Budget{
		{ID: "b", Scope: "gateway_key", SubjectID: "gk1", DailyTokens: 100, ActionOnExceed: "throttle_zero"},
	}}
	e := cost.NewWithDeps(routePricing(), store, usage, fakeDailyReader{}, nil, nil, nil, nil)
	g := cost.NewGuard(e, time.Hour)
	ctx := context.Background()
	if _, blocked := g.Blocked(ctx, "gk1", "m"); blocked {
		t.Fatal("blocked without usage")
	}
	usage.set([]db.UsageRow{{GatewayKeyID: "gk1", RequestedModel: "m", TokensIn: 101}}, nil)
	if _, blocked := g.Blocked(ctx, "gk1", "m"); blocked {
		t.Fatal("the cache was not used")
	}
	e.CheckBudgetsForSample(ctx, "svc", "", "gk1", "m") // what the usage sink calls after the insert
	if _, blocked := g.Blocked(ctx, "gk1", "m"); !blocked {
		t.Fatal("not blocked after the budget was crossed")
	}
}

func TestGuard_ReadErrors(t *testing.T) {
	ctx := context.Background()
	// Nothing known yet and the read fails: not blocked, and logged.
	f := newGuardFixture(time.Minute)
	f.usage.set(nil, errors.New("db is down"))
	if reason, blocked := f.g.Blocked(ctx, "gk1", "burrow-smart"); blocked || reason != "" {
		t.Fatalf("blocked on a failed first read: %q", reason)
	}
	if !strings.Contains(f.logs.String(), "db is down") {
		t.Fatalf("the failure was not logged: %q", f.logs.String())
	}
	// One failed read is not repeated for every request.
	f.g.Blocked(ctx, "gk1", "m")
	if n := f.usage.count(); n != 1 {
		t.Fatalf("%d reads within the ttl after a failure, want 1", n)
	}
	// The read works again: enforced.
	f.usage.set(guardRows(), nil)
	*f.now = f.now.Add(time.Minute)
	if _, blocked := f.g.Blocked(ctx, "gk1", "m"); !blocked {
		t.Fatal("not blocked after the read recovered")
	}
	// A later failure keeps what was known: a key over its budget stays out.
	f.usage.set(nil, errors.New("db is down again"))
	*f.now = f.now.Add(time.Minute)
	if _, blocked := f.g.Blocked(ctx, "gk1", "m"); !blocked {
		t.Fatal("a failed read lifted the block")
	}
	if _, blocked := f.g.Blocked(ctx, "gk3", "m"); blocked {
		t.Fatal("a failed read blocked a key that was fine")
	}
	// ... but not into the next UTC day, where yesterday's verdict says nothing.
	*f.now = time.Date(2026, 5, 20, 0, 0, 1, 0, time.UTC)
	if _, blocked := f.g.Blocked(ctx, "gk1", "m"); blocked {
		t.Fatal("yesterday's block survived midnight on a failed read")
	}
}

func TestGuard_NilAndEmpty(t *testing.T) {
	var g *cost.Guard
	if _, blocked := g.Blocked(context.Background(), "gk1", "m"); blocked {
		t.Fatal("nil guard blocked")
	}
	e := cost.NewWithDeps(routePricing(), nil, nil, nil, nil, nil, nil, nil)
	if _, blocked := cost.NewGuard(e, time.Second).Blocked(context.Background(), "gk1", "m"); blocked {
		t.Fatal("engine without stores blocked")
	}
}

func TestGuard_Concurrent(t *testing.T) {
	f := newGuardFixture(0) // every call reads
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, blocked := f.g.Blocked(ctx, "gk1", "m"); !blocked {
					t.Error("gk1 not blocked")
				}
				if _, blocked := f.g.Blocked(ctx, "gk3", "burrow-simple"); blocked {
					t.Error("gk3 blocked")
				}
			}
		}()
	}
	wg.Wait()
}

// The whole path on a real database: the usage sink writes a row, the engine
// sees the budget crossed, and the guard refuses the key from the next
// request on, without waiting for its ttl. A request that fell back to another
// provider is charged to the model the client asked for.
func TestGuard_WithSinkAndDatabase(t *testing.T) {
	raw, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(raw); err != nil {
		t.Fatal(err)
	}
	x := db.Wrap(raw)
	t.Cleanup(func() { _ = x.Close() })
	checkGuardWithSink(t, x, "u-guard")
}

// checkGuardWithSink is the check behind TestGuard_WithSinkAndDatabase; the
// Postgres test runs it too. Everything it writes hangs on userID, so it can
// run again against the same database.
func checkGuardWithSink(t *testing.T, x *db.DB, userID string) {
	t.Helper()
	ctx := context.Background()
	gk1, gk2, smart := "gk1-"+userID, "gk2-"+userID, "burrow-smart-"+userID
	budgets := []db.Budget{
		{ID: "b-key-" + userID, Scope: "gateway_key", SubjectID: gk1, DailyTokens: 100, ActionOnExceed: "throttle_zero"},
		{ID: "b-model-" + userID, Scope: "model", SubjectID: smart, DailyUSD: 1, ActionOnExceed: "throttle_zero"},
	}
	reset := func() {
		_ = x.DeleteUser(ctx, userID) // takes the service and its usage rows along
		for _, b := range budgets {
			_ = x.DeleteBudget(ctx, b.ID)
		}
	}
	reset()
	t.Cleanup(reset)
	if err := x.CreateUser(ctx, db.User{ID: userID, Email: userID + "@test.invalid", PasswordHash: "h", Role: "user"}); err != nil {
		t.Fatal(err)
	}
	svc, err := x.GetOrCreateService(ctx, userID, "svc", "http")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range budgets {
		if err := x.CreateBudget(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	e := cost.New(x, routePricing())
	sink := aimeter.NewSQLSink(x)
	sink.Budgets = e
	g := cost.NewGuard(e, time.Hour)

	record := func(key, requested, provider, target string, in, out int) {
		t.Helper()
		if err := sink.Record(ctx, aimeter.Sample{ServiceID: svc.ID, Kind: "unknown", TokensIn: in, TokensOut: out,
			GatewayKeyID: key, Dialect: "openai", RequestedModel: requested, ProviderSlug: provider, TargetModel: target}); err != nil {
			t.Fatal(err)
		}
	}
	blocked := func(key, model string) bool {
		_, b := g.Blocked(ctx, key, model)
		return b
	}
	if blocked(gk1, "m") || blocked(gk2, smart) {
		t.Fatal("blocked without usage")
	}
	record(gk1, "m", "ollama", "mistral", 60, 40) // exactly at the cap: not over
	if blocked(gk1, "m") {
		t.Fatal("blocked at the cap")
	}
	record(gk1, "m", "ollama", "mistral", 1, 0)
	if !blocked(gk1, "m") || blocked(gk2, "m") {
		t.Fatalf("after crossing the token cap: gk1 %v, gk2 %v", blocked(gk1, "m"), blocked(gk2, "m"))
	}
	// The model answered by its fallback target zai/glm-5.1 (1 USD per
	// million input tokens): the model the client asked for pays.
	record(gk2, smart, "zai", "glm-5.1", 1_500_000, 0)
	if !blocked(gk2, smart) || !blocked("gk9-"+userID, smart) || blocked(gk2, "zai/glm-5.1") {
		t.Fatal("the model budget does not follow the requested model")
	}
	// The sink's row is complete on either database: the two flags are
	// BOOLEAN columns on Postgres and INTEGER ones on SQLite.
	if err := sink.Record(ctx, aimeter.Sample{ServiceID: svc.ID, Kind: "unknown", GatewayKeyID: "gk-flags-" + userID,
		Streamed: true, CacheHit: true, UpstreamStatus: 200}); err != nil {
		t.Fatal(err)
	}
	var flagged int
	if err := x.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_events WHERE service_id = ? AND streamed = ? AND cache_hit = ?`, svc.ID, true, true).Scan(&flagged); err != nil || flagged != 1 {
		t.Fatalf("rows with both flags = %d (%v), want 1", flagged, err)
	}
	usages, err := e.BudgetUsages(ctx, budgets)
	if err != nil || usages[0].Tokens != 101 || !usages[0].Exceeded || usages[1].USD != 1.5 || usages[1].Tokens != 1_500_000 || !usages[1].Exceeded {
		t.Fatalf("usages = %+v (%v)", usages, err)
	}
	groups, err := e.SummaryBy(ctx, "today", "gateway_key")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]cost.GroupRow{}
	for _, g := range groups {
		seen[g.Key] = g
	}
	if a, b := seen[gk1], seen[gk2]; a.Requests != 2 || a.TokensIn != 61 || a.TokensOut != 40 || a.USD != 0 ||
		b.Requests != 1 || b.TokensIn != 1_500_000 || b.USD != 1.5 {
		t.Fatalf("groups = %+v", groups)
	}
}
