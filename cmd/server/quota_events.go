// quota_events.go — announces what the AI data plane refused: a
// "quota.exceeded" webhook event when a rate limit or day quota denies a
// request, and a "guardrail.refused" audit entry when a guardrail refuses one.
// Both only report a decision that was already taken.
package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/quota"
)

// eventPublisher is the webhook dispatcher as this file needs it.
// (*webhook.Dispatcher).Publish never blocks: it puts the event on a bounded
// queue and drops it when the queue is full; delivery, signing and the
// destination checks happen on the dispatcher's own worker.
type eventPublisher interface {
	Publish(ctx context.Context, event string, payload any)
}

// quotaEventInterval is how often one limit may be announced.
const quotaEventInterval = time.Minute

// eventThrottleMaxKeys bounds the throttle's memory.
const eventThrottleMaxKeys = 1024

// eventThrottleFullLogEvery is how often a throttle that is full says so.
const eventThrottleFullLogEvery = time.Minute

// eventThrottle lets one event per key through per interval.
type eventThrottle struct {
	mu    sync.Mutex
	last  map[string]time.Time
	now   func() time.Time
	every time.Duration
	// name and log are for the one thing a throttle reports itself: that it
	// is full and refuses keys it has not seen. Without a log it stays
	// silent.
	name       string
	log        *slog.Logger
	fullLogged time.Time
}

// allow reports whether key may publish now, and if so starts its interval.
// When the map is full, entries whose interval is over are dropped; while it
// is full of live entries a new key is refused, so the map never grows past
// eventThrottleMaxKeys. Such a refusal is logged, at most once a minute, with
// the throttle's name and never the key (a key may hold an address or an id).
func (t *eventThrottle) allow(key string) bool {
	allowed, full := t.take(key)
	if full && t.log != nil {
		t.log.Warn("event throttle full, new keys are refused until entries expire",
			"throttle", t.name, "max_keys", eventThrottleMaxKeys, "interval", t.every.String())
	}
	return allowed
}

// take is allow's locked part. full is true when the key was refused because
// the map is full and it is time to say so again.
func (t *eventThrottle) take(key string) (allowed, full bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if last, ok := t.last[key]; ok {
		if now.Sub(last) < t.every {
			return false, false
		}
		t.last[key] = now
		return true, false
	}
	if len(t.last) >= eventThrottleMaxKeys {
		for k, at := range t.last {
			if now.Sub(at) >= t.every {
				delete(t.last, k)
			}
		}
		if len(t.last) >= eventThrottleMaxKeys {
			if t.fullLogged.IsZero() || now.Sub(t.fullLogged) >= eventThrottleFullLogEvery {
				t.fullLogged = now
				return false, true
			}
			return false, false
		}
	}
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	t.last[key] = now
	return true, false
}

func (t *eventThrottle) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.last)
}

// quotaEvents publishes "quota.exceeded" for denied requests: one event per
// (scope, subject, dimension, window) per minute, however many requests the
// limit refuses in that minute.
type quotaEvents struct {
	pub      eventPublisher
	throttle *eventThrottle
}

// newQuotaEvents returns the publisher for the quota middleware. pub may be
// nil (nothing is published); now may be nil (time.Now); log may be nil (a
// full throttle is then not reported).
func newQuotaEvents(pub eventPublisher, now func() time.Time, log *slog.Logger) *quotaEvents {
	if now == nil {
		now = time.Now
	}
	return &quotaEvents{pub: pub, throttle: &eventThrottle{now: now, every: quotaEventInterval, name: "quota_exceeded", log: log}}
}

// denied announces a denial. The payload describes the limit that denied and
// the service the request was for: ids and numbers, nothing of the request.
func (q *quotaEvents) denied(ctx context.Context, who quota.Subjects, dec quota.Decision) {
	if q == nil || q.pub == nil || dec.Allow {
		return
	}
	if !q.throttle.allow(dec.LimitingScope + "\x00" + dec.LimitingSubject + "\x00" + dec.Dimension + "\x00" + dec.Window) {
		return
	}
	q.pub.Publish(ctx, "quota.exceeded", map[string]any{
		"scope":      dec.LimitingScope,
		"subject":    dec.LimitingSubject,
		"dimension":  dec.Dimension,
		"limit":      dec.Limit,
		"window":     dec.Window,
		"service_id": who.ServiceID,
	})
}

// auditAppender is *audit.Logger as the guardrail auditor needs it.
type auditAppender interface {
	Append(ctx context.Context, e audit.Event) error
}

// guardrailAuditTimeout bounds the audit write of one refusal.
const guardrailAuditTimeout = 5 * time.Second

// guardrailAuditQueue is how many refusals may wait for the audit writer.
const guardrailAuditQueue = 256

// guardrailRefusal is one refusal waiting to be audited. A value with ack
// set is a marker from Flush instead: the worker closes ack when it gets
// there.
type guardrailRefusal struct {
	service, pattern, action, gatewayKey string
	// direction is "" for a refused request, "answer" for an answer a
	// pattern matched.
	direction string
	// judge is set for a judgement of the judge model (see judged); pattern
	// and action are then unused.
	judge *aigw.JudgeEvent
	ack   chan struct{}
}

// guardrailAuditor writes the guardrail.refused audit entries. The chain
// calls refused on the request's goroutine; the audit write (a transaction
// on the hash chain) runs on the auditor's single worker, so a slow or stuck
// database cannot hold a refusal back. The queue is bounded: when it is full
// the entry is dropped and a warning is logged, at most once a minute.
type guardrailAuditor struct {
	audit auditAppender
	log   *slog.Logger
	queue chan guardrailRefusal
	quit  chan struct{}
	once  sync.Once
	full  *eventThrottle
}

// newGuardrailAuditor starts the one worker. log may be nil.
func newGuardrailAuditor(a auditAppender, log *slog.Logger) *guardrailAuditor {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	g := &guardrailAuditor{
		audit: a,
		log:   log,
		queue: make(chan guardrailRefusal, guardrailAuditQueue),
		quit:  make(chan struct{}),
		full:  &eventThrottle{now: time.Now, every: time.Minute, name: "guardrail_audit_warnings", log: log},
	}
	go g.work()
	return g
}

// refused is the chain's OnGuardrailRefuse hook. It never blocks: it reads
// the gateway key from the request's route and hands the refusal over.
func (g *guardrailAuditor) refused(ctx context.Context, serviceID, pattern, action string) {
	g.enqueue(ctx, guardrailRefusal{service: serviceID, pattern: pattern, action: action})
}

// answered is the chain's OnGuardrailAnswer hook: an answer pattern matched
// an answer, which was delivered (action "flag", audited as
// guardrail.flagged) or withheld ("block", audited as guardrail.refused
// with direction "answer"). It never blocks.
func (g *guardrailAuditor) answered(ctx context.Context, serviceID, pattern, action string) {
	g.enqueue(ctx, guardrailRefusal{service: serviceID, pattern: pattern, action: action, direction: "answer"})
}

// judged is the chain's OnGuardrailJudge hook: the judge blocked a request
// or an answer (audited as guardrail.refused with tier "judge" and the
// judge's reason), or it failed (guardrail.judge_failed with the class). It
// never blocks.
func (g *guardrailAuditor) judged(ctx context.Context, serviceID string, e aigw.JudgeEvent) {
	g.enqueue(ctx, guardrailRefusal{service: serviceID, judge: &e})
}

func (g *guardrailAuditor) enqueue(ctx context.Context, job guardrailRefusal) {
	if ri, ok := aigw.RouteFrom(ctx); ok {
		job.gatewayKey = ri.GatewayKeyID
	}
	select {
	case g.queue <- job:
	default:
		if g.full.allow("full") {
			g.log.Warn("guardrail audit: queue full, dropping entries", "capacity", guardrailAuditQueue)
		}
	}
}

func (g *guardrailAuditor) work() {
	for {
		select {
		case <-g.quit:
			return
		case job := <-g.queue:
			if job.ack != nil {
				close(job.ack)
				continue
			}
			g.write(job)
		}
	}
}

// write appends the entry for one refusal: guardrail.refused for the
// service, with the id of the pattern that matched (never the matched text),
// the action taken and the gateway key that asked, if one did. An answer a
// pattern matched carries direction "answer"; when it was delivered (flag)
// the entry is guardrail.flagged with result "ok" instead. The actor is
// empty: the system refused. The audit logger keeps one row per service,
// pattern, action and gateway key per hour, so a client that keeps tripping
// a guardrail cannot grow the hash chain at will. Best-effort: an error or a
// panic is logged and the worker goes on.
func (g *guardrailAuditor) write(job guardrailRefusal) {
	defer func() {
		if rec := recover(); rec != nil {
			g.log.Error("guardrail audit: write panicked", "service_id", job.service, "panic", rec)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), guardrailAuditTimeout)
	defer cancel()
	if job.judge != nil {
		g.writeJudge(ctx, job)
		return
	}
	payload := map[string]string{"pattern": job.pattern, "action": job.action}
	if job.gatewayKey != "" {
		payload["gateway_key_id"] = job.gatewayKey
	}
	action, result := audit.ActionGuardrailRefused, "denied"
	if job.direction != "" {
		payload["direction"] = job.direction
		if job.action == guardrails.AnswerActionFlag {
			action, result = audit.ActionGuardrailFlagged, "ok" // the answer was delivered
		}
	}
	err := g.audit.Append(ctx, audit.Event{
		Action:         action,
		SubjectID:      job.service,
		SubjectLabel:   job.pattern,
		Result:         result,
		Payload:        audit.MustJSON(payload),
		AggregationKey: job.service + "\x00" + job.pattern + "\x00" + job.action + "\x00" + job.gatewayKey + "\x00" + job.direction,
	})
	if err != nil {
		g.log.Warn("guardrail audit: write failed", "service_id", job.service, "err", err)
	}
}

// writeJudge appends the entry for one judgement of the judge model. A block
// is guardrail.refused with tier "judge", the direction and the judge's
// reason (the model's words about the content, never the content). A failure
// is guardrail.judge_failed with the class as its reason and what
// on_failure did; its result is "denied" when the request was refused or
// the answer withheld, "ok" when it went through. Both are kept to one entry
// per service, kind, direction and gateway key per hour (for a failure also
// per class), like the pattern entries: the reason of later blocks in that
// hour is in the inspector, not here.
func (g *guardrailAuditor) writeJudge(ctx context.Context, job guardrailRefusal) {
	e := *job.judge
	var ev audit.Event
	if e.Verdict == aigw.JudgeBlock {
		payload := map[string]string{"tier": "judge", "direction": e.Direction, "action": "block", "reason": e.Reason}
		if job.gatewayKey != "" {
			payload["gateway_key_id"] = job.gatewayKey
		}
		ev = audit.Event{
			Action: audit.ActionGuardrailRefused, SubjectID: job.service, SubjectLabel: "judge", Result: "denied",
			Payload:        audit.MustJSON(payload),
			AggregationKey: job.service + "\x00judge\x00" + e.Direction + "\x00" + job.gatewayKey,
		}
	} else {
		onFailure, result := guardrails.JudgeOnFailureAllow, "ok"
		if e.Refused {
			onFailure, result = guardrails.JudgeOnFailureRefuse, "denied"
		}
		payload := map[string]string{"reason": e.Failure, "direction": e.Direction, "on_failure": onFailure}
		if job.gatewayKey != "" {
			payload["gateway_key_id"] = job.gatewayKey
		}
		ev = audit.Event{
			Action: audit.ActionGuardrailJudgeFailed, SubjectID: job.service, SubjectLabel: e.Failure, Result: result,
			Payload:        audit.MustJSON(payload),
			AggregationKey: job.service + "\x00" + e.Failure + "\x00" + e.Direction + "\x00" + onFailure + "\x00" + job.gatewayKey,
		}
	}
	if err := g.audit.Append(ctx, ev); err != nil {
		g.log.Warn("guardrail audit: write failed", "service_id", job.service, "err", err)
	}
}

// Flush waits until the refusals queued so far are written, or ctx is done.
// Called at shutdown, before the database closes.
func (g *guardrailAuditor) Flush(ctx context.Context) error {
	if g == nil {
		return nil
	}
	ack := make(chan struct{})
	select {
	case g.queue <- guardrailRefusal{ack: ack}:
	case <-ctx.Done():
		return ctx.Err()
	case <-g.quit:
		return nil
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-g.quit:
		return nil
	}
}

// stop ends the worker without draining: after Flush at shutdown, and in the
// cleanup of a test that built one.
func (g *guardrailAuditor) stop() {
	if g != nil {
		g.once.Do(func() { close(g.quit) })
	}
}
