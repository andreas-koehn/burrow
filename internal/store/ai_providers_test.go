package store

import (
	"context"
	"testing"
)

func TestProviderSlugFromName(t *testing.T) {
	cases := map[string]string{
		"ollama":       "ollama",
		"Smoke Ollama": "smoke-ollama",
		"my_model.v2":  "my-model-v2",
		"  -- x --  ":  "",
		"ä":            "",
		"v1":           "",
		"A":            "",
	}
	for in, want := range cases {
		if got := ProviderSlugFromName(in); got != want {
			t.Errorf("ProviderSlugFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidProviderSlug_ReservesV1(t *testing.T) {
	if ValidProviderSlug("v1") {
		t.Fatal("v1 must be reserved")
	}
	if !ValidProviderSlug("zai") {
		t.Fatal("zai must be valid")
	}
}

func TestBackfillAIProviders(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := mustCreateUser(t, s, "bf@x", "user")
	a := mustGetOrCreateService(t, s, u.ID, "ollama", "http")
	b := mustGetOrCreateService(t, s, u.ID, "Ollama", "http")
	mustGetOrCreateService(t, s, u.ID, "web", "http")
	mustGetOrCreateService(t, s, u.ID, "pg", "tcp")
	// GetOrCreateService leaves the slug empty; the store normally assigns one.
	b.Subdomain = "f5wpq8"
	if err := s.q.SetServiceSubdomain(ctx, a.ID, "p7baeh"); err != nil {
		t.Fatal(err)
	}
	if err := s.q.SetServiceSubdomain(ctx, b.ID, b.Subdomain); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if err := s.q.SetServiceAccessMode(ctx, id, "api_key", "Authorization"); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.BackfillAIProviders(ctx)
	if err != nil || n != 2 {
		t.Fatalf("first run: n=%d err=%v, want 2", n, err)
	}
	p1, err := s.ProviderBySlug(ctx, "ollama")
	if err != nil || p1.Kind != "tunnel" || p1.APIFormat != "openai" {
		t.Fatalf("ollama: %v %+v", err, p1)
	}
	// The second service cannot take "ollama"; it falls back to its service slug.
	if _, err := s.ProviderBySlug(ctx, b.Subdomain); err != nil {
		t.Fatalf("collision fallback: %v", err)
	}
	if n, err := s.BackfillAIProviders(ctx); err != nil || n != 0 {
		t.Fatalf("second run: n=%d err=%v, want 0", n, err)
	}
}
