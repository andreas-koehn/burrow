package db

import (
	"context"
	"errors"
	"testing"
)

// seedSvc inserts a services row for the given user + service ID via the
// services GetOrCreate path, returning the resolved service id. The test
// data layer does not currently expose a "create with explicit id" helper,
// so we use the (user, name) → id round-trip from GetOrCreateService and
// keep the produced id for the model_aliases rows.
func seedSvc(t *testing.T, x *DB, userID, name string) string {
	t.Helper()
	s, err := x.GetOrCreateService(context.Background(), userID, name, "http")
	if err != nil {
		t.Fatalf("seed service: %v", err)
	}
	return s.ID
}

func TestModelAliasesCRUD(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svcA := seedSvc(t, x, "u1", "svc-a")
	svcB := seedSvc(t, x, "u1", "svc-b")

	// Empty list returns a non-nil empty slice.
	rows, err := x.ListModelAliases(ctx)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("want empty non-nil slice, got %v", rows)
	}

	// Create + read-back.
	if err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: "fast", ConcreteModel: "gpt-4o-mini", ServiceID: svcA,
	}); err != nil {
		t.Fatalf("create fast: %v", err)
	}
	if err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: "smart", ConcreteModel: "gpt-4o", ServiceID: svcB,
	}); err != nil {
		t.Fatalf("create smart: %v", err)
	}
	got, err := x.GetModelAlias(ctx, "fast")
	if err != nil {
		t.Fatalf("get fast: %v", err)
	}
	if got.ConcreteModel != "gpt-4o-mini" || got.ServiceID != svcA {
		t.Fatalf("got %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("created_at zero (sqlite default not applied?)")
	}

	// List returns both, ordered by alias.
	rows, err = x.ListModelAliases(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 || rows[0].Alias != "fast" || rows[1].Alias != "smart" {
		t.Fatalf("list order: %+v", rows)
	}

	// Update: replaces concrete + service.
	if err := x.UpdateModelAlias(ctx, "fast", "gpt-3.5", svcB); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = x.GetModelAlias(ctx, "fast")
	if got.ConcreteModel != "gpt-3.5" || got.ServiceID != svcB {
		t.Fatalf("update did not persist: %+v", got)
	}

	// Delete removes; subsequent get → ErrNotFound.
	if err := x.DeleteModelAlias(ctx, "fast"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := x.GetModelAlias(ctx, "fast"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}

	// Delete unknown alias → ErrNotFound.
	if err := x.DeleteModelAlias(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown: %v, want ErrNotFound", err)
	}
	// Update unknown alias → ErrNotFound.
	if err := x.UpdateModelAlias(ctx, "nope", "x", svcA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: %v, want ErrNotFound", err)
	}
}

func TestModelAliasesCreateConflict(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc")

	if err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: "dup", ConcreteModel: "gpt-4o", ServiceID: svc,
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: "dup", ConcreteModel: "other", ServiceID: svc,
	})
	if !errors.Is(err, ErrAliasExists) {
		t.Fatalf("second create: %v, want ErrAliasExists", err)
	}
}

func TestModelAliasesCascadeOnServiceDelete(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc")
	if err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: "a", ConcreteModel: "m", ServiceID: svc,
	}); err != nil {
		t.Fatal(err)
	}
	// Cascade delete via services.user_id → users(id) ON DELETE CASCADE.
	if err := x.DeleteUser(ctx, "u1"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := x.GetModelAlias(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("alias should cascade away with service; got %v", err)
	}
}

// TestModelAliasProviderPriority verifies that provider and priority are
// stored and retrieved correctly (v0.5.0 columns).
func TestModelAliasProviderPriority(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc")

	if err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: "fast", ConcreteModel: "llama3.1:8b", ServiceID: svc,
		Provider: "ollama", Priority: 50,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := x.GetModelAlias(ctx, "fast")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Provider != "ollama" {
		t.Errorf("provider: got %q, want %q", got.Provider, "ollama")
	}
	if got.Priority != 50 {
		t.Errorf("priority: got %d, want 50", got.Priority)
	}

	// List also exposes provider + priority.
	list, err := x.ListModelAliases(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Provider != "ollama" || list[0].Priority != 50 {
		t.Fatalf("list: %+v", list)
	}
}

// TestGetAliasesByPriority verifies that GetAliasesByPriority returns rows
// for the given alias ordered by priority ASC (v0.5.0).
func TestGetAliasesByPriority(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svcA := seedSvc(t, x, "u1", "svc-a")
	svcB := seedSvc(t, x, "u1", "svc-b")
	svcC := seedSvc(t, x, "u1", "svc-c")

	// Insert in non-priority order to prove the ORDER BY is effective.
	if err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: "fast", ConcreteModel: "claude-3-5", ServiceID: svcC,
		Provider: "anthropic", Priority: 100,
	}); err != nil {
		t.Fatalf("create anthropic: %v", err)
	}
	if err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: "fast2", ConcreteModel: "gpt-4o-mini", ServiceID: svcB,
		Provider: "openai", Priority: 50,
	}); err != nil {
		t.Fatalf("create openai: %v", err)
	}
	if err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: "fast3", ConcreteModel: "llama3.1:8b", ServiceID: svcA,
		Provider: "ollama", Priority: 0,
	}); err != nil {
		t.Fatalf("create ollama: %v", err)
	}

	// GetAliasesByPriority for "fast" should return only the one matching alias.
	rows, err := x.GetAliasesByPriority(ctx, "fast")
	if err != nil {
		t.Fatalf("get by priority: %v", err)
	}
	if len(rows) != 1 || rows[0].Provider != "anthropic" {
		t.Fatalf("expected 1 anthropic row; got %+v", rows)
	}

	// Empty alias → empty result (not an error).
	rows2, err := x.GetAliasesByPriority(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("get nonexistent: %v", err)
	}
	if len(rows2) != 0 {
		t.Fatalf("want 0 rows for nonexistent alias; got %d", len(rows2))
	}
}

// TestUpdateModelAliasFull verifies UpdateModelAliasFull persists provider
// and priority (v0.5.0).
func TestUpdateModelAliasFull(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svcA := seedSvc(t, x, "u1", "svc-a")
	svcB := seedSvc(t, x, "u1", "svc-b")

	if err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: "m", ConcreteModel: "old", ServiceID: svcA,
		Provider: "ollama", Priority: 100,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := x.UpdateModelAliasFull(ctx, "m", "new-model", svcB, "openai", 10); err != nil {
		t.Fatalf("update full: %v", err)
	}
	got, _ := x.GetModelAlias(ctx, "m")
	if got.ConcreteModel != "new-model" || got.ServiceID != svcB || got.Provider != "openai" || got.Priority != 10 {
		t.Fatalf("update did not persist: %+v", got)
	}

	// Unknown alias → ErrNotFound.
	if err := x.UpdateModelAliasFull(ctx, "nope", "x", svcA, "ollama", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: %v, want ErrNotFound", err)
	}
}

// checkAliasLookup exercises GetAliasesByPriority the way the /ai/ gateway
// calls it. It runs against SQLite here and against Postgres in
// aliases_postgres_test.go, so the query has to be valid on both.
func checkAliasLookup(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(context.Background(), userID) })
	svc := seedSvc(t, x, userID, "alias-lookup")
	alias := "alias-lookup-" + userID
	if err := x.CreateModelAlias(ctx, ModelAlias{
		Alias: alias, ConcreteModel: "qwen2.5:0.5b", ServiceID: svc, Provider: "ollama", Priority: 0,
	}); err != nil {
		t.Fatalf("create alias: %v", err)
	}
	t.Cleanup(func() { _ = x.DeleteModelAlias(context.Background(), alias) })

	rows, err := x.GetAliasesByPriority(ctx, alias)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(rows) != 1 || rows[0].ConcreteModel != "qwen2.5:0.5b" || rows[0].ServiceID != svc {
		t.Fatalf("rows = %+v", rows)
	}
	rows, err = x.GetAliasesByPriority(ctx, alias+"-missing")
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("missing alias: rows %v err %v", rows, err)
	}
}

func TestGetAliasesByPriority_Lookup(t *testing.T) {
	checkAliasLookup(t, testDB(t), "u-alias")
}
