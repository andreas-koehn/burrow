package main

import (
	"context"
	"testing"

	"github.com/ankoehn/burrow/internal/aigateway"
	"github.com/ankoehn/burrow/internal/cost"
)

// The gateway carries both routing sources, and they read what the providers
// page and the pricing table hold.
func TestNewAIGateway_RoutingSources(t *testing.T) {
	src := &countingMetrics{p95: map[string]int{"svc-1": 420}}
	m, _ := testAIMetrics(src, nil)
	eng := cost.New(nil, cost.Pricing{Entries: map[string]cost.Entry{
		"zai/glm":  {InputPerMillion: 4, OutputPerMillion: 8},
		"bare-one": {InputPerMillion: 2, OutputPerMillion: 2},
	}})
	g := newAIGateway(aiGatewayParts{CostEngine: eng, Latency: aigateway.LatencySource(m.serviceLatency)})
	if g.Latency == nil || g.Prices == nil {
		t.Fatalf("Latency set: %v, Prices set: %v", g.Latency != nil, g.Prices != nil)
	}
	if ms, ok := g.Latency(context.Background(), "svc-1"); !ok || ms != 420 {
		t.Errorf("latency = %d, %v", ms, ok)
	}
	if _, ok := g.Latency(context.Background(), "nope"); ok {
		t.Error("latency of an unmeasured service reported ok")
	}
	if v, ok := g.Prices("zai", "glm"); !ok || v != 5 { // 0.75*4 + 0.25*8
		t.Errorf("price = %v, %v", v, ok)
	}
	if v, ok := g.Prices("other", "bare-one"); !ok || v != 2 {
		t.Errorf("bare-model fallback = %v, %v", v, ok)
	}
	if _, ok := g.Prices("zai", "unknown"); ok {
		t.Error("unpriced model reported ok")
	}
}
