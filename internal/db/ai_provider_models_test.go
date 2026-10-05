package db

import (
	"context"
	"errors"
	"testing"
)

func TestAIProviderModels(t *testing.T) {
	x, svcID := newDBWithService(t)
	ctx := context.Background()
	_ = x.CreateAIProvider(ctx, AIProvider{Slug: "ollama", Name: "Ollama", Kind: "tunnel", ServiceID: svcID, APIFormat: "openai"})

	first := []AIProviderModel{
		{ProviderSlug: "ollama", ModelID: "mistral", DisplayName: "Mistral", ContextLength: 32768},
		{ProviderSlug: "ollama", ModelID: "google/gemini-x", DisplayName: "Gemini"},
	}
	if err := x.ReplaceAIProviderModels(ctx, "ollama", first); err != nil {
		t.Fatal(err)
	}
	got, err := x.ListAIProviderModels(ctx, "ollama")
	if err != nil || len(got) != 2 || got[0].ModelID != "google/gemini-x" || got[1].ContextLength != 32768 {
		t.Fatalf("list: %v %+v", err, got)
	}

	// Replace drops models that are gone.
	if err := x.ReplaceAIProviderModels(ctx, "ollama", first[:1]); err != nil {
		t.Fatal(err)
	}
	got, _ = x.ListAIProviderModels(ctx, "ollama")
	if len(got) != 1 || got[0].ModelID != "mistral" {
		t.Fatalf("after replace: %+v", got)
	}

	if err := x.UpsertAIProviderModel(ctx, AIProviderModel{ProviderSlug: "ollama", ModelID: "mistral", DisplayName: "Mistral 7B"}); err != nil {
		t.Fatal(err)
	}
	got, _ = x.ListAIProviderModels(ctx, "ollama")
	if len(got) != 1 || got[0].DisplayName != "Mistral 7B" {
		t.Fatalf("after upsert: %+v", got)
	}

	// Models follow a provider rename and disappear with the provider.
	if err := x.UpdateAIProvider(ctx, "ollama", "local", "Local"); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.ListAIProviderModels(ctx, "local"); len(got) != 1 {
		t.Fatalf("models did not follow the rename: %+v", got)
	}
	if err := x.DeleteAIProviderModel(ctx, "local", "mistral"); err != nil {
		t.Fatal(err)
	}
	if err := x.DeleteAIProviderModel(ctx, "local", "mistral"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	// Deleting the provider removes its catalog.
	if err := x.UpsertAIProviderModel(ctx, AIProviderModel{ProviderSlug: "local", ModelID: "mistral"}); err != nil {
		t.Fatal(err)
	}
	if err := x.DeleteAIProvider(ctx, "local"); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.ListAIProviderModels(ctx, "local"); len(got) != 0 {
		t.Fatalf("models survived their provider: %+v", got)
	}
	if got, _ := x.ListAIProviderModels(ctx, "nope"); got == nil || len(got) != 0 {
		t.Fatalf("unknown provider list = %#v, want empty non-nil", got)
	}
}
