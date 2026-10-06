package cost

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Guard answers "is this key or model over a hard budget right now" on the
// request path, before any upstream is called. A hard budget is one with
// scope gateway_key or model and action throttle_zero or disable_key.
//
// It caches the set of exceeded budgets for ttl so a busy gateway does not
// aggregate usage on every request, and drops the cache the moment the engine
// sees a budget fire (the usage row that crossed the cap). Usage is written
// when a response ends, so requests already under way when a budget is
// crossed still complete: the overshoot is what those requests use.
//
// When the budgets or today's usage cannot be read the guard keeps what it
// last knew for the rest of that UTC day (a key over its budget stays out)
// and blocks nothing it did not know to be over: a database failure does not
// take the gateway down. Every failed read is logged.
type Guard struct {
	e   *Engine
	ttl time.Duration

	mu       sync.Mutex
	now      func() time.Time
	loadedAt time.Time
	loaded   bool
	stale    bool            // a budget fired since the last read
	day      string          // UTC day the maps are for
	keys     map[string]bool // gateway key ids over a hard budget
	models   map[string]bool // model names over a hard budget
}

// NewGuard returns a guard over e's budgets that reads at most once per ttl,
// and again as soon as a budget fires.
func NewGuard(e *Engine, ttl time.Duration) *Guard {
	g := &Guard{e: e, ttl: ttl, now: time.Now}
	if e != nil {
		e.addExceedListener(g.invalidate)
	}
	return g
}

// SetClock overrides the time source for deterministic tests.
func (g *Guard) SetClock(now func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.now = now
}

// invalidate makes the next Blocked read again.
func (g *Guard) invalidate() {
	g.mu.Lock()
	g.stale = true
	g.mu.Unlock()
}

// Blocked reports whether a budget with action throttle_zero or disable_key
// is exceeded for this gateway key or model. reason is safe to show to the
// caller: it says which of the two is used up and nothing else (no amounts,
// no budget of anyone else). model is the model name the request asks for;
// either argument may be "".
func (g *Guard) Blocked(ctx context.Context, gatewayKeyID, model string) (reason string, blocked bool) {
	if g == nil || g.e == nil {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Refreshing under the mutex is simple and correct: it is one list of
	// budgets and one aggregate, at most once per ttl.
	if now := g.now(); !g.loaded || g.stale || now.Sub(g.loadedAt) >= g.ttl {
		g.refresh(ctx, now)
	}
	if gatewayKeyID != "" && g.keys[gatewayKeyID] {
		return "the daily budget for this key is used up", true
	}
	if model != "" && g.models[model] {
		return "the daily budget for model " + model + " is used up", true
	}
	return "", false
}

// refresh reads the exceeded hard budgets. Caller holds g.mu.
func (g *Guard) refresh(ctx context.Context, now time.Time) {
	day := now.UTC().Format("2006-01-02")
	keys, models, err := g.e.hardExceeded(ctx)
	g.loaded, g.loadedAt = true, now
	if err != nil {
		g.e.log.Error("cost: budget guard could not read budgets or usage; hard budgets are not enforced beyond what was last known",
			slog.String("err", err.Error()))
		if g.day != day {
			// Yesterday's verdict says nothing about today.
			g.keys, g.models, g.day = nil, nil, day
		}
		// The next read is after the ttl, not on every request.
		g.stale = false
		return
	}
	g.keys, g.models, g.day, g.stale = keys, models, day, false
}
