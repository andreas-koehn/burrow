package main

// e2e_failover_test.go — Wave-2 Task 7 of the v0.4.0 integration plan.
//
// Real-stack e2e for multi-backend upstream failover, circuit-breaker
// behaviour, and the idempotency-key retry rule (spec Part C.2).
//
// ## Intended behaviour (the executable contract these tests describe)
//
// Spec Part C.2 mandates that when a service is configured with a
// `routing` policy of strategy == "failover" and >= 2 backends:
//
//   1. The chain consults route.Router.Pick to choose the primary
//      backend. The proxy Director then dials *that* backend's
//      LocalAddr, not the v0.3.0 default service's LocalAddr.
//
//   2. If the primary upstream returns a network error OR a 5xx BEFORE
//      any byte is streamed back to the visitor, the chain MUST invoke
//      pick.Retry() and dispatch the same request to the next healthy
//      backend in the policy.
//
//   3. The retry is allowed ONLY when either (a) no body bytes have
//      streamed back yet (the upstream failed during connect / before
//      writing its response), OR (b) the visitor's request carries an
//      `Idempotency-Key` header. A POST that has begun streaming bytes
//      back to the visitor MUST NOT be retried on a fresh upstream.
//
//   4. Each upstream failure reports into the Router's HealthChecker
//      ring buffer (Trip() on connection-error / 5xx storm). When the
//      rolling-window failure rate exceeds `circuit_breaker.failure_pct`
//      the backend is marked tripped and pick.Pick skips it. After
//      `cool_down_seconds` the breaker reopens and the backend is probed
//      again.
//
// ## Wiring deferral (spec drift — flagged in the integration report)
//
// As of tip 7805962, NONE of the four steps above are wired end-to-end:
//
//   - aigw.Chain.run (internal/aigw/chain.go, Step 8) consults
//     Router.Pick when cfg.Routing is non-nil, but the result is
//     log-only — see the explicit comment at chain.go ~line 477
//     ("log-only seam for Task 10. Task 12 wires the actual
//     upstream-pick into the proxy Director."). The picked
//     pick.ServiceID is NOT plumbed into the proxy.Director, so the
//     v0.3.0 default service routing is always used.
//
//   - cmd/server/v04_loader.go (decodeServiceAIConfig) intentionally
//     skips the `routing` sub-object — see the explicit comment at
//     v04_loader.go:145 ("routing is intentionally NOT decoded here").
//     Even if the chain consulted cfg.Routing, the loader would always
//     return cfg.Routing == nil for services configured via the DB, so
//     the chain's Step 8 branch never fires for DB-backed services.
//
//   - cmd/server/v04_wiring.go wires `routeLookupNoop{}` as the
//     Router's Lookup (explicit comment at v04_wiring.go:143). The
//     noop always reports "unknown service", so the Router's own
//     HealthChecker has no BackendRecord to probe and the circuit
//     breaker never actually tracks per-backend state for live
//     services.
//
//   - internal/proxy/proxy.go has no Router awareness at all
//     (`grep -nE 'Router|Pick|failover|circuit' internal/proxy/proxy.go`
//     finds zero matches). The Director picks an upstream from the
//     resolved subdomain → service mapping; there is no retry seam,
//     no idempotency-key check, no byte-streamed-yet flag.
//
// Closing the wiring requires four cohesive changes that belong in a
// dedicated follow-up commit (NOT buried inside a test file):
//
//   (a) v04_loader.go: decode service_ai_config.routing into
//       aigw.ServiceAIConfig.Routing (a *route.Policy).
//   (b) v04_wiring.go: replace routeLookupNoop with a real adapter
//       backed by *db.DB + the live tunnel registry (so the Router
//       can resolve service-id → BackendRecord for probing).
//   (c) chain.go Step 8: when pick.ServiceID is set, propagate it to
//       the proxy Director (likely via a new request-scoped value
//       on the context, or a new field on aigw.Service the proxy
//       reads back) so the Director can dial the picked backend's
//       LocalAddr instead of the service's own.
//   (d) chain.go Step 9: wrap the upstream-side response writer with
//       a "bytes-streamed-yet" flag. On 5xx/network-error before any
//       byte streamed (or when Idempotency-Key is set), invoke
//       pick.Retry() and re-dispatch; otherwise let the visitor see
//       the partial bytes + a Burrow-Upstream-Failed trailer.
//
// Each of those is non-trivial and crosses package boundaries. Per
// the integration plan ("PREFER skipping cleanly if the wiring is more
// than a 1-hour endeavor"), the three tests below SKIP with detailed
// deferral notes, preserving the executable contract for the next
// agent to close.
//
// The skip messages name the specific code locations (with current
// line numbers) so a future code reviewer can trace skip → wire-up
// without re-reading this entire file.

import (
	"testing"
)

// TestE2EFailover_OnConnectionError — Task 7, sub-test 1.
//
// Intent: seed two http services (svc_primary, svc_secondary), register
// a service_ai_config row whose routing.strategy=="failover" lists both
// backends, make the primary's tunnel return 500 on every request and
// the secondary return 200 OK with a known body. POST through the
// proxy ingress; assert the visitor sees the secondary's body and that
// each upstream was hit exactly once (the primary failed → failover →
// secondary served).
//
// SKIPPED — wiring deferred. See the package doc-comment above for the
// full deferral analysis. The wiring spans (a) routing decode in
// v04_loader.go, (b) real Lookup in v04_wiring.go, (c) Pick →
// Director plumbing in aigw.Chain.run + internal/proxy, and (d) the
// Retry seam in chain.go Step 9. None of these are present at tip
// 7805962 so the test cannot meaningfully exercise failover today.
func TestE2EFailover_OnConnectionError(t *testing.T) {
	t.Skip("failover/circuit-breaker not implemented (route.Router runs with routeLookupNoop) — see dev/BACKLOG_failover.md")
}

// TestE2EFailover_CircuitBreakerTrip — Task 7, sub-test 2.
//
// Intent: configure failover with circuit_breaker.failure_pct=50,
// window_seconds=5, cool_down_seconds=2. Send 10 POSTs with the primary
// always returning 500; assert that after the breaker trips the primary
// upstream counter stops incrementing (Pick skips the tripped backend);
// after cool_down_seconds elapses, send another request and assert the
// primary IS probed again.
//
// SKIPPED — wiring deferred. Same root cause as
// TestE2EFailover_OnConnectionError: without the Pick → Director
// plumbing and without the Trip()/ReportSuccess() callbacks from the
// proxy hot path, the breaker has no signal to track. The Router's
// breaker logic itself is unit-tested in internal/route/router_test.go;
// this e2e exercises the end-to-end signal flow that's not yet
// connected.
func TestE2EFailover_CircuitBreakerTrip(t *testing.T) {
	t.Skip("failover/circuit-breaker not implemented (route.Router runs with routeLookupNoop) — see dev/BACKLOG_failover.md")
}

// TestE2EFailover_IdempotencyKeyRule — Task 7, sub-test 3.
//
// Intent: two scenarios that pin spec Part C.2's idempotency rule.
//
//   1. POST with `Idempotency-Key: abc` whose primary returns 500
//      BEFORE streaming any byte → retry to secondary IS allowed,
//      visitor sees secondary's 200 body.
//
//   2. POST WITHOUT `Idempotency-Key` whose primary streams 2 bytes
//      back then aborts with a network error → retry MUST NOT happen
//      (the visitor has already received bytes from the primary and
//      a fresh POST to the secondary would silently duplicate any
//      side effect). The visitor receives the partial bytes and a
//      trailer / final-status indicator that records the upstream
//      failure.
//
// SKIPPED — wiring deferred. The byte-streamed-yet flag and the
// idempotency-key check both live in the Retry seam that chain.go
// Step 9 needs and that v04_wiring.go's noop Lookup makes impossible
// today. See the package doc-comment for the full chain.
func TestE2EFailover_IdempotencyKeyRule(t *testing.T) {
	t.Skip("failover/circuit-breaker not implemented (route.Router runs with routeLookupNoop) — see dev/BACKLOG_failover.md")
}
