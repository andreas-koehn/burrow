package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestCreateDirectProvider(t *testing.T) {
	s := newStore(t)
	ownerID := mustCreateUser(t, s, "admin@x", "admin").ID
	ctx := context.Background()
	in := DirectProviderInput{Slug: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1", CredentialSlot: "OPENROUTER"}

	p, err := s.CreateDirectProvider(ctx, ownerID, in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "direct" || p.ServiceID == "" || p.APIFormat != "openai" || p.Billing != "metered" ||
		p.AuthHeader != "Authorization" || p.AuthFormat != "Bearer {key}" {
		t.Fatalf("defaults: %+v", p)
	}
	// Keys work on the backing service straight away.
	if _, _, err := s.CreateAPIKey(ctx, ownerID, "admin", p.ServiceID, "ci"); err != nil {
		t.Fatalf("api key on backing service: %v", err)
	}

	const u = "https://x.example/v1"
	bad := map[string]DirectProviderInput{
		"http url":          {Slug: "a-1", Name: "A", BaseURL: "http://x.example/v1", CredentialSlot: "S"},
		"empty url":         {Slug: "a-2", Name: "A", CredentialSlot: "S"},
		"url with userinfo": {Slug: "a-2b", Name: "A", BaseURL: "https://u:pw@x.example/v1", CredentialSlot: "S"},
		"url with query":    {Slug: "a-2c", Name: "A", BaseURL: "https://x.example/v1?k=v", CredentialSlot: "S"},
		"bad slot":          {Slug: "a-3", Name: "A", BaseURL: u, CredentialSlot: "lower case"},
		"empty slot":        {Slug: "a-4", Name: "A", BaseURL: u},
		"format w/o key":    {Slug: "a-5", Name: "A", BaseURL: u, CredentialSlot: "S", AuthFormat: "Bearer"},
		"format two keys":   {Slug: "a-5b", Name: "A", BaseURL: u, CredentialSlot: "S", AuthFormat: "{key} {key}"},
		"format with CRLF":  {Slug: "a-5c", Name: "A", BaseURL: u, CredentialSlot: "S", AuthFormat: "Bearer {key}\r\nX-B: w"},
		"format with ctl":   {Slug: "a-5d", Name: "A", BaseURL: u, CredentialSlot: "S", AuthFormat: "Bearer\x01{key}"},
		"format too long":   {Slug: "a-5e", Name: "A", BaseURL: u, CredentialSlot: "S", AuthFormat: "{key}" + strings.Repeat("x", 128)},
		"bad header name":   {Slug: "a-6", Name: "A", BaseURL: u, CredentialSlot: "S", AuthHeader: "X Y"},
		"auth header host":  {Slug: "a-6b", Name: "A", BaseURL: u, CredentialSlot: "S", AuthHeader: "Host"},
		"auth header CRLF":  {Slug: "a-6c", Name: "A", BaseURL: u, CredentialSlot: "S", AuthHeader: "X-A\r\nX-B"},
		"bad billing":       {Slug: "a-7", Name: "A", BaseURL: u, CredentialSlot: "S", Billing: "free"},
		"bad api format":    {Slug: "a-8", Name: "A", BaseURL: u, CredentialSlot: "S", APIFormat: "grpc"},
		"auth as extra":     {Slug: "a-10", Name: "A", BaseURL: u, CredentialSlot: "S", ExtraHeaders: map[string]string{"authorization": "x"}},
		"credential header as extra": {Slug: "a-10b", Name: "A", BaseURL: u, CredentialSlot: "S", AuthHeader: "X-Upstream-Key", AuthFormat: "{key}",
			ExtraHeaders: map[string]string{"x-upstream-key": "x"}},
		"header injection":  {Slug: "a-11", Name: "A", BaseURL: u, CredentialSlot: "S", ExtraHeaders: map[string]string{"X-A": "v\r\nX-B: w"}},
		"header ctl value":  {Slug: "a-11b", Name: "A", BaseURL: u, CredentialSlot: "S", ExtraHeaders: map[string]string{"X-A": "v\x00"}},
		"header bad name":   {Slug: "a-11c", Name: "A", BaseURL: u, CredentialSlot: "S", ExtraHeaders: map[string]string{"X A": "v"}},
		"header name colon": {Slug: "a-11d", Name: "A", BaseURL: u, CredentialSlot: "S", ExtraHeaders: map[string]string{"X-A:": "v"}},
		"too many headers":  {Slug: "a-12", Name: "A", BaseURL: u, CredentialSlot: "S", ExtraHeaders: manyHeaders(17)},
		"headers too large": {Slug: "a-13", Name: "A", BaseURL: u, CredentialSlot: "S", ExtraHeaders: map[string]string{
			"X-A": strings.Repeat("a", 500), "X-B": strings.Repeat("a", 500), "X-C": strings.Repeat("a", 500), "X-D": strings.Repeat("a", 500),
			"X-E": strings.Repeat("a", 500), "X-F": strings.Repeat("a", 500), "X-G": strings.Repeat("a", 500), "X-H": strings.Repeat("a", 500),
			"X-I": strings.Repeat("a", 500)}},
		"header value too long": {Slug: "a-14", Name: "A", BaseURL: u, CredentialSlot: "S", ExtraHeaders: map[string]string{"X-A": strings.Repeat("a", 513)}},
		"no name":               {Slug: "a-15", BaseURL: u, CredentialSlot: "S"},
		"long name":             {Slug: "a-16", Name: strings.Repeat("n", 121), BaseURL: u, CredentialSlot: "S"},
	}
	for _, h := range []string{"Host", "Content-Length", "Transfer-Encoding", "Connection", "Keep-Alive", "TE", "Trailer", "Upgrade",
		"Proxy-Authorization", "proxy-connection", "X-Api-Key", "Cookie", "Forwarded", "X-Forwarded-For", "x-forwarded-host"} {
		bad["reserved header "+h] = DirectProviderInput{Slug: "a-9", Name: "A", BaseURL: u, CredentialSlot: "S", ExtraHeaders: map[string]string{h: "evil"}}
	}
	for name, in := range bad {
		if _, err := s.CreateDirectProvider(ctx, ownerID, in); !errors.Is(err, ErrInvalidProviderConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidProviderConfig", name, err)
		} else if strings.Contains(err.Error(), "evil") || strings.Contains(err.Error(), "pw@") {
			t.Errorf("%s: the reason repeats a header value or the URL: %v", name, err)
		}
	}
	if _, err := s.CreateDirectProvider(ctx, ownerID, DirectProviderInput{Slug: "v1", Name: "A", BaseURL: u, CredentialSlot: "S"}); !errors.Is(err, ErrInvalidProviderSlug) {
		t.Errorf("reserved slug err = %v", err)
	}
	if _, err := s.CreateDirectProvider(ctx, ownerID, in); !errors.Is(err, ErrProviderExists) {
		t.Errorf("duplicate err = %v", err)
	}
	// Nothing of the refused attempts was stored.
	if ps, _ := s.ListProviders(ctx); len(ps) != 1 {
		t.Fatalf("providers = %+v, want only openrouter", ps)
	}

	// A harmless extra header is kept.
	ok := DirectProviderInput{Slug: "zai", Name: "z.ai", BaseURL: "https://api.z.ai/v4", CredentialSlot: "ZAI", AuthHeader: "X-Api-Key", AuthFormat: "{key}",
		Billing: "flat", ExtraHeaders: map[string]string{"HTTP-Referer": "https://burrow.example", "X-Title": "Burrow"}}
	z, err := s.CreateDirectProvider(ctx, ownerID, ok)
	if err != nil || z.ExtraHeaders["X-Title"] != "Burrow" || z.Billing != "flat" || z.AuthHeader != "X-Api-Key" {
		t.Fatalf("valid provider: %v %+v", err, z)
	}

	// Deleting a direct provider removes its backing service, keys and models.
	if err := s.AddProviderModel(ctx, "openrouter", "glm-5.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProvider(ctx, "openrouter"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ServiceByID(ctx, p.ServiceID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("backing service survived: %v", err)
	}
	if keys, _ := s.q.ListServiceAPIKeys(ctx, p.ServiceID); len(keys) != 0 {
		t.Errorf("api keys survived: %+v", keys)
	}
	if models, _ := s.q.ListAIProviderModels(ctx, "openrouter"); len(models) != 0 {
		t.Errorf("models survived: %+v", models)
	}
}

func manyHeaders(n int) map[string]string {
	h := map[string]string{}
	for i := 0; i < n; i++ {
		h[fmt.Sprintf("X-H%d", i)] = "v"
	}
	return h
}

// Deleting a tunnel provider still keeps its service.
func TestDeleteProvider_KeepsTunnelService(t *testing.T) {
	s := newStore(t)
	svcA, _, _ := providerFixture(t, s)
	ctx := context.Background()
	if _, err := s.CreateTunnelProvider(ctx, "ollama", "Ollama", svcA); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProvider(ctx, "ollama"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ServiceByID(ctx, svcA); err != nil {
		t.Fatalf("tunnel service gone: %v", err)
	}
}

func TestUpdateProviderUpstream(t *testing.T) {
	s := newStore(t)
	ownerID := mustCreateUser(t, s, "admin@x", "admin").ID
	svcA, _, _ := providerFixture(t, s)
	ctx := context.Background()
	if _, err := s.CreateDirectProvider(ctx, ownerID, DirectProviderInput{Slug: "zai", Name: "z.ai", BaseURL: "https://api.z.ai/v4", CredentialSlot: "ZAI",
		AuthHeader: "X-Api-Key", AuthFormat: "{key}", ExtraHeaders: map[string]string{"X-Title": "Burrow"}}); err != nil {
		t.Fatal(err)
	}

	// Base URL and billing change; what the caller left out is kept.
	p, err := s.UpdateProviderUpstream(ctx, "zai", DirectProviderInput{BaseURL: "https://api.z.ai/v5", Billing: "flat"})
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseURL != "https://api.z.ai/v5" || p.Billing != "flat" || p.CredentialSlot != "ZAI" || p.AuthHeader != "X-Api-Key" ||
		p.AuthFormat != "{key}" || p.ExtraHeaders["X-Title"] != "Burrow" || p.Name != "z.ai" || p.Kind != "direct" {
		t.Fatalf("after update: %+v", p)
	}
	// Changing the slot and clearing the extra headers is explicit.
	p, err = s.UpdateProviderUpstream(ctx, "zai", DirectProviderInput{CredentialSlot: "ZAI_2", ExtraHeaders: map[string]string{}})
	if err != nil || p.CredentialSlot != "ZAI_2" || len(p.ExtraHeaders) != 0 || p.BaseURL != "https://api.z.ai/v5" {
		t.Fatalf("slot change: %v %+v", err, p)
	}

	for name, in := range map[string]DirectProviderInput{
		"http url":      {BaseURL: "http://api.z.ai/v4"},
		"bad slot":      {CredentialSlot: "no"},
		"hop header":    {ExtraHeaders: map[string]string{"Host": "evil"}},
		"cred as extra": {ExtraHeaders: map[string]string{"X-API-KEY": "x"}},
		"bad format":    {AuthFormat: "Bearer"},
	} {
		if _, err := s.UpdateProviderUpstream(ctx, "zai", in); !errors.Is(err, ErrInvalidProviderConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidProviderConfig", name, err)
		}
	}
	if got, _ := s.ProviderBySlug(ctx, "zai"); got.BaseURL != "https://api.z.ai/v5" || got.CredentialSlot != "ZAI_2" {
		t.Fatalf("a refused update changed the row: %+v", got)
	}

	if _, err := s.UpdateProviderUpstream(ctx, "gone", DirectProviderInput{BaseURL: "https://x.example/v1"}); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("unknown provider err = %v", err)
	}
	if _, err := s.CreateTunnelProvider(ctx, "ollama", "Ollama", svcA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProviderUpstream(ctx, "ollama", DirectProviderInput{BaseURL: "https://x.example/v1", CredentialSlot: "S"}); !errors.Is(err, ErrInvalidProviderConfig) {
		t.Fatalf("tunnel provider err = %v", err)
	}
}

func TestProviderModels(t *testing.T) {
	s := newStore(t)
	svcA, _, _ := providerFixture(t, s)
	ctx := context.Background()
	if _, err := s.CreateTunnelProvider(ctx, "ollama", "Ollama", svcA); err != nil {
		t.Fatal(err)
	}

	if err := s.AddProviderModel(ctx, "ollama", "google/gemini-x"); err != nil {
		t.Fatal(err)
	}
	// Adding the same id again is not an error and does not duplicate it.
	if err := s.AddProviderModel(ctx, "ollama", "google/gemini-x"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", strings.Repeat("x", 201), "a\nb", "a\x00", " a"} {
		if err := s.AddProviderModel(ctx, "ollama", id); !errors.Is(err, ErrInvalidProviderConfig) {
			t.Errorf("AddProviderModel(%q) err = %v, want ErrInvalidProviderConfig", id, err)
		}
	}
	if err := s.AddProviderModel(ctx, "gone", "m"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("add on unknown provider err = %v", err)
	}
	got, err := s.ListProviderModels(ctx, "ollama")
	if err != nil || len(got) != 1 || got[0].ModelID != "google/gemini-x" {
		t.Fatalf("list: %v %+v", err, got)
	}
	if _, err := s.ListProviderModels(ctx, "gone"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("list on unknown provider err = %v", err)
	}

	// Replace is idempotent, drops what is gone, and takes only valid rows.
	next := []db.AIProviderModel{{ModelID: "mistral", DisplayName: "Mistral", ContextLength: 32768}, {ModelID: "glm-5.1"}}
	for i := 0; i < 2; i++ {
		if err := s.ReplaceProviderModels(ctx, "ollama", next); err != nil {
			t.Fatal(err)
		}
		got, _ = s.ListProviderModels(ctx, "ollama")
		if len(got) != 2 || got[0].ModelID != "glm-5.1" || got[1].ModelID != "mistral" || got[1].ContextLength != 32768 {
			t.Fatalf("after replace %d: %+v", i, got)
		}
	}
	for name, models := range map[string][]db.AIProviderModel{
		"bad id":    {{ModelID: "ok"}, {ModelID: "a\x00"}},
		"repeated":  {{ModelID: "ok"}, {ModelID: "ok"}},
		"too many":  make([]db.AIProviderModel, 5001),
		"long name": {{ModelID: "ok", DisplayName: strings.Repeat("n", 201)}},
		"negative":  {{ModelID: "ok", ContextLength: -1}},
	} {
		if err := s.ReplaceProviderModels(ctx, "ollama", models); !errors.Is(err, ErrInvalidProviderConfig) {
			t.Errorf("replace %s: err = %v, want ErrInvalidProviderConfig", name, err)
		}
	}
	if got, _ = s.ListProviderModels(ctx, "ollama"); len(got) != 2 {
		t.Fatalf("a refused replace changed the catalog: %+v", got)
	}
	if err := s.ReplaceProviderModels(ctx, "gone", next); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("replace on unknown provider err = %v", err)
	}

	if err := s.RemoveProviderModel(ctx, "ollama", "mistral"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveProviderModel(ctx, "ollama", "mistral"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("second remove err = %v", err)
	}
	if err := s.RemoveProviderModel(ctx, "gone", "mistral"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("remove on unknown provider err = %v", err)
	}
}

// The backing row of a direct provider is not an ordinary service: its slug
// and access mode cannot be changed, its keys can be managed.
func TestDirectBackingService_Guards(t *testing.T) {
	s := newStore(t)
	ownerID := mustCreateUser(t, s, "admin@x", "admin").ID
	ctx := context.Background()
	p, err := s.CreateDirectProvider(ctx, ownerID, DirectProviderInput{Slug: "zai", Name: "z.ai", BaseURL: "https://api.z.ai/v4", CredentialSlot: "ZAI"})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"open", "burrow_login", "api_key"} {
		if err := s.SetServiceAccessMode(ctx, ownerID, "admin", p.ServiceID, mode, "", nil); !errors.Is(err, ErrDirectService) {
			t.Errorf("access mode %s err = %v, want ErrDirectService", mode, err)
		}
	}
	if _, err := s.SetServiceSlug(ctx, ownerID, "admin", p.ServiceID, "my-slug"); !errors.Is(err, ErrDirectService) {
		t.Errorf("slug err = %v, want ErrDirectService", err)
	}
	svc, _ := s.ServiceByID(ctx, p.ServiceID)
	if svc.AccessMode != "api_key" || svc.Subdomain != "" {
		t.Fatalf("backing row changed: %+v", svc)
	}
	// It cannot be turned into a tunnel provider either.
	if _, err := s.CreateTunnelProvider(ctx, "second", "Second", p.ServiceID); !errors.Is(err, ErrProviderService) {
		t.Errorf("tunnel provider on a direct row err = %v", err)
	}
	id, _, err := s.CreateAPIKey(ctx, ownerID, "admin", p.ServiceID, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAPIKey(ctx, ownerID, "admin", p.ServiceID, id); err != nil {
		t.Fatalf("delete key: %v", err)
	}
}

// After a rename the old slug and the old name are free again: the backing
// service follows the provider's name and its id does not come from the slug.
func TestRenameDirectProvider_FreesSlugAndName(t *testing.T) {
	s := newStore(t)
	ownerID := mustCreateUser(t, s, "admin@x", "admin").ID
	ctx := context.Background()
	in := DirectProviderInput{Slug: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1", CredentialSlot: "OPENROUTER"}

	first, err := s.CreateDirectProvider(ctx, ownerID, in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.ServiceID, "openrouter") {
		t.Fatalf("service id %q is derived from the slug", first.ServiceID)
	}
	renamed, err := s.UpdateProvider(ctx, "openrouter", "router2", "Router Two")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ServiceID != first.ServiceID {
		t.Fatalf("rename changed the backing service: %q -> %q", first.ServiceID, renamed.ServiceID)
	}
	if svc, err := s.ServiceByID(ctx, first.ServiceID); err != nil || svc.Name != "Router Two" {
		t.Fatalf("backing service = %+v (%v), want name Router Two", svc, err)
	}
	second, err := s.CreateDirectProvider(ctx, ownerID, in)
	if err != nil {
		t.Fatalf("old slug and name are still taken: %v", err)
	}
	if second.ServiceID == first.ServiceID {
		t.Fatal("both providers share a backing service")
	}
}
