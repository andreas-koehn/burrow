package aigateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

func tgt(slug, svc, model string) Target {
	return Target{Provider: db.AIProvider{Slug: slug, ServiceID: svc}, Model: model}
}

func slugs(ts []Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Provider.Slug
	}
	return out
}

func TestOrderTargets(t *testing.T) {
	a, b, c := tgt("a", "svc-a", "m1"), tgt("b", "svc-b", "m2"), tgt("c", "svc-c", "m3")
	lat := map[string]int{"svc-a": 900, "svc-b": 200, "svc-c": 500000}
	price := map[string]float64{"b/m2": 5, "c/m3": 1}
	g := &Gateway{
		Latency: func(_ context.Context, svc string) (int, bool) { ms, ok := lat[svc]; return ms, ok },
		Prices:  func(p, m string) (float64, bool) { v, ok := price[p+"/"+m]; return v, ok },
	}
	ctx := context.Background()
	cases := []struct {
		routing string
		want    []string
	}{
		{"ordered", []string{"a", "b", "c"}},
		{"", []string{"a", "b", "c"}},
		{"fastest", []string{"b", "a", "c"}},  // c's 500 s is an outlier: unmeasured, last in configured order
		{"cheapest", []string{"c", "b", "a"}}, // a has no price: last
	}
	for _, tc := range cases {
		got := slugs(g.orderTargets(ctx, tc.routing, []Target{a, b, c}))
		if !equal(got, tc.want) {
			t.Errorf("%q: got %v, want %v", tc.routing, got, tc.want)
		}
	}
	// No sources: every mode keeps configured order.
	if got := slugs((&Gateway{}).orderTargets(ctx, "fastest", []Target{a, b, c})); !equal(got, []string{"a", "b", "c"}) {
		t.Errorf("no latency source: %v", got)
	}
	// Ties keep configured order.
	lat2 := map[string]int{"svc-a": 300, "svc-b": 300}
	g2 := &Gateway{Latency: func(_ context.Context, s string) (int, bool) { ms, ok := lat2[s]; return ms, ok }}
	if got := slugs(g2.orderTargets(ctx, "fastest", []Target{a, b})); !equal(got, []string{"a", "b"}) {
		t.Errorf("tie: %v", got)
	}
	// The input slice is not reordered in place.
	in := []Target{a, b, c}
	_ = g.orderTargets(ctx, "fastest", in)
	if slugs(in)[0] != "a" {
		t.Error("orderTargets changed its input")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A model routed "fastest" or "cheapest" sends its first attempt to the
// winner and falls back unchanged.
func TestRouting_FirstAttemptAndFallback(t *testing.T) {
	latency := func(_ context.Context, svc string) (int, bool) {
		return map[string]int{"prov-zai": 800, "prov-or": 100}[svc], true
	}
	prices := func(p, _ string) (float64, bool) {
		return map[string]float64{"zai": 9, "openrouter": 1}[p], true
	}
	for _, mode := range []string{"fastest", "cheapest"} {
		t.Run(mode+" first", func(t *testing.T) {
			s := script(map[string]http.HandlerFunc{
				"zai#ZAI":       status(200, `{"from":"zai"}`),
				"openrouter#OR": status(200, `{"from":"or"}`),
			})
			g, _ := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.Routing = mode })
			g.Latency, g.Prices = latency, prices
			rec := call(g, smartBody)
			wantHeaders(t, rec, "openrouter", "google/gemini-x", "1")
			if s.n("zai#ZAI") != 0 {
				t.Fatal("the slower/dearer target was called")
			}
		})
		t.Run(mode+" falls back", func(t *testing.T) {
			s := script(map[string]http.HandlerFunc{
				"zai#ZAI":       status(200, `{"from":"zai"}`),
				"openrouter#OR": status(503, `down`),
			})
			g, _ := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.Routing = mode })
			g.Latency, g.Prices = latency, prices
			rec := call(g, smartBody)
			wantHeaders(t, rec, "zai", "glm-5.1", "2")
		})
	}
	t.Run("ordered ignores the sources", func(t *testing.T) {
		s := script(map[string]http.HandlerFunc{
			"zai#ZAI":       status(200, `{}`),
			"openrouter#OR": status(200, `{}`),
		})
		g, _ := failoverGateway(s, "ZAI", nil)
		g.Latency, g.Prices = latency, prices
		wantHeaders(t, call(g, smartBody), "zai", "glm-5.1", "1")
	})
}
