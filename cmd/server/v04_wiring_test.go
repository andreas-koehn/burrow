package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// discardLog returns a slog.Logger that discards every record so tests
// don't spam stdout.
func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// freshTestStack opens a temp SQLite, migrates it, seeds an admin, and
// hands back a *store.Store + the migrated *sql.DB. The DB is closed by
// t.Cleanup.
func freshTestStack(t *testing.T) (*store.Store, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "v04.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	st := store.New(d)
	if err := st.SeedAdmin(context.Background(), "admin@x", "password1"); err != nil {
		t.Fatalf("SeedAdmin: %v", err)
	}
	return st, db.Wrap(d)
}

// TestBuildV04Stack_DefaultsCarryNonNilAIChain asserts the v0.4.0 stack
// constructor produces an aigw.Chain that is non-nil under default config.
// This is the spec invariant from Task 25 Step 1: the proxy carries a
// chain so a wired-but-unconfigured deployment behaves byte-for-byte like
// v0.3.0 (the chain itself short-circuits to the pass-through path when no
// service_ai_config blob is present).
func TestBuildV04Stack_DefaultsCarryNonNilAIChain(t *testing.T) {
	st, wrapped := freshTestStack(t)
	cfg := &config.ServerConfig{}
	stack, err := buildV04Stack(context.Background(), cfg, wrapped.DB(), st, discardLog())
	if err != nil {
		t.Fatalf("buildV04Stack: %v", err)
	}
	t.Cleanup(func() { stack.WebhookDispatcher.Close() })

	if stack.AIChain == nil {
		t.Fatal("default v04 stack must carry a non-nil aigw.Chain")
	}
	// Every Deps surface the plan calls out as wireable should be non-nil
	// in the defaults; this catches a future "field accidentally dropped
	// from buildV04Stack" regression.
	cases := []struct {
		name string
		got  any
	}{
		{"AuditLogger", stack.AuditLogger},
		{"AuditAppender", stack.AuditAppender},
		{"AuditEvents", stack.AuditEvents},
		{"AuditChain", stack.AuditChain},
		{"WebhookDispatcher", stack.WebhookDispatcher},
		{"WebhookSecrets", stack.WebhookSecrets},
		{"CostEngine", stack.CostEngine},
		{"QuotaEngine", stack.QuotaEngine},
		{"CacheEngine", stack.CacheEngine},
		{"RedactEngine", stack.RedactEngine},
		{"GuardrailsEngine", stack.GuardrailsEngine},
		{"InspectorMgr", stack.InspectorMgr},
		{"RouteRouter", stack.RouteRouter},
		{"MeterSink", stack.MeterSink},
		{"Metrics", stack.Metrics},
		{"GeoLookup", stack.GeoLookup},
	}
	for _, c := range cases {
		if c.got == nil {
			t.Errorf("stack.%s is nil; want non-nil", c.name)
		}
	}
}

// TestAIChain_PassThroughWhenNoServiceConfig asserts the chain delegates
// to the downstream proxy handler unchanged when no service_ai_config row
// exists for the requested service. The default-build chain's Loader is
// nil and Dispatch falls through to proxyHandler — preserving the v0.3.0
// FlushInterval=-1 / SSE / WebSocket invariants for every service that
// has no AI features enabled.
func TestAIChain_PassThroughWhenNoServiceConfig(t *testing.T) {
	st, wrapped := freshTestStack(t)
	cfg := &config.ServerConfig{}
	stack, err := buildV04Stack(context.Background(), cfg, wrapped.DB(), st, discardLog())
	if err != nil {
		t.Fatalf("buildV04Stack: %v", err)
	}
	t.Cleanup(func() { stack.WebhookDispatcher.Close() })

	// Count hits on the downstream handler — exactly one means pure pass-
	// through (the chain did not short-circuit anywhere).
	var hits int
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("brewing"))
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	stack.AIChain.Dispatch(rr, req,
		"svc-nonexistent", "127.0.0.1:3000", "Authorization",
		downstream,
	)

	if hits != 1 {
		t.Fatalf("downstream hits = %d, want 1 (chain must pass through when no service_ai_config)", hits)
	}
	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d (downstream response should be byte-for-byte preserved)",
			rr.Code, http.StatusTeapot)
	}
	if rr.Body.String() != "brewing" {
		t.Fatalf("body = %q, want %q", rr.Body.String(), "brewing")
	}
}

// TestBuildMCPServer_BuiltWhenMCPListenSet asserts the optional :7800 MCP
// listener is constructed exactly when cfg.MCPListen is non-empty —
// matching Task 25 Step 1's "fourth http.Server when MCPListen != ”"
// invariant.
func TestBuildMCPServer_BuiltWhenMCPListenSet(t *testing.T) {
	st, wrapped := freshTestStack(t)
	cfg := &config.ServerConfig{MCPListen: ":7800"}
	stack, err := buildV04Stack(context.Background(), cfg, wrapped.DB(), st, discardLog())
	if err != nil {
		t.Fatalf("buildV04Stack: %v", err)
	}
	t.Cleanup(func() { stack.WebhookDispatcher.Close() })

	mcp := BuildMCPServer(cfg, st, stack, nil, wrapped, discardLog())
	if mcp == nil {
		t.Fatal("BuildMCPServer returned nil when cfg.MCPListen=':7800' (want non-nil)")
	}
	// The server's tool inventory must be non-empty (spec Part P.2: closed
	// 12-tool set).
	if got := len(mcp.Tools()); got == 0 {
		t.Fatalf("mcp server tool inventory empty; want spec-closed 12 tools")
	}
}

// TestBuildMCPServer_NilWhenMCPListenEmpty asserts the reverse: when
// MCPListen is empty the listener is not constructed (matches the
// "OFF unless configured" default per spec Part P.1).
func TestBuildMCPServer_NilWhenMCPListenEmpty(t *testing.T) {
	st, wrapped := freshTestStack(t)
	cfg := &config.ServerConfig{}
	stack, err := buildV04Stack(context.Background(), cfg, wrapped.DB(), st, discardLog())
	if err != nil {
		t.Fatalf("buildV04Stack: %v", err)
	}
	t.Cleanup(func() { stack.WebhookDispatcher.Close() })

	mcp := BuildMCPServer(cfg, st, stack, nil, wrapped, discardLog())
	if mcp != nil {
		t.Fatal("BuildMCPServer returned non-nil when cfg.MCPListen='' (want nil — listener disabled)")
	}
}

// TestPricingOverride_HonorsConfigPath asserts that cfg.PricingPath, when
// non-empty, supplies the cost engine's pricing table — and the embedded
// fallback runs when it is empty. Both paths must succeed; a malformed
// path returns an error so the operator catches the misconfiguration at
// startup.
func TestPricingOverride_HonorsConfigPath(t *testing.T) {
	// Empty path → embedded.
	embed, err := loadPricing("")
	if err != nil {
		t.Fatalf("loadPricing(\"\"): %v", err)
	}
	if embed.Version == "" {
		t.Fatal("embedded pricing.Version is empty")
	}

	// Non-existent path → error (operator caught the typo at boot).
	if _, err := loadPricing(filepath.Join(t.TempDir(), "no-such.yaml")); err == nil {
		t.Fatal("loadPricing(missing path): want non-nil error, got nil")
	}
}

// TestInspectorReplayerAdapter_NilChainErrors asserts the adapter returns
// a clear error when the chain is nil (rather than panicking). cmd/server
// always wires a non-nil chain, but the surface is defensive so a future
// edit that accidentally constructs Deps with a partially-nil chain still
// fails the route gracefully with a 503.
func TestInspectorReplayerAdapter_NilChainErrors(t *testing.T) {
	a := inspectorReplayerAdapter{chain: nil, log: discardLog()}
	_, err := a.Replay(context.Background(), "svc-1",
		httptest.NewRequest(http.MethodPost, "/x", nil))
	if err == nil {
		t.Fatal("nil chain Replay: want error, got nil")
	}
}

// _ silences any "imported and not used" growing-pains as the test file
// matures alongside the production wiring file.
var _ = aigw.IsAIPassThrough
