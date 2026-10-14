package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// fakeStore is an in-memory webhook+delivery store for tests. It mirrors
// the *db.DB CRUD surface used by the dispatcher.
type fakeStore struct {
	mu         sync.Mutex
	webhooks   map[string]db.Webhook
	deliveries []db.WebhookDelivery
}

func newFakeStore() *fakeStore {
	return &fakeStore{webhooks: map[string]db.Webhook{}}
}

func (f *fakeStore) addWebhook(w db.Webhook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.webhooks[w.ID] = w
}

func (f *fakeStore) ListWebhooks(_ context.Context) ([]db.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.Webhook, 0, len(f.webhooks))
	for _, w := range f.webhooks {
		out = append(out, w)
	}
	return out, nil
}

func (f *fakeStore) GetWebhook(_ context.Context, id string) (db.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.webhooks[id]
	if !ok {
		return db.Webhook{}, db.ErrNotFound
	}
	return w, nil
}

func (f *fakeStore) InsertWebhookDelivery(_ context.Context, d db.WebhookDelivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveries = append(f.deliveries, d)
	return nil
}

func (f *fakeStore) SetWebhookFailureCounters(_ context.Context, id string, count int, firstFailureAt *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.webhooks[id]
	if !ok {
		return db.ErrNotFound
	}
	w.ConsecutiveFailures = count
	w.FirstFailureAt = firstFailureAt
	f.webhooks[id] = w
	return nil
}

func (f *fakeStore) SetWebhookPaused(_ context.Context, id string, paused bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.webhooks[id]
	if !ok {
		return db.ErrNotFound
	}
	w.Paused = paused
	f.webhooks[id] = w
	return nil
}

func (f *fakeStore) CountConsecutive4xxSince(_ context.Context, webhookID string, since time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, d := range f.deliveries {
		if d.WebhookID == webhookID && d.StatusCode >= 400 && d.StatusCode < 500 &&
			!d.Ts.Before(since) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) webhookSnapshot(id string) db.Webhook {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.webhooks[id]
}

func (f *fakeStore) deliveryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deliveries)
}

// fakeSecrets implements SecretLookup with one hardcoded entry.
type fakeSecrets struct{ id, plaintext string }

func (f *fakeSecrets) Plaintext(id string) (string, bool) {
	if id == f.id {
		return f.plaintext, true
	}
	return "", false
}

// fakeAuditor records every Append call.
type fakeAuditor struct {
	mu     sync.Mutex
	events []audit.Event
}

func (f *fakeAuditor) Append(_ context.Context, e audit.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakeAuditor) snapshot() []audit.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.Event, len(f.events))
	copy(out, f.events)
	return out
}

// TestComputeSignature_GitHubConvention is a known-vector test: secret
// "shh", timestamp "2026-05-20T00:00:00Z", body `{"event":"webhook.test"}`.
// We compute the HMAC by hand and assert the dispatcher's helper agrees.
func TestComputeSignature_GitHubConvention(t *testing.T) {
	secret := "shh"
	ts := "2026-05-20T00:00:00Z"
	body := []byte(`{"event":"webhook.test"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	if got := computeSignature(secret, ts, body); got != want {
		t.Fatalf("computeSignature mismatch:\ngot  %s\nwant %s", got, want)
	}
	if !VerifySignature(secret, ts, body, "sha256="+want) {
		t.Fatal("VerifySignature must accept its own output")
	}
	if VerifySignature("WRONG", ts, body, "sha256="+want) {
		t.Fatal("VerifySignature must reject a wrong secret")
	}
	if VerifySignature(secret, ts, body, "md5="+want) {
		t.Fatal("VerifySignature must reject a non-sha256 algorithm prefix")
	}
}

// TestPublish_DeliversWithCorrectHeadersAndSignature stands up an httptest
// server, publishes one event, and asserts the receiver got the spec
// headers + a signature it can re-verify.
func TestPublish_DeliversWithCorrectHeadersAndSignature(t *testing.T) {
	const secret = "test-secret-1"
	got := make(chan *http.Request, 1)
	gotBody := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody <- body
		got <- r
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID: "wh1", Name: "test-hook", URL: srv.URL,
		SecretHash: "sha-of-secret", Events: `["tunnel.connected"]`,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: secret}, nil, nil)
	d.Start()
	defer d.Close()

	d.Publish(context.Background(), "tunnel.connected", map[string]string{"id": "t1"})

	select {
	case r := <-got:
		body := <-gotBody
		// Header presence.
		if r.Header.Get("Burrow-Event") != "tunnel.connected" {
			t.Errorf("Burrow-Event = %q", r.Header.Get("Burrow-Event"))
		}
		if r.Header.Get("Burrow-Delivery") == "" {
			t.Error("Burrow-Delivery header missing")
		}
		ts := r.Header.Get("Burrow-Timestamp")
		if ts == "" {
			t.Error("Burrow-Timestamp header missing")
		}
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("Burrow-Timestamp not RFC3339: %v", err)
		}
		sig := r.Header.Get("Burrow-Signature")
		if !strings.HasPrefix(sig, "sha256=") {
			t.Errorf("Burrow-Signature missing sha256= prefix: %q", sig)
		}
		// Re-compute HMAC over (timestamp + "." + body) with the same secret.
		if !VerifySignature(secret, ts, body, sig) {
			t.Errorf("signature failed re-verification (secret=%q ts=%q body=%q sig=%q)",
				secret, ts, string(body), sig)
		}
		// Body shape: {"event":"...","data":...}
		var env map[string]any
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if env["event"] != "tunnel.connected" {
			t.Errorf("body.event = %v", env["event"])
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive delivery within 2s")
	}

	// Wait for the success post-processing (counter clear) to land.
	waitFor(t, 2*time.Second, func() bool {
		return store.webhookSnapshot("wh1").ConsecutiveFailures == 0 &&
			store.deliveryCount() == 1
	})
	if got := store.deliveryCount(); got != 1 {
		t.Errorf("delivery rows = %d want 1", got)
	}
}

// TestPublish_RetriesThreeTimesOnNon2xx confirms the spec retry schedule:
// three attempts on a server that always returns 500. We override the
// backoff to tiny waits so the test runs fast.
func TestPublish_RetriesThreeTimesOnNon2xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID: "wh1", Name: "hook", URL: srv.URL,
		Events: `["tunnel.connected"]`,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	d.SetRetryBackoff([]time.Duration{
		5 * time.Millisecond,
		10 * time.Millisecond,
	})
	d.Start()
	defer d.Close()

	d.Publish(context.Background(), "tunnel.connected", map[string]string{})

	// 3 attempts (initial + 2 retries) must hit the server.
	waitFor(t, 2*time.Second, func() bool {
		return atomic.LoadInt32(&hits) == 3 && store.deliveryCount() == 3
	})
	if h := atomic.LoadInt32(&hits); h != 3 {
		t.Fatalf("hits = %d want 3", h)
	}
	if c := store.deliveryCount(); c != 3 {
		t.Fatalf("delivery rows = %d want 3", c)
	}
	// All rows should be 500 and attempt numbers should be 1,2,3.
	for _, dl := range store.deliveries {
		if dl.StatusCode != 500 {
			t.Errorf("attempt %d status = %d want 500", dl.Attempt, dl.StatusCode)
		}
	}
	// One Publish that exhausted retries = one consecutive-failure tick.
	// The "Failing" UI threshold (3) is crossed on the third such
	// Publish in a row — covered by TestFailingStreak below.
	if n := store.webhookSnapshot("wh1").ConsecutiveFailures; n != 1 {
		t.Errorf("ConsecutiveFailures = %d want 1 (one Publish, one tick)", n)
	}
}

// TestFailingStreak_EmitsAuditAtThreshold asserts that three consecutive
// Publish failures (each Publish exhausts its retries) advance the
// consecutive_failures counter to 3 ("Failing" UI threshold) and emit
// exactly one webhook.delivery.failed audit event at the transition.
// All failures here are 5xx, so the row stays unpaused (auto-pause is
// 4xx-only — covered by TestPublish_AutoPauseOn10Consecutive4xx).
func TestFailingStreak_EmitsAuditAtThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 5xx: failure, no auto-pause
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID: "wh1", Name: "hook", URL: srv.URL,
		Events: `["tunnel.connected"]`,
	})
	auditor := &fakeAuditor{}
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, auditor, nil)
	// Single attempt per Publish so the test is fast and the streak is
	// easy to count.
	d.SetRetryBackoff(nil)
	d.Start()
	defer d.Close()

	// Three Publish calls in sequence; each is one failed delivery, so
	// the counter advances 1 → 2 → 3.
	for i := 0; i < 3; i++ {
		d.Publish(context.Background(), "tunnel.connected", nil)
		waitFor(t, 2*time.Second, func() bool {
			return store.webhookSnapshot("wh1").ConsecutiveFailures == i+1
		})
	}

	wh := store.webhookSnapshot("wh1")
	if wh.ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures = %d want 3", wh.ConsecutiveFailures)
	}
	if wh.Paused {
		t.Error("5xx-only streak must NOT auto-pause (auto-pause is for 4xx)")
	}

	// Exactly one webhook.delivery.failed audit event at the
	// 3-failure transition.
	failed := 0
	for _, e := range auditor.snapshot() {
		if e.Action == audit.ActionWebhookDeliveryFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("webhook.delivery.failed audit events = %d want 1", failed)
	}
}

// TestPublish_AutoPauseOn10Consecutive4xx asserts the 4xx-in-1h
// auto-pause heuristic. We pre-seed 9 historic 4xx deliveries in the
// store; one more 4xx triggers the pause + audit event.
func TestPublish_AutoPauseOn10Consecutive4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 400
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID: "wh1", Name: "hook", URL: srv.URL,
		Events: `["tunnel.connected"]`,
	})
	// Pre-seed 9 historic 4xx rows within the last hour.
	now := time.Now().UTC()
	for i := 0; i < 9; i++ {
		_ = store.InsertWebhookDelivery(context.Background(), db.WebhookDelivery{
			ID: "seed-" + string(rune('a'+i)), WebhookID: "wh1",
			Event: "tunnel.connected", Ts: now.Add(-time.Duration(i) * time.Minute),
			StatusCode: 404, Attempt: 1, LatencyMs: 1,
		})
	}

	auditor := &fakeAuditor{}
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, auditor, nil)
	// Single attempt (no retries) so each Publish produces exactly one
	// new delivery row in the count.
	d.SetRetryBackoff(nil)
	d.Start()
	defer d.Close()

	// Publish one event: the new 400 makes 10 consecutive 4xx in 1h →
	// auto-pause + one audit event.
	d.Publish(context.Background(), "tunnel.connected", nil)

	waitFor(t, 2*time.Second, func() bool {
		return store.webhookSnapshot("wh1").Paused
	})
	if !store.webhookSnapshot("wh1").Paused {
		t.Fatal("webhook must be auto-paused after 10 consecutive 4xx in 1h")
	}
	failed := 0
	for _, e := range auditor.snapshot() {
		if e.Action == audit.ActionWebhookDeliveryFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("webhook.delivery.failed audit events = %d want 1", failed)
	}
}

// TestPublish_NoEventMatch_NoDelivery verifies the events filter: a
// webhook subscribed only to "tunnel.failed" must not receive
// "tunnel.connected".
func TestPublish_NoEventMatch_NoDelivery(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID: "wh1", Name: "hook", URL: srv.URL,
		Events: `["tunnel.failed"]`,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	d.Start()
	defer d.Close()

	d.Publish(context.Background(), "tunnel.connected", nil)
	// Give the worker a beat to (not) deliver.
	time.Sleep(50 * time.Millisecond)
	if h := atomic.LoadInt32(&hits); h != 0 {
		t.Fatalf("non-subscribed webhook received delivery: hits=%d", h)
	}
	if c := store.deliveryCount(); c != 0 {
		t.Fatalf("delivery rows = %d want 0 (non-subscribed)", c)
	}
}

// TestPublish_PausedSkipsDelivery verifies that a paused=true webhook is
// silently skipped.
func TestPublish_PausedSkipsDelivery(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID: "wh1", Name: "hook", URL: srv.URL,
		Events: `["tunnel.connected"]`, Paused: true,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	d.Start()
	defer d.Close()

	d.Publish(context.Background(), "tunnel.connected", nil)
	time.Sleep(50 * time.Millisecond)
	if h := atomic.LoadInt32(&hits); h != 0 {
		t.Fatalf("paused webhook received delivery: hits=%d", h)
	}
}

// TestPublish_DropsNonClosedEvent verifies that an off-vocabulary event
// name is silently dropped without dispatch.
func TestPublish_DropsNonClosedEvent(t *testing.T) {
	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID: "wh1", Name: "hook", URL: "http://localhost:0",
		Events: `["tunnel.connected"]`,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	d.Start()
	defer d.Close()
	d.Publish(context.Background(), "not.a.real.event", nil)
	time.Sleep(50 * time.Millisecond)
	if c := store.deliveryCount(); c != 0 {
		t.Fatalf("non-closed event produced %d deliveries", c)
	}
}

// TestPublish_RecoversFromNetworkError verifies that a transport error
// (status=0) is recorded as a failure and counted toward the streak,
// and that a subsequent 2xx clears it.
func TestPublish_RecoversFromNetworkError(t *testing.T) {
	// Stand up a server that we close immediately so the URL is
	// unroutable, then later swap in a 200-server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID: "wh1", Name: "hook",
		URL:    "http://127.0.0.1:1", // guaranteed-unreachable port
		Events: `["tunnel.connected"]`,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	d.SetRetryBackoff([]time.Duration{
		5 * time.Millisecond,
		5 * time.Millisecond,
	})
	d.Start()
	defer d.Close()

	d.Publish(context.Background(), "tunnel.connected", nil)
	waitFor(t, 5*time.Second, func() bool {
		return store.deliveryCount() == 3
	})
	// All three attempts should be recorded with status=0 (network err).
	for _, dl := range store.deliveries {
		if dl.StatusCode != 0 {
			t.Errorf("attempt %d status=%d want 0 (network err)", dl.Attempt, dl.StatusCode)
		}
	}
	// One Publish exhausted retries → one consecutive-failure tick.
	if n := store.webhookSnapshot("wh1").ConsecutiveFailures; n != 1 {
		t.Errorf("ConsecutiveFailures = %d want 1", n)
	}
}

// TestPublish_PreviewTruncation asserts that bodies > PreviewCap are
// truncated in webhook_deliveries.request_preview, but the actual HTTP
// body sent is not truncated.
func TestPublish_PreviewTruncation(t *testing.T) {
	var bodyLen int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		atomic.StoreInt32(&bodyLen, int32(len(b)))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID: "wh1", Name: "hook", URL: srv.URL,
		Events: `["webhook.test"]`,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	d.Start()
	defer d.Close()

	// Build a payload that, once wrapped in {"event":..."data":...},
	// exceeds PreviewCap.
	big := strings.Repeat("a", PreviewCap*2)
	d.Publish(context.Background(), "webhook.test", map[string]string{"big": big})

	waitFor(t, 2*time.Second, func() bool { return store.deliveryCount() == 1 })

	if atomic.LoadInt32(&bodyLen) <= int32(PreviewCap) {
		t.Fatalf("HTTP body sent was %d bytes, want > %d (full body must NOT be truncated)",
			bodyLen, PreviewCap)
	}
	row := store.deliveries[0]
	if row.RequestPreview == nil {
		t.Fatal("RequestPreview must be set")
	}
	if !strings.HasSuffix(*row.RequestPreview, "...(truncated)") {
		t.Errorf("RequestPreview not marked truncated: %q",
			(*row.RequestPreview)[len(*row.RequestPreview)-20:])
	}
}

// TestDeliverNow_DeliversSynchronously verifies the synchronous DeliverNow
// path used by POST /webhooks/{id}/test.
func TestDeliverNow_DeliversSynchronously(t *testing.T) {
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Burrow-Event") == "webhook.test" {
			seen.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID: "wh1", Name: "hook", URL: srv.URL,
		Events: `["webhook.test"]`,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	// No Start: DeliverNow runs in the caller's goroutine.

	attempts, _, err := d.DeliverNow(context.Background(), "wh1", "webhook.test",
		map[string]string{"hi": "there"})
	if err != nil {
		t.Fatalf("DeliverNow: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts=%d want 1 (server returned 200)", attempts)
	}
	if seen.Load() != 1 {
		t.Fatalf("server did not receive the test event")
	}
	if c := store.deliveryCount(); c != 1 {
		t.Errorf("delivery row count = %d want 1", c)
	}
}

// TestQueueOverflow_DropsWithoutBlocking publishes more than queueCapacity
// items with no worker started; Publish must not block.
func TestQueueOverflow_DropsWithoutBlocking(t *testing.T) {
	store := newFakeStore()
	d := New(store, &fakeSecrets{}, nil, nil)
	// Do NOT Start; the queue will fill and then overflow.

	done := make(chan struct{})
	go func() {
		for i := 0; i < queueCapacity*2; i++ {
			d.Publish(context.Background(), "webhook.test", nil)
		}
		close(done)
	}()
	select {
	case <-done:
		// pass
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on full queue (must drop, not block)")
	}
}

// TestIsClosedEvent covers the closed vocabulary check.
func TestIsClosedEvent(t *testing.T) {
	if !IsClosedEvent("webhook.test") {
		t.Error("webhook.test must be closed-vocab")
	}
	if !IsClosedEvent("budget.exceeded") {
		t.Error("budget.exceeded must be closed-vocab")
	}
	if IsClosedEvent("definitely.not.real") {
		t.Error("free-form event must not pass IsClosedEvent")
	}
	// v0.5.0 Task 10 extended the vocabulary from 12 → 18; v0.5.2 Task 10
	// added custom_domain.cert.expiring → 19.
	if len(ClosedEvents) != 19 {
		t.Errorf("ClosedEvents length = %d want 19 (12 original + 6 v0.5.0 + 1 v0.5.2)",
			len(ClosedEvents))
	}
	if !IsClosedEvent("custom_domain.cert.expiring") {
		t.Error("custom_domain.cert.expiring must be in ClosedEvents (v0.5.2 Task 10)")
	}
	// New events must all be in the closed set.
	for _, ev := range []string{
		"ai.upstream_error",
		"ai.cache_promotion",
		"audit.policy_change",
		"service.created",
		"service.deleted",
		"connection.session_summary",
	} {
		if !IsClosedEvent(ev) {
			t.Errorf("new v0.5.0 event %q must be in ClosedEvents", ev)
		}
	}
}

// TestDispatcherFiresAiUpstreamErrorEvent confirms that EmitAIUpstreamError
// delivers a POST to a webhook subscribed to "ai.upstream_error".
func TestDispatcherFiresAiUpstreamErrorEvent(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID:     "wh1",
		Name:   "ai-errors",
		URL:    srv.URL,
		Events: `["ai.upstream_error"]`,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	d.SetRetryBackoff(nil)
	d.Start()
	defer d.Close()

	svcID := fmt.Sprintf("svc-fires-%d", time.Now().UnixNano())
	d.EmitAIUpstreamError(svcID, "be-1", 502, "upstream timeout", 3)

	select {
	case body := <-got:
		var env map[string]any
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if env["event"] != "ai.upstream_error" {
			t.Errorf("body.event = %v want ai.upstream_error", env["event"])
		}
		data, ok := env["data"].(map[string]any)
		if !ok {
			t.Fatalf("body.data not a map: %T", env["data"])
		}
		// Keys must be snake_case per spec H.2/E.3.
		if data["service_id"] != svcID {
			t.Errorf("service_id = %v want %s", data["service_id"], svcID)
		}
		if data["backend_service_id"] != "be-1" {
			t.Errorf("backend_service_id = %v want be-1", data["backend_service_id"])
		}
		if rc, _ := data["retry_count"].(float64); rc != 3 {
			t.Errorf("retry_count = %v want 3", data["retry_count"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery within 2s")
	}
}

// TestDispatcherRateLimit_AIUpstreamError confirms that the second
// EmitAIUpstreamError within the hour window is silently suppressed.
func TestDispatcherRateLimit_AIUpstreamError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID:     "wh1",
		Name:   "ai-errors",
		URL:    srv.URL,
		Events: `["ai.upstream_error"]`,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	d.SetRetryBackoff(nil)
	d.Start()
	defer d.Close()

	// The rate limiter is a process-wide singleton (one event per service and
	// hour): a service id of its own keeps -count=N runs apart.
	svcID := fmt.Sprintf("rl-test-svc-%d", time.Now().UnixNano())
	d.EmitAIUpstreamError(svcID, "be", 502, "err", 0)
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&hits) >= 1 })
	d.EmitAIUpstreamError(svcID, "be", 502, "err", 1) // within 1h window → suppressed
	time.Sleep(50 * time.Millisecond)
	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Errorf("hits=%d want 1 (second emit should be rate-limited)", h)
	}
}

// TestDispatcherPayloadTemplate confirms that a webhook with a non-empty
// PayloadTemplate receives the rendered body instead of the default body.
func TestDispatcherPayloadTemplate(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- body
		w.WriteHeader(200)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID:              "wh1",
		Name:            "templated",
		URL:             srv.URL,
		Events:          `["service.created"]`,
		PayloadTemplate: `svc={{.service_id}},name={{.name}}`,
	})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	d.SetRetryBackoff(nil)
	d.Start()
	defer d.Close()

	d.EmitServiceCreated("svc-42", "my-service", "http", "open")

	select {
	case body := <-got:
		s := string(body)
		if s != "svc=svc-42,name=my-service" {
			t.Errorf("rendered body = %q want svc=svc-42,name=my-service", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery within 2s")
	}
}

// TestDispatcherConnectionSessionSummary_SnakeCaseAndTopSourceIPs confirms
// that EmitConnectionSessionSummary delivers snake_case keys and includes
// top_source_ips in the payload per spec E.3/H.2.
func TestDispatcherConnectionSessionSummary_SnakeCaseAndTopSourceIPs(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newFakeStore()
	store.addWebhook(db.Webhook{
		ID:     "wh-css",
		Name:   "session-summary",
		URL:    srv.URL,
		Events: `["connection.session_summary"]`,
	})
	d := New(store, &fakeSecrets{id: "wh-css", plaintext: "s"}, nil, nil)
	d.SetRetryBackoff(nil)
	d.Start()
	defer d.Close()

	now := time.Now().UTC().Truncate(time.Second)
	rollup := RollupData{
		Sessions:      42,
		BytesIn:       1024,
		BytesOut:      2048,
		AvgDurationMs: 150,
		P95DurationMs: 400,
		TopSourceIPs: []TopSourceIP{
			{IP: "10.0.0.1", Sessions: 20},
			{IP: "10.0.0.2", Sessions: 15},
		},
	}
	d.EmitConnectionSessionSummary("svc-99", "http", now, now.Add(time.Hour), rollup)

	select {
	case body := <-got:
		var env map[string]any
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		data, ok := env["data"].(map[string]any)
		if !ok {
			t.Fatalf("body.data not a map: %T", env["data"])
		}
		// Assert snake_case keys are present.
		if data["service_id"] != "svc-99" {
			t.Errorf("service_id = %v want svc-99", data["service_id"])
		}
		if data["window_start"] == "" || data["window_start"] == nil {
			t.Error("window_start must be set")
		}
		if data["sessions"] == nil {
			t.Error("sessions must be set")
		}
		if data["bytes_in"] == nil {
			t.Error("bytes_in must be set")
		}
		if data["avg_duration_ms"] == nil {
			t.Error("avg_duration_ms must be set")
		}
		if data["p95_duration_ms"] == nil {
			t.Error("p95_duration_ms must be set")
		}
		// Assert top_source_ips is a non-empty slice with the right shape.
		ips, ok := data["top_source_ips"].([]any)
		if !ok {
			t.Fatalf("top_source_ips not a slice: %T", data["top_source_ips"])
		}
		if len(ips) != 2 {
			t.Fatalf("top_source_ips len = %d want 2", len(ips))
		}
		first, ok := ips[0].(map[string]any)
		if !ok {
			t.Fatalf("top_source_ips[0] not a map: %T", ips[0])
		}
		if first["ip"] != "10.0.0.1" {
			t.Errorf("top_source_ips[0].ip = %v want 10.0.0.1", first["ip"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery within 2s")
	}
}

// waitFor polls cond until it returns true or timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
