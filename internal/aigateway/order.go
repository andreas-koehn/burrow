package aigateway

import (
	"context"
	"sort"
)

// LatencySource returns the p95 latency in milliseconds per backing service
// id of a provider, as the providers page shows it; a service without a
// measurement is absent. It is read once per request and must not block. The
// map is shared and must not be written to.
type LatencySource func(ctx context.Context) map[string]int

// PriceSource returns the blended price per million tokens of a provider's
// model (0.75*input + 0.25*output); ok is false when the pricing table has
// no entry.
type PriceSource func(provider, model string) (usdPerMillion float64, ok bool)

// maxUsableLatencyMs: a p95 above this is an outlier and counts as
// unmeasured.
const maxUsableLatencyMs = 120000

// orderTargets returns targets in the order the model's routing mode asks
// for. ordered (or empty, or a mode whose source is not set) returns them
// unchanged. Measured/priced targets come first, sorted ascending (stable:
// ties keep configured order); the rest follow in configured order. The
// input slice is never modified.
func (g *Gateway) orderTargets(ctx context.Context, routing string, targets []Target) []Target {
	if len(targets) < 2 {
		return targets
	}
	var score func(t Target) (float64, bool)
	switch routing {
	case "fastest":
		if g.Latency == nil {
			return targets
		}
		lat := g.Latency(ctx)
		score = func(t Target) (float64, bool) {
			ms, ok := lat[t.Provider.ServiceID]
			if !ok || ms < 0 || ms > maxUsableLatencyMs {
				return 0, false
			}
			return float64(ms), true
		}
	case "cheapest":
		if g.Prices == nil {
			return targets
		}
		score = func(t Target) (float64, bool) {
			return g.Prices(t.Provider.Slug, t.Model)
		}
	default:
		return targets
	}
	var scored, rest []Target
	val := map[int]float64{}
	for _, t := range targets {
		if v, ok := score(t); ok {
			val[len(scored)] = v
			scored = append(scored, t)
		} else {
			rest = append(rest, t)
		}
	}
	idx := make([]int, len(scored))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return val[idx[a]] < val[idx[b]] })
	out := make([]Target, 0, len(targets))
	for _, i := range idx {
		out = append(out, scored[i])
	}
	return append(out, rest...)
}
