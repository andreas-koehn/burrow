package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func newDBWithService(t *testing.T) (*DB, string) {
	t.Helper()
	x := testDB(t)
	mustUser(t, x, "u1")
	return x, seedSvc(t, x, "u1", "svc-a")
}

func TestAIProviders_CRUD(t *testing.T) {
	x, svcID := newDBWithService(t)
	ctx := context.Background()

	p := AIProvider{Slug: "ollama", Name: "Ollama", Kind: "tunnel", ServiceID: svcID, APIFormat: "openai"}
	if err := x.CreateAIProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := x.CreateAIProvider(ctx, p); !errors.Is(err, ErrDuplicateProvider) {
		t.Fatalf("duplicate slug err = %v", err)
	}
	dupSvc := AIProvider{Slug: "other", Name: "Other", Kind: "tunnel", ServiceID: svcID, APIFormat: "openai"}
	if err := x.CreateAIProvider(ctx, dupSvc); !errors.Is(err, ErrDuplicateProvider) {
		t.Fatalf("one service backing two providers err = %v", err)
	}

	got, err := x.GetAIProvider(ctx, "ollama")
	if err != nil || got.ServiceID != svcID || got.Kind != "tunnel" || got.CreatedAt.IsZero() {
		t.Fatalf("get: %v %+v", err, got)
	}
	bySvc, err := x.GetAIProviderByService(ctx, svcID)
	if err != nil || bySvc.Slug != "ollama" {
		t.Fatalf("by service: %v %+v", err, bySvc)
	}
	if _, err := x.GetAIProvider(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}

	if err := x.UpdateAIProvider(ctx, "ollama", "local", "Local models"); err != nil {
		t.Fatal(err)
	}
	list, err := x.ListAIProviders(ctx)
	if err != nil || len(list) != 1 || list[0].Slug != "local" || list[0].Name != "Local models" {
		t.Fatalf("list after rename: %v %+v", err, list)
	}
	if err := x.UpdateAIProvider(ctx, "gone", "x-y-z", "n"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing err = %v", err)
	}

	if err := x.DeleteAIProvider(ctx, "local"); err != nil {
		t.Fatal(err)
	}
	if err := x.DeleteAIProvider(ctx, "local"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	list, _ = x.ListAIProviders(ctx)
	if list == nil || len(list) != 0 {
		t.Fatalf("list after delete = %#v, want empty non-nil", list)
	}
}

func TestAIProviders_DeletedWithService(t *testing.T) {
	x, svcID := newDBWithService(t)
	ctx := context.Background()
	_ = x.CreateAIProvider(ctx, AIProvider{Slug: "ollama", Name: "Ollama", Kind: "tunnel", ServiceID: svcID, APIFormat: "openai"})
	if _, err := x.sqlDB.ExecContext(ctx, `DELETE FROM services WHERE id=?`, svcID); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetAIProvider(ctx, "ollama"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("provider survived its service: %v", err)
	}
}

func TestDirectAIProvider_CreateReadDelete(t *testing.T) {
	x, _ := newDBWithService(t)
	ctx := context.Background()
	svc := Service{ID: "prov-openrouter", UserID: "u1", Name: "OpenRouter", Type: "direct", AccessMode: "api_key"}
	p := AIProvider{
		Slug: "openrouter", Name: "OpenRouter", Kind: "direct", ServiceID: svc.ID, APIFormat: "openai",
		BaseURL: "https://openrouter.ai/api/v1", CredentialSlot: "OPENROUTER",
		AuthHeader: "Authorization", AuthFormat: "Bearer {key}",
		ExtraHeaders: map[string]string{"X-Title": "Burrow"}, Billing: "metered",
	}
	if err := x.CreateDirectAIProvider(ctx, svc, p); err != nil {
		t.Fatal(err)
	}
	got, err := x.GetAIProvider(ctx, "openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != p.BaseURL || got.CredentialSlot != "OPENROUTER" || got.Billing != "metered" ||
		got.ExtraHeaders["X-Title"] != "Burrow" || got.AuthFormat != "Bearer {key}" {
		t.Fatalf("round trip: %+v", got)
	}
	backing, err := x.GetServiceByID(ctx, "prov-openrouter")
	if err != nil || backing.Type != "direct" || backing.AccessMode != "api_key" {
		t.Fatalf("backing service: %v %+v", err, backing)
	}

	// A failed provider insert must not leave the backing service behind.
	svc2 := Service{ID: "prov-dup", UserID: svc.UserID, Name: "Dup", Type: "direct", AccessMode: "api_key"}
	dup := p
	dup.ServiceID = svc2.ID // same slug as p
	if err := x.CreateDirectAIProvider(ctx, svc2, dup); !errors.Is(err, ErrDuplicateProvider) {
		t.Fatalf("duplicate err = %v", err)
	}
	if _, err := x.GetServiceByID(ctx, "prov-dup"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan backing service left behind: %v", err)
	}

	if err := x.DeleteAIProviderAndBacking(ctx, "openrouter"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetServiceByID(ctx, "prov-openrouter"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backing service survived: %v", err)
	}
}

func TestDeleteAIProviderAndBacking_KeepsTunnelService(t *testing.T) {
	x, svcID := newDBWithService(t)
	ctx := context.Background()
	_ = x.CreateAIProvider(ctx, AIProvider{Slug: "ollama", Name: "Ollama", Kind: "tunnel", ServiceID: svcID, APIFormat: "openai"})
	if err := x.DeleteAIProviderAndBacking(ctx, "ollama"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetServiceByID(ctx, svcID); err != nil {
		t.Fatalf("a tunnel provider's service must be kept: %v", err)
	}
	if err := x.DeleteAIProviderAndBacking(ctx, "ollama"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}

func TestTunnelProvider_ReadsDefaults(t *testing.T) {
	x, svcID := newDBWithService(t)
	ctx := context.Background()
	_ = x.CreateAIProvider(ctx, AIProvider{Slug: "ollama", Name: "Ollama", Kind: "tunnel", ServiceID: svcID, APIFormat: "openai"})
	got, _ := x.GetAIProvider(ctx, "ollama")
	if got.ExtraHeaders == nil || len(got.ExtraHeaders) != 0 || got.Billing != "metered" || got.AuthHeader != "Authorization" {
		t.Fatalf("defaults: %+v", got)
	}
}

func directFixture(t *testing.T, x *DB) (Service, AIProvider) {
	t.Helper()
	svc := Service{ID: "prov-openrouter", UserID: "u1", Name: "OpenRouter", Type: "direct", AccessMode: "api_key"}
	p := AIProvider{
		Slug: "openrouter", Name: "OpenRouter", Kind: "direct", ServiceID: svc.ID, APIFormat: "openai",
		BaseURL: "https://openrouter.ai/api/v1", CredentialSlot: "OPENROUTER",
	}
	if err := x.CreateDirectAIProvider(context.Background(), svc, p); err != nil {
		t.Fatal(err)
	}
	return svc, p
}

// The function owns the row types: a caller cannot make a "direct" provider
// out of an http service row or a tunnel provider row.
func TestCreateDirectAIProvider_ForcesDirect(t *testing.T) {
	x, _ := newDBWithService(t)
	ctx := context.Background()
	svc := Service{ID: "prov-zai", UserID: "u1", Name: "z.ai", Type: "http", AccessMode: "api_key"}
	p := AIProvider{Slug: "zai", Name: "z.ai", Kind: "tunnel", ServiceID: svc.ID, APIFormat: "openai", BaseURL: "https://api.z.ai/v1", CredentialSlot: "ZAI"}
	if err := x.CreateDirectAIProvider(ctx, svc, p); err != nil {
		t.Fatal(err)
	}
	got, _ := x.GetAIProvider(ctx, "zai")
	backing, _ := x.GetServiceByID(ctx, "prov-zai")
	if got.Kind != "direct" || backing.Type != "direct" {
		t.Fatalf("kind %q type %q, want direct/direct", got.Kind, backing.Type)
	}
}

func TestModifyAIProviderUpstream(t *testing.T) {
	x, _ := newDBWithService(t)
	ctx := context.Background()
	_, p := directFixture(t, x)

	p.BaseURL, p.CredentialSlot, p.Billing, p.APIFormat = "https://api.z.ai/v4", "ZAI", "flat", "anthropic"
	p.AuthHeader, p.AuthFormat = "X-Api-Key", "{key}"
	p.ExtraHeaders = map[string]string{"X-Title": "Burrow"}
	if err := x.ModifyAIProviderUpstream(ctx, "openrouter", func(stored AIProvider) (AIProvider, error) {
		if stored.BaseURL != "https://openrouter.ai/api/v1" || stored.CredentialSlot != "OPENROUTER" {
			t.Errorf("modify was handed %+v, want the stored row", stored)
		}
		// Only the upstream settings of the result are stored.
		p.Name, p.Kind = "Ignored", "tunnel"
		return p, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := x.GetAIProvider(ctx, "openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "https://api.z.ai/v4" || got.CredentialSlot != "ZAI" || got.Billing != "flat" || got.APIFormat != "anthropic" ||
		got.AuthHeader != "X-Api-Key" || got.AuthFormat != "{key}" || got.ExtraHeaders["X-Title"] != "Burrow" ||
		got.Kind != "direct" || got.Name != "OpenRouter" {
		t.Fatalf("after update: %+v", got)
	}

	if err := x.ModifyAIProviderUpstream(ctx, "gone", func(p AIProvider) (AIProvider, error) {
		t.Error("modify ran for an unknown slug")
		return p, nil
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown slug err = %v, want ErrNotFound", err)
	}
}

// A provider of kind "direct" whose service is not of type "direct" (a row
// written by hand): the provider goes, the foreign service stays.
func TestDeleteAIProviderAndBacking_DirectOnForeignService(t *testing.T) {
	x, svcID := newDBWithService(t)
	ctx := context.Background()
	if err := x.CreateAIProvider(ctx, AIProvider{Slug: "odd", Name: "Odd", Kind: "direct", ServiceID: svcID, APIFormat: "openai"}); err != nil {
		t.Fatal(err)
	}
	if err := x.DeleteAIProviderAndBacking(ctx, "odd"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetAIProvider(ctx, "odd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("provider survived the delete: %v", err)
	}
	if _, err := x.GetServiceByID(ctx, svcID); err != nil {
		t.Fatalf("a service that is not of type direct must be kept: %v", err)
	}
}

// Deleting a direct provider takes its backing service's API keys along.
func TestDeleteAIProviderAndBacking_RemovesKeysAndModels(t *testing.T) {
	x, _ := newDBWithService(t)
	ctx := context.Background()
	svc, _ := directFixture(t, x)
	if err := x.CreateServiceAPIKey(ctx, ServiceAPIKey{ID: "k1", ServiceID: svc.ID, Name: "ci", KeyHash: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := x.UpsertAIProviderModel(ctx, AIProviderModel{ProviderSlug: "openrouter", ModelID: "glm-5.1"}); err != nil {
		t.Fatal(err)
	}
	if err := x.DeleteAIProviderAndBacking(ctx, "openrouter"); err != nil {
		t.Fatal(err)
	}
	if keys, _ := x.ListServiceAPIKeys(ctx, svc.ID); len(keys) != 0 {
		t.Fatalf("api keys survived: %+v", keys)
	}
	if models, _ := x.ListAIProviderModels(ctx, "openrouter"); len(models) != 0 {
		t.Fatalf("models survived: %+v", models)
	}
}

// A tunnel client registering under the name of a direct provider's backing
// row must not be handed that row.
func TestGetOrCreateService_NeverReturnsDirectRow(t *testing.T) {
	x, _ := newDBWithService(t)
	ctx := context.Background()
	directFixture(t, x) // backing row: user u1, name "OpenRouter"

	got, err := x.GetOrCreateService(ctx, "u1", "OpenRouter", "http")
	if !errors.Is(err, ErrServiceNameReserved) {
		t.Fatalf("err = %v (service %+v), want ErrServiceNameReserved", err, got)
	}
	if got.ID != "" {
		t.Fatalf("the direct row leaked to the caller: %+v", got)
	}
	backing, _ := x.GetServiceByID(ctx, "prov-openrouter")
	if backing.Type != "direct" || backing.Subdomain != "" {
		t.Fatalf("backing row changed: %+v", backing)
	}
	// Another user may use the name; it is unique per user only.
	mustUser(t, x, "u2")
	if s, err := x.GetOrCreateService(ctx, "u2", "OpenRouter", "http"); err != nil || s.Type != "http" {
		t.Fatalf("other user: %v %+v", err, s)
	}
}

// A rename of a direct provider renames its backing service with it, so the
// old name is free again for the same owner. A tunnel provider's service is a
// user's own service and keeps its name.
func TestUpdateAIProvider_RenamesDirectBackingService(t *testing.T) {
	x, svcID := newDBWithService(t)
	ctx := context.Background()
	svc, _ := directFixture(t, x)
	if err := x.UpdateAIProvider(ctx, "openrouter", "router2", "Router Two"); err != nil {
		t.Fatal(err)
	}
	if backing, _ := x.GetServiceByID(ctx, svc.ID); backing.Name != "Router Two" || backing.Type != "direct" {
		t.Fatalf("backing service = %+v, want name Router Two", backing)
	}

	before, _ := x.GetServiceByID(ctx, svcID)
	if err := x.CreateAIProvider(ctx, AIProvider{Slug: "local", Name: "Local", Kind: "tunnel", ServiceID: svcID, APIFormat: "openai"}); err != nil {
		t.Fatal(err)
	}
	if err := x.UpdateAIProvider(ctx, "local", "local2", "Renamed"); err != nil {
		t.Fatal(err)
	}
	if after, _ := x.GetServiceByID(ctx, svcID); after.Name != before.Name {
		t.Fatalf("tunnel service renamed: %q -> %q", before.Name, after.Name)
	}

	// A name the owner already uses is refused and nothing changes.
	if err := x.UpdateAIProvider(ctx, "router2", "router3", before.Name); !errors.Is(err, ErrDuplicateProvider) {
		t.Fatalf("name clash err = %v, want ErrDuplicateProvider", err)
	}
	if p, err := x.GetAIProvider(ctx, "router2"); err != nil || p.Name != "Router Two" {
		t.Fatalf("refused rename left a trace: %v %+v", err, p)
	}
}

// Two admins saving at the same time: the second write is applied to what
// the first one stored, not to the copy it read before.
func TestModifyAIProviderUpstream_NoLostUpdate(t *testing.T) {
	x, _ := newDBWithService(t)
	ctx := context.Background()
	directFixture(t, x)

	calls := 0
	err := x.ModifyAIProviderUpstream(ctx, "openrouter", func(p AIProvider) (AIProvider, error) {
		calls++
		if calls == 1 {
			// The other admin gets in between this read and this write.
			if err := x.ModifyAIProviderUpstream(ctx, "openrouter", func(other AIProvider) (AIProvider, error) {
				other.Billing, other.ExtraHeaders = "flat", map[string]string{"X-Title": "Burrow"}
				return other, nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		p.BaseURL = "https://openrouter.ai/api/v2"
		return p, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("modify ran %d times, want 2 (once more on the fresh row)", calls)
	}
	p, _ := x.GetAIProvider(ctx, "openrouter")
	if p.BaseURL != "https://openrouter.ai/api/v2" || p.Billing != "flat" || p.ExtraHeaders["X-Title"] != "Burrow" {
		t.Fatalf("an update was lost: %+v", p)
	}

	// modify's error stops the update; an unknown slug is ErrNotFound.
	boom := errors.New("refused")
	if err := x.ModifyAIProviderUpstream(ctx, "openrouter", func(p AIProvider) (AIProvider, error) {
		p.BaseURL = "https://evil.example/v1"
		return p, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want modify's error", err)
	}
	if p, _ := x.GetAIProvider(ctx, "openrouter"); p.BaseURL != "https://openrouter.ai/api/v2" {
		t.Fatalf("a refused update was stored: %+v", p)
	}
	if err := x.ModifyAIProviderUpstream(ctx, "gone", func(p AIProvider) (AIProvider, error) { return p, nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown slug err = %v", err)
	}
}

// A row that changes under every attempt ends in ErrProviderBusy, with the
// other writer's last value in place.
func TestModifyAIProviderUpstream_GivesUpWithErrProviderBusy(t *testing.T) {
	x, _ := newDBWithService(t)
	ctx := context.Background()
	directFixture(t, x)

	calls := 0
	err := x.ModifyAIProviderUpstream(ctx, "openrouter", func(p AIProvider) (AIProvider, error) {
		calls++
		if _, err := x.sqlDB.ExecContext(ctx, `UPDATE ai_providers SET credential_slot=? WHERE slug=?`, fmt.Sprintf("SLOT_%d", calls), "openrouter"); err != nil {
			t.Fatal(err)
		}
		p.BaseURL = "https://openrouter.ai/api/v2"
		return p, nil
	})
	if !errors.Is(err, ErrProviderBusy) {
		t.Fatalf("err = %v, want ErrProviderBusy", err)
	}
	if p, _ := x.GetAIProvider(ctx, "openrouter"); calls != 5 || p.BaseURL != "https://openrouter.ai/api/v1" || p.CredentialSlot != "SLOT_5" {
		t.Fatalf("calls=%d row=%+v", calls, p)
	}
}

// The 24 h metrics split each kind into what upstreams reported and the
// tokens that are left to the price table.
func TestAIEndpointMetrics24h_ReportedCost(t *testing.T) {
	x, svc := newDBWithService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	quarter, zero := 0.25, 0.0
	for i, ev := range []UsageEvent{
		{Kind: "openai", TokensIn: 100, TokensOut: 50, CostUSD: &quarter},
		{Kind: "openai", TokensIn: 10, TokensOut: 5},
		{Kind: "openai", TokensIn: 7, TokensOut: 3, CostUSD: &zero},
		{Kind: "anthropic", TokensIn: 4, TokensOut: 2},
	} {
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, cost_usd)
			 VALUES(?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("m-%d", i), svc, "k1", now, ev.Kind, ev.TokensIn, ev.TokensOut, ev.CostUSD); err != nil {
			t.Fatal(err)
		}
	}
	agg, err := x.AIEndpointMetrics24h(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Requests != 4 || agg.TokensIn != 121 || agg.TokensOut != 60 {
		t.Fatalf("totals = %+v", agg)
	}
	byKind := map[string]AIEndpointKindTokens{}
	for _, k := range agg.ByKind {
		byKind[k.Kind] = k
	}
	if k := byKind["openai"]; k.TokensIn != 117 || k.TokensOut != 58 || k.ReportedUSD != 0.25 || k.PricedTokensIn != 10 || k.PricedTokensOut != 5 {
		t.Errorf("openai = %+v, want tokens 117/58, reported 0.25, priced 10/5", k)
	}
	if k := byKind["anthropic"]; k.ReportedUSD != 0 || k.PricedTokensIn != 4 || k.PricedTokensOut != 2 {
		t.Errorf("anthropic = %+v, want reported 0, priced 4/2", k)
	}
}
