package aigw_test

import (
	"context"
	"fmt"
	"strings"
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

func TestRoute_Translation(t *testing.T) {
	r := aigw.NewRoute("gk", "anthropic", "burrow-medium", "req")
	if ri := r.Snapshot(); ri.Translated != "" || ri.Dropped != "" {
		t.Fatalf("fresh route: %+v", ri)
	}
	r.SetTranslation("messages-chat", []string{"top_k", "cache_control", "top_k"})
	ri := r.Snapshot()
	if ri.Translated != "messages-chat" || ri.Dropped != "cache_control,top_k" { // sorted, de-duplicated
		t.Fatalf("after SetTranslation: %+v", ri)
	}
	r.SetTranslation("", nil) // a later native attempt clears it
	if ri := r.Snapshot(); ri.Translated != "" || ri.Dropped != "" {
		t.Fatalf("after clearing: %+v", ri)
	}
	// Without a pair nothing was translated, so nothing was dropped either.
	r.SetTranslation("", []string{"top_k"})
	if ri := r.Snapshot(); ri.Translated != "" || ri.Dropped != "" {
		t.Fatalf("dropped without a pair: %+v", ri)
	}
}

// Dropped names come from field names of the request, which the client
// chooses: what is stored is cleaned and bounded.
func TestRoute_TranslationIsBounded(t *testing.T) {
	r := aigw.NewRoute("gk", "anthropic", "m", "req")
	r.SetTranslation("messages-chat", []string{"unknown:a,b", "x\r\ny\x00", " ", "", "tool:web search", "unknown:é\"<"})
	if got := r.Snapshot().Dropped; got != "tool:websearch,unknown:,unknown:ab,xy" {
		t.Fatalf("cleaned names = %q", got)
	}

	// One name is cut to 64 bytes.
	r.SetTranslation("messages-chat", []string{strings.Repeat("n", 500)})
	if got := r.Snapshot().Dropped; got != strings.Repeat("n", 64) {
		t.Fatalf("long name stored as %d bytes", len(got))
	}

	// Many names: at most 512 bytes, cut at a comma, at most 32 entries.
	var many []string
	for i := 0; i < 500; i++ {
		many = append(many, fmt.Sprintf("unknown:field_%03d_%s", i, strings.Repeat("x", 30)))
	}
	r.SetTranslation("messages-chat", many)
	got := r.Snapshot().Dropped
	parts := strings.Split(got, ",")
	if len(got) > 512 || len(parts) > 32 || len(parts) < 5 {
		t.Fatalf("dropped is %d bytes in %d entries", len(got), len(parts))
	}
	for i, p := range parts {
		if p != many[i] {
			t.Fatalf("entry %d = %q, want the whole name %q", i, p, many[i])
		}
	}
	short := make([]string, 100)
	for i := range short {
		short[i] = fmt.Sprintf("f%03d", i)
	}
	r.SetTranslation("messages-chat", short)
	if n := len(strings.Split(r.Snapshot().Dropped, ",")); n != 32 {
		t.Fatalf("%d entries stored, want 32", n)
	}

	// The pair id is one of a few constants; a wrong caller still cannot
	// store more than a short name.
	r.SetTranslation("messages-chat\n"+strings.Repeat("z", 300), nil)
	if got := r.Snapshot().Translated; len(got) != 64 || strings.ContainsAny(got, "\n ") {
		t.Fatalf("pair stored as %q", got)
	}
}
