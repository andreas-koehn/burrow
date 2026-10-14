package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/cache/semantic"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/metrics"
	"github.com/ankoehn/burrow/internal/observe"
	wtemplate "github.com/ankoehn/burrow/internal/webhook/template"
)

// recEmitter records what the observer publishes. Unlike the dispatcher it
// has no rate limit of its own on the Emit… helpers.
type recEmitter struct {
	mu     sync.Mutex
	events []recEvent
}

type recEvent struct {
	name    string
	payload map[string]any
}

func (e *recEmitter) add(name string, payload map[string]any) {
	e.mu.Lock()
	e.events = append(e.events, recEvent{name, payload})
	e.mu.Unlock()
}

func (e *recEmitter) Publish(_ context.Context, event string, payload any) {
	p, _ := payload.(map[string]any)
	e.add(event, p)
}

func (e *recEmitter) EmitAIUpstreamError(serviceID, backendServiceID string, status int, errMsg string, retryCount int) {
	e.add("ai.upstream_error", map[string]any{"service_id": serviceID, "backend_service_id": backendServiceID, "status": status, "error": errMsg, "retry_count": retryCount})
}

func (e *recEmitter) EmitServiceCreated(serviceID, name, kind, accessMode string) {
	e.add("service.created", map[string]any{"service_id": serviceID, "name": name, "type": kind, "access_mode": accessMode})
}

// named returns the payloads of the events called name, in order.
func (e *recEmitter) named(name string) []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []map[string]any
	for _, ev := range e.events {
		if ev.name == name {
			out = append(out, ev.payload)
		}
	}
	return out
}

// recAudit records audit entries; block, when set, holds every Append until
// it is closed.
type recAudit struct {
	mu    sync.Mutex
	got   []audit.Event
	block chan struct{}
}

func (a *recAudit) Append(ctx context.Context, e audit.Event) error {
	if a.block != nil {
		select {
		case <-a.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.mu.Lock()
	a.got = append(a.got, e)
	a.mu.Unlock()
	return nil
}

func (a *recAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.got))
	for _, e := range a.got {
		out = append(out, e.Action+":"+e.SubjectID)
	}
	return out
}

// flatPricer prices every token at one micro-dollar.
type flatPricer struct{}

func (flatPricer) RowUSD(r db.UsageRow) float64 {
	return r.ReportedUSD + float64(r.PricedTokensIn+r.PricedTokensOut)/1e6
}

func newTestObserver(t *testing.T) (*observer, *metrics.Recorder, *recEmitter, *recAudit) {
	t.Helper()
	rec, em, au := metrics.New(), &recEmitter{}, &recAudit{}
	o := newObserver(observerParts{Recorder: rec, Events: em, Audit: au, Cost: flatPricer{}})
	t.Cleanup(o.stop)
	return o, rec, em, au
}

// expo renders the recorder.
func expo(t *testing.T, rec *metrics.Recorder) string {
	t.Helper()
	var buf bytes.Buffer
	if err := rec.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// wantLines fails for every sample line that is not in text.
func wantLines(t *testing.T, text string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains("\n"+text, "\n"+line+"\n") {
			t.Errorf("missing sample %q in:\n%s", line, grepBurrow(text, strings.SplitN(line, "{", 2)[0]))
		}
	}
}

// grepBurrow returns the lines of text that start with prefix.
func grepBurrow(text, prefix string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// fieldsOf checks a payload's field names against the template documentation.
func fieldsOf(t *testing.T, event string, payload map[string]any) {
	t.Helper()
	want := wtemplate.Fields(event)
	if len(payload) != len(want) {
		t.Errorf("%s: payload %v, documented fields %v", event, payload, want)
		return
	}
	for _, f := range want {
		if _, ok := payload[f]; !ok {
			t.Errorf("%s: payload %v lacks the documented field %q", event, payload, f)
		}
	}
}

func TestObserver_Sessions(t *testing.T) {
	o, rec, em, _ := newTestObserver(t)
	open := func(session, token string) observe.Session {
		return observe.Session{Kind: observe.SessionOpened, SessionID: session, UserID: "u1", TokenID: token, ConnectedAt: time.Now()}
	}
	o.ObserveSession(open("s1", "tok1"))
	o.ObserveSession(open("s2", "tok2"))
	text := expo(t, rec)
	wantLines(t, text, `burrow_client_session_count{user="u1"} 2`)
	if strings.Contains(text, "burrow_control_reconnects_total{") {
		t.Fatalf("a first session counted as a reconnect:\n%s", grepBurrow(text, "burrow_control_reconnects_total"))
	}

	o.ObserveSession(observe.Session{Kind: observe.TunnelOpened, SessionID: "s1", UserID: "u1", TunnelID: "t1", TunnelType: "http", TunnelName: "web", ServiceID: "svc1", AccessMode: "open", ServiceCreated: true})
	o.ObserveSession(observe.Session{Kind: observe.TunnelClosed, SessionID: "s1", UserID: "u1", TunnelID: "t1", TunnelType: "http", ServiceID: "svc1"})
	closed := open("s1", "tok1")
	closed.Kind, closed.Duration = observe.SessionClosed, 90*time.Second
	o.ObserveSession(closed)
	o.ObserveSession(open("s3", "tok1")) // the same token again: its client came back

	text = expo(t, rec)
	wantLines(t, text,
		`burrow_client_session_count{user="u1"} 2`,
		`burrow_client_session_duration_seconds_count{user="u1"} 1`,
		`burrow_client_session_duration_seconds_sum{user="u1"} 90`,
		`burrow_control_reconnects_total{client="tok1"} 1`,
		`burrow_connections_total{service="",kind="control",status="closed_clean"} 1`,
	)
	for _, event := range []string{"tunnel.connected", "tunnel.disconnected", "service.created"} {
		got := em.named(event)
		if len(got) != 1 {
			t.Fatalf("%d %s events, want 1", len(got), event)
		}
		fieldsOf(t, event, got[0])
	}
	if p := em.named("tunnel.connected")[0]; p["ServiceID"] != "svc1" || p["TunnelID"] != "t1" || p["Kind"] != "http" {
		t.Fatalf("tunnel.connected payload: %v", p)
	}
	if p := em.named("service.created")[0]; p["service_id"] != "svc1" || p["name"] != "web" || p["type"] != "http" || p["access_mode"] != "open" {
		t.Fatalf("service.created payload: %v", p)
	}

	// The same refusal twice in a minute is one event; another reason is another.
	refused := observe.Session{Kind: observe.TunnelRefused, UserID: "u1", TunnelType: "tcp", RefusalCode: "port_unavailable"}
	o.ObserveSession(refused)
	o.ObserveSession(refused)
	refused.RefusalCode = "unknown_tunnel_type"
	o.ObserveSession(refused)
	failed := em.named("tunnel.failed")
	if len(failed) != 2 || failed[0]["Error"] != "port_unavailable" {
		t.Fatalf("tunnel.failed events: %v", failed)
	}
	fieldsOf(t, "tunnel.failed", failed[0])
}

// One user's clients may cause tunnelEventsPerMinute tunnel events a minute.
func TestObserver_TunnelEventBudget(t *testing.T) {
	o, _, em, _ := newTestObserver(t)
	for i := 0; i < tunnelEventsPerMinute+25; i++ {
		o.ObserveSession(observe.Session{Kind: observe.TunnelOpened, UserID: "noisy", TunnelID: "t", TunnelType: "tcp"})
	}
	o.ObserveSession(observe.Session{Kind: observe.TunnelOpened, UserID: "quiet", TunnelID: "t2", TunnelType: "tcp"})
	if n := len(em.named("tunnel.connected")); n != tunnelEventsPerMinute+1 {
		t.Fatalf("%d tunnel.connected events, want %d for the noisy user and 1 for the quiet one", n, tunnelEventsPerMinute)
	}
}

func TestObserver_Traffic(t *testing.T) {
	o, rec, em, _ := newTestObserver(t)
	o.ObserveTraffic(observe.Traffic{
		Kind: observe.TrafficHTTP, ServiceID: "svc1", UserID: "u1", Method: "POST", Status: 201,
		Outcome: observe.OutcomeClean, BytesIn: 10, BytesOut: 20, Duration: 30 * time.Millisecond,
	})
	o.ObserveTraffic(observe.Traffic{
		Kind: observe.TrafficTCP, UserID: "u1", Outcome: observe.OutcomeClean, BytesIn: 5, BytesOut: 7, Duration: time.Second,
	})
	// A method nobody knows and a status no server sends stay bounded.
	o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, ServiceID: "svc1", Method: "BREW\n", Status: 799, Outcome: observe.OutcomeClean})
	text := expo(t, rec)
	wantLines(t, text,
		`burrow_http_requests_total{service="svc1",method="POST",status="2xx"} 1`,
		`burrow_http_requests_total{service="svc1",method="other",status="none"} 1`,
		`burrow_http_request_duration_seconds_count{service="svc1",method="POST"} 1`,
		`burrow_http_request_bytes_in_total{service="svc1"} 10`,
		`burrow_http_request_bytes_out_total{service="svc1"} 20`,
		`burrow_connections_total{service="svc1",kind="http_proxy",status="closed_clean"} 2`,
		`burrow_connections_total{service="",kind="tcp_proxy",status="closed_clean"} 1`,
		`burrow_connection_bytes_in_total{service="",kind="tcp_proxy"} 5`,
		`burrow_connection_bytes_out_total{service="svc1",kind="http_proxy"} 20`,
		`burrow_client_bytes_in_total{user="u1"} 15`,
		`burrow_client_bytes_out_total{user="u1"} 27`,
	)

	// Refusals: one event per service and reason per minute, with the address.
	denied := observe.Traffic{Kind: observe.TrafficHTTP, ServiceID: "svc1", Method: "GET", Status: 401, Outcome: observe.OutcomeRejected, RejectReason: observe.RejectedAccess, SourceIP: "203.0.113.7"}
	for i := 0; i < 50; i++ {
		o.ObserveTraffic(denied)
	}
	got := em.named("access.denied")
	if len(got) != 1 || got[0]["ServiceID"] != "svc1" || got[0]["Reason"] != observe.RejectedAccess || got[0]["IP"] != "203.0.113.7" {
		t.Fatalf("access.denied events: %v", got)
	}
	fieldsOf(t, "access.denied", got[0])
	if strings.Contains(expo(t, rec), "203.0.113.7") {
		t.Fatal("a visitor's address is in the metrics")
	}

	// A model service whose upstream failed; a plain service's failure is no
	// upstream error of a model.
	o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, ServiceID: "svc-ai", Method: "POST", Status: 502, Outcome: observe.OutcomeError, KeyService: true})
	o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, ServiceID: "svc1", Method: "POST", Status: 502, Outcome: observe.OutcomeError})
	wantLines(t, expo(t, rec), `burrow_ai_upstream_errors_total{service="svc-ai",status="5xx"} 1`)
	up := em.named("ai.upstream_error")
	if len(up) != 1 || up[0]["service_id"] != "svc-ai" || up[0]["status"] != 502 || up[0]["error"] != "http_502" {
		t.Fatalf("ai.upstream_error events: %v", up)
	}
	fieldsOf(t, "ai.upstream_error", up[0])
}

// promotingCache is a semantic tier with an index of two entries.
type promotingCache struct{ semantic.NoopCache }

func (promotingCache) Stats(context.Context, string) (semantic.Stats, error) {
	return semantic.Stats{Entries: 2, OnDiskBytes: 4096}, nil
}

func TestObserver_ModelRequest(t *testing.T) {
	rec, em, au := metrics.New(), &recEmitter{}, &recAudit{}
	chain := aigw.NewChain(nil, promotingCache{}, nil, nil, nil, nil, nil, nil, nil)
	o := newObserver(observerParts{Recorder: rec, Events: em, Audit: au, Cost: flatPricer{}, Chain: chain})
	t.Cleanup(o.stop)

	reported := 0.25
	// Answered after a failover, by a service key, priced by the table.
	o.ObserveModelRequest(observe.ModelRequest{
		ServiceID: "svc-a", APIKeyID: "key1", ProviderSlug: "beta", TargetModel: "m", Kind: "openai",
		Outcome: observe.ModelAnswered, Status: 200, Metered: true, TokensIn: 300, TokensOut: 200,
		Cache: observe.CacheMiss, SemanticAsked: true, SemanticPromoted: true, PromotedKey: "global:abc",
		Redactions: []observe.Redaction{{Rule: "email", Count: 2}},
		Attempts: []observe.Attempt{
			{ProviderSlug: "alpha", ServiceID: "svc-a", Status: 500, Failed: true, Sent: true},
			{ProviderSlug: "alpha", ServiceID: "svc-a", Status: 0, Failed: true, Sent: true},
			{ProviderSlug: "beta", ServiceID: "svc-b", Status: 200, Sent: true},
		},
	})
	// From the cache, by a gateway key, with a stated cost.
	o.ObserveModelRequest(observe.ModelRequest{
		ServiceID: "svc-a", GatewayKeyID: "gk1", Kind: "openai", Outcome: observe.ModelCacheHit, Status: 200,
		Metered: true, ReportedCostUSD: &reported, Cache: observe.CacheHit,
		SemanticAsked: true, SemanticHit: true, SemanticPolicy: "return_cached_marked", SemanticErr: "timeout",
	})
	// Refused: no usage, no tokens.
	o.ObserveModelRequest(observe.ModelRequest{
		ServiceID: "svc-a", Outcome: observe.ModelGuardrailRefused, Status: 403,
		GuardrailPattern: "ignore_prev", GuardrailAction: "refuse_403", TokensIn: 999,
	})
	o.ObserveModelRequest(observe.ModelRequest{ServiceID: "svc-a", Outcome: observe.ModelGuardrailRefused, GuardrailPattern: "ignore_prev"})
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	wantLines(t, expo(t, rec),
		`burrow_ai_tokens_in_total{service="svc-a",api_key="key1"} 300`,
		`burrow_ai_tokens_out_total{service="svc-a",api_key="key1"} 200`,
		`burrow_ai_cost_usd_total{service="svc-a",api_key="key1"} 0.0005`,
		`burrow_ai_tokens_in_total{service="svc-a",api_key="gw:gk1"} 0`,
		`burrow_ai_cost_usd_total{service="svc-a",api_key="gw:gk1"} 0.25`,
		`burrow_ai_cache_hits_total{service="svc-a"} 1`,
		`burrow_ai_cache_misses_total{service="svc-a"} 1`,
		`burrow_ai_semantic_lookups_total{service="svc-a"} 2`,
		`burrow_ai_semantic_hits_total{service="svc-a",policy="return_cached_marked"} 1`,
		`burrow_ai_semantic_errors_total{service="svc-a",kind="timeout"} 1`,
		`burrow_ai_semantic_promotions_total{service="svc-a"} 1`,
		`burrow_ai_semantic_index_entries{service="svc-a"} 2`,
		`burrow_ai_semantic_index_bytes{service="svc-a"} 4096`,
		`burrow_ai_failover_events_total{service="svc-a",from="alpha",to="alpha",cross_provider="false"} 1`,
		`burrow_ai_failover_events_total{service="svc-a",from="alpha",to="beta",cross_provider="true"} 1`,
		`burrow_ai_upstream_errors_total{service="svc-a",status="5xx"} 1`,
		`burrow_ai_upstream_errors_total{service="svc-a",status="none"} 1`,
	)
	up := em.named("ai.upstream_error")
	if len(up) != 2 || up[0]["backend_service_id"] != "svc-a" || up[0]["error"] != "http_500" || up[1]["error"] != "no_response" || up[1]["retry_count"] != 1 {
		t.Fatalf("ai.upstream_error events: %v", up)
	}
	promo := em.named("ai.cache_promotion")
	if len(promo) != 1 || promo[0]["exact_key_hash"] != "global:abc" || promo[0]["prompt_fingerprint"] != "" {
		t.Fatalf("ai.cache_promotion events: %v", promo)
	}
	fieldsOf(t, "ai.cache_promotion", promo[0])
	for event, want := range map[string]map[string]any{
		"redaction.applied": {"ServiceID": "svc-a", "RuleID": "email"},
		"guardrail.refused": {"ServiceID": "svc-a", "Pattern": "ignore_prev"},
	} {
		got := em.named(event)
		if len(got) != 1 { // the second refusal is inside the minute
			t.Fatalf("%d %s events, want 1", len(got), event)
		}
		fieldsOf(t, event, got[0])
		for k, v := range want {
			if got[0][k] != v {
				t.Errorf("%s payload %v, want %s=%v", event, got[0], k, v)
			}
		}
	}
	acts := strings.Join(au.actions(), ",")
	if !strings.Contains(acts, "ai.cache.promoted:svc-a") || !strings.Contains(acts, "redaction.applied:svc-a") {
		t.Fatalf("audit entries: %s", acts)
	}
}

// A nil observer, and one built from nothing, do nothing and do not panic.
func TestObserver_NilAndEmpty(t *testing.T) {
	var none *observer
	empty := newObserver(observerParts{})
	t.Cleanup(empty.stop)
	for _, o := range []*observer{none, empty} {
		o.ObserveSession(observe.Session{Kind: observe.SessionOpened, UserID: "u"})
		o.ObserveSession(observe.Session{Kind: observe.TunnelOpened, UserID: "u", ServiceCreated: true})
		o.ObserveSession(observe.Session{Kind: observe.TunnelRefused, UserID: "u"})
		o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, Outcome: observe.OutcomeRejected})
		o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, Outcome: observe.OutcomeError, KeyService: true})
		o.ObserveModelRequest(observe.ModelRequest{
			Metered: true, SemanticPromoted: true, Outcome: observe.ModelGuardrailRefused,
			Redactions: []observe.Redaction{{Rule: "email"}}, Attempts: []observe.Attempt{{Failed: true}},
		})
		if err := o.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		o.stop()
		o.stop()
	}
}

// An audit log that does not answer holds nothing up: every call returns at
// once, the queue fills and the rest is dropped, and Flush gives up with its
// context.
func TestObserver_StuckAuditDoesNotBlock(t *testing.T) {
	rec, em := metrics.New(), &recEmitter{}
	au := &recAudit{block: make(chan struct{})}
	o := newObserver(observerParts{Recorder: rec, Events: em, Audit: au, Cost: flatPricer{}})
	t.Cleanup(o.stop)
	defer close(au.block)

	start := time.Now()
	for i := 0; i < 2000; i++ {
		// A different rule each time, so that each wants an audit entry.
		o.ObserveModelRequest(observe.ModelRequest{
			ServiceID: "svc", Metered: true, TokensIn: 1,
			Redactions: []observe.Redaction{{Rule: "rule" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676%26))}},
		})
	}
	// Without the queue the first of them would wait for the audit log for
	// ever; the bound is generous because other suites share this machine.
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("2000 events took %s with a stuck audit log", took)
	}
	wantLines(t, expo(t, rec), `burrow_ai_tokens_in_total{service="svc",api_key=""} 2000`)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := o.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Flush with a stuck audit log: %v, want the context's error", err)
	}
}

func TestEventBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := &eventBudget{now: func() time.Time { return now }, every: time.Minute, max: 3}
	for i := 0; i < 3; i++ {
		if !b.allow("k") {
			t.Fatalf("event %d refused inside the budget", i+1)
		}
	}
	if b.allow("k") {
		t.Fatal("a fourth event in the minute was let through")
	}
	if !b.allow("other") {
		t.Fatal("another key shares the budget")
	}
	now = now.Add(time.Minute)
	if !b.allow("k") {
		t.Fatal("the budget did not start again after its interval")
	}
	for i := 0; i < 2*eventThrottleMaxKeys; i++ {
		b.allow("key" + string(rune(i)))
	}
	b.mu.Lock()
	n := len(b.win)
	b.mu.Unlock()
	if n > eventThrottleMaxKeys {
		t.Fatalf("the budget remembers %d keys, more than %d", n, eventThrottleMaxKeys)
	}
}

// Concurrent opens and closes of one user's sessions leave the gauge exact:
// it is set under the same lock that counts.
func TestObserver_SessionGaugeConcurrent(t *testing.T) {
	o, rec, _, _ := newTestObserver(t)
	const n = 64
	run := func(kind string) {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o.ObserveSession(observe.Session{Kind: kind, UserID: "u1", TokenID: "tok", ConnectedAt: time.Now()})
			}()
		}
		wg.Wait()
	}
	run(observe.SessionOpened)
	wantLines(t, expo(t, rec), `burrow_client_session_count{user="u1"} 64`)
	run(observe.SessionClosed)
	wantLines(t, expo(t, rec), `burrow_client_session_count{user="u1"} 0`)
}

type panicEmitter struct{}

func (*panicEmitter) Publish(context.Context, string, any) { panic("publish") }
func (*panicEmitter) EmitAIUpstreamError(string, string, int, string, int) {
	panic("emit")
}
func (*panicEmitter) EmitServiceCreated(string, string, string, string) { panic("emit") }

type panicPricer struct{}

func (panicPricer) RowUSD(db.UsageRow) float64 { panic("price") }

// An observer whose parts panic, or are typed nils, never panics into the
// goroutine of what it observes.
func TestObserver_PartsThatPanicOrAreTypedNil(t *testing.T) {
	var nilEm *recEmitter
	var nilAu *recAudit
	var nilRec *metrics.Recorder
	for name, p := range map[string]observerParts{
		"panicking": {Recorder: metrics.New(), Events: &panicEmitter{}, Audit: &recAudit{}, Cost: panicPricer{}},
		"typed nil": {Recorder: nilRec, Events: nilEm, Audit: nilAu, Cost: (*flatPricer)(nil), Chain: (*aigw.Chain)(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			o := newObserver(p)
			t.Cleanup(o.stop)
			o.ObserveSession(observe.Session{Kind: observe.SessionOpened, UserID: "u"})
			o.ObserveSession(observe.Session{Kind: observe.SessionClosed, UserID: "u"})
			o.ObserveSession(observe.Session{Kind: observe.TunnelOpened, UserID: "u", ServiceCreated: true})
			o.ObserveSession(observe.Session{Kind: observe.TunnelRefused, UserID: "u", RefusalCode: "x"})
			o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, Outcome: observe.OutcomeRejected})
			o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, Outcome: observe.OutcomeError, KeyService: true})
			o.ObserveModelRequest(observe.ModelRequest{
				Metered: true, SemanticPromoted: true, Outcome: observe.ModelGuardrailRefused,
				Redactions: []observe.Redaction{{Rule: "email"}}, Attempts: []observe.Attempt{{Failed: true}},
			})
			if err := o.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A session that ends with a stated outcome is counted with it; one without
// is counted as clean.
func TestObserver_SessionOutcome(t *testing.T) {
	o, rec, _, _ := newTestObserver(t)
	o.ObserveSession(observe.Session{Kind: observe.SessionClosed, UserID: "u"})
	o.ObserveSession(observe.Session{Kind: observe.SessionClosed, UserID: "u", Outcome: observe.OutcomeError})
	wantLines(t, expo(t, rec),
		`burrow_connections_total{service="",kind="control",status="closed_clean"} 1`,
		`burrow_connections_total{service="",kind="control",status="closed_error"} 1`)
}

// access.denied is for refusals: a 401 or a 403. The redirect of a first
// visit to a login service and the 500 of a misconfigured service are
// rejected requests, counted as traffic, and no event.
func TestObserver_AccessDeniedIsForRefusals(t *testing.T) {
	o, rec, em, _ := newTestObserver(t)
	rej := func(service string, status int) observe.Traffic {
		return observe.Traffic{Kind: observe.TrafficHTTP, ServiceID: service, Method: "GET", Status: status,
			Outcome: observe.OutcomeRejected, RejectReason: observe.RejectedAccess, SourceIP: "203.0.113.7"}
	}
	o.ObserveTraffic(rej("svc-login", 302))
	o.ObserveTraffic(rej("svc-broken", 500))
	if got := em.named("access.denied"); len(got) != 0 {
		t.Fatalf("access.denied for a redirect and a 500: %v", got)
	}
	wantLines(t, expo(t, rec),
		`burrow_http_requests_total{service="svc-login",method="GET",status="3xx"} 1`,
		`burrow_http_requests_total{service="svc-broken",method="GET",status="5xx"} 1`)
	o.ObserveTraffic(rej("svc-401", 401))
	o.ObserveTraffic(rej("svc-403", 403))
	got := em.named("access.denied")
	if len(got) != 2 || got[0]["ServiceID"] != "svc-401" || got[1]["ServiceID"] != "svc-403" {
		t.Fatalf("access.denied events: %v", got)
	}
}

// A visitor who hangs up before the upstream answered is no upstream error;
// an upstream that failed still is.
func TestObserver_ClientGoneIsNoUpstreamError(t *testing.T) {
	o, rec, em, _ := newTestObserver(t)
	o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, ServiceID: "svc-ai", Method: "POST", Status: 502,
		Outcome: observe.OutcomeError, KeyService: true, ClientGone: true})
	if strings.Contains(expo(t, rec), "burrow_ai_upstream_errors_total{") {
		t.Fatalf("an upstream error counted for a visitor who left:\n%s", grepBurrow(expo(t, rec), "burrow_ai_upstream_errors_total"))
	}
	if up := em.named("ai.upstream_error"); len(up) != 0 {
		t.Fatalf("ai.upstream_error for a visitor who left: %v", up)
	}
	wantLines(t, expo(t, rec), `burrow_connections_total{service="svc-ai",kind="http_proxy",status="closed_error"} 1`)
	o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, ServiceID: "svc-ai", Method: "POST", Status: 502,
		Outcome: observe.OutcomeError, KeyService: true})
	wantLines(t, expo(t, rec), `burrow_ai_upstream_errors_total{service="svc-ai",status="5xx"} 1`)
	if up := em.named("ai.upstream_error"); len(up) != 1 {
		t.Fatalf("ai.upstream_error events: %v", up)
	}
}

// A relay that is shutting down ends every session itself. Once the observer
// has stopped publishing, what it observes is still counted, and its audit
// entries are still written, but no webhook event leaves it.
func TestObserver_StoppedPublishesNothing(t *testing.T) {
	o, rec, em, au := newTestObserver(t)
	o.ObserveSession(observe.Session{Kind: observe.SessionOpened, SessionID: "s1", UserID: "u1", TokenID: "tok1", ConnectedAt: time.Now()})
	o.ObserveSession(observe.Session{Kind: observe.TunnelOpened, SessionID: "s1", UserID: "u1", TunnelID: "t1", TunnelType: "http", ServiceID: "svc1"})
	if n := len(em.named("tunnel.connected")); n != 1 {
		t.Fatalf("%d tunnel.connected events before the stop, want 1", n)
	}
	count := func() int {
		em.mu.Lock()
		defer em.mu.Unlock()
		return len(em.events)
	}
	before := count()

	o.stopPublishing()

	// The shutdown itself: the tunnel and its session end.
	o.ObserveSession(observe.Session{Kind: observe.TunnelClosed, SessionID: "s1", UserID: "u1", TunnelID: "t1", TunnelType: "http", ServiceID: "svc1"})
	o.ObserveSession(observe.Session{Kind: observe.SessionClosed, SessionID: "s1", UserID: "u1", TokenID: "tok1", Duration: 42 * time.Second})
	// And everything else that may still finish while the relay stops.
	o.ObserveSession(observe.Session{Kind: observe.TunnelOpened, UserID: "u2", TunnelID: "t2", TunnelType: "tcp", ServiceID: "svc2", ServiceCreated: true})
	o.ObserveSession(observe.Session{Kind: observe.TunnelRefused, UserID: "u2", RefusalCode: "port_unavailable"})
	o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, ServiceID: "svc1", Method: "GET", Status: 401, Outcome: observe.OutcomeRejected, RejectReason: observe.RejectedAccess})
	o.ObserveTraffic(observe.Traffic{Kind: observe.TrafficHTTP, ServiceID: "svc-ai", Method: "POST", Status: 502, Outcome: observe.OutcomeError, KeyService: true})
	o.ObserveModelRequest(observe.ModelRequest{
		ServiceID: "svc-a", Outcome: observe.ModelGuardrailRefused, GuardrailPattern: "ignore_prev",
		SemanticPromoted: true, PromotedKey: "global:abc",
		Redactions: []observe.Redaction{{Rule: "email", Count: 1}},
		Attempts:   []observe.Attempt{{ProviderSlug: "alpha", ServiceID: "svc-a", Status: 500, Failed: true, Sent: true}},
	})
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if after := count(); after != before {
		em.mu.Lock()
		defer em.mu.Unlock()
		t.Fatalf("%d events were published after the observer stopped publishing: %v", after-before, em.events[before:])
	}
	wantLines(t, expo(t, rec),
		`burrow_client_session_count{user="u1"} 0`,
		`burrow_client_session_duration_seconds_count{user="u1"} 1`,
		`burrow_client_session_duration_seconds_sum{user="u1"} 42`,
		`burrow_connections_total{service="",kind="control",status="closed_clean"} 1`,
		`burrow_connections_total{service="svc1",kind="http_proxy",status="rejected"} 1`,
		`burrow_ai_upstream_errors_total{service="svc-ai",status="5xx"} 1`,
		`burrow_ai_upstream_errors_total{service="svc-a",status="5xx"} 1`,
		`burrow_ai_semantic_promotions_total{service="svc-a"} 1`,
	)
	acts := strings.Join(au.actions(), ",")
	if !strings.Contains(acts, "redaction.applied:svc-a") {
		t.Fatalf("the audit entry of a redaction was not written after the stop: %s", acts)
	}

	// The worker's stop silences an observer that was never told to.
	late, _, lateEvents, _ := newTestObserver(t)
	late.stop()
	late.ObserveSession(observe.Session{Kind: observe.TunnelClosed, UserID: "u1", TunnelID: "t1", TunnelType: "http"})
	if n := len(lateEvents.named("tunnel.disconnected")); n != 0 {
		t.Fatalf("%d events from an observer whose worker was stopped", n)
	}
	// A nil observer has nothing to stop.
	var none *observer
	none.stopPublishing()
}

// The section that counts sessions releases its lock also when it panics:
// the entry point recovers the panic, and a lock left held would block every
// session open and close after it.
func TestObserver_PanicUnderTheLockDoesNotHoldIt(t *testing.T) {
	o, rec, _, _ := newTestObserver(t)
	sessions := o.sessions
	o.sessions = nil // a write to a nil map panics inside the locked section
	o.ObserveSession(observe.Session{Kind: observe.SessionOpened, SessionID: "s1", UserID: "u1", TokenID: "tok1"})
	o.sessions = sessions

	done := make(chan struct{})
	go func() {
		defer close(done)
		o.ObserveSession(observe.Session{Kind: observe.SessionOpened, SessionID: "s2", UserID: "u1", TokenID: "tok2"})
		o.ObserveSession(observe.Session{Kind: observe.SessionClosed, SessionID: "s2", UserID: "u1", TokenID: "tok2"})
		o.ObserveSession(observe.Session{Kind: observe.SessionOpened, SessionID: "s3", UserID: "u1", TokenID: "tok3"})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a panic inside the locked section left the observer's lock held")
	}
	wantLines(t, expo(t, rec), `burrow_client_session_count{user="u1"} 1`)
}
