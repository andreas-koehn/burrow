// observer.go — the one implementation of Burrow's observation points.
//
// The control server, the proxy and the AI chain each hand a finished event
// (package observe) to an Observer interface of their own. All three are this
// type. It turns an event into what an operator sees of it: metric samples,
// webhook events and audit entries. A later consumer (traces) is one more
// method call in here and no new call site anywhere else.
//
// Every method is called on the goroutine of what it observes, so it only
// counts, and hands on: metrics are atomic adds in memory, a webhook event is
// put on the dispatcher's bounded queue, an audit entry on this file's own.
// Nothing here waits for a database or a network.
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/cache/semantic"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/metrics"
	"github.com/ankoehn/burrow/internal/observe"
)

// eventEmitter is the webhook dispatcher as the observer needs it:
// *webhook.Dispatcher. Publish and the Emit… helpers never block (see
// eventPublisher in quota_events.go).
type eventEmitter interface {
	eventPublisher
	EmitAIUpstreamError(serviceID, backendServiceID string, status int, errMsg string, retryCount int)
	EmitServiceCreated(serviceID, name, kind, accessMode string)
}

// rowPricer prices one usage row: *cost.Engine.
type rowPricer interface {
	RowUSD(r db.UsageRow) float64
}

// observerParts is what an observer is built from. Any of them may be nil:
// that consumer is then left out.
type observerParts struct {
	Recorder *metrics.Recorder
	Events   eventEmitter
	Audit    auditAppender
	Cost     rowPricer
	// Chain is read for its semantic tier, to refresh the index gauges after
	// a promotion. main sets the tier on the chain after this is built.
	Chain *aigw.Chain
	Log   *slog.Logger
	// Names names the actor and the subject of the tcp audit rows (*db.DB);
	// nil leaves them unnamed.
	Names auditNames
}

// auditNames reads the email of a user and the name of a service for an
// audit row.
type auditNames interface {
	GetUserByID(ctx context.Context, id string) (db.User, error)
	GetServiceByID(ctx context.Context, id string) (db.Service, error)
}

const (
	// observerQueue is how many jobs (audit entries, gauge refreshes) may
	// wait for the observer's worker.
	observerQueue = 256
	// observerJobTimeout bounds one job.
	observerJobTimeout = 5 * time.Second
	// observerMaxTokens bounds the memory of which client tokens connected.
	observerMaxTokens = 16384
	// tunnelEventsPerMinute is how many tunnel events one user's clients may
	// cause per minute; beyond it they are dropped (and logged).
	tunnelEventsPerMinute = 60
	// tcpBanEventsPerMinute is how many tcp.banned events one service may
	// cause per minute (an attack from many sources bans many); the audit
	// keeps every ban.
	tcpBanEventsPerMinute = 30
)

// observer implements server.Observer, proxy.Observer,
// aigw.RequestObserver and observe.ToolCallObserver. A nil *observer does nothing.
type observer struct {
	observerParts
	log *slog.Logger

	jobs chan observerJob
	quit chan struct{}
	once sync.Once
	// muted is set when the relay begins to shut down: from then on nothing
	// is published (see stopPublishing).
	muted atomic.Bool

	perMinute *eventThrottle // one event per key per minute
	perHour   *eventThrottle // one per key per hour
	full      *eventThrottle // the "queue full" warning
	budget    *eventBudget   // tunnel events per user
	bans      *eventBudget   // tcp.banned events per service

	mu       sync.Mutex
	sessions map[string]int      // user id → live control sessions
	tokens   map[string]struct{} // client token ids that have had a session
}

// observerJob is work the observer hands to its worker. A job with ack set is
// a marker from Flush: the worker closes ack when it gets there.
type observerJob struct {
	run func(ctx context.Context)
	ack chan struct{}
}

// newObserver starts the observer's one worker.
func newObserver(p observerParts) *observer {
	log := p.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// A typed nil in an interface is not == nil: drop those parts too, so that
	// "any part may be nil" holds for them.
	if isNil(p.Events) {
		p.Events = nil
	}
	if isNil(p.Audit) {
		p.Audit = nil
	}
	if isNil(p.Cost) {
		p.Cost = nil
	}
	o := &observer{
		observerParts: p,
		log:           log,
		jobs:          make(chan observerJob, observerQueue),
		quit:          make(chan struct{}),
		perMinute:     &eventThrottle{now: time.Now, every: time.Minute, name: "observer_per_minute", log: log},
		perHour:       &eventThrottle{now: time.Now, every: time.Hour, name: "observer_per_hour", log: log},
		full:          &eventThrottle{now: time.Now, every: time.Minute, name: "observer_warnings", log: log},
		budget:        &eventBudget{now: time.Now, every: time.Minute, max: tunnelEventsPerMinute},
		bans:          &eventBudget{now: time.Now, every: time.Minute, max: tcpBanEventsPerMinute},
		sessions:      map[string]int{},
		tokens:        map[string]struct{}{},
	}
	go o.work()
	return o
}

// isNil reports whether v is nil or holds a nil pointer, map, slice, func or
// channel.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// recovered ends an entry point: a panic in a part of the observer (a
// dispatcher, a pricer) is logged with what kind of event it was, never with
// its contents, and does not reach the goroutine of what is observed.
func (o *observer) recovered(point, kind string) {
	if r := recover(); r != nil {
		o.log.Error("observer: panic while observing", "point", point, "kind", kind, "panic", r)
	}
}

// ---------------------------------------------------------------------------
// Control sessions and tunnels (server.Observer)
// ---------------------------------------------------------------------------

// ObserveSession counts sessions and announces tunnels.
func (o *observer) ObserveSession(e observe.Session) {
	if o == nil {
		return
	}
	defer o.recovered("session", e.Kind)
	switch e.Kind {
	case observe.SessionOpened:
		// A token that already had a session counts as a reconnect, also when
		// the earlier session is still open (a second client with the same
		// token); tokens beyond observerMaxTokens are not remembered and
		// never count.
		if again := o.sessionOpened(e.UserID, e.TokenID); again && o.Recorder != nil {
			o.Recorder.IncControlReconnect(e.TokenID)
		}
	case observe.SessionClosed:
		o.sessionClosed(e.UserID)
		if o.Recorder != nil {
			o.Recorder.ObserveClientSessionDuration(e.UserID, e.Duration.Seconds())
			outcome := e.Outcome
			if outcome == "" {
				outcome = observe.OutcomeClean // no error known to the server
			}
			o.Recorder.IncConnections("", "control", outcome)
		}
	case observe.TunnelOpened:
		o.publishTunnel(e.UserID, "tunnel.connected", map[string]any{
			"ServiceID": e.ServiceID, "TunnelID": e.TunnelID, "Kind": e.TunnelType,
		})
		if ev := o.events(); e.ServiceCreated && ev != nil && o.budget.allow(e.UserID) {
			ev.EmitServiceCreated(e.ServiceID, e.TunnelName, e.TunnelType, e.AccessMode)
		}
	case observe.TunnelClosed:
		o.publishTunnel(e.UserID, "tunnel.disconnected", map[string]any{
			"ServiceID": e.ServiceID, "TunnelID": e.TunnelID, "Kind": e.TunnelType,
		})
	case observe.TCPPortReclaimed:
		o.tcpPortReclaimed(e)
	case observe.ForwardOpened:
		o.forwardOpened(e)
	case observe.TCPBanned:
		o.tcpBanned(e)
	case observe.TCPQuotaWarning:
		o.tcpQuotaWarning(e)
	case observe.TunnelRefused:
		// One per user and reason per minute: a client that keeps asking for
		// a taken port must not fill the queue.
		if o.perMinute.allow("tunnel.failed\x00" + e.UserID + "\x00" + e.RefusalCode) {
			o.publish("tunnel.failed", map[string]any{"ServiceID": "", "TunnelID": "", "Error": e.RefusalCode})
		}
		// A registration that created its service and was then refused (a
		// port it could not have) announces the service: the next one
		// finds the row and does not.
		if ev := o.events(); e.ServiceCreated && ev != nil && o.budget.allow(e.UserID) {
			ev.EmitServiceCreated(e.ServiceID, e.TunnelName, e.TunnelType, e.AccessMode)
		}
	}
}

// tcpPortReclaimed writes the audit entry of a tcp port taken from an
// offline service: the subject is the service that lost the port, the actor
// the owner of the client that took it.
func (o *observer) tcpPortReclaimed(e observe.Session) {
	if o.Audit == nil {
		return
	}
	ev := audit.Event{
		ActorID: e.UserID, Action: audit.ActionServiceTCPPortReclaimed, SubjectID: e.FromServiceID, Result: "ok",
		Payload: audit.MustJSON(map[string]any{
			"service_id": e.ServiceID, "from_service_id": e.FromServiceID, "port": e.Port, "reason": e.Reason,
		}),
	}
	o.enqueue(func(ctx context.Context) {
		o.name(ctx, &ev)
		if err := o.Audit.Append(ctx, ev); err != nil {
			o.log.Warn("observer: audit write failed", "action", audit.ActionServiceTCPPortReclaimed, "err", err)
		}
	})
}

// forwardOpened writes the audit entry of a forward stream, one per user and
// service a minute: a remote desktop that reconnects, or a pool of database
// connections, writes one entry, while the connection log keeps every
// stream. The actor is the user who forwards, the subject the service.
func (o *observer) forwardOpened(e observe.Session) {
	if o.Audit == nil || !o.perMinute.allow("tcp.forward.open\x00"+e.UserID+"\x00"+e.ServiceID) {
		return
	}
	ev := audit.Event{
		ActorID: e.UserID, Action: audit.ActionTCPForwardOpen, SubjectID: e.ServiceID, Result: "ok",
		SourceIP: e.SourceIP,
		Payload: audit.MustJSON(map[string]any{
			"service_id": e.ServiceID, "owner_id": e.OwnerID, "session_id": e.SessionID,
			"token_id": e.TokenID, "token_name": e.TokenName, "tunnel_id": e.TunnelID,
		}),
	}
	o.enqueue(func(ctx context.Context) {
		o.name(ctx, &ev)
		if err := o.Audit.Append(ctx, ev); err != nil {
			o.log.Warn("observer: audit write failed", "action", audit.ActionTCPForwardOpen, "err", err)
		}
	})
}

// tcpBanned announces a source the relay banned from a tcp service and
// writes its audit entry. The actor is the relay (none); the subject is the
// service. The webhook is limited per service and minute, the audit is not.
func (o *observer) tcpBanned(e observe.Session) {
	banMin := int(time.Until(e.Until).Round(time.Minute) / time.Minute)
	if o.events() != nil && o.bans.allow(e.ServiceID) {
		o.publish("tcp.banned", map[string]any{
			"service_id": e.ServiceID, "source": e.SourceIP, "until": e.Until.UTC().Format(time.RFC3339),
			"reason": e.Reason, "ban_min": banMin,
		})
	}
	if o.Audit == nil {
		return
	}
	ev := audit.Event{
		Action: audit.ActionTCPBanAdd, SubjectID: e.ServiceID, Result: "ok",
		Payload: audit.MustJSON(map[string]any{
			"source": e.SourceIP, "until": e.Until.UTC().Format(time.RFC3339), "ban_min": banMin,
			"reason": e.Reason, "by": "auto", "owner_id": e.OwnerID,
		}),
	}
	o.enqueue(func(ctx context.Context) {
		o.name(ctx, &ev)
		if err := o.Audit.Append(ctx, ev); err != nil {
			o.log.Warn("observer: audit write failed", "action", audit.ActionTCPBanAdd, "err", err)
		}
	})
}

// tcpQuotaWarning announces that a tcp service carried 80 % of its daily
// volume; the relay tells it once per service and day.
func (o *observer) tcpQuotaWarning(e observe.Session) {
	percent := 0
	if e.Limit > 0 {
		percent = int(e.Bytes * 100 / e.Limit)
	}
	o.publish("tcp.quota.warning", map[string]any{
		"service_id": e.ServiceID, "day": e.Day, "bytes": e.Bytes, "limit": e.Limit, "percent": percent,
	})
}

// name fills the actor's email and the subject service's name into an
// audit row, on the observer's worker (the session's goroutine never waits
// for the database). What cannot be read stays empty; the row is written.
func (o *observer) name(ctx context.Context, ev *audit.Event) {
	if o.Names == nil {
		return
	}
	if ev.ActorID != "" {
		if u, err := o.Names.GetUserByID(ctx, ev.ActorID); err == nil {
			ev.ActorEmail = u.Email
		}
	}
	if ev.SubjectID != "" {
		if svc, err := o.Names.GetServiceByID(ctx, ev.SubjectID); err == nil {
			ev.SubjectLabel = svc.Name
		}
	}
}

// sessionOpened counts one more open session of the user and remembers the
// token; again says that the token had a session before. The gauge is set
// under the lock that counts, so concurrent opens and closes of one user
// cannot leave an older count in it. The lock is released by defer: a panic
// in here ends in the entry point's recover, and a lock left held would
// block every later session open and close.
func (o *observer) sessionOpened(userID, tokenID string) (again bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sessions[userID]++
	_, again = o.tokens[tokenID]
	if tokenID != "" && !again && len(o.tokens) < observerMaxTokens {
		o.tokens[tokenID] = struct{}{}
	}
	if o.Recorder != nil {
		o.Recorder.SetClientSessionCount(userID, o.sessions[userID])
	}
	return again
}

// sessionClosed counts one open session of the user less, under the same
// lock and for the same reasons as sessionOpened.
func (o *observer) sessionClosed(userID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sessions[userID] > 0 {
		o.sessions[userID]--
	}
	if o.Recorder != nil {
		o.Recorder.SetClientSessionCount(userID, o.sessions[userID])
	}
}

// events is the dispatcher the observer publishes to, or nil: when there is
// none, and once the observer has stopped publishing.
func (o *observer) events() eventEmitter {
	if o.muted.Load() {
		return nil
	}
	return o.Events
}

// stopPublishing ends the observer's webhook events, for good. main calls it
// when the relay begins to shut down, before the control listener ends its
// sessions: those sessions and their tunnels end because the relay stops,
// and one tunnel.disconnected per tunnel would make the shutdown wait on
// every webhook receiver. What is observed afterwards is still counted in
// the metrics, and audit entries are still written.
func (o *observer) stopPublishing() {
	if o != nil {
		o.muted.Store(true)
	}
}

// publishTunnel publishes a tunnel event within the user's budget.
func (o *observer) publishTunnel(userID, event string, payload map[string]any) {
	if o.events() == nil {
		return
	}
	if !o.budget.allow(userID) {
		if o.full.allow("budget\x00" + userID) {
			o.log.Warn("observer: tunnel events dropped, one user's clients cause too many", "user_id", userID, "per_minute", tunnelEventsPerMinute)
		}
		return
	}
	o.publish(event, payload)
}

func (o *observer) publish(event string, payload map[string]any) {
	if ev := o.events(); ev != nil {
		// Background: the event outlives whatever caused it.
		ev.Publish(context.Background(), event, payload)
	}
}

// ---------------------------------------------------------------------------
// Proxied requests and tcp connections (server.Observer, proxy.Observer)
// ---------------------------------------------------------------------------

// ObserveTraffic counts one finished request or connection.
func (o *observer) ObserveTraffic(e observe.Traffic) {
	if o == nil {
		return
	}
	defer o.recovered("traffic", e.Kind)
	if r := o.Recorder; r != nil {
		if e.Kind == observe.TrafficHTTP {
			r.IncHTTPRequest(e.ServiceID, e.Method, e.Status)
			r.ObserveHTTPDuration(e.ServiceID, e.Method, e.Duration.Seconds())
			r.AddHTTPBytesIn(e.ServiceID, e.BytesIn)
			r.AddHTTPBytesOut(e.ServiceID, e.BytesOut)
			if e.GRPC {
				r.IncGRPCRequest(e.ServiceID, e.GRPCStatus)
			}
		}
		r.IncConnections(e.ServiceID, e.Kind, e.Outcome)
		r.AddConnectionBytesIn(e.ServiceID, e.Kind, e.BytesIn)
		r.AddConnectionBytesOut(e.ServiceID, e.Kind, e.BytesOut)
		if e.UserID != "" {
			r.AddClientBytesIn(e.UserID, e.BytesIn)
			r.AddClientBytesOut(e.UserID, e.BytesOut)
		}
	}
	switch {
	case e.Outcome == observe.OutcomeRejected:
		// A refusal is a 401 or a 403: the redirect of a first visit to the
		// login page and the 500 of a misconfigured service are not. A tcp
		// visitor the relay closed before the home client was told (its ip
		// policy, a limit) is one too; it has no status.
		tcp := e.Kind == observe.TrafficTCP || e.Kind == observe.TrafficTCPForward
		if !tcp && e.Status != http.StatusUnauthorized && e.Status != http.StatusForbidden {
			break
		}
		// One per service and reason per minute, however many are refused.
		if o.perMinute.allow("access.denied\x00" + e.ServiceID + "\x00" + e.RejectReason) {
			o.publish("access.denied", map[string]any{"ServiceID": e.ServiceID, "Reason": e.RejectReason, "IP": e.SourceIP})
		}
	case e.Outcome == observe.OutcomeError && e.KeyService && !e.ClientGone && !e.GRPC:
		// Where the proxy writes its ai.upstream_error audit entry. A gRPC
		// call is no model request: it bypasses the AI gateway, and an error
		// code may be the relay's own answer (H4, a deadline).
		o.upstreamError(e.ServiceID, e.ServiceID, e.Status, 0)
	}
}

// upstreamError counts one failed upstream of a model service and announces
// it. service is the service the request ran under, backend the service of
// the upstream that failed. status 0 (no response) is the label "none" and
// the error "no_response". Through ObserveTraffic the status is what the
// visitor received, so a proxied upstream that did not answer shows as the
// class of the proxy's own answer (5xx), not "none". The dispatcher lets one such event per service
// through per hour.
func (o *observer) upstreamError(service, backend string, status, retries int) {
	if o.Recorder != nil {
		o.Recorder.IncAIUpstreamError(backend, status)
	}
	if ev := o.events(); ev != nil {
		ev.EmitAIUpstreamError(service, backend, status, upstreamErrorText(status), retries)
	}
}

// upstreamErrorText is the "error" of an ai.upstream_error event: what
// happened in Burrow's own words, never the upstream's.
func upstreamErrorText(status int) string {
	if status == 0 {
		return "no_response"
	}
	return "http_" + strconv.Itoa(status)
}

// ---------------------------------------------------------------------------
// MCP tool calls (observe.ToolCallObserver)
// ---------------------------------------------------------------------------

// ObserveToolCall counts one finished tools/call per source and outcome. The
// tool's name is not a label.
func (o *observer) ObserveToolCall(e observe.ToolCall) {
	if o == nil {
		return
	}
	defer o.recovered("tool_call", e.Outcome)
	if o.Recorder != nil {
		o.Recorder.IncMCPToolCall(e.Source, e.Outcome)
	}
}

// ---------------------------------------------------------------------------
// Model requests (aigw.RequestObserver)
// ---------------------------------------------------------------------------

// ObserveModelRequest counts one request the AI chain finished.
func (o *observer) ObserveModelRequest(e observe.ModelRequest) {
	if o == nil {
		return
	}
	defer o.recovered("model_request", e.Outcome)
	key := e.APIKeyID
	if key == "" && e.GatewayKeyID != "" {
		key = aigw.GatewayKeySubjectPrefix + e.GatewayKeyID
	}
	if r := o.Recorder; r != nil {
		if e.Metered {
			r.AddAITokensIn(e.ServiceID, key, int64(e.TokensIn))
			r.AddAITokensOut(e.ServiceID, key, int64(e.TokensOut))
			r.AddAICostUSD(e.ServiceID, key, o.costUSD(e))
		}
		switch e.Cache {
		case observe.CacheHit:
			r.IncAICacheHit(e.ServiceID)
		case observe.CacheMiss:
			r.IncAICacheMiss(e.ServiceID)
		}
		if e.SemanticAsked {
			r.IncAISemanticLookups(e.ServiceID)
		}
		if e.SemanticHit {
			r.IncAISemanticHits(e.ServiceID, e.SemanticPolicy)
		}
		if e.SemanticErr != "" {
			r.IncAISemanticErrors(e.ServiceID, e.SemanticErr)
		}
		if e.SemanticPromoted {
			r.IncAISemanticPromotions(e.ServiceID)
		}
		for i := 1; i < len(e.Attempts); i++ {
			from, to := e.Attempts[i-1].ProviderSlug, e.Attempts[i].ProviderSlug
			r.IncAIFailover(e.ServiceID, from, to, strconv.FormatBool(from != to))
		}
	}
	for i, a := range e.Attempts {
		if a.Failed {
			o.upstreamError(e.ServiceID, a.ServiceID, a.Status, i)
		}
	}
	if e.SemanticPromoted {
		o.cachePromoted(e)
	}
	for _, red := range e.Redactions {
		o.redactionApplied(e.ServiceID, red.Rule)
	}
	o.guardrailEvent(e)
}

// guardrailEvent announces a guardrail hit as guardrail.refused: a refused
// request (direction "request"), or an answer a pattern matched or the judge
// withheld (direction "answer", action "flag" when it was delivered, "block"
// when it was withheld). tier is "pattern" or "judge"; a judge's event has no
// pattern (its reason is not announced). At most one per service, tier,
// pattern and direction per minute. An answer a pattern flagged before the
// judge withheld it (AnswerFlag) announces the flag first, then the block.
func (o *observer) guardrailEvent(e observe.ModelRequest) {
	if e.AnswerFlag != "" {
		o.announceGuardrail(e.ServiceID, "pattern", e.AnswerFlag, "answer", "flag")
	}
	direction := e.GuardrailDirection
	tier := e.GuardrailTier
	if tier == "" {
		tier = "pattern"
	}
	switch {
	case e.Outcome == observe.ModelGuardrailRefused:
		if direction == "" {
			direction = "request"
		}
	case direction == "answer" && (e.GuardrailPattern != "" || tier == "judge"):
	default:
		return
	}
	o.announceGuardrail(e.ServiceID, tier, e.GuardrailPattern, direction, e.GuardrailAction)
}

// announceGuardrail publishes one guardrail.refused event, at most one per
// service, tier, pattern and direction per minute.
func (o *observer) announceGuardrail(serviceID, tier, pattern, direction, action string) {
	if !o.perMinute.allow("guardrail.refused\x00" + serviceID + "\x00" + tier + "\x00" + pattern + "\x00" + direction) {
		return
	}
	o.publish("guardrail.refused", map[string]any{
		"ServiceID": serviceID, "Pattern": pattern,
		"direction": direction, "tier": tier, "action": action,
	})
}

// tokenless is 1 for a request that was answered with no token count, which
// the cost summary prices per request (see db.UsageRow.TokenlessRequests).
func tokenless(e observe.ModelRequest) int64 {
	if e.Outcome == observe.ModelAnswered && e.Status >= 200 && e.Status < 300 && e.TokensIn == 0 && e.TokensOut == 0 {
		return 1
	}
	return 0
}

// costUSD is what the request cost: what a trusted upstream stated, else the
// price table for its tokens, as the cost summary prices a usage row.
func (o *observer) costUSD(e observe.ModelRequest) float64 {
	if e.ReportedCostUSD != nil {
		return *e.ReportedCostUSD
	}
	if o.Cost == nil {
		return 0
	}
	return o.Cost.RowUSD(db.UsageRow{
		Kind: e.Kind, ProviderSlug: e.ProviderSlug, TargetModel: e.TargetModel,
		PricedTokensIn: int64(e.TokensIn), PricedTokensOut: int64(e.TokensOut),
		TokenlessRequests: tokenless(e),
	})
}

// cachePromoted announces a promotion into the semantic index (the
// dispatcher lets one per service through per hour), writes its audit entry
// at the same rate, and refreshes the index gauges of the service.
func (o *observer) cachePromoted(e observe.ModelRequest) {
	// One event per service per hour, like the dispatcher's own helper. No
	// fingerprint and no similarity: nothing measures either, so the payload
	// does not carry them.
	if o.perHour.allow("ai.cache_promotion\x00" + e.ServiceID) {
		o.publish("ai.cache_promotion", map[string]any{
			"service_id": e.ServiceID, "exact_key_hash": e.PromotedKey, "prompt_fingerprint": "",
		})
	}
	service := e.ServiceID
	if o.Audit != nil && o.perHour.allow("ai.cache.promoted\x00"+service) {
		o.enqueue(func(ctx context.Context) {
			if err := o.Audit.Append(ctx, audit.Event{
				Action: audit.ActionAICachePromoted, SubjectID: service, Result: "ok",
			}); err != nil {
				o.log.Warn("observer: audit write failed", "action", audit.ActionAICachePromoted, "err", err)
			}
		})
	}
	if o.Recorder == nil || o.Chain == nil || !semantic.Available(o.Chain.Semantic) {
		return
	}
	o.enqueue(func(ctx context.Context) {
		st, err := o.Chain.Semantic.Stats(ctx, service)
		if err != nil {
			return // logged by the tier; the gauges keep their last value
		}
		o.Recorder.SetAISemanticIndexEntries(service, int64(st.Entries))
		o.Recorder.SetAISemanticIndexBytes(service, st.OnDiskBytes)
	})
}

// redactionApplied announces that a redaction rule changed a request of the
// service, at most once per service and rule per minute: as the webhook event
// redaction.applied and as the audit entry of the same name (of which the
// audit logger itself keeps one per service and rule per hour).
func (o *observer) redactionApplied(service, rule string) {
	if !o.perMinute.allow("redaction.applied\x00" + service + "\x00" + rule) {
		return
	}
	o.publish("redaction.applied", map[string]any{"ServiceID": service, "RuleID": rule})
	if o.Audit == nil {
		return
	}
	o.enqueue(func(ctx context.Context) {
		if err := o.Audit.Append(ctx, audit.Event{
			Action: audit.ActionRedactionApplied, SubjectID: service, SubjectLabel: rule, Result: "ok",
			Payload:        audit.MustJSON(map[string]string{"rule": rule}),
			AggregationKey: service + "\x00" + rule,
		}); err != nil {
			o.log.Warn("observer: audit write failed", "action", audit.ActionRedactionApplied, "err", err)
		}
	})
}

// ---------------------------------------------------------------------------
// The worker
// ---------------------------------------------------------------------------

// enqueue hands a job to the worker. It never blocks: when the queue is full
// the job is dropped and a warning logged, at most once a minute.
func (o *observer) enqueue(run func(ctx context.Context)) {
	select {
	case o.jobs <- observerJob{run: run}:
	default:
		if o.full.allow("full") {
			o.log.Warn("observer: queue full, dropping work", "capacity", observerQueue)
		}
	}
}

func (o *observer) work() {
	for {
		select {
		case <-o.quit:
			return
		case job := <-o.jobs:
			if job.ack != nil {
				close(job.ack)
				continue
			}
			o.runJob(job.run)
		}
	}
}

// runJob runs one job on a context of its own; a panic is logged and the
// worker goes on.
func (o *observer) runJob(run func(ctx context.Context)) {
	defer func() {
		if rec := recover(); rec != nil {
			o.log.Error("observer: job panicked", "panic", rec)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), observerJobTimeout)
	defer cancel()
	run(ctx)
}

// Flush waits until the jobs queued so far are done, or ctx is. Called at
// shutdown, before the database closes.
func (o *observer) Flush(ctx context.Context) error {
	if o == nil {
		return nil
	}
	ack := make(chan struct{})
	select {
	case o.jobs <- observerJob{ack: ack}:
	case <-ctx.Done():
		return ctx.Err()
	case <-o.quit:
		return nil
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-o.quit:
		return nil
	}
}

// stop ends the worker without draining: after Flush at shutdown, and in the
// cleanup of a test that built one. A stopped observer publishes nothing
// either.
func (o *observer) stop() {
	if o != nil {
		o.stopPublishing()
		o.once.Do(func() { close(o.quit) })
	}
}

// eventBudget lets max events per key through per interval. Like
// eventThrottle it never grows past eventThrottleMaxKeys.
type eventBudget struct {
	mu    sync.Mutex
	win   map[string]budgetWindow
	now   func() time.Time
	every time.Duration
	max   int
}

type budgetWindow struct {
	start time.Time
	n     int
}

func (b *eventBudget) allow(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	w, ok := b.win[key]
	if ok && now.Sub(w.start) < b.every {
		if w.n >= b.max {
			return false
		}
		w.n++
		b.win[key] = w
		return true
	}
	if !ok && len(b.win) >= eventThrottleMaxKeys {
		for k, old := range b.win {
			if now.Sub(old.start) >= b.every {
				delete(b.win, k)
			}
		}
		if len(b.win) >= eventThrottleMaxKeys {
			return false
		}
	}
	if b.win == nil {
		b.win = map[string]budgetWindow{}
	}
	b.win[key] = budgetWindow{start: now, n: 1}
	return true
}
