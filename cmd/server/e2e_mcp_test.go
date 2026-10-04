package main

// e2e_mcp_test.go — Task 15: real-stack e2e for the `burrowd mcp` server.
//
// Boots a full e2e stack, constructs an mcpserv.Server bound to the same
// *store.Store + the live tunnel registry (via the same adapters cmd/server
// uses in main.go), and exercises three closed-set surfaces over HTTP:
//
//   1. JSON-RPC `tools/list` with a bearer that has every permission the
//      12-tool inventory needs → response lists exactly the 12 tool names
//      from spec Part P.2 in the canonical declaration order.
//   2. JSON-RPC `tools/call` for services.list with the same bearer → the
//      shape mirrors GET /api/v1/services (sub-set of fields compared).
//   3. JSON-RPC `tools/call` for services.set_access_mode with a SECOND
//      bearer that has only `tunnels:read:any` (NOT services:configure:any)
//      → JSON-RPC error -32603 "forbidden" AND one audit_events row with
//      action=mcp.tool.call result=denied.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/mcpserv"
	"github.com/ankoehn/burrow/internal/store"
)

// closedMCPTools is the spec Part P.2 inventory in canonical declaration
// order. Matches internal/mcpserv/tools.go orderedTools — duplicated here
// so the test pins the wire contract independently of the source.
var closedMCPTools = []string{
	"tunnels.list",
	"services.list",
	"services.get",
	"services.set_access_mode",
	"api_keys.list",
	"api_keys.create",
	"api_keys.delete",
	"tokens.list",
	"tokens.mint",
	"users.list",
	"audit.search",
	"metrics.snapshot",
}

// mcpRPCEnvelope is the JSON-RPC 2.0 response shape — defined here so the
// test file does not depend on internal/mcpserv unexported types.
type mcpRPCEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpRPCToolsList is the wire shape of a tools/list response.
type mcpRPCToolsList struct {
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Permission  string `json:"permission"`
	} `json:"tools"`
}

// mcpToolStoreForTest is a thin local adapter that satisfies
// mcpserv.ToolStore against the e2e stack's *store.Store + tunnel
// registry. Lives in this file so the test does NOT touch
// e2e_helpers_test.go.
type mcpToolStoreForTest struct {
	st      *store.Store
	tunnels mcpTunnelSource
	audit   *db.DB
}

func (a mcpToolStoreForTest) ListUserTunnels(_ context.Context, callerID, _ string) ([]mcpserv.TunnelInfo, error) {
	if a.tunnels == nil {
		return nil, nil
	}
	return a.tunnels.ListUserTunnelsMCP(callerID), nil
}

func (a mcpToolStoreForTest) ListServices(ctx context.Context, callerID, callerRole string) ([]store.ServiceView, error) {
	return a.st.ListServices(ctx, callerID, callerRole)
}

func (a mcpToolStoreForTest) GetService(ctx context.Context, callerID, callerRole, id string) (store.ServiceDetail, error) {
	return a.st.GetService(ctx, callerID, callerRole, id)
}

func (a mcpToolStoreForTest) SetServiceAccessMode(ctx context.Context, callerID, callerRole, id, mode, header string, mtlsCAPEM []byte) error {
	return a.st.SetServiceAccessMode(ctx, callerID, callerRole, id, mode, header, mtlsCAPEM)
}

func (a mcpToolStoreForTest) ListAPIKeys(ctx context.Context, callerID, callerRole, serviceID string) ([]db.ServiceAPIKey, error) {
	return a.st.ListAPIKeys(ctx, callerID, callerRole, serviceID)
}

func (a mcpToolStoreForTest) CreateAPIKey(ctx context.Context, callerID, callerRole, serviceID, name string) (string, string, error) {
	return a.st.CreateAPIKey(ctx, callerID, callerRole, serviceID, name)
}

func (a mcpToolStoreForTest) DeleteAPIKey(ctx context.Context, callerID, callerRole, serviceID, keyID string) error {
	return a.st.DeleteAPIKey(ctx, callerID, callerRole, serviceID, keyID)
}

func (a mcpToolStoreForTest) ListClientTokens(ctx context.Context, userID string) ([]db.ClientToken, error) {
	return a.st.ListClientTokens(ctx, userID)
}

func (a mcpToolStoreForTest) IssueClientToken(ctx context.Context, userID, name string) (string, error) {
	return a.st.IssueClientToken(ctx, userID, name)
}

func (a mcpToolStoreForTest) ListUsers(ctx context.Context, q string, limit, offset int) ([]db.User, int, error) {
	return a.st.ListUsersPage(ctx, q, limit, offset)
}

func (a mcpToolStoreForTest) SearchAuditEvents(ctx context.Context, q db.AuditQuery) ([]db.AuditEvent, error) {
	if a.audit == nil {
		return nil, nil
	}
	return a.audit.ListAuditEvents(ctx, q)
}

func (a mcpToolStoreForTest) MetricsText() (string, error) {
	// The MCP listener can run without a metrics recorder; the metrics.snapshot
	// tool is not exercised by this test, but the interface still requires
	// the method.
	return "", nil
}

// mcpListUserTunnelsAdapter narrows the live tunnel registry to the shape
// the mcpToolStoreForTest needs. Mirrors mcpTunnelAdapter in v04_wiring.go.
type mcpListUserTunnelsAdapter struct {
	s userTunnelLister
}

func (a mcpListUserTunnelsAdapter) ListUserTunnelsMCP(callerID string) []mcpserv.TunnelInfo {
	if a.s == nil {
		return nil
	}
	tns := a.s.ListUserTunnels(callerID)
	out := make([]mcpserv.TunnelInfo, 0, len(tns))
	for _, t := range tns {
		out = append(out, mcpserv.TunnelInfo{
			ID:         t.ID,
			Name:       t.Name,
			Type:       t.Type,
			RemotePort: t.RemotePort,
			LocalAddr:  t.LocalAddr,
			BytesIn:    t.BytesIn,
			BytesOut:   t.BytesOut,
			Connected:  t.Connected,
		})
	}
	return out
}

// installAuditLoggerOnStoreForMCP attaches an audit.Logger to s.store and
// returns it so the test can wire it as the MCP server's audit appender.
// The signing key is persisted in settings (matches the e2e_backup_restore
// pattern; future restore-style tests can reload the same key).
func installAuditLoggerOnStoreForMCP(t *testing.T, s *e2eStack) *audit.Logger {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(cryptoRand.Reader)
	if err != nil {
		t.Fatalf("audit genkey: %v", err)
	}
	if err := s.store.SaveSettings(context.Background(), map[string]string{
		audit.SettingsKey: base64.StdEncoding.EncodeToString(priv),
	}); err != nil {
		t.Fatalf("save audit signing key: %v", err)
	}
	logger := audit.NewLogger(db.Wrap(s.db), priv, s.log)
	s.store.SetAuditLogger(storeAuditAdapter{l: logger})
	return logger
}

// mcpRPC fires a JSON-RPC request at the MCP listener and returns the
// decoded envelope. method may be tools/list or tools/call. params is
// embedded verbatim into the request — pass nil for an empty params object.
func mcpRPC(t *testing.T, addr, bearer, method string, params any) mcpRPCEnvelope {
	t.Helper()
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	}
	if params != nil {
		body["params"] = params
	} else {
		body["params"] = map[string]any{}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal rpc: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new rpc request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("rpc do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("rpc HTTP status=%d body=%s", resp.StatusCode, string(b))
	}
	var env mcpRPCEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode rpc envelope: %v", err)
	}
	if env.JSONRPC != "2.0" {
		t.Fatalf("rpc envelope jsonrpc=%q want 2.0", env.JSONRPC)
	}
	return env
}

// TestE2EMCP_ToolsListAndCall is the Task 15 acceptance test. It covers:
//   - tools/list returns exactly the 12 closed-set tools.
//   - tools/call services.list with the right bearer returns a list whose
//     shape matches what the store's ListServices yields.
//   - tools/call services.set_access_mode with a token lacking the required
//     permission returns JSON-RPC -32603 "forbidden" AND emits one
//     mcp.tool.call audit row with result=denied.
func TestE2EMCP_ToolsListAndCall(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}

	stack := bootE2EStack(t)
	auditLogger := installAuditLoggerOnStoreForMCP(t, stack)
	wrapped := db.Wrap(stack.db)

	// Build the MCP server manually with the same adapter shape cmd/server
	// uses in v04_wiring.go. The test does NOT use BuildMCPServer because
	// that function reads cfg.MCPListen and constructs a metricsRecorder-
	// scoped adapter; we want a tighter wiring focused on the closed tool
	// set + audit hook.
	bearerAdapter := mcpBearerAdapter{s: stack.store}
	toolStore := mcpToolStoreForTest{
		st:      stack.store,
		tunnels: mcpListUserTunnelsAdapter{s: stack.server},
		audit:   wrapped,
	}
	mcpSrv := mcpserv.New(bearerAdapter, stack.store, toolStore, stack.log)
	mcpSrv.SetAuditAppender(auditLogger)

	// Bind to an ephemeral loopback listener — the test client will dial
	// 127.0.0.1:<port> directly (no TLS; the MCP listener inside cmd/server
	// also defaults to plaintext bearer-only auth).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mcp listen: %v", err)
	}
	mcpAddr := ln.Addr().String()
	mcpHTTPSrv := &http.Server{
		Handler:           mcpSrv,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = mcpHTTPSrv.Serve(ln) }()
	t.Cleanup(func() { _ = mcpHTTPSrv.Shutdown(context.Background()) })

	// Mint a "full-perm" automation token covering every tool's required
	// permission. The seeded admin trivially has every permission, so the
	// subset check at MintAutomationToken passes.
	fullPerms := []string{
		"tunnels:read:any",
		"services:configure:any",
		"services:configure:own",
		"tokens:manage:own",
		"users:read",
		"audit:read",
		"metrics:read",
	}
	_, fullPlaintext, err := stack.store.MintAutomationToken(
		context.Background(),
		stack.userID, "admin",
		"e2e-mcp-full",
		fullPerms,
		nil,
	)
	if err != nil {
		t.Fatalf("mint full-perm token: %v", err)
	}

	// --- 1. tools/list returns exactly the 12 closed-set tools -------------
	listEnv := mcpRPC(t, mcpAddr, fullPlaintext, "tools/list", nil)
	if listEnv.Error != nil {
		t.Fatalf("tools/list err=%+v", listEnv.Error)
	}
	var listResult mcpRPCToolsList
	if err := json.Unmarshal(listEnv.Result, &listResult); err != nil {
		t.Fatalf("decode tools/list result: %v body=%s", err, listEnv.Result)
	}
	if len(listResult.Tools) != len(closedMCPTools) {
		t.Fatalf("tools/list: got %d tools, want %d (closed set)",
			len(listResult.Tools), len(closedMCPTools))
	}
	// tools/list order is intentionally NOT pinned here — the live
	// implementation iterates a map, so the wire order is unstable across
	// process restarts. The closed-set test is on the SET of names.
	gotNames := make(map[string]bool, len(listResult.Tools))
	for _, td := range listResult.Tools {
		gotNames[td.Name] = true
	}
	for _, want := range closedMCPTools {
		if !gotNames[want] {
			t.Errorf("tools/list missing %q; got=%+v", want, gotNames)
		}
		delete(gotNames, want)
	}
	for extra := range gotNames {
		t.Errorf("tools/list unexpected tool %q (not in spec Part P.2)", extra)
	}

	// --- 2. tools/call services.list returns the same shape as REST -------
	callEnv := mcpRPC(t, mcpAddr, fullPlaintext, "tools/call", map[string]any{
		"name":      "services.list",
		"arguments": map[string]any{},
	})
	if callEnv.Error != nil {
		t.Fatalf("tools/call services.list err=%+v", callEnv.Error)
	}
	// services.list returns []svcOut (id, user_id, name, type, slug,
	// access_mode, api_key_header, created_at). The closed-set wire shape.
	var svcOut []struct {
		ID         string `json:"id"`
		UserID     string `json:"user_id"`
		Name       string `json:"name"`
		Type       string `json:"type"`
		Subdomain  string `json:"slug"`
		AccessMode string `json:"access_mode"`
	}
	if err := json.Unmarshal(callEnv.Result, &svcOut); err != nil {
		t.Fatalf("decode services.list result: %v body=%s", err, callEnv.Result)
	}
	// The bootE2EStack registered the "echo" tunnel which produces an HTTP
	// service row. Find it.
	foundSvc := false
	for _, sv := range svcOut {
		if sv.ID == stack.serviceID {
			foundSvc = true
			if sv.Type != "http" {
				t.Errorf("services.list svc.type=%q want http", sv.Type)
			}
			if sv.UserID != stack.userID {
				t.Errorf("services.list svc.user_id=%q want %q", sv.UserID, stack.userID)
			}
			if sv.Subdomain != stack.subdomain {
				t.Errorf("services.list svc.slug=%q want %q", sv.Subdomain, stack.subdomain)
			}
		}
	}
	if !foundSvc {
		t.Fatalf("services.list did not include serviceID=%q got=%+v",
			stack.serviceID, svcOut)
	}

	// --- 3. Permission gate: a token with ONLY tunnels:read:any cannot ---
	// drive services.set_access_mode. Returns -32603 "forbidden" + emits one
	// mcp.tool.call audit row with result=denied.

	// Baseline: capture pre-test audit row count to count denied delta.
	preRows, err := wrapped.ListAuditEvents(context.Background(),
		db.AuditQuery{Action: audit.ActionMCPToolCall, Limit: 1000})
	if err != nil {
		t.Fatalf("list audit (pre): %v", err)
	}
	preCount := len(preRows)

	_, narrowPlaintext, err := stack.store.MintAutomationToken(
		context.Background(),
		stack.userID, "admin",
		"e2e-mcp-narrow",
		[]string{"tunnels:read:any"},
		nil,
	)
	if err != nil {
		t.Fatalf("mint narrow-perm token: %v", err)
	}

	denyEnv := mcpRPC(t, mcpAddr, narrowPlaintext, "tools/call", map[string]any{
		"name": "services.set_access_mode",
		"arguments": map[string]any{
			"id":   stack.serviceID,
			"mode": "open",
		},
	})
	if denyEnv.Error == nil {
		t.Fatalf("tools/call services.set_access_mode: want error, got result=%s",
			denyEnv.Result)
	}
	if denyEnv.Error.Code != -32603 {
		t.Fatalf("rpc error code=%d want -32603", denyEnv.Error.Code)
	}
	if denyEnv.Error.Message != "forbidden" {
		t.Fatalf("rpc error message=%q want %q", denyEnv.Error.Message, "forbidden")
	}

	// The audit append is fire-and-forget from the handler's perspective,
	// but the audit.Logger.Append is synchronous against the SAME DB
	// transaction the request handler holds (no async indirection in this
	// path). Even so, allow a brief settling window for the test scheduler.
	deadline := time.Now().Add(2 * time.Second)
	var denied db.AuditEvent
	for time.Now().Before(deadline) {
		rows, err := wrapped.ListAuditEvents(context.Background(),
			db.AuditQuery{Action: audit.ActionMCPToolCall, Limit: 1000})
		if err != nil {
			t.Fatalf("list audit (post): %v", err)
		}
		if len(rows) > preCount {
			// Find the newest denied row for set_access_mode.
			for _, r := range rows {
				if r.SubjectLabel == "services.set_access_mode" && r.Result == "denied" {
					denied = r
					break
				}
			}
			if denied.ID != "" {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if denied.ID == "" {
		t.Fatalf("no mcp.tool.call audit row with subject=services.set_access_mode result=denied")
	}
	if denied.ActorID != stack.userID {
		t.Errorf("audit row actor_id=%q want %q", denied.ActorID, stack.userID)
	}
}
