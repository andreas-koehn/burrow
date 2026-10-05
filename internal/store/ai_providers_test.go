package store

import (
	"context"
	"errors"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
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

// providerFixture creates one http service in api_key mode, one http service
// in open mode and one tcp service, and returns their ids.
func providerFixture(t *testing.T, s *Store) (svcA, svcOpen, svcTCP string) {
	t.Helper()
	u := mustCreateUser(t, s, "prov@x", "user")
	a := mustGetOrCreateService(t, s, u.ID, "llm", "http")
	if err := s.q.SetServiceAccessMode(context.Background(), a.ID, "api_key", "Authorization"); err != nil {
		t.Fatal(err)
	}
	return a.ID, mustGetOrCreateService(t, s, u.ID, "web", "http").ID, mustGetOrCreateService(t, s, u.ID, "pg", "tcp").ID
}

func TestCreateTunnelProvider(t *testing.T) {
	s := newStore(t)
	svcA, svcOpen, svcTCP := providerFixture(t, s)
	ctx := context.Background()

	p, err := s.CreateTunnelProvider(ctx, "ollama", "Ollama", svcA)
	if err != nil || p.Slug != "ollama" || p.Kind != "tunnel" || p.APIFormat != "openai" {
		t.Fatalf("create: %v %+v", err, p)
	}
	if _, err := s.CreateTunnelProvider(ctx, "ollama", "Again", svcA); !errors.Is(err, ErrProviderExists) {
		t.Fatalf("duplicate err = %v", err)
	}
	// One provider per service: a free slug does not help.
	if _, err := s.CreateTunnelProvider(ctx, "second", "Second", svcA); !errors.Is(err, ErrProviderExists) {
		t.Fatalf("second provider on the same service err = %v", err)
	}
	if _, err := s.CreateTunnelProvider(ctx, "v1", "Reserved", svcA); !errors.Is(err, ErrInvalidProviderSlug) {
		t.Fatalf("reserved slug err = %v", err)
	}
	if _, err := s.CreateTunnelProvider(ctx, "Bad_Slug", "Bad", svcA); !errors.Is(err, ErrInvalidProviderSlug) {
		t.Fatalf("malformed slug err = %v", err)
	}
	if _, err := s.CreateTunnelProvider(ctx, "open", "Open", svcOpen); !errors.Is(err, ErrProviderService) {
		t.Fatalf("open-mode service err = %v", err)
	}
	if _, err := s.CreateTunnelProvider(ctx, "tcp", "TCP", svcTCP); !errors.Is(err, ErrProviderService) {
		t.Fatalf("tcp service err = %v", err)
	}
	if _, err := s.CreateTunnelProvider(ctx, "ghost", "Ghost", "no-such-service"); !errors.Is(err, ErrProviderService) {
		t.Fatalf("missing service err = %v", err)
	}
	ps, err := s.ListProviders(ctx)
	if err != nil || len(ps) != 1 || ps[0].Slug != "ollama" {
		t.Fatalf("list: %v %+v", err, ps)
	}
}

func TestUpdateAndDeleteProvider(t *testing.T) {
	s := newStore(t)
	svcA, _, _ := providerFixture(t, s)
	ctx := context.Background()
	if _, err := s.CreateTunnelProvider(ctx, "ollama", "Ollama", svcA); err != nil {
		t.Fatal(err)
	}
	p, err := s.UpdateProvider(ctx, "ollama", "local", "Local models")
	if err != nil || p.Slug != "local" || p.Name != "Local models" {
		t.Fatalf("update: %v %+v", err, p)
	}
	// The rename is visible to the /ai/ lookup at once: old slug gone, new one live.
	if _, err := s.ProviderBySlug(ctx, "ollama"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("old slug err = %v", err)
	}
	if _, err := s.UpdateProvider(ctx, "local", "v1", "x"); !errors.Is(err, ErrInvalidProviderSlug) {
		t.Fatalf("reserved err = %v", err)
	}
	if _, err := s.UpdateProvider(ctx, "gone", "abc", "x"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("missing err = %v", err)
	}
	if err := s.DeleteProvider(ctx, "local"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProvider(ctx, "local"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}

// A deleted provider stays deleted: its service is still in api_key mode, but
// the start-time backfill runs only once per database.
func TestBackfillAIProviders_DeletedProviderStaysDeleted(t *testing.T) {
	s := newStore(t)
	svcA, _, _ := providerFixture(t, s)
	ctx := context.Background()

	if n, err := s.BackfillAIProviders(ctx); err != nil || n != 1 {
		t.Fatalf("first run: n=%d err=%v, want 1", n, err)
	}
	p, err := s.q.GetAIProviderByService(ctx, svcA)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProvider(ctx, p.Slug); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BackfillAIProviders(ctx); err != nil || n != 0 {
		t.Fatalf("run after delete: n=%d err=%v, want 0", n, err)
	}
	if _, err := s.q.GetAIProviderByService(ctx, svcA); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("provider re-created after delete: err = %v", err)
	}
}

// The backfill is a one-time migration step, not a standing rule.
func TestBackfillAIProviders_RunsOnlyOnce(t *testing.T) {
	s := newStore(t)
	_, svcOpen, _ := providerFixture(t, s)
	ctx := context.Background()

	if _, err := s.BackfillAIProviders(ctx); err != nil {
		t.Fatal(err)
	}
	// A service switched to api_key mode later is not picked up: providers are
	// created by hand from now on.
	if err := s.q.SetServiceAccessMode(ctx, svcOpen, "api_key", "Authorization"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BackfillAIProviders(ctx); err != nil || n != 0 {
		t.Fatalf("second run: n=%d err=%v, want 0", n, err)
	}
	if _, err := s.q.GetAIProviderByService(ctx, svcOpen); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("late service got a provider: err = %v", err)
	}
}
