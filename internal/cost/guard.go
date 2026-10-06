package cost

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// guardReadTimeout bounds one read of the budgets and today's usage. The read
// runs for the gateway, not for the request that happened to trigger it.
const guardReadTimeout = 2 * time.Second

// Guard answers "is this key or model over a hard budget right now" on the
// request path, before any upstream is called. A hard budget is one with
// scope gateway_key or model and action throttle_zero or disable_key.
//
// It keeps the set of exceeded budgets (the verdict) for ttl so a busy
// gateway does not aggregate usage on every request, and reads again the
// moment the engine sees a budget fire (the usage row that crossed the cap).
// Usage is written when a response ends, so requests already under way when a
// budget is crossed still complete: the overshoot is what those requests use.
//
// Budgets are per UTC day: a verdict is void once the UTC day it was read on
// has ended, whatever is left of the ttl.
//
// One caller at a time reads; the read is not tied to that caller's request
// (a client that disconnects does not fail it) and is cut off after
// guardReadTimeout. The other callers answer from the last verdict meanwhile
// and never wait for the database, except before the very first verdict,
// when there is nothing to answer from: then they wait for the read that is
// under way, as long as their own request lives.
//
// When the budgets or today's usage cannot be read the guard keeps the last
// verdict for the rest of that UTC day (a key over its budget stays out) and
// blocks nothing it did not know to be over: a database failure does not
// take the gateway down. Every failed read is logged, and the next one is
// tried after the ttl.
type Guard struct {
	e   *Engine
	ttl time.Duration

	mu          sync.Mutex
	now         func() time.Time
	readTimeout time.Duration
	loaded      bool            // a read has ended, well or not
	loadedAt    time.Time       // when the last read ended
	gen         uint64          // counts invalidations
	readGen     uint64          // gen the last applied read started at
	day         string          // UTC day the verdict is for
	keys        map[string]bool // gateway key ids over a hard budget
	models      map[string]bool // model names over a hard budget
	reading     chan struct{}   // non-nil while a read is under way; closed when it ends
}

// NewGuard returns a guard over e's budgets that reads at most once per ttl,
// and again as soon as a budget fires or the UTC day changes.
func NewGuard(e *Engine, ttl time.Duration) *Guard {
	g := &Guard{e: e, ttl: ttl, now: time.Now, readTimeout: guardReadTimeout}
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
	g.gen++
	g.mu.Unlock()
}

func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Blocked reports whether a budget with action throttle_zero or disable_key
// is exceeded for this gateway key or model. reason is safe to show to the
// caller: it says which of the two is used up and nothing else (no amounts,
// no budget of anyone else). model is the requested model as the usage row
// records it; either argument may be "".
func (g *Guard) Blocked(ctx context.Context, gatewayKeyID, model string) (reason string, blocked bool) {
	if g == nil || g.e == nil {
		return "", false
	}
	g.mu.Lock()
	now := g.now()
	if day := utcDay(now); g.day != day {
		// A new UTC day: the budgets have reset. Nothing is known to be over
		// until the new day's usage has been read.
		g.keys, g.models, g.day = nil, nil, day
		g.gen++
	}
	if !g.loaded || g.gen != g.readGen || now.Sub(g.loadedAt) >= g.ttl {
		switch {
		case g.reading == nil:
			// This caller reads, without the lock.
			done := make(chan struct{})
			g.reading = done
			gen, day, timeout := g.gen, g.day, g.readTimeout
			g.mu.Unlock()
			g.read(ctx, done, gen, day, timeout)
			g.mu.Lock()
		case !g.loaded:
			// No verdict yet: wait for the first read, or for the request's end.
			wait := g.reading
			g.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
			}
			g.mu.Lock()
		}
		// Otherwise another caller is reading: answer from the last verdict.
	}
	defer g.mu.Unlock()
	if gatewayKeyID != "" && g.keys[gatewayKeyID] {
		return "the daily budget for this key is used up", true
	}
	if model != "" && g.models[model] {
		return "the daily budget for model " + model + " is used up", true
	}
	return "", false
}

// read fetches the exceeded hard budgets and stores the verdict. The caller
// has set g.reading to done and does not hold g.mu. gen and day are what the
// guard stood at when the read began.
func (g *Guard) read(ctx context.Context, done chan struct{}, gen uint64, day string, timeout time.Duration) {
	var (
		keys, models map[string]bool
		err          = errors.New("the read did not return")
	)
	// Deferred so that a read that panics still ends: nobody waits on a read
	// that will never finish.
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		defer close(done)
		g.reading = nil
		g.loaded, g.loadedAt = true, g.now()
		// A budget that fired while the read ran is not in its result: the
		// next caller reads again. After a failure the next read is due
		// after the ttl, not on every request.
		g.readGen = gen
		switch {
		case err != nil:
			g.e.log.Error("cost: budget guard could not read budgets or usage; hard budgets are not enforced beyond what was last known",
				slog.String("err", err.Error()))
			g.readGen = g.gen
		case g.day == day:
			g.keys, g.models = keys, models
		}
		// Otherwise midnight passed while the read ran and its result is
		// yesterday's: it is dropped, and the day change, which counted as
		// an invalidation, makes the next caller read again.
	}()
	// The read serves every request of the next ttl, so it must not end
	// because the one request that started it did.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	keys, models, err = g.e.hardExceeded(rctx)
}
