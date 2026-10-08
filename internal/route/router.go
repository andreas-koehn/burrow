// Package route implements Burrow's v0.4.0 model registry + upstream
// routing strategies + active health checks + circuit breaker.
//
// The Router is split into two responsibilities:
//
//   - Pick(): a pure function of the current health snapshot, the Policy
//     (operator-supplied), and the per-request RouteContext. Returns a
//     Pick describing the chosen backend and a Retry closure that lets
//     the caller (the proxy hot path, Task 10) walk to the next candidate
//     when the idempotency rule allows.
//
//   - HealthChecker: an active /healthz probe loop that owns the live
//     "healthy" flag and circuit-breaker state for each registered
//     backend. The Router itself implements HealthChecker so tests can
//     swap a stub in; the real binary uses the in-process implementation.
//
// Pinned invariants (Spec Part C):
//   - Five strategies: single, failover, weighted, header_based, sticky.
//   - Failover idempotency: the *caller* decides whether to invoke
//     Pick.Retry; the Router only supplies the next candidate. (Spec:
//     retry only when a connection error occurred before any byte was
//     streamed back, OR Idempotency-Key is set AND no body bytes streamed.)
//   - Sticky uses consistent-hash (FNV-1a 64) so adding backends shifts
//     only the new ranges instead of reshuffling every session.
//   - Circuit breaker trips when rolling-window failure_pct exceeds the
//     configured threshold; it reopens once cool_down_seconds have elapsed.
package route

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Strategy is one of the five upstream routing strategies (spec Part C.2).
type Strategy string

// Strategy enum constants.
const (
	StrategySingle        Strategy = "single"
	StrategyFailover      Strategy = "failover"
	StrategyWeighted      Strategy = "weighted"
	StrategyHeaderBased   Strategy = "header_based"
	StrategySticky        Strategy = "sticky"
	StrategyMultiProvider Strategy = "multi_provider" // v0.5.0: priority-ordered walk with cross-provider gate
)

// Backend is one routing target — a (service_id, weight, concrete_model)
// triple that the proxy will direct an upstream request to.
type Backend struct {
	ServiceID     string
	Weight        int    // weighted-strategy only; 0 means "equal share"
	ConcreteModel string // exposed to the upstream Director / aliased away from the client-facing model name
}

// MultiProviderBackend is a Backend extended with provider + priority fields
// used by the multi_provider strategy (v0.5.0, spec Part C.2).
// Provider is one of: ollama | vllm | openai-compat | openai | anthropic | other.
// Priority is the walk order (lower value = higher priority; ties broken by id ASC).
type MultiProviderBackend struct {
	Backend
	Provider string // upstream provider discriminator
	Priority int    // routing priority; lower = tried first
}

// Policy is the operator-supplied routing rule for a single ai-endpoint
// (the v0.4.0 ServiceAIConfig.routing block, spec Part B.7).
type Policy struct {
	Strategy   Strategy
	ModelAlias string
	Backends   []Backend
	// MultiBackends is populated only for StrategyMultiProvider (v0.5.0).
	// The regular Backends slice is ignored in that strategy.
	MultiBackends        []MultiProviderBackend
	HeaderName           string // header_based: the request header to inspect (default "X-Burrow-Model")
	CircuitFailurePct    int    // breaker tripping threshold (0–100)
	CircuitWindowSeconds int    // rolling-window size; default 60s
	CircuitCoolDownSecs  int    // cool-down before re-evaluating
}

// Pick is the outcome of a single Pick() invocation. ServiceID + ConcreteModel
// identify the chosen upstream backend; Retry is a closure that returns the
// next candidate when the caller decides to fail over (idempotency rule).
//
// NewStickyID is non-empty only for the sticky strategy when the caller
// supplied an empty StickySessionID — in that case Pick generates a fresh
// session id and the proxy is expected to Set-Cookie it on the response.
type Pick struct {
	ServiceID     string
	ConcreteModel string
	Retry         func() (Pick, bool)
	NewStickyID   string
}

// RouteContext is the per-request input to Pick. The proxy populates it from
// the inbound request after alias resolution and after the access middleware
// has run.
type RouteContext struct {
	Kind            string // openai|anthropic|mcp|unknown
	Model           string // post-alias resolution
	Streaming       bool
	IdempotencyKey  string
	StickySessionID string // value of burrow_route_session cookie, "" if absent
	APIKeyID        string
	HeaderValues    map[string]string // for header_based strategy
}

// BackendRecord is what's read from services + service_ai_config when the
// Router needs to actually probe an upstream. The Lookup interface returns
// these per service id.
type BackendRecord struct {
	ServiceID  string
	HealthPath string // default "/healthz" when empty
	LocalAddr  string // upstream local address (v0.3.0 service.LocalAddr or equivalent)
}

// Lookup is the read-side surface the Router needs to materialise a
// BackendRecord from a service id. *db.DB (or a thin adapter) is expected
// to satisfy this in production; tests provide a stub.
type Lookup interface {
	GetBackend(ctx context.Context, serviceID string) (BackendRecord, bool, error)
}

// HealthChecker is the contract Pick consults to decide whether a backend
// is currently routable. The Router itself implements this for the simple
// in-process case; tests can swap a stub.
type HealthChecker interface {
	Healthy(serviceID string) bool
	Trip(serviceID string)
	ReportSuccess(serviceID string)
}

// Clock abstracts time.Now so the circuit-breaker cool-down logic is
// deterministic in tests. The default realClock is wall-clock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// ErrBackendUnavailable is the sentinel error returned when the routing
// strategy has no healthy candidates. The header_based strategy returns
// this when its specifically-targeted backend is currently tripped (spec:
// 503 {"error":"backend unavailable"}); other strategies return it when
// every backend in the policy is tripped.
var ErrBackendUnavailable = errors.New("backend unavailable")

// ErrNoBackends is returned when a Policy carries no Backends at all
// (operator misconfiguration).
var ErrNoBackends = errors.New("policy has no backends")

// Router is the shared instance for one Burrow process. It owns:
//   - the in-process HealthChecker state (per-backend healthy flag,
//     rolling-window failure counter, breaker tripped-at timestamp);
//   - the active health-check goroutine, when StartHealthLoop is called;
//   - lookups via the injected Lookup.
//
// Routers are safe for concurrent use. Pick is fully lock-free except for
// the breaker's atomic read of the per-backend state.
type Router struct {
	lookup Lookup
	log    *slog.Logger
	clock  Clock

	// hcOverride, when non-nil, replaces the Router's own HealthChecker.
	// Tests use this to script per-backend health without a real probe
	// loop.
	hcOverride HealthChecker

	mu       sync.RWMutex
	backends map[string]*backendState // serviceID → state (Router-owned HealthChecker)

	// Tunables. Defaults are spec-mandated; SetHealthInterval and
	// SetBreakerConfig let cmd/server override from settings before
	// StartHealthLoop runs.
	healthInterval time.Duration
	healthTimeout  time.Duration
	failurePct     int // tripping threshold (0–100)
	windowSecs     int // rolling-window size
	coolDownSecs   int // cool-down before reopening

	// Health-loop control.
	hlMu      sync.Mutex
	hlCancel  context.CancelFunc
	hlRunning bool
}

// backendState is the per-backend mutable bookkeeping the Router-owned
// HealthChecker maintains. The fields are guarded by mu (the per-backend
// mutex, not the Router's map mutex) — keep critical sections short.
type backendState struct {
	mu         sync.Mutex
	record     BackendRecord
	tripped    bool
	trippedAt  time.Time
	probes     ringBuffer // rolling-window success/failure samples
	probeCount int
	failCount  int
}

// ringBucketCount is the fixed size of the rolling-window ring (one bucket
// per second of wall-clock, 30 seconds max horizon).
const ringBucketCount = 30

// ringBuffer is a fixed 30-bucket second-resolution success/failure ring.
// Each bucket records (probes, failures) for one second of wall-clock.
type ringBuffer struct {
	buckets [ringBucketCount]bucket
	cur     int
	curSec  int64 // unix-second when cur was last written
}

type bucket struct {
	probes int
	fails  int
}

// New constructs a Router. hc, if non-nil, is the HealthChecker the Router
// will use for Pick decisions; nil means "use the Router's own impl".
func New(lookup Lookup, hc HealthChecker, log *slog.Logger) *Router {
	return NewWithClock(lookup, hc, log, realClock{})
}

// NewWithClock is New with an injected clock for tests.
func NewWithClock(lookup Lookup, hc HealthChecker, log *slog.Logger, clock Clock) *Router {
	if log == nil {
		log = slog.Default()
	}
	r := &Router{
		lookup:         lookup,
		log:            log,
		clock:          clock,
		hcOverride:     hc,
		backends:       map[string]*backendState{},
		healthInterval: 10 * time.Second,
		healthTimeout:  5 * time.Second,
		failurePct:     50,
		windowSecs:     60,
		coolDownSecs:   30,
	}
	return r
}

// SetHealthInterval overrides the probe period. The spec range is 1–300s;
// callers reading from operator config (cmd/server) are responsible for
// clamping. Sub-second values are accepted so tests can drive the loop
// quickly. Non-positive values become a 1s floor here as a final safety
// net (a zero ticker would panic).
func (r *Router) SetHealthInterval(d time.Duration) {
	if d <= 0 {
		d = time.Second
	}
	if d > 300*time.Second {
		d = 300 * time.Second
	}
	r.healthInterval = d
}

// SetBreakerConfig sets the rolling-window breaker tunables.
//
// failurePct (0–100): tripping threshold; >= this fraction of failures in
// the window flips the backend tripped.
//
// windowSeconds (1–30): rolling window size (capped at the 30-bucket ring).
//
// coolDownSecs (>= 1): how long the breaker stays tripped before reopening.
func (r *Router) SetBreakerConfig(failurePct, windowSeconds, coolDownSecs int) {
	if failurePct < 0 {
		failurePct = 0
	}
	if failurePct > 100 {
		failurePct = 100
	}
	if windowSeconds < 1 {
		windowSeconds = 1
	}
	if windowSeconds > 30 {
		windowSeconds = 30
	}
	if coolDownSecs < 1 {
		coolDownSecs = 1
	}
	r.failurePct = failurePct
	r.windowSecs = windowSeconds
	r.coolDownSecs = coolDownSecs
}

// RegisterBackend adds a backend to the in-process HealthChecker registry.
// Idempotent: a subsequent call with the same id updates the BackendRecord
// in place but keeps the existing breaker state.
func (r *Router) RegisterBackend(id string, rec BackendRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.backends[id]
	if !ok {
		r.backends[id] = &backendState{record: rec}
		return
	}
	st.mu.Lock()
	st.record = rec
	st.mu.Unlock()
}

// UnregisterBackend removes a backend from the registry. Used when an
// ai-endpoint is reconfigured to drop a target.
func (r *Router) UnregisterBackend(id string) {
	r.mu.Lock()
	delete(r.backends, id)
	r.mu.Unlock()
}

// Healthy implements HealthChecker. For backends not registered with the
// Router, returns true (the caller is in tests-with-scripted-health mode
// and uses the hcOverride for those ids).
func (r *Router) Healthy(serviceID string) bool {
	if r.hcOverride != nil {
		return r.hcOverride.Healthy(serviceID)
	}
	r.mu.RLock()
	st, ok := r.backends[serviceID]
	r.mu.RUnlock()
	if !ok {
		// Unregistered backends are assumed healthy (the Router is not the
		// source of truth for them — Pick gets called with hcOverride or
		// with a Policy whose backends have not been registered yet, which
		// can happen during startup).
		return true
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.tripped {
		return true
	}
	// Tripped → check cool-down.
	if r.clock.Now().Sub(st.trippedAt) >= time.Duration(r.coolDownSecs)*time.Second {
		// Cool-down elapsed: reopen. Clear the rolling ring too —
		// otherwise stale failures from before the trip dominate the
		// window and the next single failed probe re-trips the breaker
		// immediately, denying the backend its chance to recover.
		st.tripped = false
		st.failCount = 0
		st.probeCount = 0
		st.probes = ringBuffer{}
		return true
	}
	return false
}

// Trip implements HealthChecker — forces the breaker to the tripped state
// (used by the proxy when it observes a 5xx storm or a network error).
func (r *Router) Trip(serviceID string) {
	if r.hcOverride != nil {
		r.hcOverride.Trip(serviceID)
		return
	}
	r.mu.RLock()
	st, ok := r.backends[serviceID]
	r.mu.RUnlock()
	if !ok {
		// Trip on an unregistered backend is a no-op; the proxy may call
		// us before RegisterBackend has run.
		return
	}
	st.mu.Lock()
	st.tripped = true
	st.trippedAt = r.clock.Now()
	st.mu.Unlock()
}

// ReportSuccess implements HealthChecker — resets the failure window and,
// if currently tripped, clears the tripped flag immediately. Used by the
// proxy when it sees a successful upstream response after a transient blip.
func (r *Router) ReportSuccess(serviceID string) {
	if r.hcOverride != nil {
		r.hcOverride.ReportSuccess(serviceID)
		return
	}
	r.mu.RLock()
	st, ok := r.backends[serviceID]
	r.mu.RUnlock()
	if !ok {
		return
	}
	st.mu.Lock()
	st.tripped = false
	st.failCount = 0
	st.probeCount = 0
	st.probes = ringBuffer{}
	st.mu.Unlock()
}

// fakeClock is the test seam for Clock. It is defined here (not in a
// _test.go file) so cmd/server-style tests in other packages can use it
// too if needed — it's exported via NewWithClock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}
