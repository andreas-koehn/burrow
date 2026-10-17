package main

import (
	"context"
	"testing"

	"github.com/ankoehn/burrow/internal/config"
)

// A price table saved with PUT /cost/pricing is in the settings; the stack
// built at start-up applies it over the bundled table.
func TestBuildV04Stack_AppliesStoredPricing(t *testing.T) {
	st, wrapped := freshTestStack(t)
	saved := `{"version":"saved-by-admin","entries":[{"provider":"openai","model":"gpt-image-1","input_per_million":0,"output_per_million":0,"per_request_usd":0.04}]}`
	if err := st.SaveSettings(context.Background(), map[string]string{"cost.pricing": saved}); err != nil {
		t.Fatal(err)
	}
	stack, err := buildV04Stack(context.Background(), &config.ServerConfig{}, wrapped.DB(), st, discardLog())
	if err != nil {
		t.Fatalf("buildV04Stack: %v", err)
	}
	t.Cleanup(stack.GuardrailAudit.stop)
	t.Cleanup(stack.Observer.stop)
	t.Cleanup(func() { stack.WebhookDispatcher.Close() })
	p := stack.CostEngine.Pricing()
	if e, ok := p.Lookup("openai/gpt-image-1"); p.Version != "saved-by-admin" || !ok || e.PerRequestUSD != 0.04 {
		t.Fatalf("version %q, entry %+v %v", p.Version, e, ok)
	}
}
