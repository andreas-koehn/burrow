//go:build postgres

package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestMCPTools_Postgres runs the tool statements against a live Postgres:
// the sync upsert with its status rule, approval with and without a hash,
// approve-all and the optimistic override update.
func TestMCPTools_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres tool test")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	ctx := context.Background()
	_ = x.CreateUser(ctx, User{ID: "u-mcpt-pg", Email: "u-mcpt-pg@test.invalid", PasswordHash: "h", Role: "user"})
	_ = x.DeleteMCPSourceAndBacking(ctx, "pg-tools")
	if err := x.CreateRemoteMCPSource(ctx, Service{ID: "mcpt-pg", UserID: "u-mcpt-pg", Name: "mcp:pg-tools", AccessMode: "api_key"},
		MCPSource{Slug: "pg-tools", Name: "T", URL: "https://x.example/mcp", TimeoutS: 60, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.DeleteMCPSourceAndBacking(ctx, "pg-tools") })
	now := time.Now().UTC()

	c, err := x.ApplyMCPToolSync(ctx, "pg-tools", []MCPToolSeen{{Name: "a", Hash: "h1", Definition: `{"name":"a"}`, GuardrailHits: "[]"}, {Name: "b", Hash: "h2", Definition: `{"name":"b"}`, GuardrailHits: "[]"}}, now, "")
	if err != nil || c.Added != 2 {
		t.Fatalf("first sync: %+v %v", c, err)
	}
	if _, err := x.ApproveMCPTool(ctx, "pg-tools", "a", "wrong", now); !errors.Is(err, ErrMCPToolChanged) {
		t.Fatalf("stale approve: %v", err)
	}
	if a, err := x.ApproveMCPTool(ctx, "pg-tools", "a", "h1", now); err != nil || a.Status != MCPToolApproved || a.ApprovedDefinition != `{"name":"a"}` {
		t.Fatalf("approve: %+v %v", a, err)
	}
	if _, err := x.ApproveMCPTool(ctx, "pg-tools", "a", "", now); !errors.Is(err, ErrMCPToolNotPending) {
		t.Fatalf("approve twice: %v", err)
	}
	c, err = x.ApplyMCPToolSync(ctx, "pg-tools", []MCPToolSeen{{Name: "a", Hash: "h1b", Definition: `{"name":"a","description":"x"}`, GuardrailHits: "[]"}}, now, "warn")
	if err != nil || c.Changed != 1 || c.Removed != 1 {
		t.Fatalf("second sync: %+v %v", c, err)
	}
	a, _ := x.GetMCPTool(ctx, "pg-tools", "a")
	if a.Status != MCPToolPending || a.ApprovedHash != "h1" {
		t.Fatalf("changed tool: %+v", a)
	}
	if s, _ := x.GetMCPSource(ctx, "pg-tools"); s.LastSyncError != "warn" || s.PendingCount != 1 {
		t.Fatalf("source after sync: %+v", s)
	}
	if names, err := x.ApproveAllMCPTools(ctx, "pg-tools", nil, now); err != nil || len(names) != 1 {
		t.Fatalf("approve-all: %v %v", names, err)
	}
	ro := true
	got, err := x.ModifyMCPToolOverrides(ctx, "pg-tools", "a", now, func(MCPTool) (MCPToolOverrides, error) {
		return MCPToolOverrides{Hidden: true, Rename: "aa", ReadOnly: &ro}, nil
	})
	if err != nil || !got.Hidden || got.Rename != "aa" || got.ReadOnly == nil || !*got.ReadOnly {
		t.Fatalf("overrides: %+v %v", got, err)
	}
	got, err = x.ModifyMCPToolOverrides(ctx, "pg-tools", "a", now, func(MCPTool) (MCPToolOverrides, error) { return MCPToolOverrides{}, nil })
	if err != nil || got.ReadOnly != nil || got.Hidden {
		t.Fatalf("cleared overrides: %+v %v", got, err)
	}
	if err := x.SetMCPSourceSyncError(ctx, "pg-tools", now, "down"); err != nil {
		t.Fatal(err)
	}
}
