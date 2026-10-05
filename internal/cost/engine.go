package cost

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// Subjects identifies the caller for CheckBudgets — same shape as the quota
// engine's Subjects but kept here to avoid a circular import. Empty strings
// mean "scope not applicable" — a budget whose scope is api_key but whose
// Subjects.APIKeyID is empty will not match.
type Subjects struct {
	APIKeyID  string
	ServiceID string
	UserID    string // budgets with scope=user match by this
}

// BudgetStore is the narrow read surface the engine needs for CheckBudgets.
// *db.DB satisfies it. Tests use a fake.
type BudgetStore interface {
	ListBudgets(ctx context.Context) ([]db.Budget, error)
}

// UsageReader is the narrow read surface for the per-window aggregation
// query used by Summary + the live "current_usd" enrichment.
type UsageReader interface {
	ListUsageForWindow(ctx context.Context, window string) ([]db.UsageRow, error)
}

// DailyTokenReader exposes the per-subject daily token aggregation needed
// to compute live "current_usd" for a budget.
type DailyTokenReader interface {
	SumDailyTokensByAPIKey(ctx context.Context, apiKeyID string) (int64, int64, error)
	SumDailyTokensByService(ctx context.Context, serviceID string) (int64, int64, error)
}

// APIKeyLocator resolves an api_key id to its owning service so the engine
// can call DeleteServiceAPIKey on the disable_key path.
type APIKeyLocator interface {
	LookupServiceAPIKey(ctx context.Context, apiKeyID string) (id, serviceID string, err error)
}

// APIKeyRevoker is the narrow surface the engine uses to disable an api_key
// when a budget action_on_exceed=disable_key triggers. *db.DB's
// DeleteServiceAPIKey method satisfies it.
type APIKeyRevoker interface {
	DeleteServiceAPIKey(ctx context.Context, id, serviceID string) error
}

// Dispatcher is the narrow surface the engine uses to publish the
// budget.exceeded webhook event. Task 14 (webhook dispatcher) hasn't
// shipped yet; production wiring (Task 25) supplies the real
// implementation. Tests use a stub that records Publish calls.
type Dispatcher interface {
	Publish(ctx context.Context, event string, payload any)
}

// Engine is the cost / budget engine. It is reload-aware: the in-memory
// pricing table is replaced by ReplacePricing (called by the PUT
// /cost/pricing handler), and the per-day "already triggered" set is
// reset whenever the engine observes a new UTC day.
type Engine struct {
	mu sync.RWMutex

	pricing Pricing

	budgets    BudgetStore
	usage      UsageReader
	daily      DailyTokenReader
	keyLocator APIKeyLocator
	revoker    APIKeyRevoker
	dispatcher Dispatcher

	log *slog.Logger

	// triggered is the in-memory "already-fired-today" set. Keyed by
	// budget.id. The whole map is reset whenever the engine observes a new
	// UTC date in CheckBudgets — that is the single source of truth for
	// "exactly once per day" semantics. We deliberately do NOT persist
	// last_triggered_at on the budgets row: the spec allows either choice
	// (in-memory map OR persisted column) and the in-memory variant keeps
	// the wire shape of GET /budgets stable across process restarts (a
	// restart re-enables alerting, which matches operator intuition for
	// "process restart = clear state").
	triggered  map[string]bool
	currentDay string // YYYY-MM-DD UTC; when this changes, triggered is cleared

	// now is the clock the engine reads on CheckBudgets. Tests override
	// via SetClock; production uses time.Now.
	now func() time.Time
}

// New constructs the engine. Any of the inputs may be nil for partial wiring
// (e.g. tests that only exercise UsdFor pass everything as nil except
// pricing). When budgets/usage/daily are nil, CheckBudgets short-circuits
// to a no-op (allow). When revoker is nil, disable_key triggers will log a
// warning and skip the revocation. When dispatcher is nil, alert_webhook
// triggers will log a warning and skip the publish.
func New(d *db.DB, pr Pricing) *Engine {
	e := &Engine{
		pricing:   pr,
		triggered: make(map[string]bool),
		now:       time.Now,
		log:       slog.Default(),
	}
	if d != nil {
		e.budgets = d
		e.usage = d
		e.daily = d
		e.keyLocator = d
		e.revoker = d
	}
	return e
}

// NewWithDeps is the test-friendly constructor.
func NewWithDeps(pr Pricing, b BudgetStore, u UsageReader, dr DailyTokenReader,
	loc APIKeyLocator, rev APIKeyRevoker, dsp Dispatcher, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		pricing:    pr,
		budgets:    b,
		usage:      u,
		daily:      dr,
		keyLocator: loc,
		revoker:    rev,
		dispatcher: dsp,
		log:        log,
		triggered:  make(map[string]bool),
		now:        time.Now,
	}
}

// SetClock overrides the time source for deterministic tests.
func (e *Engine) SetClock(now func() time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = now
}

// SetDispatcher installs the webhook dispatcher (Task 25 wiring entry).
func (e *Engine) SetDispatcher(d Dispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dispatcher = d
}

// Pricing returns the current pricing table. Safe for concurrent use; the
// returned value is a copy of the Entries map header but shares the
// underlying map — callers should treat it as read-only.
func (e *Engine) Pricing() Pricing {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.pricing
}

// ReplacePricing atomically swaps the in-memory pricing table. Called by
// the PUT /api/v1/cost/pricing handler after persisting the override.
func (e *Engine) ReplacePricing(pr Pricing) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pricing = pr
}

// UsdFor returns the USD cost of (tokensIn input + tokensOut output) for the
// named model. model may be "provider/model" (canonical) or a bare model
// name; unknown models return 0 (treated as "no price").
//
//	cost = input_per_million / 1e6 * tokensIn + output_per_million / 1e6 * tokensOut
func (e *Engine) UsdFor(model string, tokensIn, tokensOut int) float64 {
	e.mu.RLock()
	entry, ok := e.pricing.Lookup(model)
	e.mu.RUnlock()
	if !ok {
		return 0
	}
	return float64(tokensIn)*entry.InputPerMillion/1_000_000 +
		float64(tokensOut)*entry.OutputPerMillion/1_000_000
}

// rowUSD prices one usage row: what upstreams reported, plus the price table
// for the tokens no upstream put a price on.
func (e *Engine) rowUSD(r db.UsageRow) float64 {
	return r.ReportedUSD + e.UsdFor(r.Kind, int(r.PricedTokensIn), int(r.PricedTokensOut))
}

// CheckBudgets aggregates today's spend for every budget that matches the
// given Subjects and, when a budget is now exceeded but was not on the
// previous call (the exceed transition), triggers its configured action
// exactly once.
//
// Return values:
//   - action: the action that was triggered ("" when no transition happened)
//   - budget: the budget that triggered the action (zero-value when action == "")
//   - err: a hard error from the underlying store (transient errors are
//     swallowed so the proxy hot path never stalls on a budget check)
//
// Side effects:
//   - alert_webhook → dispatches a "budget.exceeded" event via Dispatcher
//   - throttle_zero → records the trigger in-memory (a zero-bpm rate limit
//     for today is wired by the caller; CheckBudgets itself does not write
//     to rate_limits — that side effect belongs to the engine wiring layer)
//   - disable_key   → calls APIKeyRevoker.DeleteServiceAPIKey for the
//     api_key-scoped budget's subject id
//
// CheckBudgets is safe to call from the SQLSink hot path: it does no
// network IO of its own, and any dispatcher Publish is fire-and-forget
// from the engine's perspective (the dispatcher buffers).
func (e *Engine) CheckBudgets(ctx context.Context, subj Subjects) (string, db.Budget, error) {
	if e == nil || e.budgets == nil {
		return "", db.Budget{}, nil
	}
	// Reset the "already-fired-today" set when we cross UTC midnight.
	e.resetIfNewDay()

	budgets, err := e.budgets.ListBudgets(ctx)
	if err != nil {
		return "", db.Budget{}, err
	}

	for _, b := range budgets {
		// Match this budget against the caller's Subjects.
		if !budgetMatchesSubjects(b, subj) {
			continue
		}
		// Compute today's spend for this budget.
		current, err := e.currentUsdForBudget(ctx, b, subj)
		if err != nil {
			// Don't trigger on a read error — the caller (SQLSink) treats
			// CheckBudgets as best-effort.
			e.log.Warn("cost: current spend lookup failed",
				slog.String("budget_id", b.ID), slog.String("err", err.Error()))
			continue
		}
		if current <= b.DailyUSD {
			continue
		}
		// Exceeded — already triggered this UTC day?
		e.mu.Lock()
		if e.triggered[b.ID] {
			e.mu.Unlock()
			continue
		}
		e.triggered[b.ID] = true
		e.mu.Unlock()

		// Fire the configured action. Errors are logged + swallowed so a
		// single failed alert doesn't poison subsequent budgets.
		e.fireAction(ctx, b, current, subj)
		return b.ActionOnExceed, b, nil
	}
	return "", db.Budget{}, nil
}

// CurrentUsdFor returns today's spend in USD for the given budget, computed
// live from usage_events × pricing. Exposed for the GET /budgets handler so
// the wire response includes current_usd + exceeded without the caller
// repeating the math.
func (e *Engine) CurrentUsdFor(ctx context.Context, b db.Budget) (float64, error) {
	subj := budgetToSubjects(b)
	return e.currentUsdForBudget(ctx, b, subj)
}

// CheckBudgetsForSample is the BudgetChecker hook used by aimeter.SQLSink.
// It is called after each usage_events insert with the per-request
// (service, api_key) identity; the global scope matches unconditionally.
// All errors are logged + swallowed so a budget-check failure never breaks
// the proxy hot path.
func (e *Engine) CheckBudgetsForSample(ctx context.Context, serviceID, apiKeyID string) {
	if e == nil || e.budgets == nil {
		return
	}
	if _, _, err := e.CheckBudgets(ctx, Subjects{
		APIKeyID:  apiKeyID,
		ServiceID: serviceID,
	}); err != nil {
		e.log.Warn("cost: CheckBudgets after insert failed",
			slog.String("service_id", serviceID),
			slog.String("api_key_id", apiKeyID),
			slog.String("err", err.Error()))
	}
}

// currentUsdForBudget is the shared implementation behind CheckBudgets and
// CurrentUsdFor. For scope=api_key|service it queries the per-subject daily
// token aggregation; for scope=user it currently returns 0 (no user_id
// column on usage_events — a follow-up task plumbs the user through the
// proxy chain). For scope=global it sums across all subjects in today's
// usage rows. The kind column is used as the model lookup key.
func (e *Engine) currentUsdForBudget(ctx context.Context, b db.Budget, subj Subjects) (float64, error) {
	switch b.Scope {
	case "api_key":
		key := subj.APIKeyID
		if key == "" {
			key = b.SubjectID
		}
		if e.daily == nil || key == "" {
			return 0, nil
		}
		// Per-budget current_usd uses a single weighted average from
		// today's usage rows for this api_key. We sum tokens by kind and
		// look up each kind's price.
		return e.usdForApiKey(ctx, key)
	case "service":
		sid := subj.ServiceID
		if sid == "" {
			sid = b.SubjectID
		}
		if e.daily == nil || sid == "" {
			return 0, nil
		}
		return e.usdForService(ctx, sid)
	case "user":
		// usage_events has no user_id column yet — a future task plumbs the
		// user through the proxy chain. Report 0 so the budget never
		// triggers prematurely.
		return 0, nil
	case "global":
		if e.usage == nil {
			return 0, nil
		}
		rows, err := e.usage.ListUsageForWindow(ctx, "today")
		if err != nil {
			return 0, err
		}
		var total float64
		for _, r := range rows {
			total += e.rowUSD(r)
		}
		return total, nil
	}
	return 0, nil
}

// usdForApiKey computes today's spend by walking the per-kind usage rows
// for the given api_key and multiplying by the configured per-kind price.
// We use the engine's UsageReader (group-by query) and filter in-process
// because that keeps the DailyTokenReader surface minimal (it only returns
// totals, not per-kind splits).
func (e *Engine) usdForApiKey(ctx context.Context, apiKeyID string) (float64, error) {
	if e.usage == nil {
		// Fall back to the totals query when usage isn't wired (tests).
		in, out, err := e.daily.SumDailyTokensByAPIKey(ctx, apiKeyID)
		if err != nil {
			return 0, err
		}
		// Without per-kind info we charge "unknown" — usually 0.
		return e.UsdFor("unknown", int(in), int(out)), nil
	}
	rows, err := e.usage.ListUsageForWindow(ctx, "today")
	if err != nil {
		return 0, err
	}
	var total float64
	for _, r := range rows {
		if r.APIKeyID != apiKeyID {
			continue
		}
		total += e.rowUSD(r)
	}
	return total, nil
}

// usdForService is the service-scope variant of usdForApiKey.
func (e *Engine) usdForService(ctx context.Context, serviceID string) (float64, error) {
	if e.usage == nil {
		in, out, err := e.daily.SumDailyTokensByService(ctx, serviceID)
		if err != nil {
			return 0, err
		}
		return e.UsdFor("unknown", int(in), int(out)), nil
	}
	rows, err := e.usage.ListUsageForWindow(ctx, "today")
	if err != nil {
		return 0, err
	}
	var total float64
	for _, r := range rows {
		if r.ServiceID != serviceID {
			continue
		}
		total += e.rowUSD(r)
	}
	return total, nil
}

// fireAction dispatches the configured side-effect for an exceeded budget.
// All errors are logged + swallowed — a single failure must not break the
// caller's hot path.
func (e *Engine) fireAction(ctx context.Context, b db.Budget, currentUSD float64, subj Subjects) {
	payload := map[string]any{
		"budget_id":        b.ID,
		"scope":            b.Scope,
		"subject_id":       b.SubjectID,
		"daily_usd":        b.DailyUSD,
		"current_usd":      currentUSD,
		"action_on_exceed": b.ActionOnExceed,
	}
	switch b.ActionOnExceed {
	case "alert_webhook":
		if e.dispatcher == nil {
			e.log.Warn("cost: budget exceeded but no dispatcher wired",
				slog.String("budget_id", b.ID),
				slog.Float64("current_usd", currentUSD))
			return
		}
		e.dispatcher.Publish(ctx, "budget.exceeded", payload)
	case "throttle_zero":
		// The actual rate-limit row insertion is the wiring layer's job
		// (Task 25 — it owns both the cost engine AND the rate-limit
		// store). We publish the exceeded event so listeners (incl. the
		// throttle installer) can react.
		if e.dispatcher != nil {
			e.dispatcher.Publish(ctx, "budget.exceeded", payload)
		} else {
			e.log.Warn("cost: throttle_zero requested but no dispatcher wired",
				slog.String("budget_id", b.ID))
		}
	case "disable_key":
		if e.revoker == nil || e.keyLocator == nil {
			e.log.Warn("cost: disable_key requested but no revoker wired",
				slog.String("budget_id", b.ID))
			return
		}
		// For scope=api_key the subject_id is the api_key id directly;
		// for other scopes there's nothing to disable.
		apiKeyID := b.SubjectID
		if subj.APIKeyID != "" {
			apiKeyID = subj.APIKeyID
		}
		if b.Scope != "api_key" || apiKeyID == "" {
			e.log.Warn("cost: disable_key needs scope=api_key with a subject_id",
				slog.String("budget_id", b.ID), slog.String("scope", b.Scope))
			return
		}
		_, serviceID, err := e.keyLocator.LookupServiceAPIKey(ctx, apiKeyID)
		if err != nil {
			e.log.Warn("cost: disable_key lookup failed",
				slog.String("api_key_id", apiKeyID),
				slog.String("err", err.Error()))
			return
		}
		if err := e.revoker.DeleteServiceAPIKey(ctx, apiKeyID, serviceID); err != nil {
			e.log.Warn("cost: disable_key revoke failed",
				slog.String("api_key_id", apiKeyID),
				slog.String("err", err.Error()))
			return
		}
		// Also publish the exceeded event for audit / dashboard parity.
		if e.dispatcher != nil {
			e.dispatcher.Publish(ctx, "budget.exceeded", payload)
		}
	}
}

// resetIfNewDay clears the "already-fired-today" set when the UTC date
// advances. Called at the top of every CheckBudgets.
func (e *Engine) resetIfNewDay() {
	day := e.now().UTC().Format("2006-01-02")
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.currentDay != day {
		e.currentDay = day
		e.triggered = make(map[string]bool)
	}
}

// --- Summary -----------------------------------------------------------------

// SummaryConsumer is one row of the top_consumers list returned by Summary.
type SummaryConsumer struct {
	APIKeyID  string  `json:"api_key_id"`
	ServiceID string  `json:"service_id"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	USD       float64 `json:"usd"`
}

// Summary is the wire shape returned by GET /api/v1/cost/summary. The
// pct_of_budget field is the highest current_usd / daily_usd ratio across
// every matching budget for the window (today only — week/month/year report
// nil because budgets are daily-scoped).
type Summary struct {
	Window       string            `json:"window"`
	TotalUSD     float64           `json:"total_usd"`
	TokensIn     int64             `json:"tokens_in"`
	TokensOut    int64             `json:"tokens_out"`
	TopConsumers []SummaryConsumer `json:"top_consumers"`
	PctOfBudget  *float64          `json:"pct_of_budget"`
}

// Summary computes the cost summary for the given window. The window string
// is passed through to UsageWindowBoundary; unknown values fall back to
// "today" (handlers validate the enum first so this is a safety net).
func (e *Engine) Summary(ctx context.Context, window string) (Summary, error) {
	out := Summary{
		Window:       window,
		TopConsumers: []SummaryConsumer{},
	}
	if e.usage == nil {
		return out, nil
	}
	rows, err := e.usage.ListUsageForWindow(ctx, window)
	if err != nil {
		return out, err
	}
	// Aggregate per (api_key, service) — the top_consumers wire shape is
	// keyed by that pair.
	type ck struct{ apiKey, service string }
	byCons := map[ck]*SummaryConsumer{}
	for _, r := range rows {
		k := ck{r.APIKeyID, r.ServiceID}
		c, ok := byCons[k]
		if !ok {
			c = &SummaryConsumer{APIKeyID: r.APIKeyID, ServiceID: r.ServiceID}
			byCons[k] = c
		}
		c.TokensIn += r.TokensIn
		c.TokensOut += r.TokensOut
		usd := e.rowUSD(r)
		c.USD += usd
		out.TotalUSD += usd
		out.TokensIn += r.TokensIn
		out.TokensOut += r.TokensOut
	}
	for _, c := range byCons {
		out.TopConsumers = append(out.TopConsumers, *c)
	}
	sort.Slice(out.TopConsumers, func(i, j int) bool {
		return out.TopConsumers[i].USD > out.TopConsumers[j].USD
	})
	if len(out.TopConsumers) > 10 {
		out.TopConsumers = out.TopConsumers[:10]
	}

	// pct_of_budget: ratio against the highest daily budget for today only.
	// Week/month/year report nil because budgets are daily.
	if window == "today" && e.budgets != nil {
		budgets, err := e.budgets.ListBudgets(ctx)
		if err == nil && len(budgets) > 0 {
			var maxPct float64
			any := false
			for _, b := range budgets {
				if b.DailyUSD <= 0 {
					continue
				}
				cur, err := e.CurrentUsdFor(ctx, b)
				if err != nil {
					continue
				}
				pct := cur / b.DailyUSD
				if !any || pct > maxPct {
					maxPct = pct
					any = true
				}
			}
			if any {
				out.PctOfBudget = &maxPct
			}
		}
	}
	return out, nil
}

// --- helpers -----------------------------------------------------------------

// budgetMatchesSubjects reports whether a budget's (scope, subject_id) pair
// is satisfied by the given Subjects.
func budgetMatchesSubjects(b db.Budget, s Subjects) bool {
	switch b.Scope {
	case "api_key":
		return s.APIKeyID != "" && s.APIKeyID == b.SubjectID
	case "service":
		return s.ServiceID != "" && s.ServiceID == b.SubjectID
	case "user":
		return s.UserID != "" && s.UserID == b.SubjectID
	case "global":
		return true
	}
	return false
}

// budgetToSubjects derives the Subjects that the engine's
// per-budget aggregation expects, given only the configuration row. Used by
// CurrentUsdFor (the GET /budgets handler doesn't carry a Subjects).
func budgetToSubjects(b db.Budget) Subjects {
	switch b.Scope {
	case "api_key":
		return Subjects{APIKeyID: b.SubjectID}
	case "service":
		return Subjects{ServiceID: b.SubjectID}
	case "user":
		return Subjects{UserID: b.SubjectID}
	}
	return Subjects{}
}

// ErrNotConfigured is a sentinel callers can check against when a method is
// called on a partially-wired engine (e.g. CheckBudgets on an engine with no
// BudgetStore). Currently unused outside tests — included so consumers have
// a stable error to switch on if the surface widens.
var ErrNotConfigured = errors.New("cost: engine not fully configured")
