package main

import (
	"context"
	"testing"
	"time"

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
	g := newAIGateway(aiGatewayParts{CostEngine: eng, Latency: aigateway.LatencySource(m.serviceLatencies)})
	if g.Latency == nil || g.Prices == nil {
		t.Fatalf("Latency set: %v, Prices set: %v", g.Latency != nil, g.Prices != nil)
	}
	// The first read is a cold start: no figure, the refresh runs behind it.
	if g.Latency(context.Background()) != nil {
		t.Error("cold start returned a figure")
	}
	waitFor(t, func() bool { return g.Latency(context.Background())["svc-1"] == 420 })
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

type blockingMetrics struct {
	countingMetrics
	release chan struct{}
}

func (b *blockingMetrics) AIEndpointLatencyP95(ctx context.Context) (map[string]int, error) {
	<-b.release
	return map[string]int{"svc-1": 7}, nil
}

// The routing read never waits for the computation, however slow the source.
func TestServiceLatencies_NeverBlocks(t *testing.T) {
	src := &blockingMetrics{release: make(chan struct{})}
	m, _ := testAIMetrics(src, nil)
	start := time.Now()
	if got := m.serviceLatencies(context.Background()); got != nil {
		t.Fatalf("cold start: %v", got)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("read took %v", d)
	}
	close(src.release)
	waitFor(t, func() bool { return m.serviceLatencies(context.Background())["svc-1"] == 7 })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("condition not reached")
}
