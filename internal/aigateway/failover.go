package aigateway

import (
	"context"
	"math"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

const headerAttempts = "Burrow-Attempts"

// Timeouts, in seconds, of a synthetic model whose row holds no usable value.
const (
	defaultAttemptTimeoutS = 60
	defaultTotalTimeoutS   = 120
)

// Error codes of an attempt row that are not "http_<status>".
const (
	attemptTimeout       = "timeout"          // no response had started when the attempt's time was up
	attemptNoResponse    = "no_response"      // the attempt ended without a status
	attemptClientClosed  = "client_closed"    // the client hung up; not the provider's failure
	attemptStreamAborted = "stream_aborted"   // the response had started and broke off
	attemptPanic         = "panic"            // the upstream handler panicked before a response
	attemptBreakerOpen   = "breaker_open"     // skipped: the provider is failing; nothing was sent
	attemptWrongDialect  = "dialect_mismatch" // skipped: the provider speaks another format; nothing was sent
)

// AttemptRecorder stores what a request tried, for requests that needed more
// than one attempt or failed.
type AttemptRecorder interface {
	RecordAttempts(ctx context.Context, attempts []db.UsageAttempt) error
}

// candidate is one thing to try: a target with one credential slot.
type candidate struct {
	provider db.AIProvider // CredentialSlot narrowed to a single slot
	model    string
	pos      int // place in the request's candidate list; the attempt row's position
}

// candidatesFor expands targets into candidates: every credential slot of a
// direct provider is its own candidate, in order.
func candidatesFor(targets []Target) []candidate {
	var out []candidate
	add := func(p db.AIProvider, model string) {
		out = append(out, candidate{provider: p, model: model, pos: len(out)})
	}
	for _, t := range targets {
		slots := store.SplitSlots(t.Provider.CredentialSlot)
		if t.Provider.Kind != "direct" || len(slots) == 0 {
			add(t.Provider, t.Model)
			continue
		}
		for _, slot := range slots {
			p := t.Provider
			p.CredentialSlot = slot
			add(p, t.Model)
		}
	}
	return out
}

func allDirect(cs []candidate) bool {
	for _, c := range cs {
		if c.provider.Kind != "direct" {
			return false
		}
	}
	return true
}

// failover is the upstream the AI chain sees for a request on a dialect
// endpoint. It tries candidates in order until one produces a response worth
// giving the client.
//
// The rule everything else follows from: the switch to another candidate is
// decided on an attempt's status, before a byte of it reaches the client (see
// commitWriter). Once a response has started it is the client's: it is never
// retried, switched or cut by a timeout, and if it breaks off, it breaks off.
type failover struct {
	g          *Gateway
	candidates []candidate
	dialect    string
	route      *aigw.Route
	requestID  string

	rateLimitFallback bool
	attemptTimeout    time.Duration // 0 = none
	totalTimeout      time.Duration // 0 = none
}

// newFailover builds the handler for a resolved request. candidates are the
// ones usable for this request's endpoint.
func (g *Gateway) newFailover(res Resolution, candidates []candidate, route *aigw.Route, requestID string) *failover {
	unit := g.timeUnit
	if unit <= 0 {
		unit = time.Second
	}
	// A direct address gets no clock from here (0 = none): it has one target
	// and nothing to fall over to, no model row an operator could set a
	// timeout on, and is bounded as it always was, by its transport. A model
	// without a usable value gets the defaults rather than no time at all.
	attempt, total := 0, 0
	if res.Synthetic {
		attempt, total = res.Model.AttemptTimeoutS, res.Model.TotalTimeoutS
		if attempt <= 0 {
			attempt = defaultAttemptTimeoutS
		}
		if total <= 0 {
			total = defaultTotalTimeoutS
		}
	}
	return &failover{
		g: g, candidates: candidates, dialect: res.Dialect, route: route, requestID: requestID,
		rateLimitFallback: res.Synthetic && res.Model.FallbackOnRateLimit,
		attemptTimeout:    time.Duration(attempt) * unit,
		totalTimeout:      time.Duration(total) * unit,
	}
}

func (f *failover) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read once, here, after the chain: redaction may have changed the body,
	// and what the chain passes on is what an upstream may see. Every attempt
	// is spliced from these bytes, never from another attempt's.
	body, err := readRequestBody(r, math.MaxInt64-1) // the size was checked before the chain
	if err != nil {
		f.g.fail(w, r, http.StatusBadRequest, "invalid_request", "could not read the request body")
		return
	}

	var rows []db.UsageAttempt
	// Deferred so the log is written when an aborted stream unwinds, too.
	defer func() { f.record(rows) }()

	order, forced := f.order(&rows)
	var deadline time.Time // zero = no total timeout
	if f.totalTimeout > 0 {
		deadline = time.Now().Add(f.totalTimeout)
	}
	sent := 0 // attempts that reached an upstream handler
	outOfTime := false
	for i, c := range order {
		if r.Context().Err() != nil {
			return // the client is gone; nothing more to try
		}
		timeout := f.attemptTimeout
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				outOfTime = true
				break
			}
			timeout = min(timeout, remaining)
		}
		var next *candidate
		if i+1 < len(order) {
			next = &order[i+1]
		}
		// Asked at the moment of the attempt: this is what lets a provider
		// back in after its cool-down. A breaker that opened since the order
		// was made skips the candidate, unless it is the last thing to try.
		if f.g.Breaker != nil && !f.g.Breaker.Allow(c.provider.Slug) && !forced && next != nil {
			rows = append(rows, f.skipped(c, attemptBreakerOpen))
			continue
		}

		sent++
		res := f.attempt(w, r, c, next, body.WithModel(c.model), timeout, sent)
		rows = append(rows, res.row)
		f.report(c, next, res.outcome)
		if res.panicked && res.committed {
			// The response had started and broke off. The server must abort
			// the connection so the client sees a truncated response, not a
			// complete one; the chain above finishes its bookkeeping first.
			panic(res.panicValue)
		}
		if res.committed || r.Context().Err() != nil {
			return
		}
		outOfTime = res.timedOut
	}

	// Nothing was delivered. The error is the gateway's own, in the request's
	// dialect; it names no provider.
	w.Header().Set(headerAttempts, strconv.Itoa(sent))
	if outOfTime {
		f.g.fail(w, r, http.StatusGatewayTimeout, "gateway_timeout", "no provider answered in time")
		return
	}
	f.g.fail(w, r, http.StatusBadGateway, "upstream_unavailable", "no provider answered")
}

// order returns the candidates to try. A candidate whose provider speaks
// another format than the request is never tried (resolution already leaves
// such targets out; this is the last line). A candidate whose provider the
// breaker refuses is skipped; both skips are recorded. If the breaker refuses
// every candidate, all are tried anyway (forced): a model with nowhere else to
// go must keep trying rather than fail closed.
func (f *failover) order(rows *[]db.UsageAttempt) (order []candidate, forced bool) {
	var sameDialect, open []candidate
	for _, c := range f.candidates {
		if c.provider.APIFormat != f.dialect {
			*rows = append(*rows, f.skipped(c, attemptWrongDialect))
			continue
		}
		sameDialect = append(sameDialect, c)
	}
	if f.g.Breaker == nil {
		return sameDialect, false
	}
	for _, c := range sameDialect {
		if f.g.Breaker.Open(c.provider.Slug) {
			open = append(open, c)
			continue
		}
		order = append(order, c)
	}
	if len(order) == 0 {
		return sameDialect, true
	}
	for _, c := range open {
		*rows = append(*rows, f.skipped(c, attemptBreakerOpen))
	}
	return order, false
}

func (f *failover) skipped(c candidate, code string) db.UsageAttempt {
	return db.UsageAttempt{
		RequestID: f.requestID, Position: c.pos, Ts: time.Now().UTC(),
		ProviderSlug: c.provider.Slug, TargetModel: c.model, ErrorCode: code,
	}
}

// retryable decides whether a status from c means "try the next candidate".
// Server-side failures always do. A rate limit does when the next candidate is
// another key of the same provider, or when the model opted in. Every other
// status is the answer: a request the provider refuses as the client's fault
// must not burn through the chain.
func (f *failover) retryable(status int, c candidate, next *candidate) bool {
	if next == nil {
		return false
	}
	switch {
	case status >= 500:
		return true
	case status == http.StatusTooManyRequests:
		return f.rateLimitFallback || next.provider.Slug == c.provider.Slug
	}
	return false
}

// What an attempt says about its provider.
const (
	outcomeNeutral = iota // nothing: the client's fault, or the client left
	outcomeOK
	outcomeFailed
)

type attemptResult struct {
	row        db.UsageAttempt
	committed  bool // the response went to the client
	timedOut   bool // the attempt's time ran out before a response started
	outcome    int
	panicked   bool
	panicValue any
}

// States of an attempt. The timeout and the decision on the status race for
// the pending state, so exactly one of them wins: a response that has started
// cannot be cancelled by the timer, an attempt that timed out cannot start
// one, and an attempt that was discarded for its status is not turned into a
// timeout while its body is still arriving.
const (
	attemptPending int32 = iota
	attemptCommitted
	attemptDiscarded
	attemptTimedOut
)

// attempt sends the request to one candidate. n is the number of this attempt
// among those sent. It always returns: a panic of the upstream handler is
// caught, the timer is stopped and the attempt's context released before the
// next candidate is looked at.
func (f *failover) attempt(w http.ResponseWriter, r *http.Request, c candidate, next *candidate, body []byte, timeout time.Duration, n int) (res attemptResult) {
	ctx, cancel := context.WithCancel(r.Context())
	var state atomic.Int32
	var timer *time.Timer // nil = no timeout
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			if state.CompareAndSwap(attemptPending, attemptTimedOut) {
				cancel()
			}
		})
	}
	reached := false // the target's policy and credential passed; its upstream handler was called
	late := false    // the status came after the time was up: it is the cancellation's, not the upstream's
	cw := newCommitWriter(w,
		func(status int) bool {
			if state.Load() == attemptTimedOut {
				late = true
				return true
			}
			// A client that is gone gets no second attempt.
			if r.Context().Err() == nil && f.retryable(status, c, next) {
				if state.CompareAndSwap(attemptPending, attemptDiscarded) {
					// Nobody will see the rest of this answer: stop the
					// upstream now instead of reading its body to the end.
					cancel()
				} else {
					late = true
				}
				return true
			}
			if !state.CompareAndSwap(attemptPending, attemptCommitted) {
				late = true
				return true
			}
			return false
		},
		func(int) {
			// The usage row names the target the answer belongs to. The
			// response names it when its upstream was reached: a caller the
			// target's policy refused is not told which provider that was.
			f.route.SetTarget(c.provider.Slug, c.model)
			if first := f.candidates[0]; c.provider.Slug != first.provider.Slug || c.model != first.model {
				// Not the target the chain's cache is keyed on.
				f.route.MarkFallback()
			}
			h := w.Header()
			if reached {
				h.Set(headerProvider, c.provider.Slug)
				h.Set(headerModel, c.model)
			}
			h.Set(headerAttempts, strconv.Itoa(n))
		})
	started := time.Now()

	defer func() {
		if p := recover(); p != nil {
			res.panicked, res.panicValue = true, p
		}
		if timer != nil {
			timer.Stop()
		}
		cancel()

		res.committed = cw.committed
		// Out of time: the attempt's own clock, or the upstream handler's
		// transport giving up on the response headers.
		gaveUp := cw.upstreamTimeout || state.Load() == attemptTimedOut
		res.timedOut = gaveUp && !cw.committed
		clientGone := r.Context().Err() != nil
		status := cw.status
		if late || cw.upstreamTimeout {
			status = 0 // what was written is the gateway's word for it, not an upstream's status
		}
		code := ""
		switch {
		case clientGone:
			code, res.outcome = attemptClientClosed, outcomeNeutral
		case gaveUp:
			code, res.outcome = attemptTimeout, outcomeFailed
		case res.panicked && res.committed:
			code, res.outcome = attemptStreamAborted, outcomeFailed
		case res.panicked && status == 0:
			code, res.outcome = attemptPanic, outcomeFailed
		case status == 0:
			code, res.outcome = attemptNoResponse, outcomeFailed
		case status >= 500:
			code, res.outcome = "http_"+strconv.Itoa(status), outcomeFailed
		case status >= 400:
			// The provider answered; the request was at fault. One client's
			// bad requests say nothing about the provider.
			code, res.outcome = "http_"+strconv.Itoa(status), outcomeNeutral
		default:
			res.outcome = outcomeOK
		}
		if res.panicked && !res.committed && res.panicValue != http.ErrAbortHandler {
			f.g.Log.Error("aigateway: upstream handler panicked", "provider", c.provider.Slug, "request_id", f.requestID,
				"panic", res.panicValue, "stack", string(debug.Stack()))
		}
		// Provider, model, outcome, status, time: nothing of the request, the
		// response or a credential.
		res.row = db.UsageAttempt{
			RequestID: f.requestID, Position: c.pos, Ts: started.UTC(),
			ProviderSlug: c.provider.Slug, TargetModel: c.model,
			Status: status, ErrorCode: code, DurationMs: time.Since(started).Milliseconds(),
		}
	}()

	// A fresh request per attempt: its own context, its own reader over its
	// own body, and the chain's context values (kind, usage, error writer,
	// route) with it.
	req := r.Clone(ctx)
	setBody(req, body)
	// The target's policy first, its credential after; an error either
	// writes is an attempt's answer like any other.
	if upstream, _, ok := f.g.targetUpstream(cw, req, c.provider); ok {
		reached = true
		upstream.ServeHTTP(cw, req)
		if !cw.committed && !cw.discarded {
			// A handler that returns without writing has answered 200 with
			// an empty body, as under net/http; unless its time was up.
			cw.WriteHeader(http.StatusOK)
		}
	}
	return res
}

// report tells the breaker what an attempt showed about its provider. A
// provider with several credential slots is judged once per request: while
// another of its keys is still to be tried, a failure is not yet the
// provider's, so one dead key cannot take a working provider out of service.
func (f *failover) report(c candidate, next *candidate, outcome int) {
	if f.g.Breaker == nil {
		return
	}
	switch {
	case outcome == outcomeOK:
		f.g.Breaker.Report(c.provider.Slug, true)
	case outcome == outcomeFailed && (next == nil || next.provider.Slug != c.provider.Slug):
		f.g.Breaker.Report(c.provider.Slug, false)
	}
}

// record hands the attempt log to the gateway's writer for requests that
// needed more than one attempt, skipped a target or did not succeed. A single
// clean attempt is fully described by its usage row.
func (f *failover) record(rows []db.UsageAttempt) {
	if f.requestID == "" || len(rows) == 0 {
		return
	}
	if len(rows) == 1 && rows[0].ErrorCode == "" {
		return
	}
	f.g.logAttempts(rows)
}

// maxPendingAttemptLogs bounds the attempt logs (one per request) waiting to
// be written. Beyond it logs are dropped: the log is a diagnostic, and a
// store that has stopped answering must not grow the relay's memory.
const maxPendingAttemptLogs = 1024

const (
	attemptLogTimeout = 5 * time.Second
	// attemptLogWarnEvery is how often, at most, dropped logs are reported.
	attemptLogWarnEvery = 10 * time.Second
)

// attemptLog queues attempt logs and writes them off the request path. One
// goroutine writes while there is something to write and ends when the queue
// is empty, so nothing is left running between requests.
type attemptLog struct {
	mu      sync.Mutex
	pending [][]db.UsageAttempt
	running bool
	idle    chan struct{} // closed when the writer ends; nil when none runs

	dropped  int       // logs dropped since the last warning
	lastWarn time.Time // when dropped logs were last reported
}

// logAttempts queues one request's attempt rows. It never blocks.
func (g *Gateway) logAttempts(rows []db.UsageAttempt) {
	if g.Attempts == nil || len(rows) == 0 {
		return
	}
	q := &g.attempts
	q.mu.Lock()
	if len(q.pending) >= maxPendingAttemptLogs {
		// One line per interval, with a count: a store that is down must
		// not turn every request into a log line.
		q.dropped++
		n, now := 0, time.Now()
		if now.Sub(q.lastWarn) >= attemptLogWarnEvery {
			n, q.dropped, q.lastWarn = q.dropped, 0, now
		}
		q.mu.Unlock()
		if n > 0 {
			g.Log.Warn("aigateway: attempt logs dropped, the store is not keeping up", "dropped", n)
		}
		return
	}
	q.pending = append(q.pending, rows)
	start := !q.running
	if start {
		q.running, q.idle = true, make(chan struct{})
	}
	q.mu.Unlock()
	if start {
		go g.writeAttempts()
	}
}

func (g *Gateway) writeAttempts() {
	q := &g.attempts
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.running = false
			close(q.idle)
			q.idle = nil
			q.mu.Unlock()
			return
		}
		rows := q.pending[0]
		q.pending[0] = nil
		q.pending = q.pending[1:]
		q.mu.Unlock()

		// Detached from the request: it is over by now.
		ctx, cancel := context.WithTimeout(context.Background(), attemptLogTimeout)
		err := g.Attempts.RecordAttempts(ctx, rows)
		cancel()
		if err != nil {
			g.Log.Warn("aigateway: recording attempts failed", "request_id", rows[0].RequestID, "err", err)
		}
	}
}

// FlushAttempts waits until the attempt logs queued so far are written, or
// ctx ends (its error is returned and the rest stays queued). Call it on
// shutdown, after the listeners have stopped taking requests.
func (g *Gateway) FlushAttempts(ctx context.Context) error {
	q := &g.attempts
	for {
		q.mu.Lock()
		idle := q.idle
		q.mu.Unlock()
		if idle == nil {
			return nil
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
