package mcpserv

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// ---------- fakes ----------------------------------------------------------

// fakeBearer satisfies BearerStore.
type fakeBearer struct {
	mu     sync.Mutex
	byHash map[string]TokenInfo
}

func newFakeBearer() *fakeBearer { return &fakeBearer{byHash: map[string]TokenInfo{}} }

func (f *fakeBearer) put(plaintext string, info TokenInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byHash[sha256Hex(plaintext)] = info
}

func (f *fakeBearer) LookupBearer(_ context.Context, hash string) (TokenInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.byHash[hash]
	if !ok {
		return TokenInfo{}, db.ErrNotFound
	}
	return info, nil
}

func (f *fakeBearer) TouchBearer(_ context.Context, _ string) error { return nil }

// fakeUsers satisfies UserLookup.
type fakeUsers struct {
	users map[string]db.User
}

func (f *fakeUsers) GetUserByID(_ context.Context, id string) (db.User, error) {
	u, ok := f.users[id]
	if !ok {
		return db.User{}, db.ErrNotFound
	}
	return u, nil
}

// fakeStore satisfies ToolStore.
type fakeStore struct {
	tunnels       []TunnelInfo
	services      []store.ServiceView
	serviceDetail store.ServiceDetail
	apiKeys       []db.ServiceAPIKey
	apiKeyErr     error
	clientTokens  []db.ClientToken
	users         []db.User
	userTotal     int
	audit         []db.AuditEvent
	metricsBody   string

	// counters / capture
	lastSetMode   string
	lastSetHeader string
	lastCreatedID string
	lastCreatedPT string
	lastDeleted   string
	lastMintedPT  string
}

func (f *fakeStore) ListUserTunnels(_ context.Context, _, _ string) ([]TunnelInfo, error) {
	return append([]TunnelInfo(nil), f.tunnels...), nil
}
func (f *fakeStore) ListServices(_ context.Context, _, _ string) ([]store.ServiceView, error) {
	return append([]store.ServiceView(nil), f.services...), nil
}
func (f *fakeStore) GetService(_ context.Context, _, _, id string) (store.ServiceDetail, error) {
	if f.serviceDetail.ID == "" || f.serviceDetail.ID != id {
		return store.ServiceDetail{}, db.ErrNotFound
	}
	return f.serviceDetail, nil
}
func (f *fakeStore) SetServiceAccessMode(_ context.Context, _, _, id, mode, header string, _ []byte) error {
	if id == "missing" {
		return db.ErrNotFound
	}
	f.lastSetMode = mode
	f.lastSetHeader = header
	return nil
}
func (f *fakeStore) ListAPIKeys(_ context.Context, _, _, _ string) ([]db.ServiceAPIKey, error) {
	if f.apiKeyErr != nil {
		return nil, f.apiKeyErr
	}
	return append([]db.ServiceAPIKey(nil), f.apiKeys...), nil
}
func (f *fakeStore) CreateAPIKey(_ context.Context, _, _, _, name string) (string, string, error) {
	f.lastCreatedID = "k1"
	f.lastCreatedPT = "plain-" + name
	return "k1", "plain-" + name, nil
}
func (f *fakeStore) DeleteAPIKey(_ context.Context, _, _, _, keyID string) error {
	if keyID == "missing" {
		return db.ErrNotFound
	}
	f.lastDeleted = keyID
	return nil
}
func (f *fakeStore) ListClientTokens(_ context.Context, _ string) ([]db.ClientToken, error) {
	return append([]db.ClientToken(nil), f.clientTokens...), nil
}
func (f *fakeStore) IssueClientToken(_ context.Context, _, name string) (string, error) {
	f.lastMintedPT = "burrow_plain-" + name
	return f.lastMintedPT, nil
}
func (f *fakeStore) ListUsers(_ context.Context, _ string, _, _ int) ([]db.User, int, error) {
	return append([]db.User(nil), f.users...), f.userTotal, nil
}
func (f *fakeStore) SearchAuditEvents(_ context.Context, _ db.AuditQuery) ([]db.AuditEvent, error) {
	return append([]db.AuditEvent(nil), f.audit...), nil
}
func (f *fakeStore) MetricsText() (string, error) {
	return f.metricsBody, nil
}

// ---------- bootstrap ------------------------------------------------------

// newTestServer wires the trio (bearer, users, store) and returns an
// httptest.Server backed by the mcpserv handler. plain is the bearer
// plaintext for the test token; perms is its declared permission set; role
// is the user's current role.
func newTestServer(t *testing.T, perms []string, role string) (*httptest.Server, string, *fakeStore) {
	t.Helper()
	const plaintext = "bua_testtoken1234567890"
	const uid = "u1"

	br := newFakeBearer()
	br.put(plaintext, TokenInfo{ID: "tok1", UserID: uid, Permissions: perms})
	us := &fakeUsers{users: map[string]db.User{
		uid: {ID: uid, Email: "ci@x", Role: role, Status: "active"},
	}}
	fs := &fakeStore{}
	srv := New(br, us, fs, nil)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return hs, plaintext, fs
}

// rpcCall issues a JSON-RPC request and returns the raw response body.
func rpcCall(t *testing.T, baseURL, bearer string, body any) (int, []byte) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", baseURL, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// decodeRPC parses a JSON-RPC envelope into id/result/error fields.
type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func decodeRPC(t *testing.T, body []byte) rpcEnvelope {
	t.Helper()
	var env rpcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc field wrong: %q (body=%s)", env.JSONRPC, body)
	}
	return env
}

// ---------- Step 1 tests: tools/list, tools/call, forbidden ---------------

func TestToolsList_ReturnsClosedInventory(t *testing.T) {
	srv, tok, _ := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var lr listResult
	if err := json.Unmarshal(env.Result, &lr); err != nil {
		t.Fatal(err)
	}
	if len(lr.Tools) != len(orderedTools) {
		t.Fatalf("got %d tools, want %d (%v)", len(lr.Tools), len(orderedTools), namesOf(lr.Tools))
	}
	gotNames := namesOf(lr.Tools)
	wantSet := map[string]bool{}
	for _, n := range orderedTools {
		wantSet[n] = true
	}
	for _, n := range gotNames {
		if !wantSet[n] {
			t.Errorf("unexpected tool in inventory: %q", n)
		}
		delete(wantSet, n)
	}
	for n := range wantSet {
		t.Errorf("missing tool: %q", n)
	}
	for _, td := range lr.Tools {
		if len(td.ParametersSchema) == 0 {
			t.Errorf("tool %q has empty parameters_schema", td.Name)
		}
		if td.Permission == "" {
			t.Errorf("tool %q has empty permission", td.Name)
		}
		if td.Description == "" {
			t.Errorf("tool %q has empty description", td.Name)
		}
	}
}

func TestToolsCall_TunnelsList_ReturnsStoreData(t *testing.T) {
	srv, tok, fs := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	fs.tunnels = []TunnelInfo{
		{ID: "tun-a", Name: "alpha", Type: "http", LocalAddr: "127.0.0.1:8080", Connected: true},
		{ID: "tun-b", Name: "beta", Type: "tcp", RemotePort: 2222, Connected: false},
	}
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": "abc", "method": "tools/call",
		"params": map[string]any{
			"name":      "tunnels.list",
			"arguments": map[string]any{},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var rows []TunnelInfo
	if err := json.Unmarshal(env.Result, &rows); err != nil {
		t.Fatalf("decode result: %v body=%s", err, body)
	}
	if len(rows) != 2 || rows[0].ID != "tun-a" {
		t.Fatalf("rows mismatch: %+v", rows)
	}
}

func TestToolsCall_Forbidden_WhenTokenLacksPermission(t *testing.T) {
	// Token declares only services:configure:own — admin role gives it the
	// :any reach, but the token does NOT carry tunnels:read:any, so
	// tunnels.list must fail with -32603 forbidden.
	srv, tok, _ := newTestServer(t, []string{"services:configure:own"}, "admin")
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "tunnels.list",
			"arguments": map[string]any{},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("HTTP status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error == nil {
		t.Fatalf("want JSON-RPC error, got result=%s", env.Result)
	}
	if env.Error.Code != -32603 {
		t.Errorf("error.code=%d want -32603", env.Error.Code)
	}
	if env.Error.Message != "forbidden" {
		t.Errorf("error.message=%q want \"forbidden\"", env.Error.Message)
	}
}

// ---------- additional coverage required by Task 23 -----------------------

func TestUnknownMethod_ReturnsMethodNotFound(t *testing.T) {
	srv, tok, _ := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "nope/wat",
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error == nil || env.Error.Code != -32601 {
		t.Fatalf("want -32601 error, got %+v", env.Error)
	}
}

func TestMalformedJSON_ReturnsParseError(t *testing.T) {
	srv, tok, _ := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	env := decodeRPC(t, body)
	if env.Error == nil || env.Error.Code != -32700 {
		t.Fatalf("want -32700 parse error, got %+v body=%s", env.Error, body)
	}
}

func TestMissingBearer_Returns401(t *testing.T) {
	srv, _, _ := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	status, _ := rpcCall(t, srv.URL, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", status)
	}
}

func TestInvalidBearer_Returns401(t *testing.T) {
	srv, _, _ := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	status, _ := rpcCall(t, srv.URL, "bua_wrongtoken12345", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", status)
	}
}

func TestNonBuaBearer_Returns401(t *testing.T) {
	srv, _, _ := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	status, _ := rpcCall(t, srv.URL, "garbage_not_bua", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", status)
	}
}

func TestExpiredBearer_Returns401(t *testing.T) {
	const plaintext = "bua_expiredtoken12345"
	expired := time.Now().UTC().Add(-time.Hour)
	br := newFakeBearer()
	br.put(plaintext, TokenInfo{
		ID: "tokE", UserID: "u1",
		Permissions: []string{"tunnels:read:any"},
		ExpiresAt:   &expired,
	})
	us := &fakeUsers{users: map[string]db.User{"u1": {ID: "u1", Role: "admin", Status: "active"}}}
	srv := httptest.NewServer(New(br, us, &fakeStore{}, nil))
	t.Cleanup(srv.Close)
	status, _ := rpcCall(t, srv.URL, plaintext, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", status)
	}
}

func TestSuspendedUser_Returns401(t *testing.T) {
	const plaintext = "bua_okay123456789012345"
	br := newFakeBearer()
	br.put(plaintext, TokenInfo{ID: "tok1", UserID: "u1", Permissions: []string{"tunnels:read:any"}})
	us := &fakeUsers{users: map[string]db.User{"u1": {ID: "u1", Role: "admin", Status: "suspended"}}}
	srv := httptest.NewServer(New(br, us, &fakeStore{}, nil))
	t.Cleanup(srv.Close)
	status, _ := rpcCall(t, srv.URL, plaintext, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", status)
	}
}

func TestNonPOST_Returns405(t *testing.T) {
	srv, _, _ := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", resp.StatusCode)
	}
}

func TestRoleDemotionImmediatelyRevokesReach(t *testing.T) {
	// Token declares tunnels:read:any, but the user's CURRENT role is "user"
	// — which does NOT grant tunnels:read:any. Effective perms = ∅ → -32603.
	srv, tok, _ := newTestServer(t, []string{"tunnels:read:any"}, "user")
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "tunnels.list", "arguments": map[string]any{}},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error == nil || env.Error.Code != -32603 || env.Error.Message != "forbidden" {
		t.Fatalf("want -32603 forbidden, got %+v", env.Error)
	}
}

func TestUnknownTool_ReturnsInvalidParams(t *testing.T) {
	srv, tok, _ := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "no.such.tool"},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error == nil || env.Error.Code != -32602 {
		t.Fatalf("want -32602, got %+v", env.Error)
	}
}

func TestInvalidJSONRPCVersion(t *testing.T) {
	srv, tok, _ := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "1.0", "id": 1, "method": "tools/list",
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error == nil || env.Error.Code != -32600 {
		t.Fatalf("want -32600, got %+v", env.Error)
	}
}

func TestTunnelsFilter(t *testing.T) {
	srv, tok, fs := newTestServer(t, []string{"tunnels:read:any"}, "admin")
	fs.tunnels = []TunnelInfo{
		{ID: "1", Name: "alpha-prod"},
		{ID: "2", Name: "beta-dev"},
		{ID: "3", Name: "Alpha-staging"},
	}
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "tunnels.list",
			"arguments": map[string]any{"filter": "alpha"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var rows []TunnelInfo
	if err := json.Unmarshal(env.Result, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("filter mismatch: %+v", rows)
	}
}

func TestMetricsSnapshot(t *testing.T) {
	srv, tok, fs := newTestServer(t, []string{"metrics:read"}, "admin")
	fs.metricsBody = "# HELP burrow_x demo\nburrow_x 1\n"
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "metrics.snapshot"},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var out struct {
		Format string `json:"format"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Format != "prometheus-0.0.4" || !strings.Contains(out.Body, "burrow_x") {
		t.Fatalf("metrics snapshot mismatch: %+v", out)
	}
}

func TestMintTokenAndAPIKey(t *testing.T) {
	// User-role caller mints a client token + creates a service API key.
	// Both perms are :own, so user role suffices.
	srv, tok, fs := newTestServer(t, []string{
		"tokens:manage:own", "services:configure:own",
	}, "user")
	// tokens.mint
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "tokens.mint", "arguments": map[string]any{"name": "ci"}},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error != nil {
		t.Fatalf("mint err: %+v", env.Error)
	}
	var mintOut struct {
		Name, Token string
	}
	json.Unmarshal(env.Result, &mintOut)
	if mintOut.Token != fs.lastMintedPT {
		t.Errorf("mint plaintext mismatch: got %q want %q", mintOut.Token, fs.lastMintedPT)
	}

	// api_keys.create
	status2, body2 := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "api_keys.create",
			"arguments": map[string]any{"service_id": "svc1", "name": "prod"},
		},
	})
	if status2 != http.StatusOK {
		t.Fatalf("status=%d body=%s", status2, body2)
	}
	env2 := decodeRPC(t, body2)
	if env2.Error != nil {
		t.Fatalf("create-apikey err: %+v", env2.Error)
	}
	var ck struct {
		ID, Name, Key string
	}
	if err := json.Unmarshal(env2.Result, &ck); err != nil {
		t.Fatal(err)
	}
	if ck.Key == "" || ck.ID != "k1" {
		t.Errorf("api_keys.create return mismatch: %+v", ck)
	}
}

func TestServerToolsListMatchesRegistry(t *testing.T) {
	srv := New(nil, nil, &fakeStore{}, nil)
	descs := srv.Tools()
	if len(descs) != len(orderedTools) {
		t.Fatalf("Tools() returned %d, want %d", len(descs), len(orderedTools))
	}
}

func TestServerWithNilStore_ToolsListStillWorks(t *testing.T) {
	// Confirm tools/list works even with a nil store (degraded dashboard
	// mode) — only tools/call would fail.
	br := newFakeBearer()
	const pt = "bua_listonlytoken12345"
	br.put(pt, TokenInfo{ID: "t", UserID: "u", Permissions: []string{"tunnels:read:any"}})
	us := &fakeUsers{users: map[string]db.User{"u": {ID: "u", Role: "admin", Status: "active"}}}
	srv := httptest.NewServer(New(br, us, nil, nil))
	t.Cleanup(srv.Close)
	status, body := rpcCall(t, srv.URL, pt, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error != nil {
		t.Fatalf("unexpected err: %+v", env.Error)
	}
}

// ---------- small helper ---------------------------------------------------

func namesOf(td []ToolDescriptor) []string {
	out := make([]string, 0, len(td))
	for _, t := range td {
		out = append(out, t.Name)
	}
	return out
}

// The backing row of a direct AI provider is not a service anyone tunnels;
// REST hides it from the services list and so does the MCP tool.
func TestToolsCall_ServicesList_HidesDirectBackingRows(t *testing.T) {
	srv, tok, fs := newTestServer(t, []string{"services:configure:own"}, "admin")
	fs.services = []store.ServiceView{
		{ID: "svc-a", Name: "alpha", Type: "http", Subdomain: "alpha"},
		{ID: "svc-direct", Name: "openrouter", Type: "direct", Subdomain: "openrouter"},
		{ID: "svc-b", Name: "beta", Type: "tcp"},
	}
	status, body := rpcCall(t, srv.URL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "services.list",
			"arguments": map[string]any{},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	env := decodeRPC(t, body)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var rows []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(env.Result, &rows); err != nil {
		t.Fatalf("decode result: %v body=%s", err, body)
	}
	if len(rows) != 2 || rows[0].ID != "svc-a" || rows[1].ID != "svc-b" {
		t.Fatalf("rows = %+v, want svc-a and svc-b only", rows)
	}
}
