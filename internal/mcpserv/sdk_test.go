package mcpserv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
)

// The revisions a client may speak: the stateless one and the three with an
// initialize handshake.
var testRevisions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

// allCuratedTools are the tool names of curatedTools, sorted: what a token
// of the full preset of an admin sees.
const allCuratedTools = "ai_model_delete,ai_models_list,ai_requests_list," +
	"audit_events_list,budget_create,budgets_list,connection_logs_list,cost_summary_get," +
	"gateway_key_delete,gateway_keys_list,inspector_request_get,known_clients_list,me_get," +
	"provider_delete,provider_get,provider_metrics_get,provider_models_sync," +
	"providers_list,service_get,service_slug_update,services_list"

// ---------- fakes ----------------------------------------------------------

// auditSink satisfies AuditAppender and keeps what it was given.
type auditSink struct {
	mu   sync.Mutex
	rows []audit.Event
}

func (a *auditSink) Append(_ context.Context, e audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, e)
	return nil
}

func (a *auditSink) all() []audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]audit.Event(nil), a.rows...)
}

// ---------- bootstrap ------------------------------------------------------

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestServer builds a Server from the relay's OpenAPI document.
func newTestServer(t *testing.T, bearer BearerStore, users UserLookup, log *slog.Logger, opts ...Option) *Server {
	t.Helper()
	spec, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(bearer, users, spec, log, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// serveFor serves s.mcpHandler() for one fixed token, without the HTTP
// checks in front of it.
func serveFor(t *testing.T, s *Server, tok *authedToken) *httptest.Server {
	t.Helper()
	h := s.mcpHandler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	}))
	t.Cleanup(hs.Close)
	return hs
}

// newSDKTestServer serves s.mcpHandler() for one fixed token with perms and
// role, replaying calls through api.
func newSDKTestServer(t *testing.T, api http.Handler, perms []string, role string) (*httptest.Server, *auditSink) {
	t.Helper()
	s := newTestServer(t, nil, nil, quietLog())
	s.SetAPIHandler(api)
	sink := &auditSink{}
	s.SetAuditAppender(sink)
	return serveFor(t, s, &authedToken{tokenID: "tok1", userID: "u1", role: role, perms: perms, bearer: "Bearer bua_test"}), sink
}

// bearerTransport sends the Authorization header a configured MCP client
// sends with every request.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// dialMCP connects the SDK's client, speaking the given revision.
func dialMCP(url, bearer, revision string) (*mcp.ClientSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "burrow-test", Version: "0"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             url,
		HTTPClient:           &http.Client{Transport: bearerTransport{token: bearer}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, &mcp.ClientSessionOptions{ProtocolVersion: revision})
}

func connectMCP(t *testing.T, url, bearer, revision string) *mcp.ClientSession {
	t.Helper()
	cs, err := dialMCP(url, bearer, revision)
	if err != nil {
		t.Fatalf("connect with revision %s: %v", revision, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func listedTools(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tl := range res.Tools {
		out[tl.Name] = tl
	}
	return out
}

// sortedNames lists the curated tools of m; the generic tools, which every
// token has, are left out (generic_test.go covers them).
func sortedNames(m map[string]*mcp.Tool) string {
	names := make([]string, 0, len(m))
	for n := range m {
		if strings.HasPrefix(n, "burrow_") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	return cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
}

// resultText returns the single text block of a tool result.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("result has %d content blocks, want 1", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content block is %T, want text", res.Content[0])
	}
	return tc.Text
}

// rawPOST sends one JSON-RPC message the way a client on a 2025 revision
// does after its handshake.
func rawPOST(t *testing.T, url, bearer string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// sameJSON reports whether a and b are the same JSON value, whatever the
// order of their keys.
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatalf("not JSON: %s", a)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return reflect.DeepEqual(x, y)
}

const rawToolsList = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

// ---------- every revision -------------------------------------------------

func TestMCP_EveryRevision_ListAndCall(t *testing.T) {
	for _, rev := range testRevisions {
		t.Run(rev, func(t *testing.T) {
			api := &fakeAPI{reply: `[{"id":"svc-a","name":"alpha"},{"id":"svc-b","name":"beta"}]`}
			hs, _ := newSDKTestServer(t, api, presetPerms(authz.PresetFull), "admin")
			cs := connectMCP(t, hs.URL, "", rev)

			if got := cs.InitializeResult().ProtocolVersion; got != rev {
				t.Fatalf("negotiated revision %q, want %q", got, rev)
			}
			if id := cs.ID(); id != "" {
				t.Errorf("a stateless server handed out session id %q", id)
			}
			if info := cs.InitializeResult().ServerInfo; info == nil || info.Name != "burrow" {
				t.Errorf("server info = %+v", info)
			}

			tools := listedTools(t, cs)
			if got := sortedNames(tools); got != allCuratedTools {
				t.Fatalf("tools:\n got %s\nwant %s", got, allCuratedTools)
			}
			schema, _ := json.Marshal(tools["service_get"].InputSchema)
			if !strings.Contains(string(schema), `"serviceID"`) || !strings.Contains(string(schema), `"additionalProperties":false`) {
				t.Errorf("service_get inputSchema = %s", schema)
			}

			res, err := callTool(t, cs, "services_list", nil)
			if err != nil {
				t.Fatalf("tools/call: %v", err)
			}
			if res.IsError {
				t.Fatalf("isError: %s", resultText(t, res))
			}
			const want = `{"items":[{"id":"svc-a","name":"alpha"},{"id":"svc-b","name":"beta"}]}`
			if got := resultText(t, res); got != want {
				t.Errorf("text content\n got %s\nwant %s", got, want)
			}
			// The client decodes structuredContent into a map; compare as JSON.
			if structured, _ := json.Marshal(res.StructuredContent); !sameJSON(t, string(structured), want) {
				t.Errorf("structuredContent\n got %s\nwant %s", structured, want)
			}
		})
	}
}

// A client that only knows 2024-11-05 is answered with a revision the
// server does offer; the client then decides.
func TestMCP_Revision20241105_NotOffered(t *testing.T) {
	hs, _ := newSDKTestServer(t, &fakeAPI{}, presetPerms(authz.PresetFull), "admin")
	resp, body := rawPOST(t, hs.URL, "", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"old","version":"0"}}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("initialize: %d %s", resp.StatusCode, body)
	}
	resp, body = rawPOST(t, hs.URL, "", map[string]string{"MCP-Protocol-Version": "2024-11-05"}, rawToolsList)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("request on 2024-11-05: %d %s", resp.StatusCode, body)
	}
}

// ---------- descriptors ----------------------------------------------------

func TestMCP_Annotations(t *testing.T) {
	hs, _ := newSDKTestServer(t, &fakeAPI{}, presetPerms(authz.PresetFull), "admin")
	s := newTestServer(t, nil, nil, quietLog())
	for _, rev := range []string{"2026-07-28", "2025-11-25"} {
		tools := listedTools(t, connectMCP(t, hs.URL, "", rev))
		for _, op := range s.curated {
			name := ToolName(op.ID)
			tl := tools[name]
			if tl == nil {
				t.Fatalf("%s %s: not listed", rev, name)
			}
			a := tl.Annotations
			if a == nil || a.DestructiveHint == nil || a.OpenWorldHint == nil {
				t.Fatalf("%s %s: annotations incomplete: %+v", rev, name, a)
			}
			if a.ReadOnlyHint != (op.Method == http.MethodGet) || *a.DestructiveHint != op.Destructive || a.IdempotentHint != (op.Method == http.MethodPut) {
				t.Errorf("%s %s: hints %+v do not match %s %s (destructive %v)", rev, name, a, op.Method, op.Path, op.Destructive)
			}
			if *a.OpenWorldHint {
				t.Errorf("%s %s: openWorldHint is true", rev, name)
			}
		}
		// Spelled out.
		for name, want := range map[string][3]bool{ // readOnly, destructive, idempotent
			"providers_list":       {true, false, false},
			"provider_delete":      {false, true, false},
			"provider_models_sync": {false, true, false}, // POST, x-burrow-destructive
			"budget_create":        {false, false, false},
		} {
			a := tools[name].Annotations
			if a.ReadOnlyHint != want[0] || *a.DestructiveHint != want[1] || a.IdempotentHint != want[2] {
				t.Errorf("%s %s: %+v, want readOnly/destructive/idempotent %v", rev, name, a, want)
			}
		}
	}
}

// The descriptions: the curated text where the list gives one, otherwise
// the summary and the first paragraph of the operation's description.
func TestMCP_Descriptions(t *testing.T) {
	s := newTestServer(t, nil, nil, quietLog())
	byName := map[string]ToolDescriptor{}
	for _, d := range s.Tools() {
		byName[d.Name] = d
	}
	if got := byName["me_get"].Description; !strings.HasPrefix(got, "Who this token acts for") {
		t.Errorf("me_get description = %q", got)
	}
	op, ok := s.reg.Lookup("budgetCreate")
	if got := byName["budget_create"].Description; !ok || op.Summary == "" || !strings.HasPrefix(got, op.Summary) || strings.Contains(got, "\n\n\n") {
		t.Errorf("budget_create description = %q", got)
	}
	for _, d := range s.Tools() {
		if d.Description == "" || len(d.Description) > maxDescriptionBytes {
			t.Errorf("%s: description of %d bytes", d.Name, len(d.Description))
		}
	}
}

// Tools() describes every curated tool for the dashboard: its schema, the
// permissions that open it, its kind.
func TestMCP_ToolDescriptors(t *testing.T) {
	s := newTestServer(t, nil, nil, quietLog())
	tools := s.Tools()
	if len(tools) != len(curatedTools)+3 {
		t.Fatalf("%d descriptors, want %d", len(tools), len(curatedTools)+3)
	}
	generic := tools[len(curatedTools):]
	tools = tools[:len(curatedTools)]
	for i, want := range []struct{ name, kind string }{
		{"burrow_find_operations", "read"}, {"burrow_describe_operation", "read"}, {"burrow_call_operation", "write"},
	} {
		if d := generic[i]; d.Name != want.name || d.Kind != want.kind || !d.Generic || d.Permission != "" || d.Permissions == nil || len(d.Permissions) != 0 {
			t.Errorf("generic descriptor %d = %+v", i, d)
		}
	}
	for i, d := range tools {
		if d.Name != ToolName(curatedTools[i].ID) || d.Generic {
			t.Errorf("descriptor %d = %s generic=%v, want %s", i, d.Name, d.Generic, ToolName(curatedTools[i].ID))
		}
		var schema map[string]any
		if err := json.Unmarshal(d.ParametersSchema, &schema); err != nil || schema["type"] != "object" {
			t.Errorf("%s: schema %s", d.Name, d.ParametersSchema)
		}
	}
	byName := map[string]ToolDescriptor{}
	for _, d := range tools {
		byName[d.Name] = d
	}
	if d := byName["me_get"]; d.Permission != "" || len(d.Permissions) != 0 || d.Kind != "read" {
		t.Errorf("me_get = %+v", d)
	}
	if d := byName["services_list"]; d.Permission != "services:configure:own" ||
		strings.Join(d.Permissions, ",") != "services:configure:own,services:configure:any" || d.Kind != "read" {
		t.Errorf("services_list = %+v", d)
	}
	if d := byName["provider_delete"]; d.Kind != "delete" || !strings.Contains(string(d.ParametersSchema), `"confirm"`) {
		t.Errorf("provider_delete = %+v", d)
	}
	if d := byName["budget_create"]; d.Kind != "write" || !strings.Contains(string(d.ParametersSchema), `"dry_run"`) {
		t.Errorf("budget_create = %+v", d)
	}
	if s.OperationCount() != len(s.reg.Operations()) || s.OperationCount() < 100 {
		t.Errorf("OperationCount = %d", s.OperationCount())
	}
}

// ---------- what a token sees and may call ---------------------------------

func TestMCP_ListShowsOnlyWhatTheTokenMayCall(t *testing.T) {
	cases := []struct {
		name  string
		perms []string
		role  string
		want  string
	}{
		{"everything", presetPerms(authz.PresetFull), "admin", allCuratedTools},
		{"one permission", []string{"audit:read"}, "admin", "audit_events_list,connection_logs_list,me_get"},
		// The token still declares everything; the role of its user grants
		// only the :own permissions of a standard user.
		{"minter demoted to user", presetPerms(authz.PresetFull), "user",
			"ai_models_list,ai_requests_list,gateway_key_delete,gateway_keys_list,inspector_request_get,me_get," +
				"provider_get,provider_metrics_get,providers_list,service_get,service_slug_update,services_list"},
		{"tunnels of anyone", []string{"tunnels:read:any"}, "admin", "known_clients_list,me_get"},
		{"no tool permission", []string{"webhooks:manage"}, "admin", "me_get"},
		// A token that declares nothing still reads who it acts for.
		{"no permission declared", nil, "admin", "me_get"},
		{"unknown role", presetPerms(authz.PresetFull), "gone", "me_get"},
	}
	for _, c := range cases {
		for _, rev := range []string{"2026-07-28", "2025-11-25"} {
			hs, _ := newSDKTestServer(t, &fakeAPI{}, c.perms, c.role)
			if got := sortedNames(listedTools(t, connectMCP(t, hs.URL, "", rev))); got != c.want {
				t.Errorf("%s (%s):\n got %s\nwant %s", c.name, rev, got, c.want)
			}
		}
	}
}

// A call on a tool the token may not use is refused exactly like a call on a
// tool that does not exist, and it is the only one of the two that leaves an
// audit row. Nothing reaches the API.
func TestMCP_CallOutsideTheTokensTools(t *testing.T) {
	for _, rev := range []string{"2026-07-28", "2025-11-25"} {
		api := &fakeAPI{}
		hs, sink := newSDKTestServer(t, api, []string{"audit:read"}, "admin")
		cs := connectMCP(t, hs.URL, "", rev)

		_, errHidden := callTool(t, cs, "provider_delete", map[string]any{"slug": "zai", "confirm": "zai"})
		_, errUnknown := callTool(t, cs, "no_such_tool", nil)

		var hidden, unknown *jsonrpc.Error
		if !errors.As(errHidden, &hidden) || !errors.As(errUnknown, &unknown) {
			t.Fatalf("%s: want two JSON-RPC errors, got %v and %v", rev, errHidden, errUnknown)
		}
		if hidden.Code != jsonrpc.CodeInvalidParams || hidden.Code != unknown.Code {
			t.Errorf("%s: codes %d and %d, want %d twice", rev, hidden.Code, unknown.Code, jsonrpc.CodeInvalidParams)
		}
		a := strings.Replace(hidden.Message, "provider_delete", "NAME", 1)
		b := strings.Replace(unknown.Message, "no_such_tool", "NAME", 1)
		if a != b || len(hidden.Data) != 0 || len(unknown.Data) != 0 {
			t.Errorf("%s: the two refusals differ: %q / %q", rev, hidden.Message, unknown.Message)
		}
		if api.calls() != 0 {
			t.Errorf("%s: the refused call reached the API", rev)
		}

		rows := sink.all()
		if len(rows) != 1 {
			t.Fatalf("%s: %d audit rows, want 1: %+v", rev, len(rows), rows)
		}
		if r := rows[0]; r.Action != audit.ActionMCPToolCall || r.SubjectLabel != "provider_delete" ||
			r.Result != "denied" || r.ActorID != "u1" {
			t.Errorf("%s: audit row = %+v", rev, r)
		}
	}
}

// A tool opened by an :own and an :any permission is opened by either, each
// on the usual terms: the token declares it AND the role of its user grants
// it. Two halves of different permissions (declared :any, granted :own) open
// nothing. What tools/list shows and what tools/call accepts is decided in
// one place, so the two always agree.
func TestMCP_EitherScopeOpensATool(t *testing.T) {
	authz.SetRoles(map[string][]authz.Permission{
		"configures-any": {authz.PermServicesConfigureAny},
		"configures-own": {authz.PermServicesConfigureOwn},
		"reads-audit":    {authz.PermAuditRead},
	})
	t.Cleanup(func() { authz.SetRoles(nil) })

	const ownP, anyP = "services:configure:own", "services:configure:any"
	args := map[string]map[string]any{"service_get": {"serviceID": "svc1"}}
	for _, tool := range []string{"services_list", "service_get"} {
		for _, c := range []struct {
			name  string
			perms []string
			role  string
			open  bool
		}{
			{"only :own", []string{ownP}, "admin", true},
			{"only :any", []string{anyP}, "admin", true},
			{"both", []string{ownP, anyP}, "admin", true},
			{"neither", []string{"audit:read"}, "admin", false},
			{"nothing declared", nil, "admin", false},
			{":own, the built-in user", []string{ownP}, "user", true},
			// The token declares one, the role grants the other.
			{":any declared, the role grants only :own", []string{anyP}, "user", false},
			{":own declared, the role grants only :any", []string{ownP}, "configures-any", false},
			{":any, a role with only :any", []string{anyP}, "configures-any", true},
			{"both declared, a role with only :any", []string{ownP, anyP}, "configures-any", true},
			{"both declared, a role with only :own", []string{ownP, anyP}, "configures-own", true},
			{"both declared, a role with neither", []string{ownP, anyP, "audit:read"}, "reads-audit", false},
		} {
			what := fmt.Sprintf("%s, %s", tool, c.name)
			api := &fakeAPI{reply: `{}`}
			hs, sink := newSDKTestServer(t, api, c.perms, c.role)
			cs := connectMCP(t, hs.URL, "", "2026-07-28")
			if _, listed := listedTools(t, cs)[tool]; listed != c.open {
				t.Errorf("%s: listed = %v, want %v", what, listed, c.open)
			}
			res, err := callTool(t, cs, tool, args[tool])
			var rpcErr *jsonrpc.Error
			switch {
			case c.open && (err != nil || res.IsError):
				t.Errorf("%s: the call failed: err=%v result=%+v", what, err, res)
			case !c.open && (!errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams):
				t.Errorf("%s: the call was not refused like an unknown tool: err=%v result=%+v", what, err, res)
			}
			wantRow, wantCalls := "ok", 1
			if !c.open {
				wantRow, wantCalls = "denied", 0
			}
			if rows := sink.all(); len(rows) != 1 || rows[0].SubjectLabel != tool || rows[0].Result != wantRow {
				t.Errorf("%s: audit rows %+v, want one %s row", what, rows, wantRow)
			}
			if api.calls() != wantCalls {
				t.Errorf("%s: %d API requests, want %d", what, api.calls(), wantCalls)
			}
		}
	}
}

// An operation that needs every permission (x-burrow-permission every:
// true) is open only to a token that holds them all; one with an empty
// any_of to nobody.
func TestMayUse(t *testing.T) {
	all := make([]string, 0, len(authz.AllPermissions()))
	for _, p := range authz.AllPermissions() {
		all = append(all, string(p))
	}
	every := Operation{Access: Access{Kind: "needs", Every: true, AnyOf: []authz.Permission{authz.PermUsersManage}}}
	empty := Operation{Access: Access{Kind: "needs"}}
	for _, c := range []struct {
		name string
		op   Operation
		tok  authedToken
		want bool
	}{
		{"every, all of them", every, authedToken{role: "admin", perms: all}, true},
		{"every, one missing", every, authedToken{role: "admin", perms: all[1:]}, false},
		{"every, the role lacks some", every, authedToken{role: "user", perms: all}, false},
		{"empty any_of", empty, authedToken{role: "admin", perms: all}, false},
		{"self", Operation{Access: Access{Kind: "self"}}, authedToken{role: "gone"}, true},
		{"public", Operation{Access: Access{Kind: "public"}}, authedToken{}, true},
		{"dashboard", Operation{Access: Access{Kind: "dashboard", AnyOf: []authz.Permission{authz.PermUsersManage}}}, authedToken{role: "admin", perms: all}, false},
		{"unknown kind", Operation{Access: Access{Kind: "other"}}, authedToken{role: "admin", perms: all}, false},
	} {
		if got := c.tok.mayUse(c.op); got != c.want {
			t.Errorf("%s: mayUse = %v, want %v", c.name, got, c.want)
		}
	}
}

// ---------- results --------------------------------------------------------

// A write sends the body as JSON, with the method and path of the operation.
func TestMCP_WriteSendsTheBody(t *testing.T) {
	api := &fakeAPI{status: http.StatusNoContent}
	hs, sink := newSDKTestServer(t, api, presetPerms(authz.PresetOperator), "admin")
	cs := connectMCP(t, hs.URL, "", "2026-07-28")

	res, err := callTool(t, cs, "budget_create", map[string]any{
		"body": map[string]any{"scope": "global", "action_on_exceed": "alert_webhook", "daily_usd": 1},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: err=%v result=%+v", err, res)
	}
	if got := resultText(t, res); got != `{"status":204}` {
		t.Errorf("text = %s", got)
	}
	r := api.seen[0]
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/budgets" || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("request = %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
	}
	if !sameJSON(t, api.bodies[0], `{"scope":"global","action_on_exceed":"alert_webhook","daily_usd":1}`) {
		t.Errorf("body = %s", api.bodies[0])
	}
	if rows := sink.all(); len(rows) != 1 || rows[0].SubjectLabel != "budget_create" || rows[0].Result != "ok" {
		t.Fatalf("audit rows = %+v", rows)
	}
}

// Query arguments become the query string; a read sends no body.
func TestMCP_QueryArguments(t *testing.T) {
	api := &fakeAPI{reply: `[]`}
	hs, _ := newSDKTestServer(t, api, presetPerms(authz.PresetReadOnly), "admin")
	cs := connectMCP(t, hs.URL, "", "2026-07-28")
	res, err := callTool(t, cs, "ai_requests_list", map[string]any{"provider": "zai", "model": "glm 4.6"})
	if err != nil || res.IsError {
		t.Fatalf("call: err=%v result=%+v", err, res)
	}
	r := api.seen[0]
	if q := r.URL.Query(); q.Get("provider") != "zai" || q.Get("model") != "glm 4.6" || len(q) != 2 {
		t.Errorf("query = %s", r.URL.RawQuery)
	}
	if api.bodies[0] != "" || r.Header.Get("Content-Type") != "" {
		t.Errorf("a read sent a body: %q %q", api.bodies[0], r.Header.Get("Content-Type"))
	}
	if got := resultText(t, res); got != `{"items":[]}` {
		t.Errorf("empty list = %s", got)
	}
}

// A call that fails answers with a result the model can read (isError), not
// with a protocol error.
func TestMCP_CallFailure_IsAResult(t *testing.T) {
	for _, rev := range []string{"2026-07-28", "2025-11-25"} {
		api := &fakeAPI{status: http.StatusNotFound, reply: `{"error":"service not found"}`}
		hs, sink := newSDKTestServer(t, api, presetPerms(authz.PresetOperator), "admin")
		cs := connectMCP(t, hs.URL, "", rev)

		// Refused before the API: a required argument is missing.
		res, err := callTool(t, cs, "service_get", nil)
		if err != nil {
			t.Fatalf("%s: protocol error for a failed call: %v", rev, err)
		}
		if text := resultText(t, res); !res.IsError || !strings.Contains(text, "serviceID") {
			t.Errorf("%s: result = %s", rev, text)
		}
		if api.calls() != 0 {
			t.Errorf("%s: refused arguments reached the API", rev)
		}
		// The API's answer.
		res, err = callTool(t, cs, "service_get", map[string]any{"serviceID": "nope"})
		if err != nil || !res.IsError || resultText(t, res) != "404 Not Found: service not found" {
			t.Errorf("%s: not found: err=%v result=%+v", rev, err, res)
		}

		var got []string
		for _, r := range sink.all() {
			got = append(got, r.SubjectLabel+"="+r.Result)
		}
		if want := "service_get=error,service_get=error"; strings.Join(got, ",") != want {
			t.Errorf("%s: audit rows %v, want %s", rev, got, want)
		}
	}
}

// The arguments of a call are a JSON object; anything else is refused as a
// result, not a protocol error.
func TestMCP_ArgumentsMustBeAnObject(t *testing.T) {
	api := &fakeAPI{reply: `[]`}
	hs, sink := newSDKTestServer(t, api, presetPerms(authz.PresetReadOnly), "admin")
	for _, args := range []string{`[1,2]`, `"x"`, `7`} {
		resp, body := rawPOST(t, hs.URL, "", map[string]string{"MCP-Protocol-Version": "2025-06-18"},
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"providers_list","arguments":`+args+`}}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", args, resp.StatusCode, body)
		}
		// The SDK may refuse it itself; either way nothing is sent.
		if !strings.Contains(body, `"isError":true`) && !strings.Contains(body, `"error"`) {
			t.Errorf("%s: %s", args, body)
		}
	}
	// null and no arguments are an empty object.
	for _, params := range []string{`{"name":"providers_list","arguments":null}`, `{"name":"providers_list"}`} {
		_, body := rawPOST(t, hs.URL, "", map[string]string{"MCP-Protocol-Version": "2025-06-18"},
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+params+`}`)
		if strings.Contains(body, `"isError":true`) || !strings.Contains(body, `"items"`) {
			t.Errorf("%s: %s", params, body)
		}
	}
	if api.calls() != 2 {
		t.Errorf("%d API requests, want 2", api.calls())
	}
	for _, r := range sink.all()[:len(sink.all())-2] {
		if r.Result != "error" {
			t.Errorf("audit row %+v", r)
		}
	}
}

// panicAPI is a router whose handler panics, as a nil dependency in the
// wiring would.
type panicAPI struct{ msg string }

func (p panicAPI) ServeHTTP(http.ResponseWriter, *http.Request) { panic(p.msg) }

// The SDK runs a tool on a goroutine of its own. A panic there is not caught
// by net/http or by the router; uncaught it ends the relay.
func TestMCP_ToolPanic_IsContained(t *testing.T) {
	hs, sink := newSDKTestServer(t, panicAPI{msg: "metrics recorder is nil"}, presetPerms(authz.PresetFull), "admin")
	cs := connectMCP(t, hs.URL, "", "2025-11-25")

	res, err := callTool(t, cs, "providers_list", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError || resultText(t, res) != "internal error" {
		t.Errorf("result = %+v", res)
	}
	if rows := sink.all(); len(rows) != 1 || rows[0].Result != "error" {
		t.Errorf("audit rows = %+v", rows)
	}
	// The server still answers.
	if _, err := callTool(t, cs, "me_get", nil); err != nil {
		t.Fatalf("call after the panic: %v", err)
	}
}

// ---------- secrets --------------------------------------------------------

// The audit row of a call names the tool, the outcome and the token: not the
// arguments, not the answer, which can hold a secret the API handed out.
func TestMCP_ArgumentsAndAnswers_StayOutOfTheAuditLog(t *testing.T) {
	const secret = "bgk_SECRET-handed-out"
	api := &fakeAPI{status: http.StatusCreated, reply: `{"id":"g1","key":"` + secret + `"}`}
	hs, sink := newSDKTestServer(t, api, presetPerms(authz.PresetFull), "admin")
	cs := connectMCP(t, hs.URL, "", "2026-07-28")

	res, err := callTool(t, cs, "budget_create", map[string]any{"body": map[string]any{"scope": "user", "subject_id": "ARG-VALUE", "action_on_exceed": "throttle_zero"}})
	if err != nil || res.IsError {
		t.Fatalf("budget_create: err=%v result=%+v", err, res)
	}
	if !strings.Contains(resultText(t, res), secret) {
		t.Errorf("the caller did not get the answer: %s", resultText(t, res))
	}
	rows := sink.all()
	if len(rows) != 1 {
		t.Fatalf("%d audit rows, want 1", len(rows))
	}
	row, _ := json.Marshal(rows[0])
	// The payload names the token, and nothing else.
	if strings.Contains(string(row), secret) || strings.Contains(string(row), "ARG-VALUE") || string(rows[0].Payload) != `{"token_id":"tok1"}` {
		t.Errorf("audit row carries an argument or the answer: %s", row)
	}
}

// ---------- the wire -------------------------------------------------------

// The list depends on the caller, so no shared cache may keep it.
func TestMCP_ListIsMarkedPrivate(t *testing.T) {
	hs, _ := newSDKTestServer(t, &fakeAPI{}, []string{"audit:read"}, "admin")
	resp, body := rawPOST(t, hs.URL, "", map[string]string{"MCP-Protocol-Version": "2025-06-18"}, rawToolsList)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"cacheScope":"private"`) || strings.Contains(body, `"public"`) {
		t.Errorf("cache scope: %s", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type %q, want application/json", ct)
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.Errorf("session id header %q on a stateless endpoint", sid)
	}
	if !strings.Contains(body, `"audit_events_list"`) || strings.Contains(body, `"providers_list"`) {
		t.Errorf("list: %s", body)
	}
}

// Without a token in the request context nothing is served.
func TestMCP_NoToken_NothingServed(t *testing.T) {
	hs := httptest.NewServer(newTestServer(t, nil, nil, nil).mcpHandler())
	t.Cleanup(hs.Close)
	resp, body := rawPOST(t, hs.URL, "", map[string]string{"MCP-Protocol-Version": "2025-06-18"}, rawToolsList)
	if resp.StatusCode != http.StatusBadRequest || strings.Contains(body, "me_get") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

// A JSON-RPC batch is refused before the SDK sees it: it would let one POST
// run any number of calls past the per-request limits.
func TestMCP_Batch_IsRefused(t *testing.T) {
	batch := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"me_get","arguments":{}}},` +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"me_get","arguments":{}}}]`
	for _, rev := range append([]string{""}, testRevisions...) {
		for _, body := range []string{batch, " \n\t" + batch} {
			api := &fakeAPI{}
			hs, sink := newSDKTestServer(t, api, presetPerms(authz.PresetFull), "admin")
			hdr := map[string]string{}
			if rev != "" {
				hdr["MCP-Protocol-Version"] = rev
			}
			resp, got := rawPOST(t, hs.URL, "", hdr, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("revision %q: batch got %d %s, want 400", rev, resp.StatusCode, got)
			}
			if rows := sink.all(); len(rows) != 0 || api.calls() != 0 {
				t.Errorf("revision %q: batch left audit rows %+v or reached the API %d times", rev, rows, api.calls())
			}
		}
	}
}

// A single request object still works, also when it follows whitespace.
func TestMCP_SingleRequest_StillServed(t *testing.T) {
	for _, rev := range []string{"2025-11-25", "2025-06-18", "2025-03-26"} {
		hs, _ := newSDKTestServer(t, &fakeAPI{}, presetPerms(authz.PresetFull), "admin")
		resp, body := rawPOST(t, hs.URL, "", map[string]string{"MCP-Protocol-Version": rev}, "  \n"+rawToolsList)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"me_get"`) {
			t.Errorf("%s: %d %s", rev, resp.StatusCode, body)
		}
	}
}

// panicAudit is an AuditAppender that panics.
type panicAudit struct{}

func (panicAudit) Append(context.Context, audit.Event) error { panic("audit sink is broken") }

func TestMCP_AuditPanic_IsContained(t *testing.T) {
	s := newTestServer(t, nil, nil, quietLog())
	s.SetAPIHandler(&fakeAPI{reply: `{}`})
	s.SetAuditAppender(panicAudit{})
	hs := serveFor(t, s, &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: []string{"audit:read"}})
	cs := connectMCP(t, hs.URL, "", "2025-11-25")

	// An allowed tool, and a tool the token may not see.
	for _, name := range []string{"audit_events_list", "providers_list"} {
		res, err := callTool(t, cs, name, nil)
		if err != nil {
			t.Fatalf("%s: protocol error: %v", name, err)
		}
		if !res.IsError || resultText(t, res) != "internal error" {
			t.Errorf("%s: result = %+v", name, res)
		}
	}
	// The process and the server are still there.
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("tools/list after the panics: %v", err)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// Argument values reach no log line, refused or sent.
func TestMCP_ArgumentValues_StayOutOfTheLog(t *testing.T) {
	var logBuf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &logBuf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := newTestServer(t, nil, nil, log)
	s.SetAPIHandler(&fakeAPI{status: http.StatusBadRequest, reply: `{"error":"bad"}`})
	s.SetAuditAppender(&auditSink{})
	hs := serveFor(t, s, &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: presetPerms(authz.PresetFull)})
	cs := connectMCP(t, hs.URL, "", "2025-11-25")

	for _, args := range []map[string]any{
		{"since": "SECRET-not-a-date", "limit": "SECRET-not-a-number"},
		{"action": "SECRET-sent"},
		{"SECRET-unknown": 1},
	} {
		res, err := callTool(t, cs, "audit_events_list", args)
		if err != nil || !res.IsError {
			t.Fatalf("args %v: err=%v result=%+v", args, err, res)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logBuf.String(), "audit_events_list") {
		t.Fatalf("the calls were not logged; the test would prove nothing: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "SECRET") {
		t.Errorf("log carries an argument value: %s", logBuf.String())
	}
}

// One handler serves tokens with different permissions; each request sees
// only its own token's tools, interleaved and concurrent.
func TestMCP_TwoTokensOneHandler(t *testing.T) {
	s := newTestServer(t, nil, nil, quietLog())
	s.SetAuditAppender(&auditSink{})
	toks := map[string]*authedToken{
		"a": {tokenID: "ta", userID: "u1", role: "admin", perms: []string{"audit:read"}},
		"b": {tokenID: "tb", userID: "u2", role: "admin", perms: []string{"tunnels:read:any"}},
	}
	h := s.mcpHandler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := toks[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if tok == nil {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	}))
	t.Cleanup(hs.Close)
	want := map[string]string{"a": "audit_events_list,connection_logs_list,me_get", "b": "known_clients_list,me_get"}

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 8; i++ {
		for _, name := range []string{"a", "b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 5; j++ {
					cs, err := dialMCP(hs.URL, name, "2025-11-25")
					if err != nil {
						errs <- "connect: " + err.Error()
						return
					}
					res, err := cs.ListTools(context.Background(), nil)
					_ = cs.Close()
					if err != nil {
						errs <- "list: " + err.Error()
						return
					}
					m := map[string]*mcp.Tool{}
					for _, tl := range res.Tools {
						m[tl.Name] = tl
					}
					if got := sortedNames(m); got != want[name] {
						errs <- "token " + name + " saw " + got
					}
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

const marker = "MARKER-7f3a-secret-value"

// A panic of the API is answered with a fixed text and logged with its type
// and a stack, never its value.
func TestMCP_PanicText_StaysOutOfLogsAndResults(t *testing.T) {
	var logBuf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &logBuf}, nil))
	s := newTestServer(t, nil, nil, log)
	s.SetAPIHandler(panicAPI{msg: "index out of range near " + marker})
	s.SetAuditAppender(&auditSink{})
	hs := serveFor(t, s, &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: presetPerms(authz.PresetFull)})
	cs := connectMCP(t, hs.URL, "", "2025-11-25")

	res, err := callTool(t, cs, "providers_list", nil)
	if err != nil || !res.IsError {
		t.Fatalf("err=%v result=%+v", err, res)
	}
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), marker) || resultText(t, res) != "internal error" {
		t.Errorf("result = %s", raw)
	}
	mu.Lock()
	defer mu.Unlock()
	logs := logBuf.String()
	if strings.Contains(logs, marker) {
		t.Errorf("a log line carries the marker: %s", logs)
	}
	if !strings.Contains(logs, "providers_list") || !strings.Contains(logs, "panic_type=string") || !strings.Contains(logs, "goroutine") {
		t.Errorf("panic log: %s", logs)
	}
}

// Every result goes out twice: as structuredContent and as the text of one
// content block, the same JSON object.
func TestMCP_Results_AreStructuredAndText(t *testing.T) {
	for _, c := range []struct {
		name, ct, reply string
		status          int
		want            string
	}{
		{"object", "application/json", `{"id":"p1"}`, 200, `{"id":"p1"}`},
		{"object with spaces", "application/json", "{ \"id\": \"p1\" }\n", 200, `{"id":"p1"}`},
		{"array", "application/json", `[1,2]`, 200, `{"items":[1,2]}`},
		{"no content", "", ``, 204, `{"status":204}`},
		{"text", "text/plain; version=0.0.4", "burrow_x 1\n", 200, `{"content_type":"text/plain; version=0.0.4","text":"burrow_x 1\n"}`},
		{"scalar", "application/json", `"ok"`, 200, `{"text":"\"ok\""}`},
	} {
		api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if c.ct != "" {
				w.Header().Set("Content-Type", c.ct)
			}
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.reply)
		})
		for _, rev := range []string{"2026-07-28", "2025-03-26"} {
			hs, _ := newSDKTestServer(t, api, presetPerms(authz.PresetFull), "admin")
			res, err := callTool(t, connectMCP(t, hs.URL, "", rev), "providers_list", nil)
			if err != nil || res.IsError {
				t.Fatalf("%s %s: err=%v result=%+v", c.name, rev, err, res)
			}
			text := resultText(t, res)
			if !sameJSON(t, text, c.want) {
				t.Errorf("%s %s: text %s, want %s", c.name, rev, text, c.want)
			}
			structured, ok := res.StructuredContent.(map[string]any)
			if !ok {
				t.Fatalf("%s %s: structuredContent is %T, want an object", c.name, rev, res.StructuredContent)
			}
			a, _ := json.Marshal(structured)
			if !sameJSON(t, string(a), text) {
				t.Errorf("%s %s: structuredContent %s != text %s", c.name, rev, a, text)
			}
		}
	}
}

// ---------- tool deadline ---------------------------------------------------

// hangAPI blocks until its request's context is done, as a handler that
// hangs would, and says when it has returned.
type hangAPI struct {
	entered  chan struct{}
	returned chan struct{}
}

func (h *hangAPI) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	defer close(h.returned)
	close(h.entered)
	<-r.Context().Done()
}

func TestMCP_HangingTool_EndsAtTheDeadline(t *testing.T) {
	api := &hangAPI{entered: make(chan struct{}), returned: make(chan struct{})}
	var logs strings.Builder
	var logMu sync.Mutex
	s := newTestServer(t, nil, nil, slog.New(slog.NewTextHandler(lockedWriter{&logMu, &logs}, nil)))
	s.SetAPIHandler(api)
	s.toolTimeout = 100 * time.Millisecond
	sink := &auditSink{}
	s.SetAuditAppender(sink)
	srv := serveFor(t, s, &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: presetPerms(authz.PresetFull)})
	cs := connectMCP(t, srv.URL, "", "2025-11-25")

	start := time.Now()
	res, err := callTool(t, cs, "service_get", map[string]any{"serviceID": "secret-service-id"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("call took %v", d)
	}
	if !res.IsError || resultText(t, res) != "timed out" {
		t.Errorf("result = %+v", res)
	}
	select {
	case <-api.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the API request is still running after the deadline")
	}
	if rows := sink.all(); len(rows) != 1 || rows[0].Result != "error" {
		t.Errorf("audit rows = %+v", rows)
	}
	logMu.Lock()
	out := logs.String()
	logMu.Unlock()
	if !strings.Contains(out, "service_get") || !strings.Contains(out, "deadline=100ms") {
		t.Errorf("log lacks the tool or the deadline: %s", out)
	}
	if strings.Contains(out, "secret-service-id") {
		t.Errorf("log carries an argument value: %s", out)
	}
}
