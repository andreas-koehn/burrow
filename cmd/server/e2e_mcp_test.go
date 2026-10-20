package main

// e2e_mcp_test.go — real-stack e2e for Burrow's management MCP server.
//
// Builds the MCP server the way main.go does (BuildMCPServer with the
// embedded OpenAPI document), mounts it on an API router wired with the real
// store, hands it that router (SetAPIHandler) and drives /mcp/burrow with the
// SDK's own MCP client, once per protocol revision (2026-07-28 without a
// handshake, 2025-11-25 with one). A tool call is a request to that router
// with the caller's bearer, so what is asserted here is what the REST API
// lets the same token do:
//
//   1. tools/list follows the token's permissions (presets read-only,
//      operator, full).
//   2. services_list answers what GET /api/v1/services answers.
//   3. service_create creates the service; a delete without confirm and a
//      dry run change nothing; a path value stays one segment.
//   4. A call leaves the API's own audit rows next to the mcp.tool.call row,
//      both naming the token's user, the client address, the user agent and
//      the request id; no row holds a secret.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/mcpserv"
	"github.com/ankoehn/burrow/internal/store"
)

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

// mcpE2E is a relay for the MCP tests: the real store, the v0.4 stack, an
// API router wired with what the curated tools reach, and the MCP server
// mounted on it and replaying through it.
type mcpE2E struct {
	st      *store.Store
	wrapped *db.DB
	stack   *v04Stack
	admin   db.User
}

func newMCPE2E(t *testing.T) *mcpE2E {
	t.Helper()
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
	return &mcpE2E{st: st, wrapped: wrapped, stack: stack, admin: admin}
}

// serve mounts a server built as main.go builds it on a router for the given
// auth domain and returns the router's base URL.
func (e *mcpE2E) serve(t *testing.T, authDomain string) string {
	t.Helper()
	srv, err := BuildMCPServer(e.st, e.stack, api.OpenAPIDocument(), discardLog())
	if err != nil {
		t.Fatalf("BuildMCPServer: %v", err)
	}
	router := api.NewRouter(api.Deps{
		Log:           discardLog(),
		Users:         e.st,
		Sessions:      e.st,
		Roles:         e.st,
		Services:      e.st,
		AccessModes:   e.st,
		AIProviders:   e.st,
		Budgets:       e.wrapped,
		Bearer:        api.NewStoreBearerStore(e.st),
		Automation:    e.st,
		AuditAppender: e.stack.AuditAppender,
		AuditEvents:   e.stack.AuditEvents,
		AuthDomain:    authDomain,
		MCP:           api.MCPInfo{Enabled: true, Server: srv, Handler: srv},
	})
	srv.SetAPIHandler(router)
	hs := httptest.NewServer(router)
	t.Cleanup(hs.Close)
	return hs.URL
}

// mint returns the plaintext of an automation token of the admin.
func (e *mcpE2E) mint(t *testing.T, name string, perms []string) string {
	t.Helper()
	_, plain, err := e.st.MintAutomationToken(context.Background(), e.admin.ID, "admin", name, perms, nil)
	if err != nil {
		t.Fatalf("mint %s: %v", name, err)
	}
	return plain
}

func (e *mcpE2E) rows(t *testing.T) []db.AuditEvent {
	t.Helper()
	rows, err := e.wrapped.ListAuditEvents(context.Background(), db.AuditQuery{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	return rows
}

// rest sends one request to the JSON API with a bearer and decodes the JSON
// answer into out.
func rest(t *testing.T, method, url, bearer string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, b)
		}
	}
	return resp.StatusCode
}

func presetPermissions(t *testing.T, name string) []string {
	t.Helper()
	for _, p := range authz.Presets() {
		if p.Name == name {
			out := make([]string, len(p.Permissions))
			for i, perm := range p.Permissions {
				out[i] = string(perm)
			}
			return out
		}
	}
	t.Fatalf("no preset %s", name)
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestE2EMCP_CuratedToolsThroughTheAPI is the acceptance test of the curated
// tools against the real router, store and gate.
func TestE2EMCP_CuratedToolsThroughTheAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	ctx := context.Background()
	e := newMCPE2E(t)
	if err := e.st.CreateService(ctx, db.Service{
		ID: "svc-echo", UserID: e.admin.ID, Name: "echo", Type: "http", Subdomain: "echo",
		AccessMode: "open", APIKeyHeader: "Authorization", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	base := e.serve(t, "")
	mcpURL := base + mcpserv.EndpointPath

	readOnly := e.mint(t, "e2e-read-only", presetPermissions(t, authz.PresetReadOnly))
	operator := e.mint(t, "e2e-operator", presetPermissions(t, authz.PresetOperator))
	full := e.mint(t, "e2e-full", presetPermissions(t, authz.PresetFull))

	for _, revision := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(revision, func(t *testing.T) {
			// --- 1. tools/list follows the token --------------------------------
			ro := mcpConnect(t, mcpURL, readOnly, revision)
			names := mcpToolNames(t, ro)
			if !contains(names, "providers_list") || !contains(names, "me_get") ||
				contains(names, "services_list") || contains(names, "budget_create") {
				t.Fatalf("read-only token lists %v", names)
			}
			// A tool it does not see is refused like one that does not exist.
			_, errHidden := ro.CallTool(ctx, &mcp.CallToolParams{Name: "budget_create", Arguments: map[string]any{}})
			_, errUnknown := ro.CallTool(ctx, &mcp.CallToolParams{Name: "no_such_tool", Arguments: map[string]any{}})
			var hidden, unknown *jsonrpc.Error
			if !errors.As(errHidden, &hidden) || !errors.As(errUnknown, &unknown) || hidden.Code != unknown.Code {
				t.Fatalf("hidden tool: %v; unknown tool: %v", errHidden, errUnknown)
			}
			// me_get answers who the token acts for, through the API.
			var me struct {
				ID          string   `json:"id"`
				Permissions []string `json:"permissions"`
			}
			if err := json.Unmarshal([]byte(mcpCall(t, ro, false, "me_get", map[string]any{})), &me); err != nil || me.ID != e.admin.ID {
				t.Fatalf("me_get: %+v %v", me, err)
			}
			if contains(me.Permissions, "services:configure:any") {
				t.Errorf("me_get of the read-only token lists services:configure:any")
			}

			// --- 2. services_list answers what GET /api/v1/services answers ------
			op := mcpConnect(t, mcpURL, operator, revision)
			var viaMCP struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			}
			if err := json.Unmarshal([]byte(mcpCall(t, op, false, "services_list", map[string]any{})), &viaMCP); err != nil {
				t.Fatal(err)
			}
			var viaREST []struct {
				ID string `json:"id"`
			}
			if code := rest(t, http.MethodGet, base+"/api/v1/services", operator, &viaREST); code != http.StatusOK {
				t.Fatalf("GET /api/v1/services: %d", code)
			}
			if len(viaMCP.Items) == 0 || len(viaMCP.Items) != len(viaREST) {
				t.Fatalf("services_list %+v, REST %+v", viaMCP.Items, viaREST)
			}
			for i := range viaREST {
				if viaMCP.Items[i].ID != viaREST[i].ID {
					t.Errorf("services_list item %d = %s, REST %s", i, viaMCP.Items[i].ID, viaREST[i].ID)
				}
			}

			// --- 3. writes ------------------------------------------------------
			fu := mcpConnect(t, mcpURL, full, revision)
			id := "mcp-made-" + strings.ReplaceAll(revision, "-", "")
			listed := func() bool {
				var svcs []struct {
					ID string `json:"id"`
				}
				rest(t, http.MethodGet, base+"/api/v1/services", full, &svcs)
				for _, s := range svcs {
					if s.ID == id {
						return true
					}
				}
				return false
			}
			before := map[string]bool{}
			for _, r := range e.rows(t) {
				before[r.ID] = true
			}
			// A dry run sends nothing: no service, no audit row of the API.
			dry := mcpCall(t, fu, false, "service_create", map[string]any{"dry_run": true, "body": map[string]any{"service_id": id, "slug": id, "title": id}})
			if !strings.Contains(dry, `"dry_run":true`) || listed() {
				t.Fatalf("dry run: %s, service exists: %v", dry, listed())
			}
			// A delete without confirm is refused before anything is sent.
			if got := mcpCall(t, fu, true, "provider_delete", map[string]any{"slug": "zai"}); !strings.Contains(got, `confirm must be "zai"`) {
				t.Errorf("provider_delete without confirm: %s", got)
			}
			var added []string
			for _, r := range e.rows(t) {
				if !before[r.ID] {
					added = append(added, r.Action+"/"+r.SubjectLabel+"="+r.Result)
				}
			}
			sort.Strings(added)
			if strings.Join(added, ",") != "mcp.tool.call/provider_delete=error,mcp.tool.call/service_create=dry_run" {
				t.Errorf("rows of the dry run and the refused delete: %v", added)
			}
			// The real call creates it.
			var created struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(mcpCall(t, fu, false, "service_create", map[string]any{"body": map[string]any{"service_id": id, "slug": id, "title": id}})), &created); err != nil || created.ID != id {
				t.Fatalf("service_create: %+v %v", created, err)
			}
			if !listed() {
				t.Fatalf("GET /api/v1/services does not list %s", id)
			}
			// The handler refuses what REST refuses: a taken id.
			if got := mcpCall(t, fu, true, "service_create", map[string]any{"body": map[string]any{"service_id": id, "slug": id, "title": id}}); !strings.HasPrefix(got, "409 ") {
				t.Errorf("service_create of a taken id: %s", got)
			}
			// A path value stays one segment: ../users reaches the service
			// route, not the users route.
			if got := mcpCall(t, fu, true, "service_get", map[string]any{"serviceID": "../users"}); !strings.HasPrefix(got, "404 ") {
				t.Errorf("service_get ../users: %s", got)
			}
		})
	}
}

// TestE2EMCP_WritesLeaveTheAuditTrailOfTheAPI: what a tool changes leaves
// the rows the same change leaves through the JSON API, and the mcp.tool.call
// row next to them. Both name the token's user (id and email), the client
// address, the user agent and the request id; no row holds a secret. What the
// REST route refuses, the tool refuses with the route's status and message.
func TestE2EMCP_WritesLeaveTheAuditTrailOfTheAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	ctx := context.Background()
	e := newMCPE2E(t)
	const serviceID = "svc-audit"
	if err := e.st.CreateService(ctx, db.Service{
		ID: serviceID, UserID: e.admin.ID, Name: "echo", Type: "http", Subdomain: "echo",
		AccessMode: "open", APIKeyHeader: "Authorization", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	tokenRow, bearer, err := e.st.MintAutomationToken(ctx, e.admin.ID, "admin", "audit-trail", []string{"services:configure:own", "audit:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mcpURL := e.serve(t, "burrow.example.com") + mcpserv.EndpointPath
	const agent = "burrow-e2e-agent/1"
	setMode := func(cs *mcp.ClientSession, wantError bool, body map[string]any) string {
		t.Helper()
		return mcpCall(t, cs, wantError, "service_access_mode_update", map[string]any{"serviceID": serviceID, "body": body})
	}

	for _, revision := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(revision, func(t *testing.T) {
			cs := mcpConnectAs(t, mcpURL, mcpBearerTransport{token: bearer, userAgent: agent}, revision)
			seen := map[string]bool{}
			for _, r := range e.rows(t) {
				seen[r.ID] = true
			}

			setMode(cs, false, map[string]any{"access_mode": "api_key", "api_key_header": "X-Api-Key"})
			// Input the REST route refuses is refused, and nothing is stored.
			if got := setMode(cs, true, map[string]any{"access_mode": "api_key", "api_key_header": "X Bad: header\r\nInjected"}); got != "400 Bad Request: api_key_header must be a valid HTTP header name (RFC 7230 token)" {
				t.Errorf("bad header name: %q", got)
			}
			if svc, err := e.st.ServiceByID(ctx, serviceID); err != nil || svc.AccessMode != "api_key" || svc.APIKeyHeader != "X-Api-Key" {
				t.Errorf("after the refused call: mode %q header %q err %v", svc.AccessMode, svc.APIKeyHeader, err)
			}
			// No header is the default one, as on the REST route.
			setMode(cs, false, map[string]any{"access_mode": "api_key"})
			if svc, err := e.st.ServiceByID(ctx, serviceID); err != nil || svc.APIKeyHeader != "Authorization" {
				t.Errorf("api_key without a header: header %q err %v", svc.APIKeyHeader, err)
			}

			want := map[string]int{
				audit.ActionServiceAccessModeUpdate: 2,
				audit.ActionMCPToolCall:             3,
			}
			got := map[string]int{}
			requestIDs := map[string]map[string]bool{} // action -> request ids
			for _, r := range e.rows(t) {
				if seen[r.ID] {
					continue
				}
				got[r.Action]++
				if requestIDs[r.Action] == nil {
					requestIDs[r.Action] = map[string]bool{}
				}
				requestIDs[r.Action][r.RequestID] = true
				if r.ActorID != e.admin.ID || r.ActorEmail != "admin@x" {
					t.Errorf("%s %s: actor %q <%s>, want %s <admin@x>", r.Action, r.SubjectLabel, r.ActorID, r.ActorEmail, e.admin.ID)
				}
				if r.SourceIP != "127.0.0.1" || r.UserAgent != agent || r.RequestID == "" {
					t.Errorf("%s %s: source ip %q, user agent %q, request id %q", r.Action, r.SubjectLabel, r.SourceIP, r.UserAgent, r.RequestID)
				}
				// Round 2: the API's rows name the token, as the MCP row does.
				var p struct {
					TokenID string `json:"token_id"`
				}
				if err := json.Unmarshal([]byte(r.Payload), &p); err != nil || p.TokenID != tokenRow.ID {
					t.Errorf("%s %s: payload %s, want token_id %q", r.Action, r.SubjectLabel, r.Payload, tokenRow.ID)
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
			// The API's row of a change carries the request id of its MCP row.
			for id := range requestIDs[audit.ActionServiceAccessModeUpdate] {
				if !requestIDs[audit.ActionMCPToolCall][id] {
					t.Errorf("the API row's request id %q is on no mcp.tool.call row", id)
				}
			}
		})
	}

	// burrow_login follows the relay's auth domain, as on the REST route.
	with := mcpConnect(t, mcpURL, bearer, "2026-07-28")
	setMode(with, false, map[string]any{"access_mode": "open"})
	without := mcpConnect(t, e.serve(t, "")+mcpserv.EndpointPath, bearer, "2026-07-28")
	if got := setMode(without, true, map[string]any{"access_mode": "burrow_login"}); got != "409 Conflict: burrow_login requires a configured auth_domain" {
		t.Errorf("burrow_login without an auth domain: %q", got)
	}
	if svc, err := e.st.ServiceByID(ctx, serviceID); err != nil || svc.AccessMode != "open" {
		t.Errorf("after the refused burrow_login: mode %q err %v", svc.AccessMode, err)
	}
	setMode(with, false, map[string]any{"access_mode": "burrow_login"})
	if svc, err := e.st.ServiceByID(ctx, serviceID); err != nil || svc.AccessMode != "burrow_login" {
		t.Errorf("burrow_login with an auth domain: mode %q err %v", svc.AccessMode, err)
	}

	// No audit row holds the bearer.
	raw, err := json.Marshal(e.rows(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), bearer) {
		t.Errorf("the audit log holds the bearer")
	}
}

// TestE2EMCP_GenericCallsGoThroughTheGate: what the generic tools reach is
// what the REST API lets the same token do.
func TestE2EMCP_GenericCallsGoThroughTheGate(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	ctx := context.Background()
	e := newMCPE2E(t)
	base := e.serve(t, "")
	mcpURL := base + mcpserv.EndpointPath

	budgetIDs := func() []string {
		var list []struct {
			ID string `json:"id"`
		}
		if code := rest(t, http.MethodGet, base+"/api/v1/budgets", e.mint(t, "e2e-budget-reader", presetPermissions(t, authz.PresetFull)), &list); code != http.StatusOK {
			t.Fatalf("GET /api/v1/budgets: %d", code)
		}
		ids := make([]string, len(list))
		for i, b := range list {
			ids[i] = b.ID
		}
		return ids
	}
	find := func(cs *mcp.ClientSession, query string) []string {
		t.Helper()
		var out struct {
			Operations []struct {
				OperationID string `json:"operation_id"`
			} `json:"operations"`
		}
		if err := json.Unmarshal([]byte(mcpCall(t, cs, false, "burrow_find_operations", map[string]any{"query": query, "limit": 100})), &out); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(out.Operations))
		for i, op := range out.Operations {
			ids[i] = op.OperationID
		}
		return ids
	}
	unknown := func(cs *mcp.ClientSession, id string, args map[string]any) {
		t.Helper()
		got := mcpCall(t, cs, true, "burrow_call_operation", map[string]any{"operation_id": id, "arguments": args})
		if !strings.HasPrefix(got, "unknown operation "+id) {
			t.Errorf("%s: %q", id, got)
		}
	}
	budgetBody := map[string]any{"scope": "global", "daily_usd": 5, "action_on_exceed": "throttle_zero"}

	for _, revision := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(revision, func(t *testing.T) {
			// --- read-only: sees audit, not budgets; dashboard-only is unknown
			ro := mcpConnect(t, mcpURL, e.mint(t, "e2e-generic-ro", presetPermissions(t, authz.PresetReadOnly)), revision)
			if !contains(find(ro, "audit"), "auditEventsList") {
				t.Errorf("read-only does not find auditEventsList")
			}
			if ids := find(ro, "budget"); contains(ids, "budgetsList") || contains(ids, "budgetCreate") {
				t.Errorf("read-only finds budget operations: %v", ids)
			}
			unknown(ro, "budgetCreate", map[string]any{"body": budgetBody})
			unknown(ro, "gatewayKeyCreate", map[string]any{})

			// --- a user whose token declares what the role does not grant
			u, err := e.st.CreateUser(ctx, "plain-"+revision+"@x", "a-long-enough-password-1", "user")
			if err != nil {
				t.Fatalf("create user: %v", err)
			}
			// Minted while the user was an admin, who has the permission; the
			// role is "user" now, as after a demotion.
			_, plain, err := e.st.MintAutomationToken(ctx, u.ID, "admin", "e2e-generic-user", []string{"ai:providers:manage"}, nil)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			us := mcpConnect(t, mcpURL, plain, revision)
			unknown(us, "providerDelete", map[string]any{"slug": "sneaky", "confirm": "sneaky"})
			var providers []struct {
				Slug string `json:"slug"`
			}
			if code := rest(t, http.MethodGet, base+"/api/v1/ai/providers", e.mint(t, "e2e-provider-reader", presetPermissions(t, authz.PresetFull)), &providers); code != http.StatusOK || len(providers) != 0 {
				t.Errorf("providers after the refused create: %d %+v", code, providers)
			}

			// --- operator: dry run changes nothing, the real call creates it
			op := mcpConnect(t, mcpURL, e.mint(t, "e2e-generic-op", presetPermissions(t, authz.PresetOperator)), revision)
			before := budgetIDs()
			seen := map[string]bool{}
			for _, r := range e.rows(t) {
				seen[r.ID] = true
			}
			dry := mcpCall(t, op, false, "burrow_call_operation", map[string]any{"operation_id": "budgetCreate", "arguments": map[string]any{"dry_run": true, "body": budgetBody}})
			if !strings.Contains(dry, `"dry_run":true`) || len(budgetIDs()) != len(before) {
				t.Fatalf("dry run: %s, budgets %d -> %d", dry, len(before), len(budgetIDs()))
			}
			mcpCall(t, op, false, "burrow_call_operation", map[string]any{"operation_id": "budgetCreate", "arguments": map[string]any{"body": budgetBody}})
			if len(budgetIDs()) != len(before)+1 {
				t.Fatalf("budgets after the real call: %d, before %d", len(budgetIDs()), len(before))
			}
			got := map[string]bool{}
			for _, r := range e.rows(t) {
				if !seen[r.ID] {
					got[r.Action+"/"+r.SubjectLabel] = true
				}
			}
			if !got["mcp.tool.call/burrow_call_operation/budgetCreate"] {
				t.Errorf("no MCP row for the generic call: %v", got)
			}
			// The REST route writes no audit row for a budget today
			// (budget.create is declared in internal/audit and never
			// emitted), so the MCP row is the only one of the call.
		})
	}
}
