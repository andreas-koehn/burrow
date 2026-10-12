package mcpserv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// ---------- fakes ----------------------------------------------------------

// fakeBearer satisfies BearerStore.
type fakeBearer struct {
	mu      sync.Mutex
	byHash  map[string]TokenInfo
	err     error    // returned by LookupBearer when set
	touched []string // ids passed to TouchBearer
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
	if f.err != nil {
		return TokenInfo{}, f.err
	}
	info, ok := f.byHash[hash]
	if !ok {
		return TokenInfo{}, db.ErrNotFound
	}
	return info, nil
}

func (f *fakeBearer) touchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.touched)
}

func (f *fakeBearer) TouchBearer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, id)
	return nil
}

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
	setModeErr    error
	tunnelCalls   atomic.Int64
	clientTokens  []db.ClientToken
	users         []db.User
	userTotal     int
	audit         []db.AuditEvent
	metricsBody   string

	// counters / capture
	setModeCalls  int
	lastSetMode   string
	lastSetHeader string
	lastAuditQ    db.AuditQuery
	lastUsersPage [2]int // limit, offset
	// lastLC is the audit context a write saw on its ctx.
	lastLC        audit.LogContext
	lastCreatedID string
	lastCreatedPT string
	lastDeleted   string
	lastMintedPT  string
}

func (f *fakeStore) ListUserTunnels(_ context.Context, _, _ string) ([]TunnelInfo, error) {
	f.tunnelCalls.Add(1)
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
func (f *fakeStore) SetServiceAccessMode(ctx context.Context, _, _, id, mode, header string, _ []byte) error {
	f.setModeCalls++
	f.lastLC = audit.LogContextFrom(ctx)
	if id == "missing" {
		return db.ErrNotFound
	}
	if f.setModeErr != nil {
		return f.setModeErr
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
func (f *fakeStore) IssueClientToken(ctx context.Context, _, name string) (string, error) {
	f.lastLC = audit.LogContextFrom(ctx)
	f.lastMintedPT = "burrow_plain-" + name
	return f.lastMintedPT, nil
}
func (f *fakeStore) ListUsers(_ context.Context, _ string, limit, offset int) ([]db.User, int, error) {
	f.lastUsersPage = [2]int{limit, offset}
	return append([]db.User(nil), f.users...), f.userTotal, nil
}

// SearchAuditEvents reads the limit as db.ListAuditEvents does: zero or
// below is no limit at all.
func (f *fakeStore) SearchAuditEvents(_ context.Context, q db.AuditQuery) ([]db.AuditEvent, error) {
	f.lastAuditQ = q
	rows := append([]db.AuditEvent(nil), f.audit...)
	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return rows, nil
}
func (f *fakeStore) MetricsText() (string, error) {
	return f.metricsBody, nil
}

// ---------- bootstrap ------------------------------------------------------

const (
	testBearer = "bua_testtoken1234567890"
	testUserID = "u1"
)

// testEnv is one Server behind httptest with its fakes.
type testEnv struct {
	url    string
	srv    *Server
	bearer *fakeBearer
	users  *fakeUsers
	store  *fakeStore
	audit  *auditSink
	logs   *lockedBuffer
}

// newTestEnv serves the whole handler: origin check, bearer auth, rate
// limit, MCP. testBearer is a token of user u1 with the given permissions;
// role is that user's current role.
func newTestEnv(t *testing.T, perms []string, role string, opts ...Option) *testEnv {
	t.Helper()
	env := &testEnv{
		bearer: newFakeBearer(),
		users: &fakeUsers{users: map[string]db.User{
			testUserID: {ID: testUserID, Email: "ci@x", Role: role, Status: "active"},
		}},
		store: &fakeStore{},
		audit: &auditSink{},
		logs:  &lockedBuffer{},
	}
	env.bearer.put(testBearer, TokenInfo{ID: "tok1", UserID: testUserID, Permissions: perms})
	log := slog.New(slog.NewTextHandler(env.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := New(env.bearer, env.users, env.store, log, opts...)
	srv.SetAuditAppender(env.audit)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	env.url, env.srv = hs.URL, srv
	return env
}

// lockedBuffer is a log sink the handler's goroutines and the test share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

var legacyHeader = map[string]string{"MCP-Protocol-Version": "2025-06-18"}

// ---------- the whole path, both revisions ---------------------------------

func TestHTTP_EveryRevision_WithBearer(t *testing.T) {
	for _, rev := range testRevisions {
		t.Run(rev, func(t *testing.T) {
			env := newTestEnv(t, []string{"tunnels:read:own", "metrics:read"}, "admin")
			env.store.tunnels = []TunnelInfo{{ID: "tun-a", Name: "alpha", Type: "http"}}
			env.store.metricsBody = "# HELP burrow_x demo\nburrow_x 1\n"
			cs := connectMCP(t, env.url, testBearer, rev)

			if got := sortedNames(listedTools(t, cs)); got != "metrics_snapshot,tunnels_list" {
				t.Fatalf("tools = %s", got)
			}
			res, err := callTool(t, cs, "tunnels_list", nil)
			if err != nil || res.IsError {
				t.Fatalf("tunnels_list: err=%v result=%+v", err, res)
			}
			if !strings.Contains(resultText(t, res), `"id":"tun-a"`) {
				t.Errorf("tunnels_list = %s", resultText(t, res))
			}
			res, err = callTool(t, cs, "metrics_snapshot", nil)
			if err != nil || res.IsError || !strings.Contains(resultText(t, res), "burrow_x 1") {
				t.Fatalf("metrics_snapshot: err=%v result=%+v", err, res)
			}
			if len(env.bearer.touched) == 0 || env.bearer.touched[0] != "tok1" {
				t.Errorf("last_used was not touched: %v", env.bearer.touched)
			}
		})
	}
}

// ---------- authentication -------------------------------------------------

func TestHTTP_Unauthenticated_401WithChallenge(t *testing.T) {
	expired := time.Now().UTC().Add(-time.Hour)
	env := newTestEnv(t, []string{"tunnels:read:own"}, "admin")
	env.bearer.put("bua_expiredtoken12345", TokenInfo{ID: "tokE", UserID: testUserID,
		Permissions: []string{"tunnels:read:own"}, ExpiresAt: &expired})
	env.bearer.put("bua_suspendeduser1234", TokenInfo{ID: "tokS", UserID: "u-susp", Permissions: []string{"tunnels:read:own"}})
	env.users.users["u-susp"] = db.User{ID: "u-susp", Role: "admin", Status: "suspended"}
	env.bearer.put("bua_deleteduser123456", TokenInfo{ID: "tokD", UserID: "u-gone", Permissions: []string{"tunnels:read:own"}})

	cases := map[string]string{
		"no header":             "",
		"unknown token":         "bua_wrongtoken12345",
		"not an automation key": "garbage_not_bua",
		"expired token":         "bua_expiredtoken12345",
		"suspended user":        "bua_suspendeduser1234",
		"deleted user":          "bua_deleteduser123456",
	}
	for name, bearer := range cases {
		resp, body := rawPOST(t, env.url, bearer, legacyHeader, rawToolsList)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401 (%s)", name, resp.StatusCode, body)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("%s: WWW-Authenticate %q, want Bearer", name, got)
		}
		// One answer for all of them: nothing tells a valid token from a wrong one.
		if strings.TrimSpace(body) != `{"error":"invalid bearer token"}` {
			t.Errorf("%s: body %s", name, body)
		}
	}
	// A valid expiry in the future still works.
	later := time.Now().UTC().Add(time.Hour)
	env.bearer.put("bua_validuntillater12", TokenInfo{ID: "tokL", UserID: testUserID,
		Permissions: []string{"tunnels:read:own"}, ExpiresAt: &later})
	if resp, body := rawPOST(t, env.url, "bua_validuntillater12", legacyHeader, rawToolsList); resp.StatusCode != http.StatusOK {
		t.Errorf("token with a later expiry: %d %s", resp.StatusCode, body)
	}
	// The SDK's client reports the refusal instead of connecting.
	for _, rev := range []string{"2026-07-28", "2025-11-25"} {
		if _, err := dialMCP(env.url, "bua_wrongtoken12345", rev); err == nil {
			t.Errorf("%s: client connected with a wrong token", rev)
		}
	}
	// A GET is authenticated too, before the SDK says 405.
	resp, err := http.Get(env.url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET without a token: %d, want 401", resp.StatusCode)
	}
}

func TestHTTP_TokenStoreFails_500(t *testing.T) {
	env := newTestEnv(t, []string{"tunnels:read:own"}, "admin")
	env.bearer.err = errors.New("database is locked")
	resp, body := rawPOST(t, env.url, testBearer, legacyHeader, rawToolsList)
	if resp.StatusCode != http.StatusInternalServerError || strings.Contains(body, "database") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	// A server built without stores serves nobody.
	hs := httptest.NewServer(New(nil, nil, &fakeStore{}, quietLog()))
	t.Cleanup(hs.Close)
	if resp, _ := rawPOST(t, hs.URL, testBearer, legacyHeader, rawToolsList); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("no stores: %d, want 500", resp.StatusCode)
	}
}

// The role is read on every request: demoting the user who minted a token
// narrows the token at once.
func TestHTTP_MinterDemoted_TokenNarrowsAtOnce(t *testing.T) {
	env := newTestEnv(t, []string{"users:read", "services:configure:own"}, "admin")
	cs := connectMCP(t, env.url, testBearer, "2026-07-28")
	if got := len(listedTools(t, cs)); got != 7 { // users_list + the six services:configure:own tools
		t.Fatalf("as admin: %d tools", got)
	}

	u := env.users.users[testUserID]
	u.Role = "user"
	env.users.users[testUserID] = u

	tools := listedTools(t, cs)
	if _, still := tools["users_list"]; still || len(tools) != 6 {
		t.Errorf("after the demotion: %s", sortedNames(tools))
	}
	_, err := callTool(t, cs, "users_list", nil)
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("call after the demotion: %v", err)
	}
	rows := env.audit.all()
	if len(rows) != 1 || rows[0].SubjectLabel != "users_list" || rows[0].Result != "denied" {
		t.Errorf("audit rows = %+v", rows)
	}
}

// ---------- limits ---------------------------------------------------------

func TestHTTP_BodyOverOneMiB_413(t *testing.T) {
	env := newTestEnv(t, []string{"tunnels:read:own"}, "admin")
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tunnels_list","arguments":{"filter":"` +
		strings.Repeat("a", MaxBodyBytes) + `"}}}`
	resp, _ := rawPOST(t, env.url, testBearer, legacyHeader, big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
	// Just under the limit is served.
	ok := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tunnels_list","arguments":{"filter":"` +
		strings.Repeat("a", MaxBodyBytes-200) + `"}}}`
	if resp, body := rawPOST(t, env.url, testBearer, legacyHeader, ok); resp.StatusCode != http.StatusOK {
		t.Fatalf("under the limit: %d %s", resp.StatusCode, body[:min(len(body), 200)])
	}
}

func TestHTTP_RateLimit_PerToken(t *testing.T) {
	env := newTestEnv(t, []string{"tunnels:read:own"}, "admin", WithRateLimit(3, time.Minute))
	env.bearer.put("bua_secondtoken123456", TokenInfo{ID: "tok2", UserID: testUserID, Permissions: []string{"tunnels:read:own"}})

	for i := 1; i <= 3; i++ {
		if resp, body := rawPOST(t, env.url, testBearer, legacyHeader, rawToolsList); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, resp.StatusCode, body)
		}
	}
	resp, body := rawPOST(t, env.url, testBearer, legacyHeader, rawToolsList)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("4th request: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" || strings.TrimSpace(body) != `{"error":"rate limit exceeded"}` {
		t.Errorf("429 answer: Retry-After=%q body=%s", resp.Header.Get("Retry-After"), body)
	}
	// The budget belongs to the token, not to the user or the address.
	if resp, _ := rawPOST(t, env.url, "bua_secondtoken123456", legacyHeader, rawToolsList); resp.StatusCode != http.StatusOK {
		t.Errorf("second token of the same user: %d", resp.StatusCode)
	}
	// Requests without a valid token do not use anybody's budget.
	if resp, _ := rawPOST(t, env.url, "bua_wrongtoken12345", legacyHeader, rawToolsList); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
}

// A request over the limit costs nothing: no last_used write, no audit row,
// no tool run.
func TestHTTP_OverTheLimit_NoTouchNoAuditNoTool(t *testing.T) {
	env := newTestEnv(t, []string{"tunnels:read:own"}, "admin", WithRateLimit(2, time.Minute))
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tunnels_list","arguments":{}}}`
	for i := 1; i <= 2; i++ {
		if resp, body := rawPOST(t, env.url, testBearer, legacyHeader, call); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, resp.StatusCode, body)
		}
	}
	touches, rows, calls := env.bearer.touchCount(), len(env.audit.all()), env.store.tunnelCalls.Load()
	if touches != 2 || rows != 2 || calls != 2 {
		t.Fatalf("within the limit: touches=%d audit rows=%d tool runs=%d, want 2 each", touches, rows, calls)
	}
	resp, _ := rawPOST(t, env.url, testBearer, legacyHeader, call)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd request: %d, want 429", resp.StatusCode)
	}
	if got := env.bearer.touchCount(); got != touches {
		t.Errorf("a 429 touched last_used: %d -> %d", touches, got)
	}
	if got := len(env.audit.all()); got != rows {
		t.Errorf("a 429 wrote an audit row: %d -> %d", rows, got)
	}
	if got := env.store.tunnelCalls.Load(); got != calls {
		t.Errorf("a 429 ran the tool: %d -> %d", calls, got)
	}
}

func TestHTTP_DefaultRateLimit(t *testing.T) {
	s := New(nil, nil, nil, quietLog())
	if s.rateLimit != DefaultRateLimit || s.rateWindow != DefaultRateWindow || DefaultRateLimit != 300 || DefaultRateWindow != time.Minute {
		t.Fatalf("default limit %d per %s", s.rateLimit, s.rateWindow)
	}
}

func TestHTTP_OnlyPOST(t *testing.T) {
	env := newTestEnv(t, []string{"tunnels:read:own"}, "admin")
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		req, _ := http.NewRequest(method, env.url, nil)
		req.Header.Set("Authorization", "Bearer "+testBearer)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
			t.Errorf("%s: %d Allow=%q, want 405 POST", method, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
}

// ---------- browsers -------------------------------------------------------

// A page on another origin must not be able to drive the endpoint, token or
// no token. A client without Origin (every CLI client) is served.
func TestHTTP_CrossOrigin(t *testing.T) {
	env := newTestEnv(t, []string{"tunnels:read:own"}, "admin")
	self := env.url // http://127.0.0.1:<port>
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no Origin (CLI client)", nil, 200},
		{"same origin", map[string]string{"Origin": self}, 200},
		{"same origin, fetch metadata", map[string]string{"Origin": self, "Sec-Fetch-Site": "same-origin"}, 200},
		{"other origin", map[string]string{"Origin": "https://evil.example"}, 403},
		{"other origin, fetch metadata", map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, 403},
		{"sibling subdomain", map[string]string{"Origin": "http://app.127.0.0.1", "Sec-Fetch-Site": "same-site"}, 403},
		{"sandboxed frame", map[string]string{"Origin": "null"}, 403},
		{"cross-site without Origin", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
	}
	for _, c := range cases {
		hdr := map[string]string{"MCP-Protocol-Version": "2025-06-18"}
		for k, v := range c.hdr {
			hdr[k] = v
		}
		resp, body := rawPOST(t, env.url, testBearer, hdr, rawToolsList)
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, resp.StatusCode, c.want, strings.TrimSpace(body))
		}
		if c.want == 403 && strings.TrimSpace(body) != `{"error":"cross-origin request refused"}` {
			t.Errorf("%s: body %s", c.name, body)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Origin %q", c.name, got)
		}
	}

	// Refused before the token is looked at.
	before := len(env.bearer.touched)
	resp, _ := rawPOST(t, env.url, testBearer, map[string]string{"Origin": "https://evil.example"}, rawToolsList)
	if resp.StatusCode != http.StatusForbidden || len(env.bearer.touched) != before {
		t.Errorf("cross-origin request reached the token store: status %d", resp.StatusCode)
	}

	// A preflight gets no CORS header, so the browser stops there.
	req, _ := http.NewRequest(http.MethodOptions, env.url, nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	pre, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	pre.Body.Close()
	if pre.StatusCode < 400 || pre.Header.Get("Access-Control-Allow-Origin") != "" || pre.Header.Get("Access-Control-Allow-Headers") != "" {
		t.Errorf("preflight: %d %v", pre.StatusCode, pre.Header)
	}
}

// A relay behind a reverse proxy on the same machine is reached on a
// loopback address with the public name in Host. The SDK's default
// DNS-rebinding check would answer 403 to every such request.
func TestHTTP_BehindALocalReverseProxy(t *testing.T) {
	env := newTestEnv(t, []string{"tunnels:read:own"}, "admin")
	req, _ := http.NewRequest(http.MethodPost, env.url, strings.NewReader(rawToolsList))
	req.Host = "burrow.example.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+testBearer)
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}

// ---------- secrets --------------------------------------------------------

// Nothing secret reaches the log or the audit trail: not the bearer token,
// not a key or token a tool minted. The session below covers a call that
// works, one that fails, one that is refused, and a rejected login.
func TestHTTP_SecretsStayOutOfLogsAndAudit(t *testing.T) {
	env := newTestEnv(t, []string{"services:configure:own", "tokens:manage:own"}, "admin")
	cs := connectMCP(t, env.url, testBearer, "2025-11-25")

	if res, err := callTool(t, cs, "api_keys_create", map[string]any{"service_id": "svc1", "name": "prod"}); err != nil || res.IsError {
		t.Fatalf("api_keys_create: %v %+v", err, res)
	}
	if res, err := callTool(t, cs, "tokens_mint", map[string]any{"name": "ci"}); err != nil || res.IsError {
		t.Fatalf("tokens_mint: %v %+v", err, res)
	}
	if res, err := callTool(t, cs, "services_get", map[string]any{"id": "nope"}); err != nil || !res.IsError {
		t.Fatalf("services_get: %v %+v", err, res)
	}
	_, _ = callTool(t, cs, "users_list", nil) // not this token's tool
	rawPOST(t, env.url, "bua_wrongtoken12345", legacyHeader, rawToolsList)

	rows, _ := json.Marshal(env.audit.all())
	logs := env.logs.String()
	if !strings.Contains(logs, "services_get") {
		t.Fatalf("the failed call was not logged; the test would prove nothing: %q", logs)
	}
	for name, secret := range map[string]string{
		"bearer token":        testBearer,
		"rejected token":      "bua_wrongtoken12345",
		"minted service key":  env.store.lastCreatedPT,
		"minted client token": env.store.lastMintedPT,
	} {
		if secret == "" {
			t.Fatalf("%s is empty", name)
		}
		if strings.Contains(logs, secret) {
			t.Errorf("the log carries the %s", name)
		}
		if strings.Contains(string(rows), secret) {
			t.Errorf("the audit trail carries the %s", name)
		}
	}
	var results []string
	for _, r := range env.audit.all() {
		if r.Action != audit.ActionMCPToolCall {
			t.Errorf("audit action %q", r.Action)
		}
		results = append(results, r.SubjectLabel+"="+r.Result)
	}
	if got, want := strings.Join(results, ","), "api_keys_create=ok,tokens_mint=ok,services_get=error,users_list=denied"; got != want {
		t.Errorf("audit rows %s, want %s", got, want)
	}
}

// The backing row of a direct AI provider is not a service anyone tunnels;
// REST hides it from the services list and so does the MCP tool.
func TestHTTP_ServicesList_HidesDirectBackingRows(t *testing.T) {
	env := newTestEnv(t, []string{"services:configure:own"}, "admin")
	env.store.services = []store.ServiceView{
		{ID: "svc-a", Name: "alpha", Type: "http", Subdomain: "alpha"},
		{ID: "svc-direct", Name: "openrouter", Type: "direct", Subdomain: "openrouter"},
		{ID: "svc-b", Name: "beta", Type: "tcp"},
	}
	res, err := callTool(t, connectMCP(t, env.url, testBearer, "2026-07-28"), "services_list", nil)
	if err != nil || res.IsError {
		t.Fatalf("services_list: %v %+v", err, res)
	}
	var out struct {
		Services []struct {
			ID string `json:"id"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Services) != 2 || out.Services[0].ID != "svc-a" || out.Services[1].ID != "svc-b" {
		t.Fatalf("services = %+v, want svc-a and svc-b only", out.Services)
	}
}

// ---------- the audit trail of a write -------------------------------------

const testUserAgent = "burrow-test-agent/1"

// agentTransport sends what a client behind a forging hop would: the bearer,
// a User-Agent, and a forwarding header this package must not read (the
// router's TrustedProxyMiddleware alone decides the client address).
type agentTransport struct{ token string }

func (a agentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+a.token)
	r.Header.Set("User-Agent", testUserAgent)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	return http.DefaultTransport.RoundTrip(r)
}

// What the JSON API puts on a request for the store's audit rows (who, from
// where, with which client, in which request) travels through the SDK to the
// tool's store call, on every revision, and the mcp.tool.call row carries the
// same.
func TestHTTP_AuditContext_ReachesTheStoreAndTheRow(t *testing.T) {
	for _, rev := range testRevisions {
		t.Run(rev, func(t *testing.T) {
			env := newTestEnv(t, []string{"tokens:manage:own", "services:configure:own"}, "admin")
			// Behind the request-id middleware, as on the API router.
			hs := httptest.NewServer(middleware.RequestID(env.srv))
			t.Cleanup(hs.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "burrow-test", Version: "0"}, nil).Connect(ctx,
				&mcp.StreamableClientTransport{
					Endpoint:             hs.URL,
					HTTPClient:           &http.Client{Transport: agentTransport{token: testBearer}},
					DisableStandaloneSSE: true,
					MaxRetries:           -1,
				}, &mcp.ClientSessionOptions{ProtocolVersion: rev})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(func() { _ = cs.Close() })

			check := func(what string, lc audit.LogContext) {
				t.Helper()
				if lc.ActorID != testUserID || lc.ActorEmail != "ci@x" {
					t.Errorf("%s: actor %q <%s>, want %s <ci@x>", what, lc.ActorID, lc.ActorEmail, testUserID)
				}
				if lc.SourceIP != "127.0.0.1" {
					t.Errorf("%s: source ip %q, want the TCP peer 127.0.0.1", what, lc.SourceIP)
				}
				if lc.UserAgent != testUserAgent {
					t.Errorf("%s: user agent %q, want %q", what, lc.UserAgent, testUserAgent)
				}
				if lc.RequestID == "" {
					t.Errorf("%s: no request id", what)
				}
			}

			if res, err := callTool(t, cs, "tokens_mint", map[string]any{"name": "ci"}); err != nil || res.IsError {
				t.Fatalf("tokens_mint: %v %+v", err, res)
			}
			check("the store call of tokens_mint", env.store.lastLC)
			env.store.lastLC = audit.LogContext{}
			if res, err := callTool(t, cs, "services_set_access_mode", map[string]any{"id": "svc1", "mode": "open"}); err != nil || res.IsError {
				t.Fatalf("services_set_access_mode: %v %+v", err, res)
			}
			check("the store call of services_set_access_mode", env.store.lastLC)
			_, _ = callTool(t, cs, "users_list", nil) // not this token's tool

			rows := env.audit.all()
			var got []string
			for _, r := range rows {
				got = append(got, r.SubjectLabel+"="+r.Result)
				check("the "+r.SubjectLabel+" row", audit.LogContext{
					ActorID: r.ActorID, ActorEmail: r.ActorEmail,
					SourceIP: r.SourceIP, UserAgent: r.UserAgent, RequestID: r.RequestID,
				})
			}
			if want := "tokens_mint=ok,services_set_access_mode=ok,users_list=denied"; strings.Join(got, ",") != want {
				t.Fatalf("audit rows %v, want %s", got, want)
			}
			// Each call is a request of its own.
			if rows[0].RequestID == rows[1].RequestID {
				t.Errorf("two calls share the request id %q", rows[0].RequestID)
			}
			raw, _ := json.Marshal(rows)
			for name, secret := range map[string]string{"bearer token": testBearer, "minted token": env.store.lastMintedPT} {
				if secret == "" || strings.Contains(string(raw), secret) {
					t.Errorf("the audit rows carry the %s (or it is empty): %s", name, raw)
				}
			}
		})
	}
}

// Without the request-id middleware and without an email on the user the
// row still names the actor; nothing is invented.
func TestHTTP_AuditRow_WithoutRequestID(t *testing.T) {
	env := newTestEnv(t, []string{"tokens:manage:own"}, "admin")
	cs := connectMCP(t, env.url, testBearer, "2026-07-28")
	if res, err := callTool(t, cs, "tokens_mint", map[string]any{"name": "ci"}); err != nil || res.IsError {
		t.Fatalf("tokens_mint: %v %+v", err, res)
	}
	rows := env.audit.all()
	if len(rows) != 1 || rows[0].ActorID != testUserID || rows[0].ActorEmail != "ci@x" ||
		rows[0].SourceIP != "127.0.0.1" || rows[0].RequestID != "" {
		t.Fatalf("audit rows = %+v", rows)
	}
}

// Through the whole handler: input the REST route refuses is a result the
// model can read, and the value reaches neither the answer nor the log nor
// the store.
func TestHTTP_SetAccessMode_RefusedInput_ValueStaysOut(t *testing.T) {
	env := newTestEnv(t, []string{"services:configure:own"}, "admin")
	cs := connectMCP(t, env.url, testBearer, "2025-11-25")
	res, err := callTool(t, cs, "services_set_access_mode", map[string]any{
		"id": "svc1", "mode": "api_key", "api_key_header": "X Bad: MARKERHDR\r\nInjected",
	})
	if err != nil || !res.IsError {
		t.Fatalf("err=%v result=%+v", err, res)
	}
	if got := resultText(t, res); got != "api_key_header must be a valid HTTP header name (RFC 7230 token)" {
		t.Errorf("result text %q", got)
	}
	if res, err := callTool(t, cs, "services_set_access_mode", map[string]any{"id": "svc1", "mode": "burrow_login"}); err != nil ||
		!res.IsError || resultText(t, res) != "burrow_login requires a configured auth_domain" {
		t.Errorf("burrow_login without an auth domain: err=%v result=%+v", err, res)
	}
	if env.store.setModeCalls != 0 {
		t.Errorf("refused input reached the store %d times", env.store.setModeCalls)
	}
	logs := env.logs.String()
	if !strings.Contains(logs, "services_set_access_mode") || strings.Contains(logs, "MARKERHDR") || strings.Contains(logs, "Injected") {
		t.Errorf("log: %s", logs)
	}
	var got []string
	for _, r := range env.audit.all() {
		got = append(got, r.SubjectLabel+"="+r.Result)
	}
	if want := "services_set_access_mode=error,services_set_access_mode=error"; strings.Join(got, ",") != want {
		t.Errorf("audit rows %v, want %s", got, want)
	}
}
