//go:build postgres

package db

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestDirectAIProvider_Postgres runs the direct-provider statements against a
// live Postgres: create with its backing row, upstream update, the model
// catalog, the name guard of GetOrCreateService and the cascading delete.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestDirectAIProvider_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres direct provider test")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	ctx := context.Background()
	// The database outlives the test run: tolerate what an earlier run left.
	_ = x.CreateUser(ctx, User{ID: "u-direct-pg", Email: "u-direct-pg@test.invalid", PasswordHash: "h", Role: "user"})
	_ = x.DeleteAIProviderAndBacking(ctx, "pg-direct")

	svc := Service{ID: "prov-pg-direct", UserID: "u-direct-pg", Name: "PG Direct", Type: "http", AccessMode: "api_key"}
	p := AIProvider{Slug: "pg-direct", Name: "PG Direct", Kind: "tunnel", APIFormat: "openai", BaseURL: "https://x.example/v1", CredentialSlot: "S"}
	if err := x.CreateDirectAIProvider(ctx, svc, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.DeleteAIProviderAndBacking(ctx, "pg-direct") })
	got, err := x.GetAIProvider(ctx, "pg-direct")
	if err != nil || got.Kind != "direct" || got.ServiceID != svc.ID {
		t.Fatalf("get: %v %+v", err, got)
	}

	got.BaseURL, got.Billing, got.ExtraHeaders = "https://y.example/v2", "flat", map[string]string{"X-Title": "Burrow"}
	if err := x.UpdateAIProviderUpstream(ctx, got); err != nil {
		t.Fatal(err)
	}
	if after, _ := x.GetAIProvider(ctx, "pg-direct"); after.BaseURL != "https://y.example/v2" || after.Billing != "flat" || after.ExtraHeaders["X-Title"] != "Burrow" {
		t.Fatalf("after update: %+v", after)
	}

	if _, err := x.GetOrCreateService(ctx, "u-direct-pg", "PG Direct", "http"); !errors.Is(err, ErrServiceNameReserved) {
		t.Fatalf("name guard err = %v", err)
	}

	models := []AIProviderModel{{ModelID: "b", DisplayName: "B", ContextLength: 8192}, {ModelID: "a"}}
	for i := 0; i < 2; i++ {
		if err := x.ReplaceAIProviderModels(ctx, "pg-direct", models); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.UpsertAIProviderModel(ctx, AIProviderModel{ProviderSlug: "pg-direct", ModelID: "a", DisplayName: "A"}); err != nil {
		t.Fatal(err)
	}
	list, err := x.ListAIProviderModels(ctx, "pg-direct")
	if err != nil || len(list) != 2 || list[0].ModelID != "a" || list[0].DisplayName != "A" || list[1].ContextLength != 8192 {
		t.Fatalf("models: %v %+v", err, list)
	}
	if err := x.CreateServiceAPIKey(ctx, ServiceAPIKey{ID: "k-direct-pg", ServiceID: svc.ID, Name: "ci", KeyHash: "h-direct-pg"}); err != nil {
		t.Fatal(err)
	}

	if err := x.DeleteAIProviderAndBacking(ctx, "pg-direct"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetServiceByID(ctx, svc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backing service survived: %v", err)
	}
	if keys, _ := x.ListServiceAPIKeys(ctx, svc.ID); len(keys) != 0 {
		t.Fatalf("keys survived: %+v", keys)
	}
	if list, _ := x.ListAIProviderModels(ctx, "pg-direct"); len(list) != 0 {
		t.Fatalf("models survived: %+v", list)
	}
	if err := x.DeleteAIProviderAndBacking(ctx, "pg-direct"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}
