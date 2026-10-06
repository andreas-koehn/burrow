package db

import (
	"context"
	"testing"
)

// seedSvc inserts a services row for the given user + service ID via the
// services GetOrCreate path, returning the resolved service id. The test
// data layer does not currently expose a "create with explicit id" helper,
// so we use the (user, name) → id round-trip from GetOrCreateService and
// keep the produced id.
func seedSvc(t *testing.T, x *DB, userID, name string) string {
	t.Helper()
	s, err := x.GetOrCreateService(context.Background(), userID, name, "http")
	if err != nil {
		t.Fatalf("seed service: %v", err)
	}
	return s.ID
}

// insertModelAlias writes a model_aliases row the way an earlier version
// did. Nothing in the product writes the table any more.
func insertModelAlias(t *testing.T, x *DB, m ModelAlias) {
	t.Helper()
	if _, err := x.sqlDB.ExecContext(context.Background(),
		`INSERT INTO model_aliases(alias, concrete_model, service_id, provider, priority) VALUES(?,?,?,?,?)`,
		m.Alias, m.ConcreteModel, m.ServiceID, m.Provider, m.Priority); err != nil {
		t.Fatalf("insert alias %q: %v", m.Alias, err)
	}
}

// checkListModelAliases is shared with the Postgres test: the list is never
// nil, is ordered by alias and carries every column the import reads.
func checkListModelAliases(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	// The user may be there already: a Postgres database outlives the run.
	_ = x.CreateUser(ctx, User{ID: userID, Email: userID + "@test.invalid", PasswordHash: "h", Role: "user"})
	svcA := seedSvc(t, x, userID, "alias-list-a")
	svcB := seedSvc(t, x, userID, "alias-list-b")
	clean := func() {
		_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM model_aliases WHERE service_id IN (?,?)`, svcA, svcB)
	}
	clean() // a database that outlives the run may hold an earlier run's rows
	t.Cleanup(clean)

	mine := func() []ModelAlias {
		t.Helper()
		rows, err := x.ListModelAliases(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if rows == nil {
			t.Fatal("list returned a nil slice")
		}
		out := []ModelAlias{}
		for _, r := range rows {
			if r.ServiceID == svcA || r.ServiceID == svcB {
				out = append(out, r)
			}
		}
		return out
	}
	if got := mine(); len(got) != 0 {
		t.Fatalf("before any insert: %+v", got)
	}

	insertModelAlias(t, x, ModelAlias{Alias: userID + "-smart", ConcreteModel: "gpt-4o", ServiceID: svcB, Provider: "openai", Priority: 100})
	insertModelAlias(t, x, ModelAlias{Alias: userID + "-fast", ConcreteModel: "qwen2.5:0.5b", ServiceID: svcA, Provider: "ollama", Priority: 50})

	got := mine()
	if len(got) != 2 || got[0].Alias != userID+"-fast" || got[1].Alias != userID+"-smart" {
		t.Fatalf("list order: %+v", got)
	}
	fast := got[0]
	if fast.ConcreteModel != "qwen2.5:0.5b" || fast.ServiceID != svcA || fast.Provider != "ollama" || fast.Priority != 50 {
		t.Errorf("columns: %+v", fast)
	}
	if fast.CreatedAt.IsZero() {
		t.Error("created_at is zero: the column default was not applied")
	}
}

func TestListModelAliases(t *testing.T) {
	checkListModelAliases(t, testDB(t), "u-alias")
}

// An alias row goes with its service (and the service with its user).
func TestModelAliasesCascadeOnServiceDelete(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc")
	insertModelAlias(t, x, ModelAlias{Alias: "a", ConcreteModel: "m", ServiceID: svc})
	if err := x.DeleteUser(ctx, "u1"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if rows, err := x.ListModelAliases(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("alias should cascade away with the service; got %+v, %v", rows, err)
	}
}
