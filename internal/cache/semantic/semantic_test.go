package semantic_test

import (
	"context"
	"testing"

	"github.com/ankoehn/burrow/internal/cache/semantic"
)

// TestNoopSemanticCacheZeroValueIsSafe verifies that NoopCache (the default
// build's Cache implementation) never returns a hit and always returns a
// zero-value Candidate with no error.
func TestNoopSemanticCacheZeroValueIsSafe(t *testing.T) {
	var c semantic.Cache = semantic.NoopCache{}
	cand, hit, err := c.Lookup(context.Background(), "svc", []byte(`{"prompt":"hi"}`), semantic.Settings{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Error("NoopCache must never hit")
	}
	if cand.Similarity != 0 {
		t.Error("NoopCache must return zero candidate")
	}
}

// TestNoopCachePromoteAndClearAreNoops asserts that Promote and ClearService
// never return errors and have no observable side effects.
func TestNoopCachePromoteAndClearAreNoops(t *testing.T) {
	ctx := context.Background()
	c := semantic.NoopCache{}

	if err := c.Promote(ctx, "svc", "keyhash", []byte(`{"prompt":"hi"}`), semantic.Settings{Enabled: true}); err != nil {
		t.Fatalf("Promote: unexpected error: %v", err)
	}
	if err := c.ClearService(ctx, "svc"); err != nil {
		t.Fatalf("ClearService: unexpected error: %v", err)
	}
	stats, err := c.Stats(ctx, "svc")
	if err != nil {
		t.Fatalf("Stats: unexpected error: %v", err)
	}
	if stats.Entries != 0 || stats.OnDiskBytes != 0 || stats.HitRate24h != 0 {
		t.Errorf("Stats must be zero: %+v", stats)
	}
}

// Available tells a tier that does something from none at all.
func TestAvailable(t *testing.T) {
	if semantic.Available(nil) || semantic.Available(semantic.NoopCache{}) {
		t.Fatal("nil or the no-op tier reported as available")
	}
	if !semantic.Available(struct{ semantic.Cache }{semantic.NoopCache{}}) {
		t.Fatal("a tier of another type reported as unavailable")
	}
}
