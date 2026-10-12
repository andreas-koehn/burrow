package main

// e2e_mcp_test.go — real-stack e2e for Burrow's management MCP server.
//
// Boots a full e2e stack, constructs an mcpserv.Server bound to the same
// *store.Store + the live tunnel registry (via the same adapters cmd/server
// uses in main.go), mounts it on the API router the way main.go does, and
// drives it at /mcp/burrow on that address with the SDK's own MCP client,
// once per protocol revision (2026-07-28 without a handshake, 2025-11-25
// with one):
//
//   1. `tools/list` with a bearer that has every permission the 12-tool
//      inventory needs → exactly the 12 tool names of spec Part P.2; with a
//      bearer that has one permission → that one tool.
//   2. `tools/call` for services_list with the full bearer → the shape
//      mirrors GET /api/v1/services (sub-set of fields compared).
//   3. `tools/call` for services_set_access_mode with the SECOND bearer
//      that has only `tunnels:read:any` → refused like an unknown tool AND
//      one audit_events row with action=mcp.tool.call result=denied.

import (
	"context"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/mcpserv"
	"github.com/ankoehn/burrow/internal/store"
)

// closedMCPTools is the spec Part P.2 inventory in canonical declaration
// order. Matches internal/mcpserv/tools.go orderedTools — duplicated here
// so the test pins the wire contract independently of the source.
var closedMCPTools = []string{
	"tunnels_list",
	"services_list",
	"services_get",
	"services_set_access_mode",
	"api_keys_list",
	"api_keys_create",
	"api_keys_delete",
	"tokens_list",
	"tokens_mint",
	"users_list",
	"audit_search",
	"metrics_snapshot",
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
	// The MCP server can run without a metrics recorder; the metrics_snapshot
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

// mcpBearerTransport sends the Authorization header a configured MCP client
// sends with every request.
type mcpBearerTransport struct{ token string }

func (b mcpBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// mcpConnect connects the SDK's MCP client to url with an automation token,
// speaking the given protocol revision.
func mcpConnect(t *testing.T, url, bearer, revision string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "burrow-e2e", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             url,
		HTTPClient:           &http.Client{Transport: mcpBearerTransport{token: bearer}, Timeout: 10 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, &mcp.ClientSessionOptions{ProtocolVersion: revision})
	if err != nil {
		t.Fatalf("mcp connect (revision %s): %v", revision, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// mcpToolNames returns what tools/list shows the session, sorted.
func mcpToolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	return names
}

// TestE2EMCP_ToolsListAndCall is the acceptance test of the management MCP
// server against the real store. For both protocol revisions it covers:
//   - tools/list with a token that holds every tool permission returns
//     exactly the 12 closed-set tools; a narrow token sees only its own.
//   - tools/call services_list returns the service the stack registered,
//     as structuredContent and as text.
//   - tools/call services_set_access_mode with the narrow token is refused
//     like an unknown tool AND emits one mcp.tool.call audit row with
//     result=denied.
func TestE2EMCP_ToolsListAndCall(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}

	stack := bootE2EStack(t)
	auditLogger := installAuditLoggerOnStoreForMCP(t, stack)
	wrapped := db.Wrap(stack.db)

	// Build the MCP server manually with the same adapter shape cmd/server
	// uses in v04_wiring.go. The test does NOT use BuildMCPServer because
	// that constructs a metricsRecorder-scoped adapter; we want a tighter
	// wiring focused on the closed tool set + audit hook.
	bearerAdapter := mcpBearerAdapter{s: stack.store}
	toolStore := mcpToolStoreForTest{
		st:      stack.store,
		tunnels: mcpListUserTunnelsAdapter{s: stack.server},
		audit:   wrapped,
	}
	mcpSrv := mcpserv.New(bearerAdapter, stack.store, toolStore, stack.log)
	mcpSrv.SetAuditAppender(auditLogger)

	// The main address: the API router with the MCP server mounted, as
	// main.go wires it. There is no listener of its own any more.
	hs := httptest.NewServer(api.NewRouter(api.Deps{
		Log: stack.log,
		MCP: api.MCPInfo{Enabled: true, Server: mcpSrv, Handler: mcpSrv},
	}))
	t.Cleanup(hs.Close)
	mcpURL := hs.URL + mcpserv.EndpointPath

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

	for _, revision := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(revision, func(t *testing.T) {
			full := mcpConnect(t, mcpURL, fullPlaintext, revision)
			narrow := mcpConnect(t, mcpURL, narrowPlaintext, revision)

			// --- 1. tools/list: the closed set, and only what a token may call ---
			want := append([]string(nil), closedMCPTools...)
			sort.Strings(want)
			if got := mcpToolNames(t, full); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("tools/list (full token):\n got %v\nwant %v", got, want)
			}
			if got := mcpToolNames(t, narrow); len(got) != 1 || got[0] != "tunnels_list" {
				t.Fatalf("tools/list (narrow token) = %v, want [tunnels_list]", got)
			}

			// --- 2. tools/call services_list returns the registered service ------
			res, err := full.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "services_list", Arguments: map[string]any{},
			})
			if err != nil {
				t.Fatalf("tools/call services_list: %v", err)
			}
			if res.IsError || len(res.Content) != 1 {
				t.Fatalf("services_list result: %+v", res)
			}
			text, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("services_list content is %T, want text", res.Content[0])
			}
			// services_list returns {services: [...]} (id, user_id, name, type,
			// slug, access_mode, api_key_header, created_at).
			var svcList struct {
				Services []struct {
					ID         string `json:"id"`
					UserID     string `json:"user_id"`
					Name       string `json:"name"`
					Type       string `json:"type"`
					Subdomain  string `json:"slug"`
					AccessMode string `json:"access_mode"`
				} `json:"services"`
			}
			if err := json.Unmarshal([]byte(text.Text), &svcList); err != nil {
				t.Fatalf("decode services_list text: %v body=%s", err, text.Text)
			}
			structured, err := json.Marshal(res.StructuredContent)
			if err != nil || !strings.Contains(string(structured), stack.serviceID) {
				t.Errorf("services_list structuredContent lacks the service: %s", structured)
			}
			// The bootE2EStack registered the "echo" tunnel which produces an
			// HTTP service row. Find it.
			foundSvc := false
			for _, sv := range svcList.Services {
				if sv.ID == stack.serviceID {
					foundSvc = true
					if sv.Type != "http" {
						t.Errorf("services_list svc.type=%q want http", sv.Type)
					}
					if sv.UserID != stack.userID {
						t.Errorf("services_list svc.user_id=%q want %q", sv.UserID, stack.userID)
					}
					if sv.Subdomain != stack.subdomain {
						t.Errorf("services_list svc.slug=%q want %q", sv.Subdomain, stack.subdomain)
					}
				}
			}
			if !foundSvc {
				t.Fatalf("services_list did not include serviceID=%q got=%+v",
					stack.serviceID, svcList.Services)
			}

			// --- 3. Permission gate: a token with ONLY tunnels:read:any cannot ---
			// drive services_set_access_mode. The call is refused like a call on
			// a tool that does not exist, and one mcp.tool.call audit row with
			// result=denied is written.
			deniedRows := func() int {
				rows, err := wrapped.ListAuditEvents(context.Background(),
					db.AuditQuery{Action: audit.ActionMCPToolCall, Limit: 1000})
				if err != nil {
					t.Fatalf("list audit: %v", err)
				}
				n := 0
				for _, r := range rows {
					if r.SubjectLabel == "services_set_access_mode" && r.Result == "denied" && r.ActorID == stack.userID {
						n++
					}
				}
				return n
			}
			before := deniedRows()

			_, err = narrow.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "services_set_access_mode",
				Arguments: map[string]any{"id": stack.serviceID, "mode": "open"},
			})
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("tools/call services_set_access_mode with the narrow token: err=%v, want JSON-RPC %d",
					err, jsonrpc.CodeInvalidParams)
			}
			_, errUnknown := narrow.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "no_such_tool", Arguments: map[string]any{},
			})
			var rpcUnknown *jsonrpc.Error
			if !errors.As(errUnknown, &rpcUnknown) || rpcUnknown.Code != rpcErr.Code {
				t.Fatalf("unknown tool: err=%v, want the same code %d", errUnknown, rpcErr.Code)
			}

			// The audit append is synchronous in this path; allow a brief
			// settling window for the test scheduler all the same.
			deadline := time.Now().Add(2 * time.Second)
			for deniedRows() != before+1 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if got := deniedRows(); got != before+1 {
				t.Fatalf("denied audit rows for services_set_access_mode: %d, want %d", got, before+1)
			}
		})
	}
}
