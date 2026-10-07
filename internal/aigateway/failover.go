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
	attemptBusy          = "busy"             // the provider had no free place in time; nothing was sent
)

const msgProviderBusy = "the provider is serving as many requests as it is set to; try again shortly"

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
	unit := g.unit()
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
	sent := 0 // attempts made: sent to an upstream, or waiting in vain for a place at one
	outOfTime := false
	busy := false                       // the last candidate looked at had no free place
	full := map[string]bool{}           // providers that had no free place for this request
	admitted := map[string]*admission{} // by provider slug
	// A chain that ends without a response (the total timeout, the end of
	// the list, a panic before one started) leaves the failures it saw with
	// their providers. With a response that is done at the commit, see
	// below. Only a client that went away leaves nothing behind: it is not
	// the providers' fault.
	defer func() {
		if r.Context().Err() == nil {
			f.reportPending(order, admitted, "")
		}
	}()
	for i, c := range order {
		if r.Context().Err() != nil {
			return // the client is gone; nothing more to try
		}
		if !deadline.IsZero() && time.Until(deadline) <= 0 {
			outOfTime = true
			break
		}
		var next *candidate
		if i+1 < len(order) {
			next = &order[i+1]
		}
		// Asked at the moment of the first attempt on a provider: this is
		// what lets it back in after its cool-down. The answer holds for the
		// provider's further candidates in this request (its other keys, the
		// same provider listed again), so a trial covers all of them and the
		// one report per provider decides it. A breaker that opened since
		// the order was made skips the candidate, unless it is the last
		// thing to try.
		adm := admitted[c.provider.Slug]
		if adm == nil {
			adm = &admission{}
			if f.g.Breaker != nil {
				var ok bool
				ok, adm.trial = f.g.Breaker.Allow(c.provider.Slug)
				adm.refused = !ok
			}
			admitted[c.provider.Slug] = adm
		}
		if adm.refused && !forced && next != nil {
			rows = append(rows, f.skipped(c, attemptBreakerOpen))
			continue
		}
		// The provider's other keys share its places: a provider this request
		// has waited for in vain is not waited for again.
		if full[c.provider.Slug] {
			row := f.skipped(c, attemptBusy)
			row.Status = http.StatusTooManyRequests
			rows = append(rows, row)
			continue
		}

		// The breaker hears about the request the moment a response starts,
		// not when its body ends: the first byte shows the answering provider
		// is up, and the providers that failed before it will not be heard
		// from again. A long stream then holds no trial open, and a client
		// leaving mid-stream takes nothing back. What happens to the body
		// afterwards is not reported; the price, accepted, is that a provider
		// which always dies mid-stream never opens its breaker.
		atCommit := func(status int) {
			if r.Context().Err() != nil {
				return
			}
			if status < 400 {
				f.report(c, nil, outcomeOK, adm)
			}
			f.reportPending(order, admitted, c.provider.Slug)
		}

		sent++
		res := f.attempt(w, r, c, next, body.WithModel(c.model), deadline, sent, atCommit)
		rows = append(rows, res.row)
		f.report(c, order[i+1:], res.outcome, adm)
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
		if busy = res.busy; busy {
			full[c.provider.Slug] = true
		}
	}

	// Nothing was delivered. The error is the gateway's own, in the request's
	// dialect; it names no provider.
	w.Header().Set(headerAttempts, strconv.Itoa(sent))
	if busy {
		// The last thing tried was full, not failing: the caller is told to
		// come back, also when the request's time ran out in that queue.
		w.Header().Set("Retry-After", "1")
		f.g.fail(w, r, http.StatusTooManyRequests, "provider_busy", msgProviderBusy)
		return
	}
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
	busy       bool // the provider had no free place in time; nothing was sent
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
// among those sent; deadline is the end of the request's total time (zero =
// none).
//
// An attempt has two clocks. Waiting for a place at a provider with a
// concurrency limit is bounded by slotWait. The attempt timeout starts only
// when the place is obtained: it is the upstream's time to answer, and a
// request that queued long must not run out of it before the provider has
// seen it. Otherwise a full but healthy provider would collect timeouts and
// be taken out of service by the breaker. The total time runs through both.
//
// The lookup before that (the target's policy and credential) has the same
// allowance on a clock of its own; running out of it is a "timeout" that says
// nothing about the provider, which was not called.
//
// It always returns: a panic of the upstream handler is
// caught, the timer is stopped and the attempt's context released before the
// next candidate is looked at.
func (f *failover) attempt(w http.ResponseWriter, r *http.Request, c candidate, next *candidate, body []byte, deadline time.Time, n int, atCommit func(status int)) (res attemptResult) {
	ctx, cancel := context.WithCancel(r.Context())
	var state atomic.Int32
	var timer *time.Timer // nil = no timeout, or not admitted yet
	reached := false      // the target's policy and credential passed; its upstream handler was called
	late := false         // the status came after the time was up: it is the cancellation's, not the upstream's
	busy := false         // no place came free at the provider in time; its upstream handler was not called
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
		func(status int) {
			defer atCommit(status)
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
		res.timedOut = gaveUp && !cw.committed && !busy
		clientGone := r.Context().Err() != nil
		status := cw.status
		if late || cw.upstreamTimeout {
			status = 0 // what was written is the gateway's word for it, not an upstream's status
		}
		code := ""
		switch {
		case clientGone:
			code, res.outcome = attemptClientClosed, outcomeNeutral
		case busy:
			// Waiting in vain, whichever clock ended the wait, says nothing
			// against the provider: it is full, not failing.
			code, res.outcome, res.busy = attemptBusy, outcomeNeutral, true
			status = http.StatusTooManyRequests
		case gaveUp:
			code, res.outcome = attemptTimeout, outcomeFailed
			if !reached {
				// Time ran out on the gateway's own lookup or in the queue:
				// the provider was never called and is not to blame.
				res.outcome = outcomeNeutral
			}
		case res.panicked && res.committed:
			// The response started and broke off. The row says so; for the
			// breaker the start was the answer (reported at the commit).
			code, res.outcome = attemptStreamAborted, outcomeNeutral
			if status >= 500 {
				res.outcome = outcomeFailed
			}
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
	// own body, its own copy of the headers, and the chain's context values
	// (kind, usage, error writer, route) with it. r carries no upstream
	// credential (the chain injects none on a dialect endpoint, see
	// aigw.WithOwnCredential), so nothing an attempt puts on its copy, its
	// target's credential least of all, is seen by another attempt.
	req := r.Clone(ctx)
	setBody(req, body)
	// expire ends the attempt for lack of time, unless it is decided already.
	expire := func() {
		if state.CompareAndSwap(attemptPending, attemptTimedOut) {
			cancel()
		}
	}
	// left is the time the attempt may still take: its own timeout within
	// what remains of the request's. ok is false when the request's time is
	// up; 0 with ok means no limit (a direct address).
	left := func() (d time.Duration, ok bool) {
		d = f.attemptTimeout
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return 0, false
			}
			d = min(d, remaining)
		}
		return d, true
	}

	// The target's policy first, its credential after; an error either
	// writes is an attempt's answer like any other. The lookup has the
	// attempt's time like the upstream call after it, on a clock of its own
	// that is stopped before the wait for a place begins.
	lookup, inTime := left()
	if !inTime {
		expire()
		return res
	}
	if lookup > 0 {
		timer = time.AfterFunc(lookup, expire)
	}
	upstream, _, ok := f.g.targetUpstream(cw, req, c.provider)
	if ok {
		// Last, after this target's policy passed, and on this attempt's
		// headers only.
		f.g.applyCredential(req, c.provider)
	}
	if timer != nil && !timer.Stop() {
		// The lookup's time ran out as it returned: that stands, whichever
		// of the two got there first.
		expire()
	}
	timer = nil
	if ok {
		// The request's time may have ended during the lookup: then it is
		// out of time, whatever the provider's queue looks like.
		if _, inTime := left(); !inTime {
			expire()
		}
		if state.Load() == attemptTimedOut {
			return res
		}
		// A place at the provider, for as long as its handler runs: that is
		// the whole response, a streamed body included. The wait has its own
		// limit and ends when the client leaves; no attempt timer runs.
		release, admitted := f.g.admit(ctx, c.provider, f.slotWait(deadline))
		if !admitted {
			// Only a provider with a limit refuses.
			busy = true
			return res
		}
		// Given back when the attempt returns or its handler panics, before
		// the next candidate is looked at. Nothing stands between obtaining
		// the place and this line.
		defer release()
		// Admitted: from here the upstream's time runs, within what is left
		// of the request's.
		timeout, inTime := left()
		if !inTime {
			// The request's time ended in the queue.
			expire()
			return res
		}
		if timeout > 0 {
			timer = time.AfterFunc(timeout, expire)
		}
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

// applyCredential puts the upstream credential of a tunnel provider on r: the
// one bound to the provider's backing service, as the chain does for a
// provider path. A direct provider needs nothing here: its handler sets the
// credential of its own slot on the outgoing request (see targetUpstream).
//
// A binding that cannot be read does not stop the request: it goes out without
// a credential and the upstream refuses it, as on a provider path. The log
// names the provider and the service, never a value.
func (g *Gateway) applyCredential(r *http.Request, p db.AIProvider) {
	if g.Credentials == nil || p.Kind != "tunnel" {
		return
	}
	if _, err := g.Credentials.Apply(r.Context(), p.ServiceID, r); err != nil {
		g.Log.Warn("aigateway: credential injection failed", "provider", p.Slug, "service_id", p.ServiceID, "err", err)
	}
}

// slotWait is the longest an attempt waits for a place at its provider: the
// smallest of the gateway's wait limit, the model's attempt timeout and what
// is left of the request's total time. A direct address has neither timer and
// waits for the gateway's limit.
func (f *failover) slotWait(deadline time.Time) time.Duration {
	wait := slotWaitUnits * f.g.unit()
	if f.attemptTimeout > 0 {
		wait = min(wait, f.attemptTimeout)
	}
	if !deadline.IsZero() {
		wait = min(wait, time.Until(deadline))
	}
	return wait
}

// admission is what the breaker said about a provider for this request, and
// what this request has told the breaker about it.
type admission struct {
	refused  bool
	trial    uint64 // non-zero: this request is the provider's trial
	failed   bool   // an attempt failed in a way that counts against the provider
	reported bool   // the breaker has this request's verdict; there is no second
}

// report tells the breaker what an attempt showed about its provider. A
// provider is judged once per request, whatever number of candidates it has
// (several credential slots, or the same provider listed again further down):
// a success is reported at once (at the commit); a failure when no later candidate of the
// provider remains or the chain ends (see reportPending), so one dead key cannot take a working provider out of
// service and one request cannot count against it twice. A failure that does
// not count (the client's fault, the client gone) is never reported.
func (f *failover) report(c candidate, rest []candidate, outcome int, adm *admission) {
	// A provider the breaker refused and that was tried all the same, as a
	// last resort, was not asked for: its attempt tells the breaker nothing.
	if f.g.Breaker == nil || adm.reported || adm.refused {
		return
	}
	switch outcome {
	case outcomeOK:
		adm.reported = true
		f.g.Breaker.Report(c.provider.Slug, true, adm.trial)
	case outcomeFailed:
		adm.failed = true
		for _, later := range rest {
			if later.provider.Slug == c.provider.Slug {
				return // not yet the provider's last word
			}
		}
		adm.reported = true
		f.g.Breaker.Report(c.provider.Slug, false, adm.trial)
	}
}

// reportPending reports the failures that were waiting for a later candidate
// of their provider, when the chain ends before that candidate is tried.
// except names the provider whose attempt is under way and speaks for itself.
func (f *failover) reportPending(order []candidate, admitted map[string]*admission, except string) {
	if f.g.Breaker == nil {
		return
	}
	for _, c := range order {
		if c.provider.Slug == except {
			continue
		}
		if adm := admitted[c.provider.Slug]; adm != nil && adm.failed && !adm.reported && !adm.refused {
			adm.reported = true
			f.g.Breaker.Report(c.provider.Slug, false, adm.trial)
		}
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
