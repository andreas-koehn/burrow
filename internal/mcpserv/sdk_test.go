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
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// The revisions a client may speak: the stateless one and the three with an
// initialize handshake.
var testRevisions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

// allToolPerms is what a token needs to see the whole registry.
var allToolPerms = []string{
	"tunnels:read:own", "services:configure:own", "tokens:manage:own",
	"users:read", "audit:read", "metrics:read",
}

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

// panicStore is a ToolStore whose metrics call panics, as a nil dependency
// in the wiring would.
type panicStore struct{ fakeStore }

func (*panicStore) MetricsText() (string, error) { panic("metrics recorder is nil") }

// ---------- bootstrap ------------------------------------------------------

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newSDKTestServer serves s.mcpHandler() for one fixed token, without the
// HTTP checks in front of it.
func newSDKTestServer(t *testing.T, ts ToolStore, perms []string, role string) (*httptest.Server, *auditSink) {
	t.Helper()
	s := New(nil, nil, ts, quietLog())
	sink := &auditSink{}
	s.SetAuditAppender(sink)
	tok := &authedToken{tokenID: "tok1", userID: "u1", role: role, perms: perms}
	h := s.mcpHandler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	}))
	t.Cleanup(hs.Close)
	return hs, sink
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

func sortedNames(m map[string]*mcp.Tool) string {
	names := make([]string, 0, len(m))
	for n := range m {
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

// ---------- both revisions -------------------------------------------------

func TestMCP_EveryRevision_ListAndCall(t *testing.T) {
	for _, rev := range testRevisions {
		t.Run(rev, func(t *testing.T) {
			fs := &fakeStore{tunnels: []TunnelInfo{
				{ID: "tun-a", Name: "alpha", Type: "http", Connected: true},
				{ID: "tun-b", Name: "beta", Type: "tcp", RemotePort: 2222},
			}}
			hs, _ := newSDKTestServer(t, fs, allToolPerms, "admin")
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
			if len(tools) != len(orderedTools) {
				t.Fatalf("listed %d tools, want %d: %s", len(tools), len(orderedTools), sortedNames(tools))
			}
			schema, _ := json.Marshal(tools["tunnels_list"].InputSchema)
			if !strings.Contains(string(schema), `"filter"`) || !strings.Contains(string(schema), `"additionalProperties":false`) {
				t.Errorf("tunnels_list inputSchema = %s", schema)
			}

			res, err := callTool(t, cs, "tunnels_list", map[string]any{"filter": "ALPHA"})
			if err != nil {
				t.Fatalf("tools/call: %v", err)
			}
			if res.IsError {
				t.Fatalf("isError: %s", resultText(t, res))
			}
			const want = `{"tunnels":[{"id":"tun-a","name":"alpha","type":"http","remote_port":0,"local_addr":"","bytes_in":0,"bytes_out":0,"connected":true}]}`
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
	hs, _ := newSDKTestServer(t, &fakeStore{}, allToolPerms, "admin")
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

// ---------- annotations ----------------------------------------------------

func TestMCP_Annotations(t *testing.T) {
	hs, _ := newSDKTestServer(t, &fakeStore{}, allToolPerms, "admin")
	for _, rev := range []string{"2026-07-28", "2025-11-25"} {
		tools := listedTools(t, connectMCP(t, hs.URL, "", rev))
		for _, name := range orderedTools {
			tl, a := New(nil, nil, nil, nil).tools[name], tools[name].Annotations
			if a == nil || a.DestructiveHint == nil || a.OpenWorldHint == nil {
				t.Fatalf("%s %s: annotations incomplete: %+v", rev, name, a)
			}
			if a.ReadOnlyHint != tl.ReadOnly || *a.DestructiveHint != tl.Destructive || a.IdempotentHint != tl.Idempotent {
				t.Errorf("%s %s: hints %+v do not match the registry", rev, name, a)
			}
			if *a.OpenWorldHint {
				t.Errorf("%s %s: openWorldHint is true", rev, name)
			}
		}
		// The three that are not plain reads, spelled out.
		if a := tools["api_keys_delete"].Annotations; !*a.DestructiveHint || a.ReadOnlyHint {
			t.Errorf("%s api_keys_delete: %+v", rev, a)
		}
		if a := tools["services_set_access_mode"].Annotations; !a.IdempotentHint || *a.DestructiveHint || a.ReadOnlyHint {
			t.Errorf("%s services_set_access_mode: %+v", rev, a)
		}
		if a := tools["api_keys_create"].Annotations; a.ReadOnlyHint || *a.DestructiveHint || a.IdempotentHint {
			t.Errorf("%s api_keys_create: %+v", rev, a)
		}
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
		{"everything", allToolPerms, "admin",
			"api_keys_create,api_keys_delete,api_keys_list,audit_search,metrics_snapshot,services_get,services_list,services_set_access_mode,tokens_list,tokens_mint,tunnels_list,users_list"},
		{"one permission", []string{"tunnels:read:own"}, "admin", "tunnels_list"},
		{"read-only operator", []string{"tunnels:read:own", "audit:read", "metrics:read"}, "admin",
			"audit_search,metrics_snapshot,tunnels_list"},
		// The token still declares everything; the role of its user no
		// longer grants the admin permissions.
		{"minter demoted to user", allToolPerms, "user",
			"api_keys_create,api_keys_delete,api_keys_list,services_get,services_list,services_set_access_mode,tokens_list,tokens_mint,tunnels_list"},
		// The permission for every user's tunnels does not open the tool
		// that lists the caller's own.
		{"tunnels of anyone", []string{"tunnels:read:any"}, "admin", ""},
		{"no tool permission", []string{"webhooks:manage"}, "admin", ""},
		// What the dashboard mints today: a token that declares nothing.
		// It authenticates and is shown an empty list, not an error.
		{"no permission declared", nil, "admin", ""},
		{"unknown role", allToolPerms, "gone", ""},
	}
	for _, c := range cases {
		for _, rev := range []string{"2026-07-28", "2025-11-25"} {
			hs, _ := newSDKTestServer(t, &fakeStore{}, c.perms, c.role)
			if got := sortedNames(listedTools(t, connectMCP(t, hs.URL, "", rev))); got != c.want {
				t.Errorf("%s (%s):\n got %s\nwant %s", c.name, rev, got, c.want)
			}
		}
	}
}

// A call on a tool the token may not use is refused exactly like a call on a
// tool that does not exist, and it is the only one of the two that leaves an
// audit row.
func TestMCP_CallOutsideTheTokensTools(t *testing.T) {
	for _, rev := range []string{"2026-07-28", "2025-11-25"} {
		fs := &fakeStore{}
		hs, sink := newSDKTestServer(t, fs, []string{"tunnels:read:own"}, "admin")
		cs := connectMCP(t, hs.URL, "", rev)

		_, errHidden := callTool(t, cs, "services_set_access_mode", map[string]any{"id": "svc1", "mode": "open"})
		_, errUnknown := callTool(t, cs, "no_such_tool", nil)

		var hidden, unknown *jsonrpc.Error
		if !errors.As(errHidden, &hidden) || !errors.As(errUnknown, &unknown) {
			t.Fatalf("%s: want two JSON-RPC errors, got %v and %v", rev, errHidden, errUnknown)
		}
		if hidden.Code != jsonrpc.CodeInvalidParams || hidden.Code != unknown.Code {
			t.Errorf("%s: codes %d and %d, want %d twice", rev, hidden.Code, unknown.Code, jsonrpc.CodeInvalidParams)
		}
		a := strings.Replace(hidden.Message, "services_set_access_mode", "NAME", 1)
		b := strings.Replace(unknown.Message, "no_such_tool", "NAME", 1)
		if a != b || len(hidden.Data) != 0 || len(unknown.Data) != 0 {
			t.Errorf("%s: the two refusals differ: %q / %q", rev, hidden.Message, unknown.Message)
		}
		if fs.lastSetMode != "" {
			t.Errorf("%s: the refused call reached the store", rev)
		}

		rows := sink.all()
		if len(rows) != 1 {
			t.Fatalf("%s: %d audit rows, want 1: %+v", rev, len(rows), rows)
		}
		if r := rows[0]; r.Action != audit.ActionMCPToolCall || r.SubjectLabel != "services_set_access_mode" ||
			r.Result != "denied" || r.ActorID != "u1" {
			t.Errorf("%s: audit row = %+v", rev, r)
		}
	}
}

// ---------- results --------------------------------------------------------

func TestMCP_CallAllowed_IsAudited(t *testing.T) {
	fs := &fakeStore{}
	hs, sink := newSDKTestServer(t, fs, allToolPerms, "admin")
	cs := connectMCP(t, hs.URL, "", "2026-07-28")

	res, err := callTool(t, cs, "services_set_access_mode", map[string]any{"id": "svc1", "mode": "api_key", "api_key_header": "X-Key"})
	if err != nil || res.IsError {
		t.Fatalf("call: err=%v result=%+v", err, res)
	}
	if got := resultText(t, res); got != `{"ok":true}` {
		t.Errorf("text = %s", got)
	}
	if fs.lastSetMode != "api_key" || fs.lastSetHeader != "X-Key" {
		t.Errorf("store saw mode=%q header=%q", fs.lastSetMode, fs.lastSetHeader)
	}
	rows := sink.all()
	if len(rows) != 1 || rows[0].SubjectLabel != "services_set_access_mode" || rows[0].Result != "ok" {
		t.Fatalf("audit rows = %+v", rows)
	}
}

// A tool that fails answers with a result the model can read (isError), not
// with a protocol error.
func TestMCP_ToolFailure_IsAResult(t *testing.T) {
	for _, rev := range []string{"2026-07-28", "2025-11-25"} {
		fs := &fakeStore{apiKeyErr: store.ErrForbidden}
		hs, sink := newSDKTestServer(t, fs, allToolPerms, "admin")
		cs := connectMCP(t, hs.URL, "", rev)

		// The tool's own validation.
		res, err := callTool(t, cs, "services_get", nil)
		if err != nil {
			t.Fatalf("%s: protocol error for a failed tool: %v", rev, err)
		}
		if !res.IsError || resultText(t, res) != "id is required" {
			t.Errorf("%s: result = %+v", rev, res)
		}
		// A row that does not exist.
		res, err = callTool(t, cs, "services_get", map[string]any{"id": "nope"})
		if err != nil || !res.IsError || resultText(t, res) != "not found" {
			t.Errorf("%s: not found: err=%v result=%+v", rev, err, res)
		}
		// The store refuses on ownership: the token may use the tool, not
		// on this service.
		res, err = callTool(t, cs, "api_keys_list", map[string]any{"service_id": "someone-elses"})
		if err != nil || !res.IsError || resultText(t, res) != "forbidden" {
			t.Errorf("%s: forbidden: err=%v result=%+v", rev, err, res)
		}

		var got []string
		for _, r := range sink.all() {
			got = append(got, r.SubjectLabel+"="+r.Result)
		}
		if want := "services_get=error,services_get=error,api_keys_list=denied"; strings.Join(got, ",") != want {
			t.Errorf("%s: audit rows %v, want %s", rev, got, want)
		}
	}
}

// The SDK runs a tool on a goroutine of its own. A panic there is not caught
// by net/http or by the router; uncaught it ends the relay.
func TestMCP_ToolPanic_IsContained(t *testing.T) {
	hs, sink := newSDKTestServer(t, &panicStore{}, allToolPerms, "admin")
	cs := connectMCP(t, hs.URL, "", "2025-11-25")

	res, err := callTool(t, cs, "metrics_snapshot", nil)
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
	if _, err := callTool(t, cs, "tunnels_list", nil); err != nil {
		t.Fatalf("call after the panic: %v", err)
	}
}

// ---------- secrets --------------------------------------------------------

// The two tools that hand out a secret return it to the caller and to
// nobody else: not to the audit log.
func TestMCP_MintedSecrets_StayOutOfTheAuditLog(t *testing.T) {
	fs := &fakeStore{}
	hs, sink := newSDKTestServer(t, fs, allToolPerms, "admin")
	cs := connectMCP(t, hs.URL, "", "2026-07-28")

	res, err := callTool(t, cs, "api_keys_create", map[string]any{"service_id": "svc1", "name": "prod"})
	if err != nil || res.IsError {
		t.Fatalf("api_keys_create: err=%v result=%+v", err, res)
	}
	if !strings.Contains(resultText(t, res), fs.lastCreatedPT) {
		t.Errorf("the caller did not get the key: %s", resultText(t, res))
	}
	res, err = callTool(t, cs, "tokens_mint", map[string]any{"name": "ci"})
	if err != nil || res.IsError {
		t.Fatalf("tokens_mint: err=%v result=%+v", err, res)
	}
	if !strings.Contains(resultText(t, res), fs.lastMintedPT) {
		t.Errorf("the caller did not get the token: %s", resultText(t, res))
	}

	rows := sink.all()
	if len(rows) != 2 {
		t.Fatalf("%d audit rows, want 2", len(rows))
	}
	for _, r := range rows {
		row, _ := json.Marshal(r)
		for _, secret := range []string{fs.lastCreatedPT, fs.lastMintedPT} {
			if strings.Contains(string(row), secret) {
				t.Errorf("audit row carries a minted secret: %s", row)
			}
		}
		if len(r.Payload) != 0 {
			t.Errorf("audit row has a payload: %s", r.Payload)
		}
	}
}

// ---------- the wire -------------------------------------------------------

// The list depends on the caller, so no shared cache may keep it.
func TestMCP_ListIsMarkedPrivate(t *testing.T) {
	hs, _ := newSDKTestServer(t, &fakeStore{}, []string{"tunnels:read:own"}, "admin")
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
	if !strings.Contains(body, `"tunnels_list"`) || strings.Contains(body, `"users_list"`) {
		t.Errorf("list: %s", body)
	}
}

// Without a token in the request context nothing is served.
func TestMCP_NoToken_NothingServed(t *testing.T) {
	hs := httptest.NewServer(New(nil, nil, &fakeStore{}, nil).mcpHandler())
	t.Cleanup(hs.Close)
	resp, body := rawPOST(t, hs.URL, "", map[string]string{"MCP-Protocol-Version": "2025-06-18"}, rawToolsList)
	if resp.StatusCode != http.StatusBadRequest || strings.Contains(body, "tunnels_list") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

// ---------- review fixes ---------------------------------------------------

// A JSON-RPC batch is refused before the SDK sees it: it would let one POST
// run any number of calls past the per-request limits.
func TestMCP_Batch_IsRefused(t *testing.T) {
	batch := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tunnels_list","arguments":{}}},` +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tunnels_list","arguments":{}}}]`
	for _, rev := range append([]string{""}, testRevisions...) {
		for _, body := range []string{batch, " \n\t" + batch} {
			hs, sink := newSDKTestServer(t, &fakeStore{}, allToolPerms, "admin")
			hdr := map[string]string{}
			if rev != "" {
				hdr["MCP-Protocol-Version"] = rev
			}
			resp, got := rawPOST(t, hs.URL, "", hdr, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("revision %q: batch got %d %s, want 400", rev, resp.StatusCode, got)
			}
			if rows := sink.all(); len(rows) != 0 {
				t.Errorf("revision %q: batch left audit rows: %+v", rev, rows)
			}
		}
	}
}

// A single request object still works, also when it follows whitespace.
func TestMCP_SingleRequest_StillServed(t *testing.T) {
	for _, rev := range []string{"2025-11-25", "2025-06-18", "2025-03-26"} {
		hs, _ := newSDKTestServer(t, &fakeStore{}, allToolPerms, "admin")
		resp, body := rawPOST(t, hs.URL, "", map[string]string{"MCP-Protocol-Version": rev}, "  \n"+rawToolsList)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"tunnels_list"`) {
			t.Errorf("%s: %d %s", rev, resp.StatusCode, body)
		}
	}
}

// panicAudit is an AuditAppender that panics.
type panicAudit struct{}

func (panicAudit) Append(context.Context, audit.Event) error { panic("audit sink is broken") }

func TestMCP_AuditPanic_IsContained(t *testing.T) {
	s := New(nil, nil, &fakeStore{}, quietLog())
	s.SetAuditAppender(panicAudit{})
	tok := &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: []string{"tunnels:read:own"}}
	h := s.mcpHandler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	}))
	t.Cleanup(hs.Close)
	cs := connectMCP(t, hs.URL, "", "2025-11-25")

	// An allowed tool, and a tool the token may not see.
	for _, name := range []string{"tunnels_list", "users_list"} {
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

// Argument values reach neither the caller nor the log.
func TestMCP_BadArguments_DoNotEchoValues(t *testing.T) {
	var logBuf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &logBuf}, nil))
	s := New(nil, nil, &fakeStore{}, log)
	s.SetAuditAppender(&auditSink{})
	tok := &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: allToolPerms}
	h := s.mcpHandler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	}))
	t.Cleanup(hs.Close)
	cs := connectMCP(t, hs.URL, "", "2025-11-25")

	for _, args := range []map[string]any{
		{"since": "SECRET-not-a-date"},
		{"limit": "SECRET-not-a-number"},
	} {
		res, err := callTool(t, cs, "audit_search", args)
		if err != nil {
			t.Fatalf("protocol error: %v", err)
		}
		text := resultText(t, res)
		if !res.IsError || !strings.HasPrefix(text, "invalid arguments") || strings.Contains(text, "SECRET") {
			t.Errorf("args %v: result = %+v", args, res)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logBuf.String(), "SECRET") {
		t.Errorf("log carries an argument value: %s", logBuf.String())
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

// A store error that is not the caller's business is answered with a fixed
// text and logged.
func TestMCP_StoreError_IsNotShownToTheCaller(t *testing.T) {
	fs := &fakeStore{apiKeyErr: errors.New("pq: connection to 10.0.0.5 refused")}
	hs, _ := newSDKTestServer(t, fs, allToolPerms, "admin")
	cs := connectMCP(t, hs.URL, "", "2025-11-25")
	res, err := callTool(t, cs, "api_keys_list", map[string]any{"service_id": "svc1"})
	if err != nil || !res.IsError || resultText(t, res) != "internal error" {
		t.Errorf("err=%v result=%+v", err, res)
	}
}

// One handler serves tokens with different permissions; each request sees
// only its own token's tools, interleaved and concurrent.
func TestMCP_TwoTokensOneHandler(t *testing.T) {
	s := New(nil, nil, &fakeStore{}, quietLog())
	s.SetAuditAppender(&auditSink{})
	toks := map[string]*authedToken{
		"a": {tokenID: "ta", userID: "u1", role: "admin", perms: []string{"tunnels:read:own"}},
		"b": {tokenID: "tb", userID: "u2", role: "admin", perms: []string{"audit:read", "metrics:read"}},
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
	want := map[string]string{"a": "tunnels_list", "b": "audit_search,metrics_snapshot"}

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

// markerStore fails with texts that quote a caller-supplied value, as a
// database duplicate-key detail does, and panics the same way.
type markerStore struct{ fakeStore }

const marker = "MARKER-7f3a-secret-value"

func (*markerStore) ListAPIKeys(context.Context, string, string, string) ([]db.ServiceAPIKey, error) {
	return nil, fmt.Errorf("duplicate key value violates unique constraint: Key (name)=(%s) already exists", marker)
}

func (*markerStore) MetricsText() (string, error) { panic("index out of range near " + marker) }

func TestMCP_ErrorAndPanicText_StayOutOfLogsAndResults(t *testing.T) {
	var logBuf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &logBuf}, nil))
	s := New(nil, nil, &markerStore{}, log)
	s.SetAuditAppender(&auditSink{})
	tok := &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: allToolPerms}
	h := s.mcpHandler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	}))
	t.Cleanup(hs.Close)
	cs := connectMCP(t, hs.URL, "", "2025-11-25")

	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"api_keys_list", map[string]any{"service_id": "svc1"}},
		{"metrics_snapshot", nil},
	} {
		res, err := callTool(t, cs, c.tool, c.args)
		if err != nil || !res.IsError {
			t.Fatalf("%s: err=%v result=%+v", c.tool, err, res)
		}
		raw, _ := json.Marshal(res)
		if strings.Contains(string(raw), marker) || resultText(t, res) != "internal error" {
			t.Errorf("%s: result = %s", c.tool, raw)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	logs := logBuf.String()
	if strings.Contains(logs, marker) {
		t.Errorf("a log line carries the marker: %s", logs)
	}
	if !strings.Contains(logs, "api_keys_list") || !strings.Contains(logs, "metrics_snapshot") {
		t.Errorf("the failures were not logged; the test would prove nothing: %s", logs)
	}
}

// Every object result goes out twice: as structuredContent and as the text
// of one content block, the same JSON.
func TestMCP_ObjectResult_IsStructuredAndText(t *testing.T) {
	fs := &fakeStore{
		tunnels:     []TunnelInfo{{ID: "tun-a", Name: "alpha", Type: "http"}},
		metricsBody: "burrow_x 1\n",
	}
	for _, rev := range testRevisions {
		hs, _ := newSDKTestServer(t, fs, allToolPerms, "admin")
		cs := connectMCP(t, hs.URL, "", rev)
		for _, tool := range []string{"tunnels_list", "services_list", "tokens_list", "users_list", "audit_search", "metrics_snapshot"} {
			res, err := callTool(t, cs, tool, nil)
			if err != nil || res.IsError {
				t.Fatalf("%s %s: err=%v result=%+v", rev, tool, err, res)
			}
			structured, ok := res.StructuredContent.(map[string]any)
			if !ok {
				t.Fatalf("%s %s: structuredContent is %T, want an object", rev, tool, res.StructuredContent)
			}
			var fromText map[string]any
			if err := json.Unmarshal([]byte(resultText(t, res)), &fromText); err != nil {
				t.Fatalf("%s %s: text is not a JSON object: %v", rev, tool, err)
			}
			a, _ := json.Marshal(structured)
			b, _ := json.Marshal(fromText)
			if string(a) != string(b) {
				t.Errorf("%s %s: structuredContent %s != text %s", rev, tool, a, b)
			}
		}
	}
}

// The caller's wrong input is answered as such, not as an internal error.
func TestMCP_AccessModeInputErrors_AreUserErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{store.ErrInvalidAccessMode, "invalid access mode"},
		{store.ErrServiceNotHTTP, "api_key and burrow_login require an http service"},
		{store.ErrDirectService, "service backs a direct AI provider"},
	} {
		fs := &fakeStore{setModeErr: c.err}
		hs, _ := newSDKTestServer(t, fs, allToolPerms, "admin")
		cs := connectMCP(t, hs.URL, "", "2025-11-25")
		res, err := callTool(t, cs, "services_set_access_mode", map[string]any{"id": "svc1", "mode": "bogus"})
		if err != nil || !res.IsError || resultText(t, res) != c.want {
			t.Errorf("%v: err=%v result=%+v, want %q", c.err, err, res, c.want)
		}
	}
}

// fakeDriverErr has the method of a sqlite driver error.
type fakeDriverErr struct{ msg string }

func (e *fakeDriverErr) Error() string { return e.msg }
func (*fakeDriverErr) Code() int       { return 2067 }

type chainStore struct{ fakeStore }

func (*chainStore) ListAPIKeys(_ context.Context, _, _, serviceID string) ([]db.ServiceAPIKey, error) {
	switch serviceID {
	case "sentinel":
		return nil, fmt.Errorf("create key %q: %w", marker, store.ErrTokenNameRequired)
	default:
		return nil, fmt.Errorf("insert %q: %w", marker, &fakeDriverErr{msg: "UNIQUE constraint failed: " + marker})
	}
}

// The log says why a tool failed without quoting argument values: the types
// along the chain, the text of a known sentinel, the code of a driver error.
func TestMCP_FailureLog_NamesTheCauseNotTheText(t *testing.T) {
	var logBuf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &logBuf}, nil))
	s := New(nil, nil, &chainStore{}, log)
	s.SetAuditAppender(&auditSink{})
	tok := &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: allToolPerms}
	h := s.mcpHandler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	}))
	t.Cleanup(hs.Close)
	cs := connectMCP(t, hs.URL, "", "2025-11-25")
	for _, id := range []string{"sentinel", "driver"} {
		if res, err := callTool(t, cs, "api_keys_list", map[string]any{"service_id": id}); err != nil || !res.IsError {
			t.Fatalf("%s: err=%v result=%+v", id, err, res)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	logs := logBuf.String()
	if strings.Contains(logs, marker) {
		t.Errorf("a log line carries the marker: %s", logs)
	}
	for _, want := range []string{
		"*fmt.wrapError > *errors.errorString", // the chain of the sentinel case
		store.ErrTokenNameRequired.Error(),     // its fixed text
		"*fmt.wrapError > *mcpserv.fakeDriverErr",
		"sqlite 2067", // the driver code
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log lacks %q: %s", want, logs)
		}
	}
}

// A panic is logged with its type and a stack, not its value.
func TestMCP_PanicLog_HasTypeAndStack(t *testing.T) {
	var logBuf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &logBuf}, nil))
	s := New(nil, nil, &markerStore{}, log)
	s.SetAuditAppender(&auditSink{})
	tok := &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: allToolPerms}
	h := s.mcpHandler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	}))
	t.Cleanup(hs.Close)
	cs := connectMCP(t, hs.URL, "", "2025-11-25")
	if _, err := callTool(t, cs, "metrics_snapshot", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	logs := logBuf.String()
	if strings.Contains(logs, marker) || !strings.Contains(logs, "panic_type=string") || !strings.Contains(logs, "goroutine") {
		t.Errorf("panic log: %s", logs)
	}
}

// ---------- tool deadline ---------------------------------------------------

// hangStore blocks ListAPIKeys until its context is done, as a store call
// that hangs would, and says when it has returned.
type hangStore struct {
	fakeStore
	entered  chan struct{}
	returned chan struct{}
}

func (h *hangStore) ListAPIKeys(ctx context.Context, _, _, _ string) ([]db.ServiceAPIKey, error) {
	defer close(h.returned)
	close(h.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestMCP_HangingTool_EndsAtTheDeadline(t *testing.T) {
	hs := &hangStore{entered: make(chan struct{}), returned: make(chan struct{})}
	var logs strings.Builder
	var logMu sync.Mutex
	s := New(nil, nil, hs, slog.New(slog.NewTextHandler(lockedWriter{&logMu, &logs}, nil)))
	s.toolTimeout = 100 * time.Millisecond
	sink := &auditSink{}
	s.SetAuditAppender(sink)
	tok := &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: allToolPerms}
	h := s.mcpHandler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	}))
	t.Cleanup(srv.Close)
	cs := connectMCP(t, srv.URL, "", "2025-11-25")

	start := time.Now()
	res, err := callTool(t, cs, "api_keys_list", map[string]any{"service_id": "secret-service-id"})
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
	case <-hs.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the tool call is still running after the deadline")
	}
	if rows := sink.all(); len(rows) != 1 || rows[0].Result != "error" {
		t.Errorf("audit rows = %+v", rows)
	}
	logMu.Lock()
	out := logs.String()
	logMu.Unlock()
	if !strings.Contains(out, "api_keys_list") || !strings.Contains(out, "deadline=100ms") {
		t.Errorf("log lacks the tool or the deadline: %s", out)
	}
	if strings.Contains(out, "secret-service-id") {
		t.Errorf("log carries an argument value: %s", out)
	}
}
