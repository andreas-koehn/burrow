//go:build postgres

package db

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestMCPSources_Postgres runs the tool-source statements against a live
// Postgres: both creates, the optimistic modify, the tool counts, the service
// delete guard and the deletes of both kinds.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestMCPSources_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres tool source test")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	ctx := context.Background()
	// The database outlives the test run: tolerate what an earlier run left.
	_ = x.CreateUser(ctx, User{ID: "u-mcp-pg", Email: "u-mcp-pg@test.invalid", PasswordHash: "h", Role: "user"})
	_ = x.DeleteMCPSourceAndBacking(ctx, "pg-remote")
	_ = x.DeleteMCPSourceAndBacking(ctx, "pg-tunnel")

	svc := Service{ID: "mcp-pg-remote", UserID: "u-mcp-pg", Name: "mcp:pg-remote", AccessMode: "api_key"}
	src := MCPSource{Slug: "pg-remote", Name: "PG", URL: "https://x.example/mcp", CredentialSlot: "S", TimeoutS: 60, Enabled: true}
	if err := x.CreateRemoteMCPSource(ctx, svc, src); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.DeleteMCPSourceAndBacking(ctx, "pg-remote") })
	got, err := x.GetMCPSource(ctx, "pg-remote")
	if err != nil || got.Kind != "remote" || got.ServiceID != svc.ID || !got.Enabled || got.LastSyncAt != nil || got.AuthHeader != "Authorization" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if err := x.CreateRemoteMCPSource(ctx, Service{ID: "mcp-pg-dup", UserID: "u-mcp-pg", Name: "mcp:pg-remote2"}, src); !errors.Is(err, ErrDuplicateMCPSource) {
		t.Fatalf("duplicate slug err = %v", err)
	}

	if _, err := x.ModifyMCPSource(ctx, "pg-remote", func(s MCPSource) (MCPSource, error) {
		s.URL, s.RPM, s.Enabled = "https://y.example/mcp", 12, false
		return s, nil
	}); err != nil {
		t.Fatal(err)
	}
	if after, _ := x.GetMCPSource(ctx, "pg-remote"); after.URL != "https://y.example/mcp" || after.RPM != 12 || after.Enabled {
		t.Fatalf("after modify: %+v", after)
	}
	if _, err := x.ModifyMCPSource(ctx, "ghost", func(s MCPSource) (MCPSource, error) { return s, nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("modify unknown err = %v", err)
	}

	if _, err := b.DB().ExecContext(ctx,
		`INSERT INTO mcp_tools(source_slug, name, status) VALUES('pg-remote','a','approved'),('pg-remote','b','pending'),('pg-remote','c','removed')`); err != nil {
		t.Fatal(err)
	}
	if c, _ := x.GetMCPSource(ctx, "pg-remote"); c.ToolCount != 2 || c.PendingCount != 1 {
		t.Fatalf("tool counts: %+v", c)
	}
	// Another credential slot sends the approved tool back to pending, in
	// the update's own transaction; another rpm does not.
	if n, err := x.ModifyMCPSource(ctx, "pg-remote", func(s MCPSource) (MCPSource, error) { s.RPM = 13; return s, nil }); err != nil || n != 0 {
		t.Fatalf("rpm change: %d %v", n, err)
	}
	if n, err := x.ModifyMCPSource(ctx, "pg-remote", func(s MCPSource) (MCPSource, error) { s.CredentialSlot = "PG_OTHER"; return s, nil }); err != nil || n != 1 {
		t.Fatalf("slot change: %d %v", n, err)
	}
	if c, _ := x.GetMCPSource(ctx, "pg-remote"); c.PendingCount != 2 {
		t.Fatalf("tool counts after a slot change: %+v", c)
	}

	// Tunnel: binds an http service and closes its doors; the service stays
	// while the source uses it.
	_ = x.DeleteService(ctx, "mcp-pg-tunnel")
	if err := x.CreateService(ctx, Service{ID: "mcp-pg-tunnel", UserID: "u-mcp-pg", Name: "mcp-pg-tunnel", Type: "http", Subdomain: "mcppgtunnel", AccessMode: "open"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.DeleteService(ctx, "mcp-pg-tunnel") })
	if err := x.CreateTunnelMCPSource(ctx, MCPSource{Slug: "pg-tunnel", Name: "T", ServiceID: "mcp-pg-tunnel", TimeoutS: 60, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if s, _ := x.GetServiceByID(ctx, "mcp-pg-tunnel"); !s.GatewayOnly {
		t.Fatal("the tunnel service is not gateway-only")
	}
	if err := x.DeleteService(ctx, "mcp-pg-tunnel"); !errors.Is(err, ErrServiceHasMCPSource) {
		t.Fatalf("delete bound service err = %v", err)
	}
	if err := x.DeleteService(ctx, svc.ID); !errors.Is(err, ErrServiceHasMCPSource) {
		t.Fatalf("delete backing row err = %v", err)
	}

	if err := x.DeleteMCPSourceAndBacking(ctx, "pg-remote"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetServiceByID(ctx, svc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backing row after delete: %v", err)
	}
	if err := x.DeleteMCPSourceAndBacking(ctx, "pg-tunnel"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetServiceByID(ctx, "mcp-pg-tunnel"); err != nil {
		t.Fatalf("tunnel service after source delete: %v", err)
	}
	if err := x.DeleteMCPSourceAndBacking(ctx, "pg-tunnel"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}
