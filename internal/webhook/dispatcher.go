// Package webhook is Burrow's outbound HMAC webhook delivery layer
// (spec Part H). One process-wide *Dispatcher accepts Publish(event,
// payload) calls, fans them out to every configured webhook that
// subscribes to the event, signs each request with HMAC-SHA256(secret,
// timestamp + "." + body) (GitHub convention), and retries non-2xx /
// network errors three times at 1s/5s/30s. Three consecutive failures
// flip the webhook to a "Failing" status (UI tag); ten consecutive 4xx
// in one hour auto-pause the webhook and emit a single
// "webhook.delivery.failed" audit event.
//
// The dispatcher is intentionally in-process: one buffered chan + one
// worker goroutine. Overflow drops with slog.Warn rather than blocking
// the publisher. Per-attempt deliveries are persisted to
// webhook_deliveries so the JSON API can show the history.
//
// A relay that stops does not wait on a receiver: Close gives what is in
// flight and queued closeBudget as a whole, then cancels the rest.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	wtemplate "github.com/ankoehn/burrow/internal/webhook/template"
)

// DefaultRetryBackoff is the spec-mandated retry schedule (Part H.2):
// three attempts at 1s, 5s, 30s. The first attempt is immediate (no
// initial wait); these are the waits between attempts.
var DefaultRetryBackoff = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	30 * time.Second,
}

// FailingThreshold is the consecutive-failure count after which a webhook
// is tagged "Failing" in the UI (spec Part H.2). The dispatcher only
// writes the consecutive_failures counter to webhooks; the JSON handler
// derives the "Failing" tag from the counter.
const FailingThreshold = 3

// AutoPauseThreshold is the count of consecutive 4xx within
// AutoPauseWindow that auto-pauses the webhook (spec Part H.2).
const AutoPauseThreshold = 10

// AutoPauseWindow is the rolling window the auto-pause heuristic
// inspects (spec Part H.2 — 10 consecutive 4xx in 1h).
const AutoPauseWindow = time.Hour

// PreviewCap caps the request / response body preview written to
// webhook_deliveries.request_preview / response_preview. The actual HTTP
// body sent to the remote endpoint is NEVER truncated — this is only the
// stored audit artifact.
const PreviewCap = 4 * 1024 // 4 KiB

// queueCapacity is the buffered-chan capacity (spec architecture note).
// 256 is large enough that a brief blip in the remote endpoint (one slow
// 30s retry chain) cannot back the publisher up under normal load.
const queueCapacity = 256

// closeBudget is how long Close lets the worker go on delivering: the
// delivery in flight and every event still queued share it. A relay has to
// stop promptly whatever a receiver does, and one that does not answer would
// otherwise hold each delivery for the HTTP client's 30 seconds. Events the
// budget does not reach are dropped and counted in one log line.
const closeBudget = 5 * time.Second

// ClosedEvents is the spec-defined closed event vocabulary (Part H.3).
// Publish silently no-ops any event outside this set (defense in depth:
// callers should pass a constant from this file, never a free-form
// string), and the JSON handler validates POST /webhooks bodies against
// the same list.
var ClosedEvents = []string{
	"webhook.test",
	"tunnel.connected",
	"tunnel.disconnected",
	"tunnel.failed",
	"access.denied",
	"quota.exceeded",
	"budget.exceeded",
	"redaction.applied",
	"guardrail.refused",
	"cert.expiring",
	"audit.exported",
	"backup.completed",
	// v0.5.0 Task 10: new event vocabulary.
	"ai.upstream_error",
	"ai.cache_promotion",
	"audit.policy_change",
	"service.created",
	"service.deleted",
	"connection.session_summary",
	// v0.5.2 Task 10: tick-time edge fire on active -> cert_expiring transition.
	// Distinct from the existing PUT-time "cert.expiring" event (fired when an
	// admin uploads a cert already inside the 14-day window); subscribers can
	// pick whichever trigger path they care about.
	"custom_domain.cert.expiring",
}

// IsClosedEvent reports whether s is one of the spec-closed event names.
func IsClosedEvent(s string) bool {
	for _, e := range ClosedEvents {
		if e == s {
			return true
		}
	}
	return false
}

// Store is the narrow DB surface the dispatcher needs. *db.DB satisfies
// it directly; tests provide an in-memory fake.
type Store interface {
	ListWebhooks(ctx context.Context) ([]db.Webhook, error)
	GetWebhook(ctx context.Context, id string) (db.Webhook, error)
	InsertWebhookDelivery(ctx context.Context, d db.WebhookDelivery) error
	SetWebhookFailureCounters(ctx context.Context, id string, count int, firstFailureAt *time.Time) error
	SetWebhookPaused(ctx context.Context, id string, paused bool) error
	CountConsecutive4xxSince(ctx context.Context, webhookID string, since time.Time) (int, error)
}

// SecretLookup is the narrow seam for retrieving the plaintext signing
// secret for a webhook. Production wires this to an in-memory cache of
// plaintexts populated at POST time (the DB only stores the hash). For
// tests the helper is set inline.
//
// The dispatcher only needs the plaintext at delivery time; it never
// reads, logs, or persists it.
type SecretLookup interface {
	Plaintext(webhookID string) (string, bool)
}

// AuditLogger is the narrow surface used to emit the single
// webhook.delivery.failed event on auto-pause. *audit.Logger satisfies
// it directly. nil disables auditing (safe for tests + early wiring).
type AuditLogger interface {
	Append(ctx context.Context, e audit.Event) error
}

// Dispatcher is the singleton outbound HMAC delivery engine. Start
// launches the worker goroutine; Close shuts it down (no more events are
// accepted, the worker delivers what is queued within closeBudget).
type Dispatcher struct {
	store   Store
	secrets SecretLookup
	auditor AuditLogger
	log     *slog.Logger
	hc      *http.Client

	mu      sync.RWMutex
	backoff []time.Duration

	// queue is never closed: Publish may race with Close, and a send on a
	// closed channel panics. stopped (under mu) is what refuses new events.
	queue   chan job
	wg      sync.WaitGroup
	once    sync.Once
	stop    chan struct{}
	stopped bool

	// ctx is the context of everything the worker does: each HTTP request
	// and each write of a delivery row. Close cancels it when closeBudget is
	// spent, which ends a request in flight.
	ctx         context.Context
	cancel      context.CancelFunc
	closeBudget time.Duration
	// cutOff counts the events the worker took and could not finish because
	// Close's budget ran out.
	cutOff atomic.Int64
}

// job is one Publish unit-of-work pushed onto the queue.
type job struct {
	event   string
	payload any
}

// New constructs a Dispatcher with the spec-default retry backoff. Call
// Start to launch the worker. The returned Dispatcher is the type that
// satisfies the cost.Dispatcher interface (cost engine consumes the same
// Publish signature).
func New(store Store, secrets SecretLookup, auditor AuditLogger, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	bo := make([]time.Duration, len(DefaultRetryBackoff))
	copy(bo, DefaultRetryBackoff)
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		store:       store,
		secrets:     secrets,
		auditor:     auditor,
		log:         log,
		hc:          &http.Client{Timeout: 30 * time.Second},
		backoff:     bo,
		queue:       make(chan job, queueCapacity),
		stop:        make(chan struct{}),
		ctx:         ctx,
		cancel:      cancel,
		closeBudget: closeBudget,
	}
}

// SetHTTPClient overrides the http.Client (tests inject httptest
// servers via the default client; this seam exists for production
// timeouts and TLS config when needed).
func (d *Dispatcher) SetHTTPClient(c *http.Client) {
	if c == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hc = c
}

// SetRetryBackoff overrides the retry schedule for tests. A zero-length
// slice means a single attempt with no retries.
func (d *Dispatcher) SetRetryBackoff(b []time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	bo := make([]time.Duration, len(b))
	copy(bo, b)
	d.backoff = bo
}

// Start launches the single worker goroutine. Safe to call once;
// subsequent calls are no-ops.
func (d *Dispatcher) Start() {
	d.once.Do(func() {
		d.wg.Add(1)
		go d.workerLoop()
	})
}

// Close stops accepting events and returns within closeBudget: the worker
// goes on delivering what is in flight and queued, each event tried once (a
// wait for a retry ends here), until the queue is empty or the budget is
// spent. Then the request in flight is cancelled through its context, the
// rest of the queue is dropped, and one line logs how many events were not
// delivered. No delivery runs after Close has returned. Safe to call
// multiple times; a call after the first returns at once.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.stopped = true
	close(d.stop)
	budget := d.closeBudget
	d.mu.Unlock()

	timer := time.AfterFunc(budget, d.cancel)
	d.wg.Wait()
	timer.Stop()
	d.cancel()

	// What the worker was cut off in, plus what it never took. Nothing is
	// added to the queue any more (stopped), so this count is final.
	lost := d.cutOff.Load()
	for {
		select {
		case <-d.queue:
			lost++
			continue
		default:
		}
		break
	}
	if lost > 0 {
		d.log.Warn("webhook: events not delivered at shutdown",
			"count", lost, "budget", budget.String())
	}
}

// Compile-time check that *Dispatcher satisfies the cost.Dispatcher
// interface shape (Publish(ctx, event, payload)). We mirror the
// signature here rather than importing the cost package (which would
// create an internal-package cycle in tests). Any drift in the cost
// engine's expected shape will be caught by Task 25 wiring and by
// integration tests in cmd/server.
var _ interface {
	Publish(ctx context.Context, event string, payload any)
} = (*Dispatcher)(nil)

// Publish enqueues an event for delivery. It is fire-and-forget: the
// dispatcher fans out to every matching webhook in the worker
// goroutine. Returns immediately. Overflow drops with slog.Warn rather
// than blocking the caller — the publisher (proxy hot path, cost engine)
// MUST NOT stall on webhook delivery.
//
// Events outside the spec closed vocabulary are dropped silently with a
// warn log; this matches the "closed event vocabulary" invariant
// (callers should pass one of the constants in ClosedEvents, never a
// free-form string).
func (d *Dispatcher) Publish(ctx context.Context, event string, payload any) {
	if d == nil {
		return
	}
	if !IsClosedEvent(event) {
		d.log.Warn("webhook: dropped non-closed event", "event", event)
		return
	}
	// The read lock orders this against Close: an event is either queued
	// before Close looks at the queue, or dropped here. The send never waits.
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.stopped {
		return
	}
	select {
	case d.queue <- job{event: event, payload: payload}:
	default:
		d.log.Warn("webhook: queue full, dropping event",
			"event", event, "capacity", queueCapacity)
	}
}

// workerLoop is the single worker goroutine that consumes the queue. When
// Close is called it delivers what is still queued and exits: when the
// queue is empty, or when Close's budget is spent.
func (d *Dispatcher) workerLoop() {
	defer d.wg.Done()
	for {
		select {
		case j := <-d.queue:
			d.handleJob(j)
		case <-d.stop:
			for d.ctx.Err() == nil {
				select {
				case j := <-d.queue:
					d.handleJob(j)
				default:
					return
				}
			}
			return
		}
	}
}

// handleJob processes one Publish: list all webhooks, dispatch to those
// that subscribe to the event, retry per attempt. Each webhook with a
// non-empty PayloadTemplate renders its own body; webhooks with an empty
// template use the shared default body.
func (d *Dispatcher) handleJob(j job) {
	ctx := d.ctx
	if ctx.Err() != nil {
		d.cutOff.Add(1) // taken after Close's budget was spent
		return
	}
	hooks, err := d.store.ListWebhooks(ctx)
	if err != nil {
		d.log.Warn("webhook: list webhooks failed", "err", err)
		return
	}
	// Default body (used when PayloadTemplate is empty).
	defaultBody, err := json.Marshal(map[string]any{
		"event": j.event,
		"data":  j.payload,
	})
	if err != nil {
		d.log.Warn("webhook: marshal payload failed",
			"event", j.event, "err", err)
		return
	}
	// Convert j.payload to a field map for template rendering. If payload is
	// already a map[string]any we use it directly; otherwise we round-trip
	// through JSON to get a flat map.
	fields := payloadToFields(j.payload)

	for _, wh := range hooks {
		if wh.Paused {
			continue
		}
		if !webhookSubscribes(wh, j.event) {
			continue
		}
		body := defaultBody
		if wh.PayloadTemplate != "" {
			rendered, _, err := wtemplate.Render(wh.PayloadTemplate, fields)
			if err != nil {
				d.log.Warn("webhook: template render failed, using default body",
					"webhook_id", wh.ID, "event", j.event, "err", err)
			} else {
				body = rendered
			}
		}
		// Close's budget ran out before this webhook's turn, or during its
		// delivery: the event did not reach everyone, and the webhooks after
		// this one are not tried.
		if ctx.Err() != nil {
			d.cutOff.Add(1)
			return
		}
		if _, cut := d.deliverOne(ctx, wh, j.event, body); cut {
			d.cutOff.Add(1)
			return
		}
	}
}

// payloadToFields converts an arbitrary payload value to a map[string]any
// suitable for template.Render. map[string]any passes through unchanged;
// all other types are marshalled+unmarshalled to produce a flat map.
func payloadToFields(payload any) map[string]any {
	if payload == nil {
		return map[string]any{}
	}
	if m, ok := payload.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// DeliverNow runs the full attempt/retry loop for a single webhook and
// a single event, then returns. Used by POST /webhooks/{id}/test —
// the handler wants to deliver synchronously so the response can be
// scoped to the delivery outcome.
//
// Returns the highest attempt number that ran (1..len(backoff)+1) and
// the last status code (0 on network error). The delivery rows are
// already persisted regardless.
func (d *Dispatcher) DeliverNow(ctx context.Context, webhookID, event string, payload any) (int, int, error) {
	wh, err := d.store.GetWebhook(ctx, webhookID)
	if err != nil {
		return 0, 0, err
	}
	defaultBody, err := json.Marshal(map[string]any{
		"event": event,
		"data":  payload,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("marshal payload: %w", err)
	}
	body := defaultBody
	if wh.PayloadTemplate != "" {
		fields := payloadToFields(payload)
		if rendered, _, err := wtemplate.Render(wh.PayloadTemplate, fields); err == nil {
			body = rendered
		}
		// On render error we fall back to the default body silently.
	}
	attempts, _ := d.deliverOne(ctx, wh, event, body)
	return attempts, 0, nil
}

// deliverOne attempts to deliver one event to one webhook with the
// retry schedule. Each attempt is persisted as a webhook_deliveries
// row. Returns the number of attempts that ran. Updates the
// webhook's consecutive_failures + first_failure_at counters; on the
// 4xx-pause heuristic, calls SetWebhookPaused + emits one audit event.
//
// The second result reports that ctx ended before the receiver answered with
// a 2xx: the delivery was given up, not failed.
func (d *Dispatcher) deliverOne(ctx context.Context, wh db.Webhook, event string, body []byte) (int, bool) {
	d.mu.RLock()
	backoff := append([]time.Duration(nil), d.backoff...)
	client := d.hc
	d.mu.RUnlock()

	maxAttempts := len(backoff) + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastStatus int
	attempt := 0
	for attempt < maxAttempts {
		attempt++
		status, respPreview, latency := d.attempt(ctx, wh, event, body, client)
		if ctx.Err() != nil {
			// Cut off: by Close's budget, or the caller of DeliverNow is
			// gone. That says nothing about the receiver, so no failure is
			// counted against the webhook, and nothing can be written on
			// this context any more. An answer that arrived just before
			// counts as delivered all the same.
			return attempt, status < 200 || status >= 300
		}
		// Persist this attempt regardless of outcome.
		preview := previewBytes(body)
		_ = d.store.InsertWebhookDelivery(ctx, db.WebhookDelivery{
			ID:              newDeliveryID(),
			WebhookID:       wh.ID,
			Event:           event,
			Ts:              time.Now().UTC(),
			StatusCode:      status,
			Attempt:         attempt,
			LatencyMs:       int(latency / time.Millisecond),
			RequestPreview:  &preview,
			ResponsePreview: &respPreview,
		})
		lastStatus = status
		if status >= 200 && status < 300 {
			// Success: clear failure counters, exit retry loop.
			d.recordSuccess(ctx, wh.ID)
			return attempt, false
		}
		// Non-2xx or network error. If we have a retry left, sleep then loop.
		if attempt < maxAttempts {
			wait := backoff[attempt-1]
			select {
			case <-time.After(wait):
			case <-d.stop:
				// Shutting down — stop retrying, but record the failure first.
				d.recordFailure(ctx, wh, lastStatus)
				return attempt, false
			case <-ctx.Done():
				d.recordFailure(ctx, wh, lastStatus)
				return attempt, false
			}
		}
	}
	// All attempts exhausted without success.
	d.recordFailure(ctx, wh, lastStatus)
	return attempt, false
}

// attempt performs one HTTP POST and returns (status, response preview,
// elapsed). status is 0 for transport errors; respPreview is empty in
// that case.
func (d *Dispatcher) attempt(ctx context.Context, wh db.Webhook, event string, body []byte, client *http.Client) (int, string, time.Duration) {
	start := time.Now()
	deliveryID := newDeliveryID()
	ts := time.Now().UTC().Format(time.RFC3339)

	secret, _ := d.secrets.Plaintext(wh.ID)
	sig := computeSignature(secret, ts, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(body))
	if err != nil {
		d.log.Warn("webhook: build request failed",
			"webhook_id", wh.ID, "err", err)
		return 0, "", time.Since(start)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "burrow-webhook/1")
	req.Header.Set("Burrow-Event", event)
	req.Header.Set("Burrow-Delivery", deliveryID)
	req.Header.Set("Burrow-Timestamp", ts)
	req.Header.Set("Burrow-Signature", "sha256="+sig)

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil { // a request this side cancelled is no fault of the receiver
			d.log.Warn("webhook: HTTP error",
				"webhook_id", wh.ID, "url", wh.URL, "err", err)
		}
		return 0, "", time.Since(start)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, PreviewCap+1))
	return resp.StatusCode, previewBytes(respBody), time.Since(start)
}

// recordSuccess clears the consecutive_failures + first_failure_at
// counters on the webhook row.
func (d *Dispatcher) recordSuccess(ctx context.Context, id string) {
	if err := d.store.SetWebhookFailureCounters(ctx, id, 0, nil); err != nil {
		d.log.Warn("webhook: clear failure counters", "webhook_id", id, "err", err)
	}
}

// recordFailure increments consecutive_failures, sets first_failure_at
// on the first failure of a streak, and runs the auto-pause heuristic.
// status==0 means transport error (network); 4xx/5xx are HTTP errors —
// both are failures from the dispatcher's POV.
func (d *Dispatcher) recordFailure(ctx context.Context, wh db.Webhook, lastStatus int) {
	// Re-read so we observe any failure-counter writes from concurrent
	// deliveries of the same webhook (rare but possible: two events
	// publishing to the same hook in the same job run).
	cur, err := d.store.GetWebhook(ctx, wh.ID)
	if err != nil {
		d.log.Warn("webhook: re-read failed", "webhook_id", wh.ID, "err", err)
		cur = wh
	}
	next := cur.ConsecutiveFailures + 1
	first := cur.FirstFailureAt
	if first == nil {
		t := time.Now().UTC()
		first = &t
	}
	if err := d.store.SetWebhookFailureCounters(ctx, wh.ID, next, first); err != nil {
		d.log.Warn("webhook: set failure counters", "webhook_id", wh.ID, "err", err)
	}

	// Auto-pause: 10 consecutive 4xx in 1h.
	if lastStatus >= 400 && lastStatus < 500 {
		n, err := d.store.CountConsecutive4xxSince(ctx, wh.ID,
			time.Now().UTC().Add(-AutoPauseWindow))
		if err != nil {
			d.log.Warn("webhook: count 4xx failed", "webhook_id", wh.ID, "err", err)
			return
		}
		if n >= AutoPauseThreshold && !cur.Paused {
			if err := d.store.SetWebhookPaused(ctx, wh.ID, true); err != nil {
				d.log.Warn("webhook: auto-pause failed", "webhook_id", wh.ID, "err", err)
				return
			}
			d.emitAuditFailed(ctx, wh, lastStatus, n)
			d.log.Warn("webhook: auto-paused after 4xx streak",
				"webhook_id", wh.ID, "count", n,
				"window", AutoPauseWindow.String())
			return
		}
	}

	// 3 consecutive failures (any kind) → emit the "Failing" audit event
	// once at the transition. We use the next counter value to detect
	// the transition (only when crossing FailingThreshold for the first
	// time inside a single job run).
	if next == FailingThreshold {
		d.emitAuditFailed(ctx, wh, lastStatus, next)
	}
}

// emitAuditFailed writes one webhook.delivery.failed audit event.
// Nil-safe: when the auditor is not wired, this is a no-op log line.
func (d *Dispatcher) emitAuditFailed(ctx context.Context, wh db.Webhook, status, count int) {
	if d.auditor == nil {
		d.log.Warn("webhook: would emit audit event (no auditor wired)",
			"webhook_id", wh.ID, "name", wh.Name, "status", status, "count", count)
		return
	}
	payload := map[string]any{
		"webhook_id":           wh.ID,
		"url":                  wh.URL,
		"last_status":          status,
		"consecutive_failures": count,
	}
	raw, _ := json.Marshal(payload)
	_ = d.auditor.Append(ctx, audit.Event{
		Action:       audit.ActionWebhookDeliveryFailed,
		SubjectID:    wh.ID,
		SubjectLabel: wh.Name,
		Result:       "error",
		Payload:      raw,
	})
}

// webhookSubscribes reports whether wh.Events (a JSON-encoded array of
// event strings) contains event. A malformed Events column is treated
// as "no subscriptions" (safe default — webhook is silently inert
// rather than spamming every event).
func webhookSubscribes(wh db.Webhook, event string) bool {
	if strings.TrimSpace(wh.Events) == "" {
		return false
	}
	var events []string
	if err := json.Unmarshal([]byte(wh.Events), &events); err != nil {
		return false
	}
	for _, e := range events {
		if e == event {
			return true
		}
	}
	return false
}

// computeSignature returns the lowercase hex sha256 HMAC of
// timestamp + "." + body using the given plaintext secret. This matches
// GitHub's webhook signing convention (Spec Part H locked invariant).
func computeSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature is the inverse of computeSignature, exposed so the
// JSON handlers' tests (and any future incoming-receipt code) can
// re-validate a signature header without re-implementing the math.
// header is the full "sha256=..." string the client received.
func VerifySignature(secret, timestamp string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// previewBytes returns a string suitable for webhook_deliveries.*_preview:
// the input truncated to PreviewCap bytes, plus a "...(truncated)" marker
// when it was longer. Returns "" for nil input.
func previewBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) > PreviewCap {
		return string(b[:PreviewCap]) + "...(truncated)"
	}
	return string(b)
}

// newDeliveryID returns a fresh ulid for a webhook_deliveries.id /
// Burrow-Delivery header. We reuse audit.NewULID so every
// time-sortable id in Burrow uses the same Crockford-base32 scheme.
// On the (vanishingly rare) ulid mint failure we fall back to a
// timestamp+random hex string so the delivery row still has a unique
// id.
func newDeliveryID() string {
	if id, err := audit.NewULID(); err == nil {
		return id
	}
	return fmt.Sprintf("d-%d", time.Now().UnixNano())
}

// ---------------------------------------------------------------------------
// v0.5.0 Task 10: new event helpers + per-event rate-limiting
// ---------------------------------------------------------------------------

// TopSourceIP is one entry in the top-N source-IP list carried by
// RollupData. Up to 10 entries, sorted by session count descending (spec E.3).
type TopSourceIP struct {
	IP       string `json:"ip"`
	Sessions int    `json:"sessions"`
}

// RollupData carries the aggregated session metrics for the
// connection.session_summary event. Populated by the hourly tick in Task 17.
// TopSourceIPs holds up to 10 entries sorted by session count descending
// (spec E.3 — each entry has {ip, sessions}).
type RollupData struct {
	Sessions      int64
	BytesIn       int64
	BytesOut      int64
	AvgDurationMs int64
	P95DurationMs int64
	TopSourceIPs  []TopSourceIP
}

// emitRateLimiter is a per-event-per-serviceID in-process rate limiter.
// It records the last time each (event, serviceID) pair was emitted; callers
// that would exceed the window are silently suppressed.
//
// guarded by emitMu.
type emitRateLimiter struct {
	mu      sync.Mutex
	lastHit map[string]time.Time // key: "event:serviceID"
}

func newEmitRateLimiter() *emitRateLimiter {
	return &emitRateLimiter{lastHit: make(map[string]time.Time)}
}

// allow returns true and records the hit when the (event, key) pair
// has not fired within the given window. Returns false to suppress.
func (r *emitRateLimiter) allow(event, key string, window time.Duration) bool {
	k := event + ":" + key
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastHit[k]; ok && time.Since(last) < window {
		return false
	}
	r.lastHit[k] = time.Now()
	return true
}

// rateLimiter is the process-wide emitter rate limiter. Constructed lazily;
// callers use dispatcherRateLimiter() to access.
var (
	globalRateLimiter     *emitRateLimiter
	globalRateLimiterOnce sync.Once
)

func dispatcherRateLimiter() *emitRateLimiter {
	globalRateLimiterOnce.Do(func() { globalRateLimiter = newEmitRateLimiter() })
	return globalRateLimiter
}

// EmitAIUpstreamError publishes an "ai.upstream_error" webhook event on
// behalf of the given service. Rate-limited to 1/h per serviceID.
// Payload keys are snake_case per spec H.2/E.3.
func (d *Dispatcher) EmitAIUpstreamError(serviceID, backendServiceID string, status int, errMsg string, retryCount int) {
	const event = "ai.upstream_error"
	if !dispatcherRateLimiter().allow(event, serviceID, time.Hour) {
		return
	}
	d.Publish(context.Background(), event, map[string]any{
		"service_id":         serviceID,
		"backend_service_id": backendServiceID,
		"status":             status,
		"error":              errMsg,
		"retry_count":        retryCount,
	})
}

// EmitAuditPolicyChange publishes an "audit.policy_change" webhook event.
// Not rate-limited (admin-initiated, low-volume).
// Payload keys are snake_case per spec H.2/E.3.
func (d *Dispatcher) EmitAuditPolicyChange(actorEmail, action string, before, after any) {
	d.Publish(context.Background(), "audit.policy_change", map[string]any{
		"actor_email": actorEmail,
		"action":      action,
		"before":      before,
		"after":       after,
	})
}

// EmitServiceCreated publishes a "service.created" webhook event.
// Not rate-limited (admin-initiated, low-volume).
// Payload keys are snake_case per spec H.2/E.3; the event type field is "type"
// (not "kind") per spec H.2 table.
func (d *Dispatcher) EmitServiceCreated(serviceID, name, kind, accessMode string) {
	d.Publish(context.Background(), "service.created", map[string]any{
		"service_id":  serviceID,
		"name":        name,
		"type":        kind,
		"access_mode": accessMode,
	})
}

// EmitServiceDeleted publishes a "service.deleted" webhook event.
// Not rate-limited (admin-initiated, low-volume).
// Payload keys are snake_case per spec H.2/E.3.
func (d *Dispatcher) EmitServiceDeleted(serviceID, name string) {
	d.Publish(context.Background(), "service.deleted", map[string]any{
		"service_id": serviceID,
		"name":       name,
	})
}

// EmitConnectionSessionSummary publishes a "connection.session_summary"
// webhook event. This is the hourly-tick function; the actual tick caller
// is wired in Task 17. The function exists here so Task 17 can simply call
// it without touching the dispatcher surface.
//
// Not rate-limited at the event-helper level — the caller (Task 17's hourly
// tick) controls the cadence.
// Payload keys are snake_case per spec H.2/E.3; top_source_ips holds up to
// 10 entries sorted by session count descending.
func (d *Dispatcher) EmitConnectionSessionSummary(serviceID, kind string, windowStart, windowEnd time.Time, rollup RollupData) {
	d.Publish(context.Background(), "connection.session_summary", map[string]any{
		"service_id":      serviceID,
		"kind":            kind,
		"window_start":    windowStart.UTC().Format(time.RFC3339),
		"window_end":      windowEnd.UTC().Format(time.RFC3339),
		"sessions":        rollup.Sessions,
		"bytes_in":        rollup.BytesIn,
		"bytes_out":       rollup.BytesOut,
		"avg_duration_ms": rollup.AvgDurationMs,
		"p95_duration_ms": rollup.P95DurationMs,
		"top_source_ips":  rollup.TopSourceIPs,
	})
}

// InMemorySecrets is a tiny in-process SecretLookup backed by a
// sync.Map. cmd/server populates it on POST /webhooks (the only place
// the plaintext is generated), the dispatcher reads it on every
// attempt, and Delete is called when the webhook is removed.
//
// Restart semantics: secrets are lost on process restart — admins
// must rotate the secret (delete + recreate) on the next deploy.
// This matches the spec's "secret returned exactly once on create"
// invariant; the alternative (persisting the plaintext) would
// violate the threat model.
type InMemorySecrets struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewInMemorySecrets returns a ready-to-use plaintext cache.
func NewInMemorySecrets() *InMemorySecrets {
	return &InMemorySecrets{m: map[string]string{}}
}

// Set associates id with the plaintext secret.
func (s *InMemorySecrets) Set(id, plaintext string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = plaintext
}

// Plaintext returns the secret for id and whether one is present.
func (s *InMemorySecrets) Plaintext(id string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.m[id]
	return p, ok
}

// Delete removes the secret for id. Called when a webhook is deleted.
func (s *InMemorySecrets) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// ErrNoSecret is returned by SecretLookup adapters that wish to signal
// "no plaintext on record" with an error instead of a (string, false)
// pair. The dispatcher itself uses the bool form; this is exported so
// future adapters can stay symmetrical.
var ErrNoSecret = errors.New("webhook: no plaintext secret cached for id")
