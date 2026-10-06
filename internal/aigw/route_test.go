package aigw_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
)

func TestRoute_ContextRoundTrip(t *testing.T) {
	if _, ok := aigw.RouteFrom(context.Background()); ok {
		t.Fatal("empty context reported a route")
	}
	r := aigw.NewRoute("gk1", "anthropic", "burrow-medium", "req-1")
	ctx := aigw.WithRoute(context.Background(), r)
	r.SetTarget("openrouter", "google/gemini-x") // set after the context was built
	got, ok := aigw.RouteFrom(ctx)
	want := aigw.RouteInfo{GatewayKeyID: "gk1", Dialect: "anthropic", ProviderSlug: "openrouter", RequestedModel: "burrow-medium", TargetModel: "google/gemini-x", RequestID: "req-1"}
	if !ok || got != want {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

// A nil route in the context counts as no route.
func TestRoute_NilRouteInContext(t *testing.T) {
	if _, ok := aigw.RouteFrom(aigw.WithRoute(context.Background(), nil)); ok {
		t.Fatal("nil route reported as a route")
	}
}

func TestRoute_ConcurrentSetAndSnapshot(t *testing.T) {
	r := aigw.NewRoute("gk1", "openai", "m", "req")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); r.SetTarget("p", "t") }()
		go func() { defer wg.Done(); _ = r.Snapshot() }()
	}
	wg.Wait() // meaningful under -race
}
