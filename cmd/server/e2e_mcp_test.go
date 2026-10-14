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
//      that has only `tunnels:read:own` → refused like an unknown tool AND
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
	"github.com/ankoehn/burrow/internal/config"
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
// sends with every request, and its User-Agent when one is set.
type mcpBearerTransport struct{ token, userAgent string }

func (b mcpBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	if b.userAgent != "" {
		r.Header.Set("User-Agent", b.userAgent)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// mcpConnect connects the SDK's MCP client to url with an automation token,
// speaking the given protocol revision.
func mcpConnect(t *testing.T, url, bearer, revision string) *mcp.ClientSession {
	t.Helper()
	return mcpConnectAs(t, url, mcpBearerTransport{token: bearer}, revision)
}

// mcpConnectAs is mcpConnect with the transport given.
func mcpConnectAs(t *testing.T, url string, transport mcpBearerTransport, revision string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "burrow-e2e", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             url,
		HTTPClient:           &http.Client{Transport: transport, Timeout: 10 * time.Second},
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
		"tunnels:read:own",
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
		[]string{"tunnels:read:own"},
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

			// --- 3. Permission gate: a token with ONLY tunnels:read:own cannot ---
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

// mcpCall calls a tool and returns its result as text; wantError says
// whether the tool is expected to fail (an isError result, never a protocol
// error).
func mcpCall(t *testing.T, cs *mcp.ClientSession, wantError bool, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("tools/call %s: %d content blocks, want 1", name, len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tools/call %s: content is %T, want text", name, res.Content[0])
	}
	if res.IsError != wantError {
		t.Fatalf("tools/call %s: isError=%v, want %v: %s", name, res.IsError, wantError, text.Text)
	}
	return text.Text
}

// TestE2EMCP_WritesLeaveTheAuditTrailOfTheAPI drives the server cmd/server
// builds (BuildMCPServer), mounted on the API router, against the real store
// and the real audit log. What a tool changes must leave the rows the same
// change leaves through the JSON API: the store's own row and the
// mcp.tool.call row both name the token's user (id and email), the client
// address, the user agent and the request id, and no row holds a secret.
// The same run pins what the tools refuse and bound against the real store.
func TestE2EMCP_WritesLeaveTheAuditTrailOfTheAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	ctx := context.Background()
	st, wrapped := freshTestStack(t)
	stack, err := buildV04Stack(ctx, &config.ServerConfig{}, wrapped.DB(), st, discardLog())
	if err != nil {
		t.Fatalf("buildV04Stack: %v", err)
	}
	t.Cleanup(stack.GuardrailAudit.stop)
	t.Cleanup(stack.Observer.stop)
	t.Cleanup(func() { stack.WebhookDispatcher.Close() })

	admin, err := st.GetUserByEmail(ctx, "admin@x")
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	const serviceID = "svc-audit"
	if err := st.CreateService(ctx, db.Service{
		ID: serviceID, UserID: admin.ID, Name: "echo", Type: "http", Subdomain: "echo",
		AccessMode: "open", APIKeyHeader: "Authorization", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	_, bearer, err := st.MintAutomationToken(ctx, admin.ID, "admin", "audit-trail",
		[]string{"tokens:manage:own", "services:configure:own", "audit:read"}, nil)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	// serve mounts a server built for the given auth domain as main.go does.
	serve := func(authDomain string) string {
		srv := BuildMCPServer(st, stack, nil, wrapped, authDomain, discardLog())
		hs := httptest.NewServer(api.NewRouter(api.Deps{
			Log: discardLog(),
			MCP: api.MCPInfo{Enabled: true, Server: srv, Handler: srv},
		}))
		t.Cleanup(hs.Close)
		return hs.URL + mcpserv.EndpointPath
	}
	mcpURL := serve("burrow.example.com")
	const agent = "burrow-e2e-agent/1"

	allRows := func() []db.AuditEvent {
		rows, err := wrapped.ListAuditEvents(ctx, db.AuditQuery{})
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		return rows
	}
	var secrets []string

	for _, revision := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(revision, func(t *testing.T) {
			cs := mcpConnectAs(t, mcpURL, mcpBearerTransport{token: bearer, userAgent: agent}, revision)
			seen := map[string]bool{}
			for _, r := range allRows() {
				seen[r.ID] = true
			}

			var minted struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal([]byte(mcpCall(t, cs, false, "tokens_mint", map[string]any{"name": "ci-" + revision})), &minted); err != nil || minted.Token == "" {
				t.Fatalf("tokens_mint returned no token: %v", err)
			}
			mcpCall(t, cs, false, "services_set_access_mode", map[string]any{"id": serviceID, "mode": "api_key", "api_key_header": "X-Api-Key"})
			var key struct {
				ID  string `json:"id"`
				Key string `json:"key"`
			}
			if err := json.Unmarshal([]byte(mcpCall(t, cs, false, "api_keys_create", map[string]any{"service_id": serviceID, "name": "prod"})), &key); err != nil || key.Key == "" {
				t.Fatalf("api_keys_create returned no key: %v", err)
			}
			mcpCall(t, cs, false, "api_keys_delete", map[string]any{"service_id": serviceID, "id": key.ID})
			secrets = append(secrets, minted.Token, key.Key)

			// Input the REST route refuses is refused, and nothing is stored.
			if got := mcpCall(t, cs, true, "services_set_access_mode", map[string]any{
				"id": serviceID, "mode": "api_key", "api_key_header": "X Bad: header\r\nInjected",
			}); got != "api_key_header must be a valid HTTP header name (RFC 7230 token)" {
				t.Errorf("bad header name: %q", got)
			}
			if svc, err := st.ServiceByID(ctx, serviceID); err != nil || svc.AccessMode != "api_key" || svc.APIKeyHeader != "X-Api-Key" {
				t.Errorf("after the refused call: mode %q header %q err %v", svc.AccessMode, svc.APIKeyHeader, err)
			}
			// No header is the default one, as on the REST route.
			mcpCall(t, cs, false, "services_set_access_mode", map[string]any{"id": serviceID, "mode": "api_key"})
			if svc, err := st.ServiceByID(ctx, serviceID); err != nil || svc.APIKeyHeader != "Authorization" {
				t.Errorf("api_key without a header: header %q err %v", svc.APIKeyHeader, err)
			}

			want := map[string]int{
				audit.ActionTokenMint:               1,
				audit.ActionServiceAccessModeUpdate: 2,
				audit.ActionServiceAPIKeyCreate:     1,
				audit.ActionServiceAPIKeyRevoke:     1,
				audit.ActionMCPToolCall:             6,
			}
			got := map[string]int{}
			for _, r := range allRows() {
				if seen[r.ID] {
					continue
				}
				got[r.Action]++
				if r.ActorID != admin.ID || r.ActorEmail != "admin@x" {
					t.Errorf("%s %s: actor %q <%s>, want %s <admin@x>", r.Action, r.SubjectLabel, r.ActorID, r.ActorEmail, admin.ID)
				}
				if r.SourceIP != "127.0.0.1" || r.UserAgent != agent || r.RequestID == "" {
					t.Errorf("%s %s: source ip %q, user agent %q, request id %q", r.Action, r.SubjectLabel, r.SourceIP, r.UserAgent, r.RequestID)
				}
			}
			for action, n := range want {
				if got[action] != n {
					t.Errorf("%d new %s rows, want %d (all new rows: %v)", got[action], action, n, got)
				}
			}
			if len(got) != len(want) {
				t.Errorf("new audit rows %v, want only %v", got, want)
			}
		})
	}

	// burrow_login follows the relay's auth domain, as on the REST route.
	with := mcpConnect(t, mcpURL, bearer, "2026-07-28")
	mcpCall(t, with, false, "services_set_access_mode", map[string]any{"id": serviceID, "mode": "open"})
	without := mcpConnect(t, serve(""), bearer, "2026-07-28")
	if got := mcpCall(t, without, true, "services_set_access_mode", map[string]any{"id": serviceID, "mode": "burrow_login"}); got != "burrow_login requires a configured auth_domain" {
		t.Errorf("burrow_login without an auth domain: %q", got)
	}
	if svc, err := st.ServiceByID(ctx, serviceID); err != nil || svc.AccessMode != "open" {
		t.Errorf("after the refused burrow_login: mode %q err %v", svc.AccessMode, err)
	}
	mcpCall(t, with, false, "services_set_access_mode", map[string]any{"id": serviceID, "mode": "burrow_login"})
	if svc, err := st.ServiceByID(ctx, serviceID); err != nil || svc.AccessMode != "burrow_login" {
		t.Errorf("burrow_login with an auth domain: mode %q err %v", svc.AccessMode, err)
	}

	// No audit row holds a secret: not the bearer, not what a tool handed out.
	rows := allRows()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range append(secrets, bearer) {
		if secret == "" || strings.Contains(string(raw), secret) {
			t.Errorf("the audit log holds a secret (or one is empty)")
		}
	}

	// audit_search is bounded whatever limit is asked for. The log is made
	// longer than the default page first.
	for i := len(rows); i <= 120; i++ {
		if err := stack.AuditLogger.Append(ctx, audit.Event{Action: audit.ActionUserUpdate, SubjectLabel: "filler"}); err != nil {
			t.Fatalf("append filler row: %v", err)
		}
	}
	for _, args := range []map[string]any{{}, {"limit": 0}, {"limit": -1}, {"limit": -1000000}} {
		var out struct {
			Events []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal([]byte(mcpCall(t, with, false, "audit_search", args)), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Events) != 100 {
			t.Errorf("audit_search %v: %d events of %d in the log, want the default 100", args, len(out.Events), len(allRows()))
		}
	}
}
