package db

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// seedModelProviders creates userID with the providers "zai", "openrouter"
// (openai) and "zai-anthropic". The model tables are emptied before and
// after, and the user goes with its services and providers, so the checks
// below also run against a Postgres that outlives the test run.
func seedModelProviders(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	reset := func() {
		_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM ai_models`)
		_ = x.DeleteUser(ctx, userID)
	}
	reset()
	t.Cleanup(reset)
	mustUser(t, x, userID)
	for _, p := range []struct{ slug, format string }{{"zai", "openai"}, {"openrouter", "openai"}, {"zai-anthropic", "anthropic"}} {
		svc := seedSvc(t, x, userID, "svc-"+p.slug)
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

func TestAIModels_CRUD(t *testing.T) { checkAIModels(t, testDB(t), "u1") }

// checkAIModels exercises the ai_models and ai_model_targets statements. It
// runs against SQLite here and against a live Postgres in the postgres-tagged
// test.
func checkAIModels(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	seedModelProviders(t, x, userID)
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
	checkAIModelsFollowProviderRename(t, testDB(t), "u1")
}

// checkAIModelsFollowProviderRename: a provider's new slug reaches the target
// rows (ON UPDATE CASCADE), and a provider with a target cannot be deleted.
func checkAIModelsFollowProviderRename(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	seedModelProviders(t, x, userID)
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
	// No ON DELETE on the target's provider: the provider, its service and the
	// service's owner cannot go while the model is there, and the foreign-key
	// failure comes back as ErrProviderInUse with the model's name.
	p, err := x.GetAIProvider(ctx, "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	for what, del := range map[string]func() error{
		"provider": func() error { return x.DeleteAIProviderAndBacking(ctx, "zhipu") },
		"service":  func() error { return x.DeleteService(ctx, p.ServiceID) },
		"user":     func() error { return x.DeleteUser(ctx, userID) },
	} {
		err := del()
		if !errors.Is(err, ErrProviderInUse) || !strings.Contains(err.Error(), ": m1") || strings.Contains(err.Error(), "FOREIGN KEY") {
			t.Errorf("delete %s err = %v, want ErrProviderInUse naming m1", what, err)
		}
	}
	if _, err := x.GetAIProvider(ctx, "zhipu"); err != nil {
		t.Fatalf("provider after the refused deletes: %v", err)
	}
	if _, err := x.GetUserByID(ctx, userID); err != nil {
		t.Fatalf("user after the refused delete: %v", err)
	}
	// Other failures keep their own error.
	if err := x.DeleteService(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing service err = %v", err)
	}
	if err := x.DeleteAIModel(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if err := x.DeleteService(ctx, p.ServiceID); err != nil {
		t.Fatalf("delete service without models: %v", err)
	}
}

func TestAIModels_DeleteKeepsOtherErrors(t *testing.T) {
	checkDeleteKeepsOtherErrors(t, testDB(t), "u1", func(name, table, when string) string {
		return `CREATE TRIGGER ` + name + ` BEFORE DELETE ON ` + table + ` WHEN ` + when +
			` BEGIN SELECT RAISE(ABORT, 'refused by test'); END`
	}, func(name, table string) string { return `DROP TRIGGER IF EXISTS ` + name })
}

// checkDeleteKeepsOtherErrors: only a violation of the target's foreign key
// is reported as ErrProviderInUse. Here a model targets the provider, so the
// name query finds it, but each delete is stopped earlier by a trigger: that
// error must come back as it is. createTrigger and dropTrigger give the
// engine's statements; when is a condition on the OLD row.
func checkDeleteKeepsOtherErrors(t *testing.T, x *DB, userID string,
	createTrigger func(name, table, when string) string, dropTrigger func(name, table string) string) {
	t.Helper()
	ctx := context.Background()
	seedModelProviders(t, x, userID)
	if err := x.CreateAIModel(ctx, AIModel{Name: "m1", Enabled: true,
		Targets: []AIModelTarget{{Dialect: "openai", ProviderSlug: "zai", TargetModel: "g"}}}); err != nil {
		t.Fatal(err)
	}
	p, err := x.GetAIProvider(ctx, "zai")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what, table, when string
		del               func() error
	}{
		{"provider", "ai_providers", `OLD.slug = 'zai'`, func() error { return x.DeleteAIProviderAndBacking(ctx, "zai") }},
		{"service", "services", `OLD.id = '` + p.ServiceID + `'`, func() error { return x.DeleteService(ctx, p.ServiceID) }},
		{"user", "users", `OLD.id = '` + userID + `'`, func() error { return x.DeleteUser(ctx, userID) }},
	} {
		name := "burrow_test_refuse_" + c.table
		drop := func() { _, _ = x.DB().ExecContext(ctx, dropTrigger(name, c.table)) }
		drop()
		if _, err := x.DB().ExecContext(ctx, createTrigger(name, c.table, c.when)); err != nil {
			t.Fatalf("%s: create trigger: %v", c.what, err)
		}
		t.Cleanup(drop)
		err := c.del()
		drop()
		if err == nil || errors.Is(err, ErrProviderInUse) || !strings.Contains(err.Error(), "refused by test") {
			t.Errorf("delete %s err = %v, want the trigger's own error", c.what, err)
		}
		// Without the trigger the same delete hits the foreign key.
		if err := c.del(); !errors.Is(err, ErrProviderInUse) {
			t.Errorf("delete %s without the trigger err = %v, want ErrProviderInUse", c.what, err)
		}
	}
}

func TestIsForeignKeyViolation_PlainErrors(t *testing.T) {
	for _, err := range []error{nil, context.Canceled, ErrNotFound, errors.New("FOREIGN KEY constraint failed")} {
		if isTargetProviderFKViolation(err) {
			t.Errorf("isTargetProviderFKViolation(%v) = true", err)
		}
	}
}

func TestAIModels_ListEmptyIsNonNil(t *testing.T) {
	x := testDB(t)
	list, err := x.ListAIModels(context.Background())
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("list: %v %#v", err, list)
	}
}
