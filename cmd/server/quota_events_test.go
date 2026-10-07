package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/quota"
)

type publishedEvent struct {
	event   string
	payload map[string]any
}

type recordingPublisher struct {
	mu     sync.Mutex
	events []publishedEvent
}

func (p *recordingPublisher) Publish(_ context.Context, event string, payload any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, _ := payload.(map[string]any)
	p.events = append(p.events, publishedEvent{event, m})
}

func (p *recordingPublisher) all() []publishedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]publishedEvent(nil), p.events...)
}

type twoKeyLimits struct{}

func (twoKeyLimits) ListRateLimits(context.Context) ([]db.RateLimit, error) {
	return []db.RateLimit{
		{ID: "rl-a", Scope: quota.ScopeGatewayKey, Subject: "gk-a", Dimension: quota.DimensionRPM, Lim: 3, Burst: 1, Window: quota.WindowMinute},
		{ID: "rl-b", Scope: quota.ScopeGatewayKey, Subject: "gk-b", Dimension: quota.DimensionRPM, Lim: 5, Burst: 1, Window: quota.WindowMinute},
	}, nil
}

// One "quota.exceeded" event per limit per minute, however often the limit
// refuses; the payload describes the limit and nothing else.
func TestQuotaMiddleware_PublishesQuotaExceededOncePerMinute(t *testing.T) {
	e := quota.NewWithStores(twoKeyLimits{}, noDailyUsage{})
	if err := e.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	e.SetClock(func() time.Time { return now }) // the bucket never refills
	pub := &recordingPublisher{}
	clock := now
	events := newQuotaEvents(pub, func() time.Time { return clock })
	passed := 0
	h := buildQuotaMiddleware(e, nil, events)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { passed++ }))
	do := func(key string) int {
		ctx := quota.WithSubjects(context.Background(), quota.Subjects{ServiceID: "svc1", APIKeyID: "gw:" + key, GatewayKeyID: key, Model: "m"})
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer bgw_do-not-publish-me")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	if code := do("gk-a"); code != http.StatusOK || passed != 1 || len(pub.all()) != 0 {
		t.Fatalf("allowed request: status %d, passed %d, events %+v", code, passed, pub.all())
	}
	if code := do("gk-a"); code != http.StatusTooManyRequests {
		t.Fatalf("second request: status %d, want 429", code)
	}
	got := pub.all()
	if len(got) != 1 || got[0].event != "quota.exceeded" {
		t.Fatalf("events after the first denial = %+v", got)
	}
	want := map[string]any{"scope": "gateway_key", "subject": "gk-a", "dimension": "rpm", "limit": 3, "window": "minute", "service_id": "svc1"}
	if len(got[0].payload) != len(want) {
		t.Fatalf("payload = %+v, want %+v", got[0].payload, want)
	}
	for k, v := range want {
		if got[0].payload[k] != v {
			t.Fatalf("payload[%q] = %v, want %v", k, got[0].payload[k], v)
		}
	}

	for range 50 {
		if code := do("gk-a"); code != http.StatusTooManyRequests {
			t.Fatalf("hammering: status %d", code)
		}
	}
	if n := len(pub.all()); n != 1 {
		t.Fatalf("%d events after 51 denials in one minute, want 1", n)
	}

	// Another subject is announced on its own.
	do("gk-b")
	do("gk-b")
	if got := pub.all(); len(got) != 2 || got[1].payload["subject"] != "gk-b" || got[1].payload["limit"] != 5 {
		t.Fatalf("events = %+v", got)
	}

	// After the interval the next denial is announced again, once.
	clock = clock.Add(time.Minute)
	do("gk-a")
	do("gk-a")
	if got := pub.all(); len(got) != 3 || got[2].payload["subject"] != "gk-a" {
		t.Fatalf("events after the interval = %+v", got)
	}
	if passed != 2 {
		t.Fatalf("%d requests passed, want 2: publishing must not change who is refused", passed)
	}
}

// Without a publisher (or without the events value) a denial is still a 429.
func TestQuotaMiddleware_NoPublisher(t *testing.T) {
	for name, events := range map[string]*quotaEvents{
		"nil events":    nil,
		"nil publisher": newQuotaEvents(nil, nil),
	} {
		e := quota.NewWithStores(oneLimit{}, noDailyUsage{})
		if err := e.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		h := buildQuotaMiddleware(e, nil, events)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		ctx := quota.WithSubjects(context.Background(), quota.Subjects{ServiceID: "svc1", APIKeyID: "key-1"})
		var rec *httptest.ResponseRecorder
		for range 2 {
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx))
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: status %d, want 429", name, rec.Code)
		}
	}
}

// Concurrent denials of one limit are announced once.
func TestQuotaEvents_ConcurrentDenialsPublishOnce(t *testing.T) {
	pub := &recordingPublisher{}
	clock := time.Unix(1_000_000, 0)
	q := newQuotaEvents(pub, func() time.Time { return clock })
	dec := quota.Decision{LimitingScope: "model", LimitingSubject: "zai/glm", Dimension: "rpm", Limit: 2, Window: "day"}
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.denied(context.Background(), quota.Subjects{ServiceID: "s"}, dec)
		}()
	}
	wg.Wait()
	if n := len(pub.all()); n != 1 {
		t.Fatalf("%d events from 64 concurrent denials, want 1", n)
	}
}

func TestEventThrottle(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	th := &eventThrottle{now: func() time.Time { return clock }, every: time.Minute}
	if !th.allow("a") || th.allow("a") {
		t.Fatal("the first call passes, the second within the interval does not")
	}
	if !th.allow("b") {
		t.Fatal("another key has its own interval")
	}
	clock = clock.Add(59 * time.Second)
	if th.allow("a") {
		t.Fatal("still inside the interval")
	}
	clock = clock.Add(time.Second)
	if !th.allow("a") {
		t.Fatal("the interval is over")
	}
}

// The map is bounded: stale entries go when it is full, and while it is full
// of fresh entries a new key is refused instead of growing it.
func TestEventThrottle_Bounded(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	th := &eventThrottle{now: func() time.Time { return clock }, every: time.Minute}
	for i := range eventThrottleMaxKeys {
		if !th.allow(fmt.Sprintf("k%d", i)) {
			t.Fatalf("key %d refused below the cap", i)
		}
	}
	if th.allow("one-too-many") {
		t.Fatal("a new key was admitted into a full map of fresh entries")
	}
	if n := th.size(); n != eventThrottleMaxKeys {
		t.Fatalf("size %d, want %d", n, eventThrottleMaxKeys)
	}
	clock = clock.Add(2 * time.Minute)
	if !th.allow("after-the-interval") {
		t.Fatal("a new key was refused although every entry is stale")
	}
	if n := th.size(); n != 1 {
		t.Fatalf("size %d after pruning, want 1", n)
	}
}

func TestEventThrottle_Concurrent(t *testing.T) {
	th := &eventThrottle{now: time.Now, every: time.Minute}
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if th.allow(fmt.Sprintf("k%d", i%4)) {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if passed != 4 {
		t.Fatalf("%d calls passed, want one per key (4)", passed)
	}
}

type recordingAudit struct {
	events []audit.Event
	ctxErr error
}

func (a *recordingAudit) Append(ctx context.Context, e audit.Event) error {
	a.events = append(a.events, e)
	a.ctxErr = ctx.Err()
	return nil
}

// A refusal is audited as guardrail.refused for the service, with the
// pattern's id and the action, and survives a client that already left.
func TestGuardrailRefusalAudit(t *testing.T) {
	aud := &recordingAudit{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	guardrailRefusalAudit(aud)(ctx, "svc-1", "prompt_injection.ignore_previous", "refuse_safe")
	if len(aud.events) != 1 || aud.ctxErr != nil {
		t.Fatalf("events %+v, context error %v", aud.events, aud.ctxErr)
	}
	e := aud.events[0]
	if e.Action != audit.ActionGuardrailRefused || e.SubjectID != "svc-1" || e.Result != "denied" || e.ActorID != "" {
		t.Fatalf("event = %+v", e)
	}
	if got := string(e.Payload); got != `{"action":"refuse_safe","pattern":"prompt_injection.ignore_previous"}` {
		t.Fatalf("payload = %s", got)
	}
	// A nil logger (audit not wired) is a no-op, not a panic.
	guardrailRefusalAudit((*audit.Logger)(nil))(context.Background(), "svc-1", "p", "refuse_403")
}
