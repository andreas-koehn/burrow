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
