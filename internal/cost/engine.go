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
	// GatewayKeyID is the gateway key that asked; "" for other traffic.
	GatewayKeyID string
	// Model is the requested model as the usage row records it: a synthetic
	// model's name, or "<provider>/<native id>" for a direct address and for
	// a request on that provider's own path.
	Model string
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

// GatewayKeyRevoker revokes a gateway key when a gateway_key budget with
// action_on_exceed=disable_key is exceeded. *store.Store satisfies it.
type GatewayKeyRevoker interface {
	// budgetID is the budget that was exceeded; the revoker records it.
	// revoked is false when the key was revoked already and nothing changed.
	RevokeGatewayKeyByID(ctx context.Context, id, budgetID string) (revoked bool, err error)
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
	gwRevoker  GatewayKeyRevoker
	dispatcher Dispatcher
	// onExceed is called (outside the lock) when a budget fires: the budget
	// guard drops its cached verdict so the next request sees the new state.
	onExceed []func()

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

// SetGatewayKeyRevoker installs what disable_key calls for a gateway_key
// budget. Without one such a budget still blocks at the gateway (see Guard)
// but the key is not revoked.
func (e *Engine) SetGatewayKeyRevoker(r GatewayKeyRevoker) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gwRevoker = r
}

// addExceedListener registers fn to be called whenever a budget fires.
func (e *Engine) addExceedListener(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onExceed = append(e.onExceed, fn)
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

// priceKey returns the price-table key a usage row is priced by: the first of
// "<provider>/<target model>", the target model, and the kind that has an
// entry. "" when none has.
func (e *Engine) priceKey(r db.UsageRow) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if r.TargetModel != "" {
		if r.ProviderSlug != "" {
			if _, ok := e.pricing.Lookup(r.ProviderSlug + "/" + r.TargetModel); ok {
				return r.ProviderSlug + "/" + r.TargetModel
			}
		}
		if _, ok := e.pricing.Lookup(r.TargetModel); ok {
			return r.TargetModel
		}
	}
	if _, ok := e.pricing.Lookup(r.Kind); ok {
		return r.Kind
	}
	return ""
}

// rowUSD prices one usage row: what upstreams reported, plus the price table
// for the tokens no upstream put a price on (see priceKey).
func (e *Engine) rowUSD(r db.UsageRow) float64 {
	usd := r.ReportedUSD
	if key := e.priceKey(r); key != "" {
		usd += e.UsdFor(key, int(r.PricedTokensIn), int(r.PricedTokensOut))
	}
	return usd
}

// RowUSD prices one usage row as Summary does: what upstreams reported, plus
// the price table for the rest, by the provider and model that answered.
func (e *Engine) RowUSD(r db.UsageRow) float64 { return e.rowUSD(r) }

// BudgetExceeded reports whether a budget with the given spend and token use
// of the current UTC day is over. A budget with a token cap (DailyTokens > 0)
// is over when the tokens exceed it, or when it also has a USD cap
// (DailyUSD > 0) and the spend exceeds that. A budget without a token cap is
// over when the spend exceeds DailyUSD, as it always was.
func BudgetExceeded(b db.Budget, usd float64, tokens int64) bool {
	if b.DailyTokens > 0 {
		return tokens > b.DailyTokens || (b.DailyUSD > 0 && usd > b.DailyUSD)
	}
	return usd > b.DailyUSD
}

// todayUsage reads today's usage rows at most once for one check, however
// many budgets look at them.
type todayUsage struct {
	e      *Engine
	rows   []db.UsageRow
	loaded bool
	err    error
}

func (u *todayUsage) get(ctx context.Context) ([]db.UsageRow, error) {
	if !u.loaded {
		u.rows, u.err = u.e.usage.ListUsageForWindow(ctx, "today")
		u.loaded = true
	}
	return u.rows, u.err
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
//   - throttle_zero → dispatches the event; a gateway_key or model budget
//     is from then on refused at the gateway (see Guard)
//   - disable_key   → revokes the key of an api_key or gateway_key budget
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

	today := &todayUsage{e: e}
	for _, b := range budgets {
		// Match this budget against the caller's Subjects.
		if !budgetMatchesSubjects(b, subj) {
			continue
		}
		// Compute today's spend and tokens for this budget.
		usd, tokens, err := e.budgetUsage(ctx, b, today)
		if err != nil {
			// Don't trigger on a read error — the caller (SQLSink) treats
			// CheckBudgets as best-effort.
			e.log.Warn("cost: current spend lookup failed",
				slog.String("budget_id", b.ID), slog.String("err", err.Error()))
			continue
		}
		if !BudgetExceeded(b, usd, tokens) {
			continue
		}
		// Exceeded — already triggered this UTC day?
		e.mu.Lock()
		if e.triggered[b.ID] {
			e.mu.Unlock()
			continue
		}
		e.triggered[b.ID] = true
		listeners := append([]func(){}, e.onExceed...)
		e.mu.Unlock()

		// Fire the configured action. Errors are logged + swallowed so a
		// single failed alert doesn't poison subsequent budgets.
		e.fireAction(ctx, b, usd, tokens)
		for _, fn := range listeners {
			fn()
		}
		return b.ActionOnExceed, b, nil
	}
	return "", db.Budget{}, nil
}

// BudgetUsage is what a budget has used in the current UTC day.
type BudgetUsage struct {
	USD      float64
	Tokens   int64
	Exceeded bool
}

// BudgetUsages returns the usage of every given budget from one read of
// today's usage rows.
func (e *Engine) BudgetUsages(ctx context.Context, budgets []db.Budget) ([]BudgetUsage, error) {
	out := make([]BudgetUsage, len(budgets))
	today := &todayUsage{e: e}
	for i, b := range budgets {
		usd, tokens, err := e.budgetUsage(ctx, b, today)
		if err != nil {
			return nil, err
		}
		out[i] = BudgetUsage{USD: usd, Tokens: tokens, Exceeded: BudgetExceeded(b, usd, tokens)}
	}
	return out, nil
}

// CheckBudgetsForSample is the BudgetChecker hook used by aimeter.SQLSink.
// It is called after each usage_events insert with the identity of the
// request: service, service key, gateway key and the model name the client
// asked for; the global scope matches unconditionally. All errors are logged
// + swallowed so a budget-check failure never breaks the proxy hot path.
func (e *Engine) CheckBudgetsForSample(ctx context.Context, serviceID, apiKeyID, gatewayKeyID, requestedModel string) {
	if e == nil || e.budgets == nil {
		return
	}
	if _, _, err := e.CheckBudgets(ctx, Subjects{
		APIKeyID:     apiKeyID,
		ServiceID:    serviceID,
		GatewayKeyID: gatewayKeyID,
		Model:        requestedModel,
	}); err != nil {
		e.log.Warn("cost: CheckBudgets after insert failed",
			slog.String("service_id", serviceID),
			slog.String("api_key_id", apiKeyID),
			slog.String("gateway_key_id", gatewayKeyID),
			slog.String("err", err.Error()))
	}
}

// budgetRowFilter returns which of today's usage rows count for budget b, or
// nil when the scope has no rows to count (user: usage_events carries no
// user; an empty subject; an unknown scope).
func budgetRowFilter(b db.Budget) func(db.UsageRow) bool {
	id := b.SubjectID
	if b.Scope == "global" {
		return func(db.UsageRow) bool { return true }
	}
	if id == "" {
		return nil
	}
	switch b.Scope {
	case "api_key":
		return func(r db.UsageRow) bool { return r.APIKeyID == id }
	case "service":
		return func(r db.UsageRow) bool { return r.ServiceID == id }
	case "gateway_key":
		return func(r db.UsageRow) bool { return r.GatewayKeyID == id }
	case "model":
		// The name the client asked for, exactly. A request on a provider's
		// own path is recorded as "<provider>/<native id>", the model's
		// direct address, so both doors count for the same budget. A
		// request answered by a fallback target counts for the model the
		// client asked for, not for the one that answered.
		return func(r db.UsageRow) bool { return r.RequestedModel == id }
	}
	return nil
}

// budgetUsage is the shared implementation behind CheckBudgets, the guard
// and BudgetUsages (the GET /budgets figures): today's spend and tokens of the usage rows
// that count for b. scope=user reports 0 (usage_events has no user column).
func (e *Engine) budgetUsage(ctx context.Context, b db.Budget, today *todayUsage) (float64, int64, error) {
	match := budgetRowFilter(b)
	if match == nil {
		return 0, 0, nil
	}
	if e.usage == nil {
		// Only the totals are wired (tests): without per-kind rows the
		// tokens are priced as "unknown" — usually 0.
		if e.daily == nil {
			return 0, 0, nil
		}
		var in, out int64
		var err error
		switch b.Scope {
		case "api_key":
			in, out, err = e.daily.SumDailyTokensByAPIKey(ctx, b.SubjectID)
		case "service":
			in, out, err = e.daily.SumDailyTokensByService(ctx, b.SubjectID)
		}
		if err != nil {
			return 0, 0, err
		}
		return e.UsdFor("unknown", int(in), int(out)), in + out, nil
	}
	rows, err := today.get(ctx)
	if err != nil {
		return 0, 0, err
	}
	usd, tokens := e.usageOf(rows, match)
	return usd, tokens, nil
}

// usageOf sums the spend and the tokens (input + output) of the rows match
// accepts.
func (e *Engine) usageOf(rows []db.UsageRow, match func(db.UsageRow) bool) (usd float64, tokens int64) {
	for _, r := range rows {
		if !match(r) {
			continue
		}
		usd += e.rowUSD(r)
		tokens += r.TokensIn + r.TokensOut
	}
	return usd, tokens
}

// hardExceeded returns the gateway keys and models that are over a budget
// which stops requests (action throttle_zero or disable_key) in the current
// UTC day. One read of the budgets and one of today's usage.
func (e *Engine) hardExceeded(ctx context.Context) (keys, models map[string]bool, err error) {
	keys, models = map[string]bool{}, map[string]bool{}
	if e == nil || e.budgets == nil || e.usage == nil {
		return keys, models, nil
	}
	budgets, err := e.budgets.ListBudgets(ctx)
	if err != nil {
		return nil, nil, err
	}
	today := &todayUsage{e: e}
	for _, b := range budgets {
		if b.Scope != "gateway_key" && b.Scope != "model" {
			continue
		}
		if b.ActionOnExceed != "throttle_zero" && b.ActionOnExceed != "disable_key" {
			continue
		}
		usd, tokens, err := e.budgetUsage(ctx, b, today)
		if err != nil {
			return nil, nil, err
		}
		if !BudgetExceeded(b, usd, tokens) {
			continue
		}
		if b.Scope == "gateway_key" {
			keys[b.SubjectID] = true
		} else {
			models[b.SubjectID] = true
		}
	}
	return keys, models, nil
}

// fireAction dispatches the configured side-effect for an exceeded budget.
// All errors are logged + swallowed — a single failure must not break the
// caller's hot path.
func (e *Engine) fireAction(ctx context.Context, b db.Budget, currentUSD float64, currentTokens int64) {
	payload := map[string]any{
		"budget_id":        b.ID,
		"scope":            b.Scope,
		"subject_id":       b.SubjectID,
		"daily_usd":        b.DailyUSD,
		"current_usd":      currentUSD,
		"daily_tokens":     b.DailyTokens,
		"current_tokens":   currentTokens,
		"action_on_exceed": b.ActionOnExceed,
	}
	e.mu.RLock()
	dispatcher, gwRevoker := e.dispatcher, e.gwRevoker
	e.mu.RUnlock()
	switch b.ActionOnExceed {
	case "alert_webhook":
		if dispatcher == nil {
			e.log.Warn("cost: budget exceeded but no dispatcher wired",
				slog.String("budget_id", b.ID),
				slog.Float64("current_usd", currentUSD))
			return
		}
		dispatcher.Publish(ctx, "budget.exceeded", payload)
	case "throttle_zero":
		// A gateway_key or model budget is refused at the gateway from now
		// on (Guard). For the other scopes the event is all there is:
		// listeners may react to it.
		if dispatcher != nil {
			dispatcher.Publish(ctx, "budget.exceeded", payload)
		} else {
			e.log.Warn("cost: throttle_zero requested but no dispatcher wired",
				slog.String("budget_id", b.ID))
		}
	case "disable_key":
		switch b.Scope {
		case "gateway_key":
			if gwRevoker == nil || b.SubjectID == "" {
				e.log.Warn("cost: disable_key requested but no gateway key revoker wired",
					slog.String("budget_id", b.ID))
				return
			}
			revoked, err := gwRevoker.RevokeGatewayKeyByID(ctx, b.SubjectID, b.ID)
			if err != nil {
				e.log.Warn("cost: disable_key revoke failed",
					slog.String("gateway_key_id", b.SubjectID),
					slog.String("budget_id", b.ID),
					slog.String("err", err.Error()))
				return
			}
			// Only a revoke that changed something is one: the key may have
			// been revoked by hand, or by another budget, a moment ago.
			if revoked {
				e.log.Info("cost: gateway key revoked, its budget is exceeded",
					slog.String("gateway_key_id", b.SubjectID),
					slog.String("budget_id", b.ID),
					slog.Float64("current_usd", currentUSD),
					slog.Int64("current_tokens", currentTokens))
			}
		case "api_key":
			if e.revoker == nil || e.keyLocator == nil {
				e.log.Warn("cost: disable_key requested but no revoker wired",
					slog.String("budget_id", b.ID))
				return
			}
			apiKeyID := b.SubjectID
			if apiKeyID == "" {
				e.log.Warn("cost: disable_key needs a subject_id", slog.String("budget_id", b.ID))
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
		default:
			// A model budget has no key to disable; the gateway refuses the
			// model (Guard). For the remaining scopes there is nothing to do.
			if b.Scope != "model" {
				e.log.Warn("cost: disable_key needs scope api_key or gateway_key",
					slog.String("budget_id", b.ID), slog.String("scope", b.Scope))
				return
			}
		}
		// Also publish the exceeded event for audit / dashboard parity.
		if dispatcher != nil {
			dispatcher.Publish(ctx, "budget.exceeded", payload)
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
			today := &todayUsage{e: e, rows: rows, loaded: true}
			for _, b := range budgets {
				if b.DailyUSD <= 0 {
					continue
				}
				cur, _, err := e.budgetUsage(ctx, b, today)
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

// --- usage by key and model --------------------------------------------------

// GroupRow is one group of GET /cost/summary?group_by=…: the usage of one
// gateway key, model, provider, target model or dialect in the window.
type GroupRow struct {
	Key       string  `json:"key"`
	Requests  int64   `json:"requests"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	USD       float64 `json:"usd"`
}

// ErrBadDimension is returned by SummaryBy for a dimension it does not know.
var ErrBadDimension = errors.New("cost: unknown group-by dimension")

// MaxGroups is the most named groups SummaryBy returns. A client can send any
// model name on a provider path, so the number of groups is not bounded by
// the configuration. What is cut is added to the group with the empty key.
const MaxGroups = 200

// groupKeys maps a dimension to the group key of a usage row. Only these
// names are accepted; nothing of the dimension reaches a query.
var groupKeys = map[string]func(db.UsageRow) string{
	"gateway_key": func(r db.UsageRow) string { return r.GatewayKeyID },
	"model":       func(r db.UsageRow) string { return r.RequestedModel },
	"provider":    func(r db.UsageRow) string { return r.ProviderSlug },
	"dialect":     func(r db.UsageRow) string { return r.Dialect },
	"target_model": func(r db.UsageRow) string {
		if r.ProviderSlug == "" && r.TargetModel == "" {
			return ""
		}
		return r.ProviderSlug + "/" + r.TargetModel
	},
}

// SummaryBy groups the usage of the window by one dimension: gateway_key
// (the key's id), model (the name the client asked for), provider,
// target_model ("<provider>/<model>" that answered) or dialect. Groups are
// sorted by USD descending, then key; the group with the empty key (traffic
// the dimension does not apply to, and whatever MaxGroups cut) comes last.
// Never nil.
func (e *Engine) SummaryBy(ctx context.Context, window, dimension string) ([]GroupRow, error) {
	keyOf, ok := groupKeys[dimension]
	if !ok {
		return nil, ErrBadDimension
	}
	out := []GroupRow{}
	if e.usage == nil {
		return out, nil
	}
	rows, err := e.usage.ListUsageForWindow(ctx, window)
	if err != nil {
		return nil, err
	}
	groups := map[string]*GroupRow{}
	for _, r := range rows {
		k := keyOf(r)
		g, ok := groups[k]
		if !ok {
			g = &GroupRow{Key: k}
			groups[k] = g
		}
		g.Requests += r.Requests
		g.TokensIn += r.TokensIn
		g.TokensOut += r.TokensOut
		g.USD += e.rowUSD(r)
	}
	rest, hasRest := GroupRow{}, false
	for k, g := range groups {
		if k == "" {
			rest, hasRest = *g, true
			continue
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].USD != out[j].USD {
			return out[i].USD > out[j].USD
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > MaxGroups {
		for _, g := range out[MaxGroups:] {
			rest.Requests += g.Requests
			rest.TokensIn += g.TokensIn
			rest.TokensOut += g.TokensOut
			rest.USD += g.USD
		}
		out, hasRest = out[:MaxGroups], true
	}
	if hasRest {
		out = append(out, rest)
	}
	return out, nil
}

// --- helpers -----------------------------------------------------------------

// budgetMatchesSubjects reports whether the request described by s adds to
// budget b: only then is b looked at after the request's usage row.
func budgetMatchesSubjects(b db.Budget, s Subjects) bool {
	switch b.Scope {
	case "api_key":
		return s.APIKeyID != "" && s.APIKeyID == b.SubjectID
	case "service":
		return s.ServiceID != "" && s.ServiceID == b.SubjectID
	case "user":
		return s.UserID != "" && s.UserID == b.SubjectID
	case "gateway_key":
		return s.GatewayKeyID != "" && s.GatewayKeyID == b.SubjectID
	case "model":
		return s.Model != "" && s.Model == b.SubjectID
	case "global":
		return true
	}
	return false
}

// ErrNotConfigured is a sentinel callers can check against when a method is
// called on a partially-wired engine (e.g. CheckBudgets on an engine with no
// BudgetStore). Currently unused outside tests — included so consumers have
// a stable error to switch on if the surface widens.
var ErrNotConfigured = errors.New("cost: engine not fully configured")
