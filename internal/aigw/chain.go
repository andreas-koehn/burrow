// Package aigw — AI gateway middleware chain.
//
// chain.go composes Tasks 3–9 into a single http.Handler-shaped pipeline
// that the v0.3.0 reverse-proxy ingress calls *between* Access.Allow and
// httputil.ReverseProxy.ServeHTTP, but only for services whose
// service_ai_config has a non-default block.
//
// # Chain order (spec Part B + Task 10 plan)
//
//  1. detect()        — tag the request: openai|anthropic|mcp|unknown
//  2. ipgeo()         — STUB in Task 10 (Task 16 swaps in the real impl)
//  3. ratelimit()     — STUB in Task 10 (Task 11 swaps in the real impl)
//  4. redact()        — Task 5; rewrites the body
//  5. guardrails()    — Task 6; may refuse
//  6. cache.lookup()  — Task 4; on HIT short-circuits
//  7. inspector.pre() — Task 8; buffers req
//  8. route()         — Task 7; picks an upstream (logging seam in Task 10)
//  9. proxy() + inspector.post() + meter() — meter+capture during stream
//
// # Pass-through invariant
//
// When the Chain is invoked with a Service whose AIConfig has every section
// nil (i.e. no AI features configured), ServeHTTP delegates directly to the
// downstream proxy handler without observing or rewriting bytes. The
// v0.3.0 behavior — including FlushInterval=-1 and the Director rewrite —
// is preserved bit-for-bit.
//
// # SSE no-buffering invariant
//
// Cache HITs bypass the proxy entirely (they write a small synthetic body).
// On a MISS the chain wraps the visitor-side ResponseWriter with the
// aimeter Stream + an inspector capture writer; both forward bytes
// immediately and flush after each frame. The underlying ReverseProxy's
// FlushInterval=-1 is untouched.
//
// # Cache key + redaction ordering
//
// Redaction runs BEFORE cache-key computation and BEFORE the metered
// byte-count snapshot, so:
//   - two requests differing only in redacted fields cache-hit each other;
//   - metered bytes_in reflects what upstream saw (the redacted body).
//
// The Burrow-Cache: bypass header (used by the inspector replay-compare
// arm, Task 8) skips the cache Lookup step entirely while leaving every
// other middleware in the path.
package aigw

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/aimeter"
	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/cache/semantic"
	"github.com/ankoehn/burrow/internal/credinject"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/httpduplex"
	"github.com/ankoehn/burrow/internal/inspector"
	"github.com/ankoehn/burrow/internal/quota"
	"github.com/ankoehn/burrow/internal/redact"
	"github.com/ankoehn/burrow/internal/route"
)

// Kind names the detected request shape. Mirrors aimeter.Kind for ease of
// downstream wiring; the values are deliberately the same strings.
type Kind string

const (
	KindOpenAI    Kind = "openai"
	KindAnthropic Kind = "anthropic"
	KindMCP       Kind = "mcp"
	KindUnknown   Kind = "unknown"
)

type kindKey struct{}

// WithKind tells the chain which API format a request is in. An entry point
// that knows the format from its URL (a dialect endpoint) sets it; the chain
// then does not guess from the path and headers, so a Messages request
// without an anthropic-version header is still read as Anthropic.
func WithKind(ctx context.Context, k Kind) context.Context {
	return context.WithValue(ctx, kindKey{}, k)
}

// kindFrom returns the kind set by WithKind.
func kindFrom(ctx context.Context) (Kind, bool) {
	k, ok := ctx.Value(kindKey{}).(Kind)
	return k, ok
}

type noUsageKey struct{}

// WithoutUsage marks a request that is no inference (counting tokens): it
// runs every step of the chain (limits, redaction, guardrails, inspector)
// but writes no usage row, and the response cache neither stores nor serves
// its answer.
func WithoutUsage(ctx context.Context) context.Context {
	return context.WithValue(ctx, noUsageKey{}, true)
}

func withoutUsage(ctx context.Context) bool {
	off, _ := ctx.Value(noUsageKey{}).(bool)
	return off
}

type noCacheKey struct{}

// WithoutCache marks a request that must not be answered from the response
// cache, and whose answer is not stored there. It is the in-relay form of the
// "Burrow-Cache: bypass" request header: a caller inside the relay uses it so
// that nothing of the decision travels on to an upstream.
func WithoutCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, noCacheKey{}, true)
}

// CacheBypassed reports whether WithoutCache was set.
func CacheBypassed(ctx context.Context) bool {
	off, _ := ctx.Value(noCacheKey{}).(bool)
	return off
}

type ownCredentialKey struct{}

// WithOwnCredential marks a request whose upstream handler applies the
// upstream credential itself. The chain then injects none: a handler that may
// send the request to more than one target (a fallback chain) must give each
// target its own credential, and the one bound to the chain's service belongs
// to the first target only.
func WithOwnCredential(ctx context.Context) context.Context {
	return context.WithValue(ctx, ownCredentialKey{}, true)
}

func ownCredential(ctx context.Context) bool {
	own, _ := ctx.Value(ownCredentialKey{}).(bool)
	return own
}

// Service is the per-request input the Chain needs. proxy.Proxy constructs
// one of these from its own *proxy.Resolved + the AI config blob, then
// calls Chain.ServeHTTP. Defining it here (rather than re-exporting
// proxy.Resolved) avoids an import cycle between internal/aigw and
// internal/proxy — the chain depends on nothing in the proxy package.
type Service struct {
	// ID is the stable service identity (matches store.Service.ID).
	ID string
	// OwnerID is the user_id that owns this service (for audit/meter labels).
	OwnerID string
	// LocalHost is the upstream host (typically "127.0.0.1:3000"), used for
	// diagnostic logging only — the proxy handler still owns dial-out.
	LocalHost string
	// APIKeyHeader is the header carrying the upstream's API key. Default
	// "Authorization"; passed straight to cache canonicalisation.
	APIKeyHeader string
	// APIKeyID is the matched api_key row id (when AccessMode == api_key),
	// or "" for other access modes. Used as the cache scope prefix for
	// applies_per: per_api_key and as the usage_events label.
	APIKeyID string
	// AIConfig is the parsed service_ai_config blob. Any nil section means
	// "feature disabled for this service" — Chain treats it as a no-op.
	AIConfig ServiceAIConfig
	// TrustReportedCost says the upstream is one the relay calls itself and
	// whose own figure for the request's cost (usage.cost) is to be believed.
	// Only the /ai/ gateway sets it, and only for a direct provider. Without
	// it a reported cost is ignored and the price table applies: a tunnelled
	// model is run by the key's owner, who could report 0 to stay under a
	// budget or a large figure to exhaust one. A config loader cannot set it.
	TrustReportedCost bool
}

// ServiceAIConfig is the typed view of service_ai_config.config JSON. Any
// nil sub-section disables that middleware. Mirrors spec Part B.7.
type ServiceAIConfig struct {
	Cache      *exact.Settings      // nil = cache disabled
	Semantic   *semantic.Settings   // nil = semantic cache disabled (Task 16)
	Redaction  *RedactionConfig     // nil = redaction disabled
	Guardrails *guardrails.Settings // nil = guardrails disabled
	Inspector  *InspectorConfig     // nil = inspector disabled
	Routing    *route.Policy        // nil = single-backend (v0.3.0 default)
	Anthropic  *AnthropicConfig     // nil = no "anthropic" section (see AnthropicConfig)
}

// RedactionConfig is the per-service redaction toggle. ForLogsOnly = true
// means: redacted body is what the inspector + audit see, but the ORIGINAL
// body is forwarded upstream. ForLogsOnly = false (the default) means the
// redacted body is also what upstream sees.
type RedactionConfig struct {
	Enabled     bool
	ForLogsOnly bool
}

// InspectorConfig is the per-service inspector toggle.
type InspectorConfig struct {
	Enabled     bool
	MaxRequests int // capacity of the per-service ring; 0 → default
}

// AnthropicConfig is the "anthropic" section of a service's AI config. It
// was meant to switch on a per-service Anthropic ↔ OpenAI adapter; that
// adapter was never put in a request's path and is gone. Formats are
// translated per gateway model now (internal/aigw/translate, a model's
// "translate" flag), never per service.
//
// What the section does, and keeps doing for the services that have it: like
// every other section it takes the service off the pass-through path, so its
// requests run through the chain — forwarded to the upstream as they came,
// and metered (an Anthropic-shaped request as KindAnthropic). Nothing is
// rewritten.
type AnthropicConfig struct {
	// Enabled is what the stored section says. Nothing in the chain reads
	// it: the section counts by being there.
	Enabled bool
}

// ConfigLoader resolves the per-service AI config blob (decoded into
// ServiceAIConfig). Returning a zero-value config + ok=false sends the
// request through the v0.3.0 pass-through path. Errors are logged + treated
// as ok=false (fail-open).
//
// cmd/server (Task 25) wires the concrete implementation backed by the
// service_ai_config table; tests pass a small in-memory stub.
type ConfigLoader interface {
	LoadAIConfig(ctx context.Context, serviceID string) (Service, bool, error)
}

// DefaultMaxRequestBodyBytes caps how much of an inbound request body the
// chain will buffer into memory before short-circuiting with 413. 8 MiB is
// chosen as a deliberately generous ceiling for chat-style payloads while
// still bounding worst-case heap growth — large multi-MB JSON bodies can
// occur for embeddings batches and Anthropic multimodal content, but a
// single request larger than this is almost certainly accidental.
const DefaultMaxRequestBodyBytes int64 = 8 * 1024 * 1024

// Chain wires Tasks 3–9 into a single http.Handler-shaped pipeline. Construct
// with NewChain; pass it as a dependency to cmd/server and let the proxy
// layer dispatch into ServeHTTP for services that have an AI config.
//
// Chain is safe for concurrent use after construction.
type Chain struct {
	Loader       ConfigLoader         // nil = no AI features wired
	Cache        *exact.Cache         // nil = cache feature unavailable
	Semantic     semantic.Cache       // nil = semantic cache unavailable (NoopCache skips silently)
	CredInjector *credinject.Injector // nil = credential injection disabled
	Redact       *redact.Engine       // nil = redaction feature unavailable
	Guardrails   *guardrails.Engine   // nil = guardrails feature unavailable
	Inspector    *inspector.Manager   // nil = inspector feature unavailable
	Router       *route.Router        // nil = routing strategies unavailable
	Meter        aimeter.Sink         // nil = metering disabled
	Log          *slog.Logger

	// IPGeo and RateLimit are stubs for Tasks 16 and 11. They wrap the
	// downstream handler; the default (nil) is treated as pure pass-through.
	IPGeo     func(http.Handler) http.Handler
	RateLimit func(http.Handler) http.Handler

	// OnGuardrailRefuse is called once per refused request with the service,
	// the id of the pattern that matched (never the matched text) and the
	// action taken ("refuse_403" or "refuse_safe"). It runs on the request's
	// goroutine before the refusal is written, so it must not block (hand
	// the work to something else); a panic in it is recovered. The context
	// is the request's. nil = not reported.
	OnGuardrailRefuse func(ctx context.Context, serviceID, pattern, action string)

	// MaxRequestBodyBytes bounds how much of an inbound request body the
	// chain will buffer before short-circuiting with 413. 0 selects
	// DefaultMaxRequestBodyBytes. Tests override this to small values.
	MaxRequestBodyBytes int64
}

// NewChain returns a Chain with the given dependencies. Any nil dep is
// treated as "feature unavailable" and the corresponding step is skipped at
// request time — operators can wire only the subset they care about.
//
// Semantic promotion is NOT wired via exact.Cache.SetOnMiss. It happens
// inline in the MISS path of run() after a successful Store, with the full
// service ID, prompt bytes, and Settings available.
func NewChain(
	cache *exact.Cache,
	sem semantic.Cache,
	credInjector *credinject.Injector,
	redactEngine *redact.Engine,
	guardrailsEngine *guardrails.Engine,
	inspectorMgr *inspector.Manager,
	router *route.Router,
	meter aimeter.Sink,
	log *slog.Logger,
) *Chain {
	if log == nil {
		log = slog.Default()
	}
	ch := &Chain{
		Cache:        cache,
		Semantic:     sem,
		CredInjector: credInjector,
		Redact:       redactEngine,
		Guardrails:   guardrailsEngine,
		Inspector:    inspectorMgr,
		Router:       router,
		Meter:        meter,
		Log:          log,
	}
	return ch
}

// IsAIPassThrough reports whether the given AIConfig has every section
// nil (i.e. the service has no AI features enabled). The proxy layer
// short-circuits to the v0.3.0 path when this returns true.
func IsAIPassThrough(c ServiceAIConfig) bool {
	return c.Cache == nil && c.Redaction == nil && c.Guardrails == nil &&
		c.Inspector == nil && c.Routing == nil && c.Anthropic == nil
}

// ServeHTTP runs the middleware chain for one request. svc carries the
// resolved service identity + AI config; proxyHandler is the v0.3.0
// ReverseProxy handler that the chain calls into on a cache MISS / no
// short-circuit.
//
// Pure pass-through when svc.AIConfig has every section nil — caller is
// expected to check IsAIPassThrough before dispatching, but we re-check
// here so misuse is safe.
func (c *Chain) ServeHTTP(w http.ResponseWriter, r *http.Request, svc Service, proxyHandler http.Handler) {
	// ---------------------------------------------------------------
	// Step 0 (pre-chain): rate-limit — applied to ALL proxied traffic,
	// regardless of whether the service has AI config. Moving this here
	// (before the IsAIPassThrough guard) ensures that services without a
	// service_ai_config row still have quota enforced. The duplicate block
	// inside run() has been removed.
	// ---------------------------------------------------------------
	r, ok := c.allow(w, r, svc)
	if !ok {
		return
	}

	if IsAIPassThrough(svc.AIConfig) {
		// The body goes to the upstream as it arrives, unbuffered. Go's
		// HTTP/1 server consumes what is left of a request body when the
		// response headers are written; an upstream that answers before it
		// has read the body would then get a short one, lose its connection,
		// and the client a truncated response. Full duplex keeps the body
		// readable; httpduplex also keeps the transport's reads of the body
		// from outliving this handler. (run buffers the body and needs none
		// of this.)
		httpduplex.Serve(w, r, proxyHandler)
		return
	}
	c.run(w, r, svc, proxyHandler, false)
}

// allow runs the rate limiter for one request. It returns the request with
// the quota subjects attached, and false when the limiter already answered.
func (c *Chain) allow(w http.ResponseWriter, r *http.Request, svc Service) (*http.Request, bool) {
	if c.RateLimit == nil {
		return r, true
	}
	who := quota.Subjects{
		ServiceID: svc.ID,
		APIKeyID:  keySubject(r, svc),
	}
	// A request the gateway routed names its gateway key and the requested
	// model, under the one name it has on every door ("<provider>/<id>" on
	// a provider path): gateway_key and model limits go by them, as budgets
	// and the usage row do.
	if ri, ok := RouteFrom(r.Context()); ok {
		who.GatewayKeyID, who.Model = ri.GatewayKeyID, ri.RequestedModel
	}
	r = r.WithContext(quota.WithSubjects(r.Context(), who))
	passed := false
	c.RateLimit(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		passed = true
	})).ServeHTTP(w, r)
	return r, passed
}

// Dispatch is the entry point the proxy calls (satisfies the
// proxy.AIChain interface). It resolves the AI config via Loader, falls
// through to proxyHandler when no config exists (or the config is fully
// pass-through), and otherwise runs the chain.
//
// The proxy passes primitive arguments — keeping internal/aigw entirely
// out of internal/proxy's imports, and vice versa.
func (c *Chain) Dispatch(w http.ResponseWriter, r *http.Request,
	serviceID, localHost, apiKeyHeader, apiKeyID string,
	proxyHandler http.Handler,
) {
	c.ServeHTTP(w, r, c.resolve(r, serviceID, localHost, apiKeyHeader, apiKeyID), proxyHandler)
}

// DispatchMetered is Dispatch for callers that must account for every
// request (the /ai/ gateway): it runs the chain, and so records usage, even
// when the service has no AI features configured. trustReportedCost is
// Service.TrustReportedCost for this request.
func (c *Chain) DispatchMetered(w http.ResponseWriter, r *http.Request,
	serviceID, localHost, apiKeyHeader, apiKeyID string,
	trustReportedCost bool,
	proxyHandler http.Handler,
) {
	svc := c.resolve(r, serviceID, localHost, apiKeyHeader, apiKeyID)
	svc.TrustReportedCost = trustReportedCost
	r, ok := c.allow(w, r, svc)
	if !ok {
		return
	}
	c.run(w, r, svc, proxyHandler, false)
}

// resolve builds the Service for one request: the caller's identity fields
// plus the AI config from Loader (zero config when none exists or the load
// fails).
func (c *Chain) resolve(r *http.Request, serviceID, localHost, apiKeyHeader, apiKeyID string) Service {
	svc := Service{
		ID:           serviceID,
		LocalHost:    localHost,
		APIKeyHeader: apiKeyHeader,
		APIKeyID:     apiKeyID,
	}
	if c.Loader != nil {
		loaded, ok, err := c.Loader.LoadAIConfig(r.Context(), serviceID)
		if err != nil {
			c.Log.Warn("aigw: load ai config failed",
				slog.String("service_id", serviceID),
				slog.String("err", err.Error()))
		}
		if ok {
			// Take the loader's full Service (which carries AIConfig) but
			// preserve any caller-supplied identity fields the loader didn't
			// set — keeps the contract explicit.
			if loaded.ID == "" {
				loaded.ID = serviceID
			}
			if loaded.APIKeyHeader == "" {
				loaded.APIKeyHeader = apiKeyHeader
			}
			svc = loaded
			// The loader never knows which key authorised the request, and
			// whether the upstream is trusted is the caller's decision.
			svc.APIKeyID = apiKeyID
			svc.TrustReportedCost = false
		}
	}
	return svc
}

// Replay re-fires r through the entire chain and returns the resulting
// inspector entry. This satisfies the InspectorReplayer interface that
// Task 8's inspector handler consumes (Task 25 plumbs the wiring in
// cmd/server/main.go).
//
// When the caller has pre-set "Burrow-Cache: bypass" on r, the chain skips
// the cache Lookup step (see Task 8 replay-compare). Every other step
// runs as in a normal request.
//
// proxyHandler is the same downstream ReverseProxy handler used by
// ServeHTTP. The Replayer caller (cmd/server) is responsible for providing
// one wired up to the same StreamDialer.
func (c *Chain) Replay(ctx context.Context, svc Service, r *http.Request, proxyHandler http.Handler) (inspector.Entry, error) {
	if c.Inspector == nil {
		return inspector.Entry{}, errors.New("aigw: inspector manager not configured")
	}
	maxReq := inspector.DefaultMaxRequests
	if svc.AIConfig.Inspector != nil && svc.AIConfig.Inspector.MaxRequests > 0 {
		maxReq = svc.AIConfig.Inspector.MaxRequests
	}
	ring := c.Inspector.GetOrCreate(svc.ID, maxReq)

	// Subscribe before firing so the entry the chain captures is observable.
	sub, cancel := ring.Subscribe()
	defer cancel()

	// Use the supplied context so callers can cancel the replay.
	r = r.WithContext(ctx)

	// Run the chain into a buffer so we don't ship anything anywhere real.
	rec := &replayRecorder{header: http.Header{}, body: &bytes.Buffer{}}
	c.run(rec, r, svc, proxyHandler, true)

	// The chain's Capture call lands on the bus before run returns; with the
	// buffer of 16 it's safe to read after run.
	select {
	case e := <-sub:
		return e, nil
	default:
		// Inspector might be disabled for this service; synthesise a minimal
		// entry from the recorder so the API handler still has something.
		return inspector.Entry{
			ID:        inspector.NewID(),
			ServiceID: svc.ID,
			TS:        time.Now().UTC(),
			Method:    r.Method,
			Path:      r.URL.Path,
			Status:    rec.statusCode,
		}, nil
	}
}

// run is the shared dispatch path used by both ServeHTTP and Replay. The
// fromReplay flag exists only to flip the inspector entry's downstream
// labelling; the rest of the steps run unchanged.
func (c *Chain) run(w http.ResponseWriter, r *http.Request, svc Service, proxyHandler http.Handler, fromReplay bool) {
	started := time.Now()
	cfg := svc.AIConfig

	// ---------------------------------------------------------------
	// Step 0: cap inbound body so a multi-GB POST cannot OOM the
	// process. v0.3.0 streamed bodies untouched; once any AI feature
	// is enabled we must buffer to inspect/redact/cache-key, but the
	// buffer is hard-capped here. Over-cap → 413 short-circuit before
	// any downstream work.
	// ---------------------------------------------------------------
	maxReqBody := c.MaxRequestBodyBytes
	if maxReqBody <= 0 {
		maxReqBody = DefaultMaxRequestBodyBytes
	}
	body, overflow, _ := readBodyLimited(r, maxReqBody)
	if overflow {
		writeError(w, r, http.StatusRequestEntityTooLarge, "request body too large", "request_too_large", "request body too large")
		c.Log.Info("aigw: request body exceeds limit",
			slog.String("service_id", svc.ID),
			slog.Int64("limit_bytes", maxReqBody),
		)
		return
	}

	// ---------------------------------------------------------------
	// Step 1: detect — tag the request kind for metering + logging.
	// ---------------------------------------------------------------
	kind := DetectKind(r, body)
	forcedKind, kindForced := kindFrom(r.Context())
	if kindForced {
		kind = forcedKind // the entry point knows; detection is a guess
	}

	// ---------------------------------------------------------------
	// Step 2: ipgeo — STUB. Task 16 swaps in the real impl.
	//
	// For pure-pass-through we never construct the wrapper; this keeps the
	// happy-path allocation-free. When set on the Chain, the wrapper is
	// expected to short-circuit 403 on deny and call next on allow.
	// ---------------------------------------------------------------

	// ---------------------------------------------------------------
	// Step 3: redact — rewrites the body before cache + metering.
	// (Rate-limit was Step 3 previously; it is now applied in ServeHTTP
	// before the IsAIPassThrough guard so quota covers all proxied traffic.)
	// ---------------------------------------------------------------
	var (
		redactHits []redact.RuleHit
		redactDrop *redact.Rule
	)
	redactedBody := body
	if cfg.Redaction != nil && cfg.Redaction.Enabled && c.Redact != nil && len(body) > 0 {
		var err error
		redactedBody, redactDrop, redactHits, err = c.Redact.Apply(body, redact.ScopeRequestBody)
		if err != nil {
			c.Log.Warn("aigw: redact apply failed", slog.String("service_id", svc.ID), slog.String("err", err.Error()))
			// Fail open: forward the original body. Redaction errors should
			// never break the proxy.
			redactedBody = body
		}
		if redactDrop != nil {
			// Drop-action rule fired — short-circuit with 400 redaction.drop.
			writeError(w, r, http.StatusBadRequest, "redaction.drop", "invalid_request", "the request was refused by a redaction rule")
			c.captureEntry(svc, r, body, redactedBody, redactHits, kind, http.StatusBadRequest, nil, nil, 0, false, "MISS", fromReplay)
			return
		}
	}

	// What the upstream sees: by default the redacted body, BUT when
	// ForLogsOnly is set the original body goes upstream and the redacted
	// copy is what the inspector + audit see.
	upstreamBody := redactedBody
	if cfg.Redaction != nil && cfg.Redaction.ForLogsOnly {
		upstreamBody = body
	}

	// ---------------------------------------------------------------
	// Step 5: guardrails — may refuse with 403/200-safe-refusal.
	// ---------------------------------------------------------------
	if cfg.Guardrails != nil && cfg.Guardrails.Enabled && c.Guardrails != nil && len(redactedBody) > 0 {
		hit, pattern := c.Guardrails.Inspect(redactedBody)
		if hit {
			switch cfg.Guardrails.Action {
			case guardrails.ActionRefuse403, "":
				c.guardrailRefused(r, svc, pattern, guardrails.ActionRefuse403)
				writeError(w, r, http.StatusForbidden, "guardrail.refuse", "forbidden", "the request was refused by a guardrail")
				c.Log.Info("aigw: guardrail refuse",
					slog.String("service_id", svc.ID),
					slog.String("pattern", pattern),
				)
				c.captureEntry(svc, r, body, redactedBody, redactHits, kind, http.StatusForbidden, nil, nil, 0, false, "MISS", fromReplay)
				return
			case guardrails.ActionRefuseSafe:
				c.guardrailRefused(r, svc, pattern, guardrails.ActionRefuseSafe)
				ew := ErrorWriterFrom(r.Context())
				streamed := false
				var refusalBody []byte
				var hdr http.Header
				switch {
				case kindForced:
					// A dialect endpoint states the kind itself and gets a
					// well-formed refusal in its own format: with usage, the
					// model the client asked for, and as an event stream when
					// it asked for one. Where the format has no such answer
					// (counting tokens, embeddings, …) the refusal is the
					// endpoint's own error.
					var ok bool
					refusalBody, hdr, streamed, ok = dialectRefusal(kind, r, body)
					if !ok && ew != nil {
						ew(w, http.StatusForbidden, "forbidden", "the request was refused by a guardrail")
						c.captureEntry(svc, r, body, redactedBody, redactHits, kind, http.StatusForbidden, nil, nil, 0, false, "MISS", fromReplay)
						return
					}
					if !ok {
						refusalBody, hdr = safeRefusalBody(kind)
					}
				case ew != nil && kind != KindAnthropic && kind != KindOpenAI:
					// No upstream answer to imitate for a kind the chain does
					// not recognise: an entry point with its own error shape
					// gets a plain refusal instead of the generic envelope.
					ew(w, http.StatusForbidden, "forbidden", "the request was refused by a guardrail")
					c.captureEntry(svc, r, body, redactedBody, redactHits, kind, http.StatusForbidden, nil, nil, 0, false, "MISS", fromReplay)
					return
				default:
					refusalBody, hdr = safeRefusalBody(kind)
				}
				for k, vs := range hdr {
					for _, v := range vs {
						w.Header().Add(k, v)
					}
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(refusalBody)
				// Pass the ORIGINAL + redacted request bodies so operators
				// investigating refusals can see what was sent.
				// TruncateRequest will cap whatever we pass.
				c.captureEntry(svc, r, body, redactedBody, redactHits, kind, http.StatusOK, refusalBody, hdr, 0, streamed, "MISS", fromReplay)
				return
			case guardrails.ActionLogOnly:
				// Fall through; just record the hit in logs.
				c.Log.Info("aigw: guardrail log_only",
					slog.String("service_id", svc.ID),
					slog.String("pattern", pattern),
				)
			}
		}
	}

	// ---------------------------------------------------------------
	// Step 6: cache lookup — short-circuits with Burrow-Cache: HIT.
	// Skipped when Burrow-Cache: bypass is set (Task 8 replay-compare).
	// ---------------------------------------------------------------
	cacheStatus := "SKIP"
	// A request without usage is no inference; its answer is never taken
	// from or put into the cache of inference answers (the semantic tier
	// keys on the body alone and could not tell the two apart).
	bypass := strings.EqualFold(r.Header.Get("Burrow-Cache"), "bypass") || withoutUsage(r.Context()) || CacheBypassed(r.Context())
	if cfg.Cache != nil && cfg.Cache.Enabled && c.Cache != nil && !bypass {
		key := buildCacheKey(svc, r, redactedBody, *cfg.Cache)
		entry, hit, err := c.Cache.Lookup(r.Context(), key)
		if err != nil {
			c.Log.Warn("aigw: cache lookup failed",
				slog.String("service_id", svc.ID),
				slog.String("err", err.Error()))
		}
		if hit {
			cacheStatus = "HIT"
			c.serveCacheHit(w, entry)
			elapsed := time.Since(started)
			// Convert the cached entry's flat header map to http.Header so
			// the inspector entry's RespHeaders mirrors what the visitor
			// saw (Content-Type drives the replay-compare diff path).
			cacheHdr := make(http.Header, len(entry.Headers))
			for k, v := range entry.Headers {
				cacheHdr.Set(k, v)
			}
			c.captureEntry(svc, r, body, redactedBody, redactHits, kind, entry.Status, entry.Body, cacheHdr, 0, false, cacheStatus, fromReplay)
			c.recordMeter(r.Context(), svc, kind, 0, 0, int64(len(redactedBody)), int64(len(entry.Body)), false, true, entry.Status, cachedCost(), elapsed)
			return
		}
		cacheStatus = "MISS"
	}

	// ---------------------------------------------------------------
	// Step 6b: semantic cache lookup — runs on exact-cache MISS.
	// When cfg.Semantic is enabled and c.Semantic is wired, we ask the
	// vector tier whether any stored prompt is sufficiently similar to
	// the current one. On a hit:
	//   - fallback_policy=return_cached_marked → serve with Burrow-Cache: similar
	//   - fallback_policy=treat_as_miss        → fall through to MISS path
	// Any error degrades silently to the MISS path (spec A.1.6).
	// ---------------------------------------------------------------
	if cacheStatus == "MISS" && cfg.Semantic != nil && cfg.Semantic.Enabled && c.Semantic != nil {
		candidate, semHit, semErr := c.Semantic.Lookup(r.Context(), svc.ID, redactedBody, *cfg.Semantic)
		if semErr != nil {
			c.Log.Warn("aigw: semantic cache lookup failed",
				slog.String("service_id", svc.ID),
				slog.String("err", semErr.Error()))
		}
		if semHit && cfg.Semantic.FallbackPolicy == "return_cached_marked" {
			// Fetch the backing exact-cache entry using the candidate's key hash.
			exactEntry, exactHit, exactErr := c.Cache.Lookup(r.Context(), candidate.ExactKeyHash)
			if exactErr != nil {
				c.Log.Warn("aigw: semantic exact entry fetch failed",
					slog.String("service_id", svc.ID),
					slog.String("err", exactErr.Error()))
			}
			if exactHit {
				// Serve the semantically-similar response with spec-mandated headers.
				for k, v := range exactEntry.Headers {
					w.Header().Set(k, v)
				}
				w.Header().Set("Burrow-Cache", "similar")
				simStr := strconv.FormatFloat(candidate.Similarity, 'f', 2, 64)
				w.Header().Set("Burrow-Cache-Similarity", simStr)
				age := int(time.Since(exactEntry.CreatedAt) / time.Second)
				if age < 0 {
					age = 0
				}
				w.Header().Set("Burrow-Cache-Age", strconv.Itoa(age))
				status := exactEntry.Status
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = w.Write(exactEntry.Body)
				elapsed := time.Since(started)
				cacheHdr := make(http.Header, len(exactEntry.Headers))
				for k, v := range exactEntry.Headers {
					cacheHdr.Set(k, v)
				}
				c.captureEntry(svc, r, body, redactedBody, redactHits, kind, status, exactEntry.Body, cacheHdr, 0, false, "similar", fromReplay)
				c.recordMeter(r.Context(), svc, kind, 0, 0, int64(len(redactedBody)), int64(len(exactEntry.Body)), false, true, status, cachedCost(), elapsed)
				return
			}
		}
		// treat_as_miss, exact fetch miss, or error → fall through.
	}

	// ---------------------------------------------------------------
	// Step 7: inspector.pre — buffer request meta + body. Performed
	// implicitly by capturing the entry on response completion below.
	// ---------------------------------------------------------------

	// ---------------------------------------------------------------
	// Step 8: route — log-only seam for Task 10. Task 12 wires the
	// actual upstream-pick into the proxy Director. For now we log
	// the decision so operators can see the strategy firing.
	// ---------------------------------------------------------------
	if cfg.Routing != nil && c.Router != nil && len(cfg.Routing.Backends) > 0 {
		rc := route.RouteContext{
			Kind:           string(kind),
			Model:          extractModelFromBody(redactedBody),
			IdempotencyKey: r.Header.Get("Idempotency-Key"),
			APIKeyID:       keySubject(r, svc),
			HeaderValues:   firstValues(r.Header),
		}
		if pick, err := c.Router.Pick(r.Context(), *cfg.Routing, rc); err == nil {
			c.Log.Debug("aigw: route picked",
				slog.String("service_id", pick.ServiceID),
				slog.String("model", pick.ConcreteModel),
			)
		}
	}

	// ---------------------------------------------------------------
	// Step 8b: credinject — strips the visitor's credential header and
	// injects the upstream's real credential (spec B.3). Runs AFTER
	// route so the injected header format matches the chosen upstream.
	// Skipped when c.CredInjector is nil (nil-safe), and when the upstream
	// handler applies the credential itself (WithOwnCredential).
	// ---------------------------------------------------------------
	if c.CredInjector != nil && !ownCredential(r.Context()) {
		if _, err := c.CredInjector.Apply(r.Context(), svc.ID, r); err != nil {
			c.Log.Warn("aigw: credential injection failed",
				slog.String("service_id", svc.ID),
				slog.String("err", err.Error()))
			// Fail open: continue without injected credential so the upstream
			// receives the request unchanged. The upstream will 401/403 if its
			// credential is absent.
		}
	}

	// ---------------------------------------------------------------
	// Step 9: proxy + inspector.post + meter — wrap the visitor writer
	// and call the downstream handler.
	// ---------------------------------------------------------------

	// Replace the request body with the (possibly redacted, possibly
	// original) bytes the upstream should see. Reset Content-Length so
	// the ReverseProxy doesn't keep the wrong size.
	if len(upstreamBody) != len(body) {
		r.ContentLength = int64(len(upstreamBody))
		r.Header.Set("Content-Length", strconv.Itoa(len(upstreamBody)))
	}
	r.Body = io.NopCloser(bytes.NewReader(upstreamBody))

	// Wrap the visitor-side writer in:
	//  - an aimeter Stream (forwards immediately + flushes; tracks tokens)
	//  - an inspector capture buffer (truncated at MaxRespBodyBytes)
	capw := newCapture(w, inspector.MaxRespBodyBytes)
	stream := aimeter.WrapResponse(capw, aimeter.Kind(kind))

	wrapped := &chainResponseWriter{
		ResponseWriter: w,
		stream:         stream,
		statusCode:     0,
	}

	// Under a real http.Server a ReverseProxy panics http.ErrAbortHandler
	// when the client hangs up mid-response. The tokens were spent all the
	// same, so the bookkeeping below still runs; the panic is then raised
	// again, unchanged, so the server aborts the connection as before.
	aborted, abort := serveUpstream(proxyHandler, wrapped, r)
	// The response ends here; what follows (cache store, semantic promote,
	// inspector capture) is the relay's own bookkeeping and is not latency.
	elapsed := time.Since(started)
	if aborted {
		defer panic(abort)
	}
	_ = stream.Close()

	// Read counters before recording the meter row.
	bytesIn, bytesOut := stream.Bytes()
	tokens := stream.Tokens()

	// Non-stream upstream responses carry usage in the JSON body itself
	// (no `data: ` prefix → openAIParser.inspect won't fire). For these,
	// reparse the captured body so usage_events records authoritative
	// token counts instead of the byte-estimate fallback.
	tokens = bodyTokens(kind, wrapped.Header(), capw, tokens)

	// Cost the upstream itself reports (OpenRouter). Streamed responses carry
	// it in the final usage chunk; plain JSON responses in the body. It is
	// read from the bytes already copied to the client, never from a second
	// pass that could hold the response back. A response that reports none,
	// or was cut off before the chunk that does, leaves this nil and the
	// price table applies. So does every upstream that is not trusted with
	// its own price (see Service.TrustReportedCost).
	var reportedUSD *float64
	if svc.TrustReportedCost {
		if usd, ok := stream.Cost(); ok {
			reportedUSD = &usd
		} else if kind == KindOpenAI && !isStreamedResponse(wrapped.Header()) && !capw.truncated() {
			if usd, ok := aimeter.ParseOpenAICost(capw.bytes()); ok {
				reportedUSD = &usd
			}
		}
	}

	// Should we cache the response? Only when:
	//  - cache feature is enabled for this service
	//  - upstream returned 2xx
	//  - response is not streamed (no SSE / chunked transfer encoding)
	//  - upstream provided an explicit Content-Length (absence implies
	//    Go-auto-chunked / unknown-size — must not be cached)
	//  - capture buffer did NOT hit its cap (otherwise the cached body
	//    would be truncated and a later HIT would serve an incomplete
	//    body with the original Content-Length → silent corruption)
	//  - the response was not cut off (an aborted response is a fragment)
	//  - the answer is the first target's: the key is that target's service
	//    and model, and a fallback target's answer is neither's (this also
	//    keeps it out of the semantic index, which is filled from here)
	fallbackAnswer := false
	if ri, ok := RouteFrom(r.Context()); ok {
		fallbackAnswer = ri.Fallback
		if ri.UpstreamUsage {
			// The answer was translated on its way here: what passed through
			// this writer is the caller's format, with the caller's view of
			// the usage (or none). The gateway read the upstream's own bytes.
			tokens = aimeter.Tokens{In: ri.TokensIn, Out: ri.TokensOut, Total: ri.TokensIn + ri.TokensOut}
		}
	}
	if !aborted && !fallbackAnswer && cfg.Cache != nil && cfg.Cache.Enabled && c.Cache != nil && !bypass &&
		wrapped.statusCode >= 200 && wrapped.statusCode < 300 &&
		!isStreamedResponse(wrapped.Header()) &&
		wrapped.Header().Get("Content-Length") != "" {
		if capw.truncated() {
			c.Log.Info("aigw: cache store skipped (response exceeds inspector capture cap)",
				slog.String("service_id", svc.ID),
				slog.Int("inspector_cap_bytes", inspector.MaxRespBodyBytes),
			)
		} else {
			cachedBody := capw.bytes()
			if int64(len(cachedBody)) <= int64(cfg.Cache.MaxPerEntryKB)*1024 {
				key := buildCacheKey(svc, r, redactedBody, *cfg.Cache)
				entry := exact.Entry{
					Body:       cachedBody,
					Status:     wrapped.statusCode,
					Headers:    cacheableHeaders(wrapped.Header()),
					CreatedAt:  time.Now().UTC(),
					TTLSeconds: cfg.Cache.TTLSeconds,
				}
				storeCtx, cancelStore := detached(r.Context())
				storeErr := c.Cache.Store(storeCtx, key, entry)
				cancelStore()
				if storeErr != nil {
					c.Log.Warn("aigw: cache store failed",
						slog.String("service_id", svc.ID),
						slog.String("err", storeErr.Error()))
				}
				// After a successful Store, promote into the semantic index so
				// future similar prompts can hit via the vector-similarity tier.
				// Promote is called synchronously here (post-stream, post-Store)
				// so it never delays the visitor path. The semantic cache
				// degrades silently on error (spec A.1.6).
				if storeErr == nil && cfg.Semantic != nil && cfg.Semantic.Enabled &&
					cfg.Semantic.PromoteOnMiss && c.Semantic != nil {
					promoteCtx := context.Background()
					if err := c.Semantic.Promote(promoteCtx, svc.ID, key, redactedBody, *cfg.Semantic); err != nil {
						c.Log.Warn("aigw: semantic promote failed",
							slog.String("service_id", svc.ID),
							slog.String("err", err.Error()))
					}
				}
			}
		}
	}

	// Inspector capture: only when this service has the feature enabled.
	// An aborted response is captured too, with the bytes that were sent.
	c.captureEntry(svc, r, body, redactedBody, redactHits, kind,
		wrapped.statusCode, capw.bytes(), wrapped.Header(), bytesIn, isStreamedResponse(wrapped.Header()), cacheStatus, fromReplay)

	// Meter the request — always, even on non-2xx or after an abort, so
	// quota usage reflects reality. cache_hit is false on this MISS path; cacheStatus tells the
	// inspector "MISS" while still recording the bytes/tokens.
	c.recordMeter(r.Context(), svc, kind,
		tokens.In, tokens.Out,
		int64(len(redactedBody)), bytesOut,
		isStreamedResponse(wrapped.Header()), false,
		wrapped.statusCode,
		reportedUSD,
		elapsed,
	)
}

// bodyTokens returns the usage of an answer that came in one piece, read
// from its captured body, and tokens when the answer was streamed, was too
// large to capture or names no usage.
func bodyTokens(kind Kind, header http.Header, capw *capture, tokens aimeter.Tokens) aimeter.Tokens {
	if isStreamedResponse(header) || capw.truncated() {
		return tokens
	}
	body := capw.bytes()
	if len(body) == 0 {
		return tokens
	}
	var t aimeter.Tokens
	switch kind {
	case KindOpenAI:
		t = aimeter.ParseOpenAIBody(body)
	case KindAnthropic:
		t = aimeter.ParseAnthropicBody(body)
	}
	if t.Total > 0 || t.In > 0 || t.Out > 0 {
		return t
	}
	return tokens
}

// UpstreamUsage meters an upstream answer that does not pass through the
// chain's own writer as it is: the gateway translates it for a caller of
// another format, and the usage row must hold the upstream's figures. The
// gateway writes the upstream's bytes to it as they arrive and hands the
// result to the route (Route.SetUpstreamUsage). It reads an answer exactly as
// the chain reads a native one of that kind: the usage frames of a stream as
// they pass (so a stream that is cut leaves what had arrived), the body of an
// answer in one piece, and the byte estimate when no usage was seen. It
// keeps at most the inspector's capture size of the body and logs nothing.
type UpstreamUsage struct {
	kind   Kind
	capw   *capture
	stream *aimeter.Stream
}

// NewUpstreamUsage meters an answer of an upstream that speaks kind.
func NewUpstreamUsage(kind Kind) *UpstreamUsage {
	capw := newCapture(io.Discard, inspector.MaxRespBodyBytes)
	return &UpstreamUsage{kind: kind, capw: capw, stream: aimeter.WrapResponse(capw, aimeter.Kind(kind))}
}

// Write takes the next bytes of the upstream's body.
func (u *UpstreamUsage) Write(p []byte) (int, error) { return u.stream.Write(p) }

// Tokens ends the answer and returns its usage. header is the upstream's
// response header: it says whether the answer was a stream.
func (u *UpstreamUsage) Tokens(header http.Header) (in, out int) {
	_ = u.stream.Close()
	t := bodyTokens(u.kind, header, u.capw, u.stream.Tokens())
	return t.In, t.Out
}

// cachedCost is the cost of an answer served from the cache: nothing was
// spent upstream, so the row says zero instead of leaving it to the price
// table. This is the relay's own knowledge, not an upstream's claim, so it
// does not depend on Service.TrustReportedCost.
func cachedCost() *float64 {
	zero := 0.0
	return &zero
}

// serveUpstream calls the upstream handler and reports a panic instead of
// letting it unwind, so the caller can finish its bookkeeping first.
func serveUpstream(h http.Handler, w http.ResponseWriter, r *http.Request) (panicked bool, value any) {
	// A handler may legally call panic(nil) on older Go versions; the flag,
	// not the value, says whether it returned.
	panicked = true
	defer func() {
		if panicked {
			value = recover()
		}
	}()
	h.ServeHTTP(w, r)
	panicked = false
	return false, nil
}

// guardrailRefused reports a refusal to OnGuardrailRefuse, if one is set. A
// hook that panics is logged and otherwise ignored: reporting a refusal must
// not get in the way of answering it.
func (c *Chain) guardrailRefused(r *http.Request, svc Service, pattern, action string) {
	if c.OnGuardrailRefuse == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			c.Log.Error("aigw: guardrail refusal hook panicked",
				slog.String("service_id", svc.ID), slog.Any("panic", rec))
		}
	}()
	c.OnGuardrailRefuse(r.Context(), svc.ID, pattern, action)
}

// postResponseTimeout bounds bookkeeping that runs after the response has
// been sent (usage row, cache entry). It runs on a context detached from the
// request, because a client that hangs up right after the last byte cancels
// the request context before these writes start.
const postResponseTimeout = 5 * time.Second

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), postResponseTimeout)
}

// captureEntry records one inspector entry IF the per-service inspector
// feature is enabled. The fromReplay flag is informational — the inspector
// API exposes captured entries the same way regardless.
//
// respHeaders is the response header set the upstream produced (or the
// cache HIT replayed). It MUST be passed in: the inspector API's
// replay-compare diff path consults RespHeaders["Content-Type"] to decide
// between a textual unified diff and a binary metadata-only summary, and
// without these the diff always falls back to binary.
func (c *Chain) captureEntry(svc Service, r *http.Request,
	origBody, redactedBody []byte, redactHits []redact.RuleHit,
	kind Kind, status int, respBody []byte, respHeaders http.Header,
	bytesIn int64, streamed bool, cacheStatus string, fromReplay bool) {
	if c.Inspector == nil {
		return
	}
	cfg := svc.AIConfig
	if cfg.Inspector == nil || !cfg.Inspector.Enabled {
		return
	}
	max := cfg.Inspector.MaxRequests
	if max <= 0 {
		max = inspector.DefaultMaxRequests
	}
	ring := c.Inspector.GetOrCreate(svc.ID, max)

	reqBody, reqOmitted, reqTrunc := inspector.TruncateRequest(redactedBody)
	respBodyT, respOmitted, respTrunc := inspector.TruncateResponse(respBody)

	hits := make([]inspector.RedactionHit, 0, len(redactHits))
	for _, h := range redactHits {
		hits = append(hits, inspector.RedactionHit{Rule: h.Rule.Name, Count: h.Count})
	}

	entry := inspector.Entry{
		ID:           inspector.NewID(),
		ServiceID:    svc.ID,
		APIKeyID:     svc.APIKeyID,
		TS:           time.Now().UTC(),
		Method:       r.Method,
		Path:         r.URL.Path,
		Status:       status,
		BytesIn:      int64(len(origBody)),
		BytesOut:     int64(len(respBody)),
		ReqHeaders:   inspectorHeaders(r.Header, svc.APIKeyHeader),
		ReqBody:      reqBody,
		RespHeaders:  inspectorHeaders(respHeaders, ""),
		RespBody:     respBodyT,
		Truncated:    reqTrunc || respTrunc,
		BytesOmitted: reqOmitted + respOmitted,
		Cache:        cacheStatus,
		Redactions:   hits,
	}
	ring.Capture(entry)
	_ = bytesIn // future: include in DurationMs / metering
	_ = streamed
	_ = fromReplay
}

// recordMeter writes one usage_events row when a Meter sink is configured.
// Non-blocking: any error is logged + swallowed by the SQLSink. costUSD is
// what the upstream reported for the request, nil when it reported nothing.
// latency is the time from the request's arrival in the chain to the end of
// the response; the caller takes it before any post-response bookkeeping.
// The route is read here, not earlier, so the row names the target that
// answered even when the gateway switched target while serving.
func (c *Chain) recordMeter(ctx context.Context, svc Service, kind Kind,
	tokensIn, tokensOut int, bytesIn, bytesOut int64, streamed, cacheHit bool, status int, costUSD *float64,
	latency time.Duration) {
	if c.Meter == nil || withoutUsage(ctx) {
		return
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	sample := aimeter.Sample{
		ServiceID:      svc.ID,
		APIKeyID:       svc.APIKeyID,
		Model:          "", // Task 12 fills this once routing supplies the post-alias model
		Kind:           aimeter.Kind(kind),
		TokensIn:       tokensIn,
		TokensOut:      tokensOut,
		BytesIn:        bytesIn,
		BytesOut:       bytesOut,
		Streamed:       streamed,
		CacheHit:       cacheHit,
		UpstreamStatus: status,
		CostUSD:        costUSD,
		LatencyMs:      latency.Milliseconds(),
	}
	if ri, ok := RouteFrom(ctx); ok {
		sample.GatewayKeyID, sample.Dialect, sample.ProviderSlug = ri.GatewayKeyID, ri.Dialect, ri.ProviderSlug
		sample.RequestedModel, sample.TargetModel, sample.RequestID = ri.RequestedModel, ri.TargetModel, ri.RequestID
		sample.Translated, sample.Dropped = ri.Translated, ri.Dropped
	}
	_ = c.Meter.Record(ctx, sample)
}

// serveCacheHit writes a cache entry back to the visitor with the spec
// Burrow-Cache: HIT + Burrow-Cache-Age headers. Streamed entries are
// never stored (filter is in run() before Store), so the body is always
// safe to write in one shot here.
func (c *Chain) serveCacheHit(w http.ResponseWriter, e exact.Entry) {
	for k, v := range e.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Burrow-Cache", "HIT")
	age := int(time.Since(e.CreatedAt) / time.Second)
	if age < 0 {
		age = 0
	}
	w.Header().Set("Burrow-Cache-Age", strconv.Itoa(age))
	status := e.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(e.Body)
}

// --- helpers ---------------------------------------------------------------

// DetectKind tags a request as openai|anthropic|mcp|unknown per the v0.4.0
// spec heuristics:
//
//   - openai: POST + path ∈ {/v1/chat/completions, /v1/completions,
//     /v1/embeddings, /v1/responses} OR JSON body with top-level "model".
//   - anthropic: path matches /v1/messages AND anthropic-version header
//     is present.
//   - mcp: JSON-RPC tools/* or prompts/* body, OR SSE upgrade on MCP path.
//     v0.4.0 detects-only — no other behaviour change.
//   - else: unknown (proxy through, byte-only metering).
func DetectKind(r *http.Request, body []byte) Kind {
	// Anthropic first — /v1/messages is unambiguous when the header is set.
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/messages") &&
		r.Header.Get("Anthropic-Version") != "" {
		return KindAnthropic
	}

	// OpenAI by path.
	if r.Method == http.MethodPost {
		switch r.URL.Path {
		case "/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/responses":
			return KindOpenAI
		}
	}

	// JSON body with a top-level "model" key → OpenAI-shaped.
	ct := r.Header.Get("Content-Type")
	if r.Method == http.MethodPost && strings.HasPrefix(strings.ToLower(ct), "application/json") && len(body) > 0 {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err == nil {
			if _, ok := top["model"]; ok {
				return KindOpenAI
			}
			// MCP JSON-RPC: method=tools/* or prompts/*
			if mraw, ok := top["method"]; ok {
				var method string
				if err := json.Unmarshal(mraw, &method); err == nil {
					if strings.HasPrefix(method, "tools/") || strings.HasPrefix(method, "prompts/") {
						return KindMCP
					}
				}
			}
		}
	}

	return KindUnknown
}

// readBodyLimited drains r.Body up to limit bytes. If the body is larger
// than limit, the function returns overflow=true; the caller MUST reject
// the request (413). On any non-overflow outcome r.Body is replaced with
// a fresh ReadCloser over the bytes read so downstream handlers see the
// same bytes.
//
// We read limit+1 bytes from a LimitReader to disambiguate "exactly at
// limit" (legitimate) from "more than limit" (reject). The +1 byte that
// pushes us over is discarded; the request is rejected before it reaches
// any downstream handler.
func readBodyLimited(r *http.Request, limit int64) (body []byte, overflow bool, err error) {
	if r.Body == nil {
		return nil, false, nil
	}
	// Read up to limit+1 so we can detect overflow without buffering more.
	lr := io.LimitReader(r.Body, limit+1)
	b, err := io.ReadAll(lr)
	_ = r.Body.Close()
	if int64(len(b)) > limit {
		// Overflow: do NOT restore r.Body — caller will short-circuit.
		return nil, true, nil
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, false, err
}

// buildCacheKey constructs the fully-prefixed cache key for a request. The
// scope prefix matches the applies_per setting:
//
//	global       → "global"
//	per_endpoint → "endpoint:<service_id>:<path>"
//	per_api_key  → "apikey:<api_key_id>" (falls back to "global" when "")
func buildCacheKey(svc Service, r *http.Request, body []byte, s exact.Settings) string {
	canon := exact.Canonicalise(exact.CanonicaliseInput{
		Method:       r.Method,
		Scheme:       schemeOf(r),
		Host:         r.Host,
		Path:         r.URL.Path,
		Headers:      r.Header,
		Body:         body,
		APIKeyHeader: svc.APIKeyHeader,
	})
	hash := exact.HashKey(canon)
	var scope string
	switch s.AppliesPer {
	case "per_endpoint":
		scope = "endpoint:" + svc.ID + ":" + r.URL.Path
	case "per_api_key":
		if subject := keySubject(r, svc); subject != "" {
			scope = "apikey:" + subject
		} else {
			scope = "global"
		}
	default:
		scope = "global"
	}
	return scope + ":" + hash
}

// keySubject is the per-key identity of a request for everything that is
// kept apart by key: the cache scope "per_api_key" and api_key rate limits
// and quotas. It is the service key id when a service key authorised the
// request. A gateway key has no service key id; its subject is
// GatewayKeySubjectPrefix + its id, read from the request's Route. "" means
// the request has no key. The usage row is not affected: it keeps
// api_key_id empty and the gateway key id in its own column.
func keySubject(r *http.Request, svc Service) string {
	if svc.APIKeyID != "" {
		return svc.APIKeyID
	}
	if ri, ok := RouteFrom(r.Context()); ok && ri.GatewayKeyID != "" {
		return GatewayKeySubjectPrefix + ri.GatewayKeyID
	}
	return ""
}

// schemeOf returns the request's URL scheme, falling back to "https" when
// the URL is host-relative (which is the case for vhost-proxied requests
// where the scheme is implicit at the TLS edge).
func schemeOf(r *http.Request) string {
	if r.URL.Scheme != "" {
		return r.URL.Scheme
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// firstValues collapses a multi-valued http.Header into a single-value
// map[string]string by taking the first value for each name. This is what
// the inspector ring stores (its Entry.ReqHeaders is map[string]string).
// Sensitive headers (Authorization, Cookie) are redacted to "[redacted]".
func firstValues(h http.Header) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) == 0 {
			continue
		}
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "cookie" || lk == "proxy-authorization" {
			out[k] = "[redacted]"
			continue
		}
		out[k] = vs[0]
	}
	return out
}

// inspectorHeaders is firstValues for an inspector capture: on top of the
// always-sensitive headers it redacts X-Api-Key and the service's configured
// API-key header (matched case-insensitively), which carry the caller's
// Burrow key on host- and path-routed services.
func inspectorHeaders(h http.Header, apiKeyHeader string) map[string]string {
	out := firstValues(h)
	for k := range out {
		if strings.EqualFold(k, "X-Api-Key") || (apiKeyHeader != "" && strings.EqualFold(k, apiKeyHeader)) {
			out[k] = "[redacted]"
		}
	}
	return out
}

// extractModelFromBody returns the value of top-level "model" in a JSON
// body, or "" when the body is non-JSON / has no model field. Used to
// supply route.RouteContext.Model for the routing-strategy log seam.
func extractModelFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var top struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return ""
	}
	return top.Model
}

// isStreamedResponse returns true when the response headers indicate a
// chunked / SSE stream. The cache MUST NOT Store these (spec invariant).
func isStreamedResponse(h http.Header) bool {
	if strings.EqualFold(h.Get("Transfer-Encoding"), "chunked") {
		return true
	}
	if strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "text/event-stream") {
		return true
	}
	return false
}

// cacheableHeaders selects the subset of response headers worth replaying
// from a cache HIT. v0.4.0 keeps Content-Type at minimum + a handful of
// metadata headers; everything else (Date, Set-Cookie, Server, …) is
// recomputed on the way back out.
func cacheableHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	if ct := h.Get("Content-Type"); ct != "" {
		out["Content-Type"] = ct
	}
	if cl := h.Get("Content-Length"); cl != "" {
		out["Content-Length"] = cl
	}
	if cc := h.Get("Cache-Control"); cc != "" {
		out["Cache-Control"] = cc
	}
	return out
}

// safeRefusalBody returns the upstream-shaped safe-refusal body the
// guardrails ActionRefuseSafe path emits where the request's kind is only
// detected (host routes, /ai/<provider>/). The body shape mirrors the
// upstream API family; for unknown we fall back to a generic JSON envelope.
// A dialect endpoint answers with dialectRefusal instead.
func safeRefusalBody(kind Kind) ([]byte, http.Header) {
	hdr := http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}
	switch kind {
	case KindAnthropic:
		body := `{"id":"msg_burrow_refusal","type":"message","role":"assistant","content":[{"type":"text","text":"I can't help with that."}],"model":"burrow-guardrail","stop_reason":"end_turn"}`
		return []byte(body), hdr
	case KindOpenAI:
		body := `{"id":"chatcmpl-burrow-refusal","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"I can't help with that."},"finish_reason":"stop"}]}`
		return []byte(body), hdr
	default:
		body := `{"error":"guardrail.refuse_safe"}`
		return []byte(body), hdr
	}
}

// refusalText is what a safe refusal says.
const refusalText = "I can't help with that."

// dialectRefusal returns the safe refusal for a request on a dialect
// endpoint: a complete answer of that format to the call that was made. ok
// is false when the path has no answer a refusal could take the shape of.
// The model named in it is the one the client asked for (the body may
// already carry the target's), and a client that asked for a stream gets
// the format's event sequence.
func dialectRefusal(kind Kind, r *http.Request, body []byte) (out []byte, hdr http.Header, streamed, ok bool) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &req) // a body that is no JSON object asks for no stream
	if ri, found := RouteFrom(r.Context()); found && ri.RequestedModel != "" {
		req.Model = ri.RequestedModel
	}
	model, _ := json.Marshal(req.Model)
	text, _ := json.Marshal(refusalText)
	m, t := string(model), string(text)

	var b strings.Builder
	path := strings.TrimRight(r.URL.Path, "/")
	switch {
	case kind == KindAnthropic && path == "/v1/messages" && !req.Stream:
		b.WriteString(`{"id":"msg_burrow_refusal","type":"message","role":"assistant","model":` + m +
			`,"content":[{"type":"text","text":` + t + `}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	case kind == KindAnthropic && path == "/v1/messages":
		event := func(name, data string) { b.WriteString("event: " + name + "\ndata: " + data + "\n\n") }
		event("message_start", `{"type":"message_start","message":{"id":"msg_burrow_refusal","type":"message","role":"assistant","model":`+m+
			`,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`)
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+t+`}}`)
		event("content_block_stop", `{"type":"content_block_stop","index":0}`)
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":0}}`)
		event("message_stop", `{"type":"message_stop"}`)
	case kind == KindOpenAI && path == "/v1/chat/completions" && !req.Stream:
		b.WriteString(`{"id":"chatcmpl-burrow-refusal","object":"chat.completion","created":` + strconv.FormatInt(time.Now().Unix(), 10) + `,"model":` + m +
			`,"choices":[{"index":0,"message":{"role":"assistant","content":` + t + `},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	case kind == KindOpenAI && path == "/v1/chat/completions":
		head := `{"id":"chatcmpl-burrow-refusal","object":"chat.completion.chunk","created":` + strconv.FormatInt(time.Now().Unix(), 10) + `,"model":` + m + `,"choices":[{"index":0,`
		b.WriteString("data: " + head + `"delta":{"role":"assistant","content":` + t + `},"finish_reason":null}]}` + "\n\n")
		b.WriteString("data: " + head + `"delta":{},"finish_reason":"stop"}]}` + "\n\n")
		b.WriteString("data: [DONE]\n\n")
	default:
		return nil, nil, false, false
	}
	contentType := "application/json"
	if req.Stream {
		contentType = "text/event-stream"
	}
	return []byte(b.String()), http.Header{"Content-Type": []string{contentType}}, req.Stream, true
}

// writeJSONError writes a JSON error envelope with the given status code.
func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, code)
}

// --- response writer wrappers ---------------------------------------------

// chainResponseWriter intercepts the upstream response to (a) capture the
// status code for cache + meter, and (b) feed every byte through the
// aimeter Stream so tokens are accumulated and the visitor sees every
// frame immediately (the Stream forwards + flushes per frame).
type chainResponseWriter struct {
	http.ResponseWriter
	stream     *aimeter.Stream
	statusCode int
	headerSent bool
}

func (w *chainResponseWriter) WriteHeader(code int) {
	if w.headerSent {
		return
	}
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
	w.headerSent = true
}

func (w *chainResponseWriter) Write(p []byte) (int, error) {
	if !w.headerSent {
		w.statusCode = http.StatusOK
		w.headerSent = true
	}
	// Route through the aimeter Stream — it forwards + flushes per frame.
	return w.stream.Write(p)
}

// Flush forwards through the wrapped writer + the underlying ResponseWriter
// (the aimeter Stream also flushes after every forwarded frame, but the
// reverse-proxy copy loop may invoke Flush directly on the writer).
func (w *chainResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying ResponseWriter when supported. The
// Stream layer doesn't observe hijacked bytes — by design, websocket /
// h2-bidi flows are not metered.
func (w *chainResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("aigw: underlying ResponseWriter does not implement Hijacker")
}

// capture is an io.Writer that splits writes: every byte goes to the
// downstream writer immediately (preserving the SSE flush invariant) AND
// is appended to an internal buffer up to a cap so the inspector + cache
// can see what was sent. Bytes beyond the cap are forwarded but dropped
// from the capture; truncated() reports whether any bytes were dropped so
// callers (the cache) can refuse to store an incomplete body.
type capture struct {
	dst     io.Writer
	buf     bytes.Buffer
	maxBuf  int
	dropped bool // true once any byte was forwarded but not buffered
}

func newCapture(dst io.Writer, maxBuf int) *capture {
	return &capture{dst: dst, maxBuf: maxBuf}
}

func (c *capture) Write(p []byte) (int, error) {
	// Forward first so the visitor sees the bytes ASAP.
	n, err := c.dst.Write(p)
	if n > 0 {
		remaining := c.maxBuf - c.buf.Len()
		take := n
		if take > remaining {
			take = remaining
		}
		if take > 0 {
			c.buf.Write(p[:take])
		}
		if take < n {
			c.dropped = true
		}
	}
	return n, err
}

func (c *capture) bytes() []byte { return c.buf.Bytes() }

// truncated reports whether any forwarded byte was dropped from the
// internal buffer because the cap was reached. The cache uses this to
// avoid storing an incomplete body alongside the original Content-Length.
func (c *capture) truncated() bool { return c.dropped }

// replayRecorder is the http.ResponseWriter the Replay path writes into.
// We don't actually ship the response anywhere — the Replayer caller just
// wants the resulting inspector entry — so the recorder is mostly a sink.
type replayRecorder struct {
	header     http.Header
	statusCode int
	body       *bytes.Buffer
}

func (r *replayRecorder) Header() http.Header { return r.header }
func (r *replayRecorder) WriteHeader(code int) {
	if r.statusCode == 0 {
		r.statusCode = code
	}
}
func (r *replayRecorder) Write(p []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.body.Write(p)
}
func (r *replayRecorder) Flush() {}
