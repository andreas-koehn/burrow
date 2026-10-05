package db

import (
	"context"
	"errors"
	"testing"
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
