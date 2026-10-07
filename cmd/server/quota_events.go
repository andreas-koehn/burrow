// quota_events.go — announces what the AI data plane refused: a
// "quota.exceeded" webhook event when a rate limit or day quota denies a
// request, and a "guardrail.refused" audit entry when a guardrail refuses one.
// Both only report a decision that was already taken.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
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

// eventThrottle lets one event per key through per interval.
type eventThrottle struct {
	mu    sync.Mutex
	last  map[string]time.Time
	now   func() time.Time
	every time.Duration
}

// allow reports whether key may publish now, and if so starts its interval.
// When the map is full, entries whose interval is over are dropped; while it
// is full of live entries a new key is refused, so the map never grows past
// eventThrottleMaxKeys.
func (t *eventThrottle) allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if last, ok := t.last[key]; ok {
		if now.Sub(last) < t.every {
			return false
		}
		t.last[key] = now
		return true
	}
	if len(t.last) >= eventThrottleMaxKeys {
		for k, at := range t.last {
			if now.Sub(at) >= t.every {
				delete(t.last, k)
			}
		}
		if len(t.last) >= eventThrottleMaxKeys {
			return false
		}
	}
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	t.last[key] = now
	return true
}

func (t *eventThrottle) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.last)
}

// quotaEvents publishes "quota.exceeded" for denied requests: one event per
// (scope, subject, dimension) per minute, however many requests the limit
// refuses in that minute.
type quotaEvents struct {
	pub      eventPublisher
	throttle *eventThrottle
}

// newQuotaEvents returns the publisher for the quota middleware. pub may be
// nil (nothing is published); now may be nil (time.Now).
func newQuotaEvents(pub eventPublisher, now func() time.Time) *quotaEvents {
	if now == nil {
		now = time.Now
	}
	return &quotaEvents{pub: pub, throttle: &eventThrottle{now: now, every: quotaEventInterval}}
}

// denied announces a denial. The payload describes the limit that denied and
// the service the request was for: ids and numbers, nothing of the request.
func (q *quotaEvents) denied(ctx context.Context, who quota.Subjects, dec quota.Decision) {
	if q == nil || q.pub == nil || dec.Allow {
		return
	}
	if !q.throttle.allow(dec.LimitingScope + "\x00" + dec.LimitingSubject + "\x00" + dec.Dimension) {
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

// auditAppender is *audit.Logger as the guardrail hook needs it.
type auditAppender interface {
	Append(ctx context.Context, e audit.Event) error
}

// guardrailAuditTimeout bounds the audit write of a refusal.
const guardrailAuditTimeout = 5 * time.Second

// guardrailRefusalAudit returns the chain's OnGuardrailRefuse hook: it
// appends a guardrail.refused audit entry for the service, with the id of the
// pattern that matched (never the matched text) and the action taken. The
// actor is empty: the system refused. The write is best-effort and runs
// detached from the request, so a client that hangs up does not lose it.
// audit.Logger keeps this action to one row per service per hour.
func guardrailRefusalAudit(l auditAppender) func(ctx context.Context, serviceID, pattern, action string) {
	return func(ctx context.Context, serviceID, pattern, action string) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), guardrailAuditTimeout)
		defer cancel()
		_ = l.Append(ctx, audit.Event{
			Action:       audit.ActionGuardrailRefused,
			SubjectID:    serviceID,
			SubjectLabel: pattern,
			Result:       "denied",
			Payload:      audit.MustJSON(map[string]string{"pattern": pattern, "action": action}),
		})
	}
}
