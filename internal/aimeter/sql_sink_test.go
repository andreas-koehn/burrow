package aimeter

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

type countingBudgets struct{ calls int }

func (c *countingBudgets) CheckBudgetsForSample(context.Context, string, string, string, string) {
	c.calls++
}

// A tool call's row is stored, but it is no model traffic: the budgets are
// not checked for it (D12).
func TestSQLSink_ToolCallsSkipTheBudgetCheck(t *testing.T) {
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	budgets := &countingBudgets{}
	sink := &SQLSink{DB: db.Wrap(sqldb), Budgets: budgets}
	ctx := context.Background()
	_ = sink.Record(ctx, Sample{Kind: KindOpenAI, GatewayKeyID: "gk", RequestedModel: "m"})
	_ = sink.Record(ctx, Sample{Kind: KindMCP, GatewayKeyID: "gk", ProviderSlug: "gh", RequestedModel: "echo"})
	var n int
	if err := sqldb.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d (%v), want 2", n, err)
	}
	if budgets.calls != 1 {
		t.Fatalf("budget checks = %d, want 1 (the model request only)", budgets.calls)
	}
}
