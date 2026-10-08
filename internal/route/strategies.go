package route

// strategies.go: the five Pick() strategies (single, failover, weighted,
// header_based, sticky) and their helpers, extracted from router.go so
// router.go can stay focused on the Router lifecycle / breaker state /
// HealthChecker contract.
//
// Pure refactor — no behaviour change vs router.go pre-extraction.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	mrand "math/rand/v2"
	"sort"
)

// Pick returns a chosen backend per Policy + RouteContext. Behaviour by
// strategy is documented inline; the caller is responsible for honouring
// the idempotency rule when invoking Pick.Retry.
func (r *Router) Pick(_ context.Context, p Policy, rc RouteContext) (Pick, error) {
	if len(p.Backends) == 0 {
		return Pick{}, ErrNoBackends
	}
	hc := r.healthChecker()

	switch p.Strategy {
	case StrategySingle, "":
		// Default to single when strategy is empty (safest fallback).
		return r.pickSingle(p, hc)
	case StrategyFailover:
		return r.pickFailover(p, hc)
	case StrategyWeighted:
		return r.pickWeighted(p, hc)
	case StrategyHeaderBased:
		return r.pickHeaderBased(p, rc, hc)
	case StrategySticky:
		return r.pickSticky(p, rc, hc)
	}
	return Pick{}, fmt.Errorf("unknown strategy %q", p.Strategy)
}

// PickMulti is the entry point for the multi_provider strategy (v0.5.0).
// It uses p.MultiBackends (sorted by Priority ASC, then by the slice index
// as a stable tiebreaker) and applies the provider gate described in spec
// C.2.
//
// The RouteContext.Kind drives the provider filter: the backend's Provider
// must speak the request's wire format (openai-wire → ollama/vllm/openai/
// openai-compat; anthropic-wire → anthropic only). A request is never routed
// to a backend of another format: nothing on this path translates a body.
// (Formats are translated per gateway model, in internal/aigw/translate.)
//
// Backends whose circuit breaker is OPEN are skipped; the returned Retry
// closure walks to the next candidate in the filtered+sorted list.
func (r *Router) PickMulti(_ context.Context, p Policy, rc RouteContext) (Pick, error) {
	if len(p.MultiBackends) == 0 {
		return Pick{}, ErrNoBackends
	}
	hc := r.healthChecker()
	return r.pickMultiProvider(p, rc, hc)
}

// openaiKindProviders is the closed set of provider values that are
// compatible with openai-wire-protocol requests (kind == "openai").
var openaiKindProviders = map[string]bool{
	"ollama":        true,
	"vllm":          true,
	"openai":        true,
	"openai-compat": true,
}

// multiProviderCompatible reports whether a MultiProviderBackend is
// routable for the request kind: the provider must match the request's wire
// format.
//
//   - kind "openai" → provider in {ollama, vllm, openai, openai-compat}
//   - kind "anthropic" → provider == "anthropic"
//   - any other kind → no filter applied (pass through)
func multiProviderCompatible(b MultiProviderBackend, kind string) bool {
	switch kind {
	case "openai":
		return openaiKindProviders[b.Provider]
	case "anthropic":
		return b.Provider == "anthropic"
	default:
		return true
	}
}

// pickMultiProvider implements the multi_provider strategy: sort by priority
// ASC (stable, preserving original slice order for ties), filter by provider
// compatibility and breaker health, then return the first healthy candidate
// with a Retry closure that walks the remaining list.
func (r *Router) pickMultiProvider(p Policy, rc RouteContext, hc HealthChecker) (Pick, error) {
	// Sort a copy by priority ASC; the sort is stable so ties preserve
	// the original slice order (which the DB query orders by id ASC).
	sorted := make([]MultiProviderBackend, len(p.MultiBackends))
	copy(sorted, p.MultiBackends)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority < sorted[j].Priority
	})

	// Filter by provider compatibility.
	kind := rc.Kind
	filtered := make([]MultiProviderBackend, 0, len(sorted))
	for _, b := range sorted {
		if multiProviderCompatible(b, kind) {
			filtered = append(filtered, b)
		}
	}

	// Walk the filtered list skipping tripped backends.
	healthy := make([]MultiProviderBackend, 0, len(filtered))
	for _, b := range filtered {
		if hc.Healthy(b.ServiceID) {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) == 0 {
		return Pick{}, ErrBackendUnavailable
	}
	return multiProviderPickFromList(healthy, 0), nil
}

// multiProviderPickFromList builds a Pick pointing at healthy[idx] whose
// Retry closure walks to healthy[idx+1].
func multiProviderPickFromList(healthy []MultiProviderBackend, idx int) Pick {
	b := healthy[idx]
	return Pick{
		ServiceID:     b.ServiceID,
		ConcreteModel: b.ConcreteModel,
		Retry: func() (Pick, bool) {
			if idx+1 >= len(healthy) {
				return Pick{}, false
			}
			return multiProviderPickFromList(healthy, idx+1), true
		},
	}
}

func (r *Router) healthChecker() HealthChecker {
	if r.hcOverride != nil {
		return r.hcOverride
	}
	return r
}

// pickSingle: return the first backend, regardless of health (a "single"
// strategy operator opted out of failover).
func (r *Router) pickSingle(p Policy, _ HealthChecker) (Pick, error) {
	b := p.Backends[0]
	return Pick{
		ServiceID:     b.ServiceID,
		ConcreteModel: b.ConcreteModel,
		Retry:         func() (Pick, bool) { return Pick{}, false },
	}, nil
}

// pickFailover: walk the Backends list in order, skipping tripped ones;
// the returned Retry walks to the next healthy one.
func (r *Router) pickFailover(p Policy, hc HealthChecker) (Pick, error) {
	healthy := make([]Backend, 0, len(p.Backends))
	for _, b := range p.Backends {
		if hc.Healthy(b.ServiceID) {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) == 0 {
		return Pick{}, ErrBackendUnavailable
	}
	return failoverPickFromList(healthy, 0), nil
}

// failoverPickFromList builds a Pick that points at healthy[idx] and whose
// Retry closure walks to healthy[idx+1]. When idx is past the end, Retry
// returns (Pick{}, false) to signal exhaustion.
func failoverPickFromList(healthy []Backend, idx int) Pick {
	b := healthy[idx]
	return Pick{
		ServiceID:     b.ServiceID,
		ConcreteModel: b.ConcreteModel,
		Retry: func() (Pick, bool) {
			if idx+1 >= len(healthy) {
				return Pick{}, false
			}
			return failoverPickFromList(healthy, idx+1), true
		},
	}
}

// pickWeighted: cumulative-weight random draw across the healthy subset.
// Skips tripped backends; the weights of healthy backends still sum to
// the *configured* total only if none are tripped — a tripped backend is
// excluded from the draw entirely.
func (r *Router) pickWeighted(p Policy, hc HealthChecker) (Pick, error) {
	type wb struct {
		b   Backend
		cum int64
	}
	cum := []wb{}
	var total int64
	for _, b := range p.Backends {
		if !hc.Healthy(b.ServiceID) {
			continue
		}
		w := int64(b.Weight)
		if w <= 0 {
			w = 1
		}
		total += w
		cum = append(cum, wb{b: b, cum: total})
	}
	if len(cum) == 0 {
		return Pick{}, ErrBackendUnavailable
	}
	x := mrand.Int64N(total)
	for _, c := range cum {
		if x < c.cum {
			return Pick{
				ServiceID:     c.b.ServiceID,
				ConcreteModel: c.b.ConcreteModel,
				Retry:         func() (Pick, bool) { return Pick{}, false },
			}, nil
		}
	}
	// Unreachable — cum[-1].cum == total and x < total.
	last := cum[len(cum)-1].b
	return Pick{ServiceID: last.ServiceID, ConcreteModel: last.ConcreteModel,
		Retry: func() (Pick, bool) { return Pick{}, false }}, nil
}

// pickHeaderBased: index into Backends by ConcreteModel == rc.HeaderValues[HeaderName].
// Absent header → first backend (single fallback per spec). Target tripped → 503.
func (r *Router) pickHeaderBased(p Policy, rc RouteContext, hc HealthChecker) (Pick, error) {
	headerName := p.HeaderName
	if headerName == "" {
		headerName = "X-Burrow-Model"
	}
	v := rc.HeaderValues[headerName]
	if v == "" {
		// Fallback to single (first backend) — must still be healthy.
		first := p.Backends[0]
		if !hc.Healthy(first.ServiceID) {
			return Pick{}, ErrBackendUnavailable
		}
		return Pick{
			ServiceID:     first.ServiceID,
			ConcreteModel: first.ConcreteModel,
			Retry:         func() (Pick, bool) { return Pick{}, false },
		}, nil
	}
	for _, b := range p.Backends {
		if b.ConcreteModel == v {
			if !hc.Healthy(b.ServiceID) {
				return Pick{}, ErrBackendUnavailable
			}
			return Pick{
				ServiceID:     b.ServiceID,
				ConcreteModel: b.ConcreteModel,
				Retry:         func() (Pick, bool) { return Pick{}, false },
			}, nil
		}
	}
	// Header value didn't match any backend's ConcreteModel — fall back
	// to first (single) behaviour. Spec is ambiguous here; "absent header
	// falls back to `single`" is the closest documented behaviour, and
	// the same logic is the safest extension to "unknown value".
	first := p.Backends[0]
	if !hc.Healthy(first.ServiceID) {
		return Pick{}, ErrBackendUnavailable
	}
	return Pick{
		ServiceID:     first.ServiceID,
		ConcreteModel: first.ConcreteModel,
		Retry:         func() (Pick, bool) { return Pick{}, false },
	}, nil
}

// pickSticky uses a real consistent-hash ring: each backend gets
// `vnodesPerWeight * weight` virtual nodes hashed onto a 64-bit ring;
// each session id is hashed to a position and routed to the next vnode
// in sorted order. Adding a backend only "steals" the ranges
// immediately adjacent to its new vnodes — the rest of the sessions
// keep mapping to the same backend.
func (r *Router) pickSticky(p Policy, rc RouteContext, hc HealthChecker) (Pick, error) {
	// Sort backends by ServiceID for deterministic ordering across
	// processes — operators can rely on the same session id mapping the
	// same way on every node.
	sorted := append([]Backend(nil), p.Backends...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ServiceID < sorted[j].ServiceID })

	// Filter to healthy.
	healthy := make([]Backend, 0, len(sorted))
	for _, b := range sorted {
		if hc.Healthy(b.ServiceID) {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) == 0 {
		return Pick{}, ErrBackendUnavailable
	}

	// Generate a fresh session id when the caller has none — the proxy
	// will Set-Cookie it on the response.
	newID := ""
	sid := rc.StickySessionID
	if sid == "" {
		var err error
		sid, err = NewStickySessionID()
		if err != nil {
			return Pick{}, fmt.Errorf("generate sticky session: %w", err)
		}
		newID = sid
	}

	// Build the consistent-hash ring out of virtual nodes. Using 64
	// vnodes per unit weight gives a smooth distribution and bounds the
	// reshuffle on adding/removing a backend to ~1/N of sessions.
	const vnodesPerWeight = 64
	type vnode struct {
		pos uint64
		idx int // index into healthy[]
	}
	ring := make([]vnode, 0, len(healthy)*vnodesPerWeight)
	for i, b := range healthy {
		w := b.Weight
		if w <= 0 {
			w = 1
		}
		n := vnodesPerWeight * w
		for j := 0; j < n; j++ {
			h := fnv.New64a()
			_, _ = h.Write([]byte(b.ServiceID))
			_, _ = h.Write([]byte{'#'})
			var jb [8]byte
			for k := 0; k < 8; k++ {
				jb[k] = byte(j >> (k * 8))
			}
			_, _ = h.Write(jb[:])
			ring = append(ring, vnode{pos: h.Sum64(), idx: i})
		}
	}
	sort.Slice(ring, func(i, j int) bool { return ring[i].pos < ring[j].pos })

	// Hash the session id onto the ring; pick the first vnode with
	// position >= the session's hash (wrap to ring[0] when past the end).
	h := fnv.New64a()
	_, _ = h.Write([]byte(sid))
	x := h.Sum64()
	pickIdx := sort.Search(len(ring), func(i int) bool { return ring[i].pos >= x })
	if pickIdx == len(ring) {
		pickIdx = 0
	}
	b := healthy[ring[pickIdx].idx]
	return Pick{
		ServiceID:     b.ServiceID,
		ConcreteModel: b.ConcreteModel,
		NewStickyID:   newID,
		Retry:         func() (Pick, bool) { return Pick{}, false },
	}, nil
}

// NewStickySessionID returns a fresh 16-byte random sticky session id,
// base64url-encoded (no padding). Suitable for the `burrow_route_session`
// cookie value (spec C.2 sticky).
func NewStickySessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
