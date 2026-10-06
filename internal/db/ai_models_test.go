package db

import (
	"context"
	"errors"
	"testing"
)

func seedModelProviders(t *testing.T, x *DB) {
	t.Helper()
	ctx := context.Background()
	mustUser(t, x, "u1")
	for _, p := range []struct{ slug, format string }{{"zai", "openai"}, {"openrouter", "openai"}, {"zai-anthropic", "anthropic"}} {
		svc := seedSvc(t, x, "u1", "svc-"+p.slug)
		if err := x.CreateAIProvider(ctx, AIProvider{Slug: p.slug, Name: p.slug, Kind: "tunnel", ServiceID: svc, APIFormat: p.format}); err != nil {
			t.Fatal(err)
		}
	}
}

func countTargets(t *testing.T, x *DB) int {
	t.Helper()
	var n int
	if err := x.DB().QueryRow(`SELECT COUNT(*) FROM ai_model_targets`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAIModels_CRUD(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	seedModelProviders(t, x)
	m := AIModel{
		Name: "burrow-intelligence", Description: "best", Enabled: true,
		AttemptTimeoutS: 60, TotalTimeoutS: 120,
		Targets: []AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "zai", TargetModel: "glm-5.1"},
			{Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
			{Dialect: "anthropic", Position: 0, ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1"},
		},
	}
	if err := x.CreateAIModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := x.GetAIModel(ctx, m.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "best" || !got.Enabled || got.FallbackOnRateLimit || got.AttemptTimeoutS != 60 || got.TotalTimeoutS != 120 || got.CreatedAt.IsZero() {
		t.Fatalf("fields: %+v", got)
	}
	if len(got.Targets) != 3 || got.Targets[0].Dialect != "anthropic" || got.Targets[1].ProviderSlug != "zai" || got.Targets[2].Position != 1 {
		t.Fatalf("targets: %+v", got.Targets)
	}
	if err := x.CreateAIModel(ctx, m); !errors.Is(err, ErrDuplicateModel) {
		t.Fatalf("duplicate err = %v", err)
	}
	if n := countTargets(t, x); n != 3 {
		t.Fatalf("targets after duplicate = %d", n)
	}

	m2 := AIModel{Name: "burrow-smart", Enabled: true, AttemptTimeoutS: 30, TotalTimeoutS: 60,
		Targets: []AIModelTarget{{Dialect: "openai", ProviderSlug: "zai", TargetModel: "glm-5.1"}}}
	if err := x.UpdateAIModel(ctx, "burrow-intelligence", m2); err != nil {
		t.Fatal(err)
	}
	got, err = x.GetAIModel(ctx, "burrow-smart")
	if err != nil || len(got.Targets) != 1 || got.AttemptTimeoutS != 30 || got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatalf("after update: %v %+v", err, got)
	}
	if _, err := x.GetAIModel(ctx, "burrow-intelligence"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old name err = %v", err)
	}
	if err := x.UpdateAIModel(ctx, "missing", m2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing err = %v", err)
	}

	if err := x.CreateAIModel(ctx, AIModel{Name: "aaa-model", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := x.UpdateAIModel(ctx, "aaa-model", AIModel{Name: "burrow-smart"}); !errors.Is(err, ErrDuplicateModel) {
		t.Fatalf("rename onto existing err = %v", err)
	}
	list, err := x.ListAIModels(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "aaa-model" || list[0].Targets == nil || len(list[1].Targets) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}

	names, err := x.ListAIModelNamesByProvider(ctx, "zai")
	if err != nil || len(names) != 1 || names[0] != "burrow-smart" {
		t.Fatalf("by provider: %v %v", err, names)
	}
	names, err = x.ListAIModelNamesByProvider(ctx, "openrouter")
	if err != nil || names == nil || len(names) != 0 {
		t.Fatalf("by provider empty: %v %#v", err, names)
	}

	if err := x.DeleteAIModel(ctx, "burrow-smart"); err != nil {
		t.Fatal(err)
	}
	if err := x.DeleteAIModel(ctx, "burrow-smart"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	if n := countTargets(t, x); n != 0 {
		t.Fatalf("targets after delete = %d", n)
	}
}

func TestAIModels_FollowProviderRename(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	seedModelProviders(t, x)
	if err := x.CreateAIModel(ctx, AIModel{Name: "m1", Enabled: true,
		Targets: []AIModelTarget{{Dialect: "openai", ProviderSlug: "zai", TargetModel: "g"}}}); err != nil {
		t.Fatal(err)
	}
	if err := x.UpdateAIProvider(ctx, "zai", "zhipu", "Zhipu"); err != nil {
		t.Fatal(err)
	}
	got, err := x.GetAIModel(ctx, "m1")
	if err != nil || len(got.Targets) != 1 || got.Targets[0].ProviderSlug != "zhipu" {
		t.Fatalf("after rename: %v %+v", err, got)
	}
}

func TestAIModels_ListEmptyIsNonNil(t *testing.T) {
	x := testDB(t)
	list, err := x.ListAIModels(context.Background())
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("list: %v %#v", err, list)
	}
}
