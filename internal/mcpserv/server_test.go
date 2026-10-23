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
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
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
	api    *fakeAPI
	audit  *auditSink
	logs   *lockedBuffer
}

// newTestEnv serves the whole handler: origin check, bearer auth, rate
// limit, MCP, with calls replayed through a fake API that answers {}.
// testBearer is a token of user u1 with the given permissions; role is that
// user's current role.
func newTestEnv(t *testing.T, perms []string, role string, opts ...Option) *testEnv {
	t.Helper()
	env := &testEnv{
		bearer: newFakeBearer(),
		users: &fakeUsers{users: map[string]db.User{
			testUserID: {ID: testUserID, Email: "ci@x", Role: role, Status: "active"},
		}},
		api:   &fakeAPI{reply: `{}`},
		audit: &auditSink{},
		logs:  &lockedBuffer{},
	}
	env.bearer.put(testBearer, TokenInfo{ID: "tok1", UserID: testUserID, Permissions: perms})
	log := slog.New(slog.NewTextHandler(env.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := newTestServer(t, env.bearer, env.users, log, opts...)
	srv.SetAPIHandler(env.api)
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
			env := newTestEnv(t, []string{"audit:read"}, "admin")
			env.api.reply = `[{"id":"ev1","action":"user.update"}]`
			cs := connectMCP(t, env.url, testBearer, rev)

			if got := sortedNames(listedTools(t, cs)); got != "audit_events_list,connection_logs_list,me_get" {
				t.Fatalf("tools = %s", got)
			}
			res, err := callTool(t, cs, "audit_events_list", nil)
			if err != nil || res.IsError {
				t.Fatalf("audit_events_list: err=%v result=%+v", err, res)
			}
			if !strings.Contains(resultText(t, res), `"id":"ev1"`) {
				t.Errorf("audit_events_list = %s", resultText(t, res))
			}
			// The API is asked with the caller's own bearer, from the caller's
			// address.
			r := env.api.seen[len(env.api.seen)-1]
			if r.Header.Get("Authorization") != "Bearer "+testBearer || !strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") {
				t.Errorf("API request: Authorization %q, RemoteAddr %q", r.Header.Get("Authorization"), r.RemoteAddr)
			}
			if env.bearer.touchCount() == 0 || env.bearer.touched[0] != "tok1" {
				t.Errorf("last_used was not touched: %v", env.bearer.touched)
			}
		})
	}
}

// ---------- authentication -------------------------------------------------

func TestHTTP_Unauthenticated_401WithChallenge(t *testing.T) {
	expired := time.Now().UTC().Add(-time.Hour)
	env := newTestEnv(t, []string{"audit:read"}, "admin")
	env.bearer.put("bua_expiredtoken12345", TokenInfo{ID: "tokE", UserID: testUserID,
		Permissions: []string{"audit:read"}, ExpiresAt: &expired})
	env.bearer.put("bua_suspendeduser1234", TokenInfo{ID: "tokS", UserID: "u-susp", Permissions: []string{"audit:read"}})
	env.users.users["u-susp"] = db.User{ID: "u-susp", Role: "admin", Status: "suspended"}
	env.bearer.put("bua_deleteduser123456", TokenInfo{ID: "tokD", UserID: "u-gone", Permissions: []string{"audit:read"}})

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
		Permissions: []string{"audit:read"}, ExpiresAt: &later})
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
	env := newTestEnv(t, []string{"audit:read"}, "admin")
	env.bearer.err = errors.New("database is locked")
	resp, body := rawPOST(t, env.url, testBearer, legacyHeader, rawToolsList)
	if resp.StatusCode != http.StatusInternalServerError || strings.Contains(body, "database") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	// A server built without stores serves nobody.
	hs := httptest.NewServer(newTestServer(t, nil, nil, quietLog()))
	t.Cleanup(hs.Close)
	if resp, _ := rawPOST(t, hs.URL, testBearer, legacyHeader, rawToolsList); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("no stores: %d, want 500", resp.StatusCode)
	}
}

// The role is read on every request: demoting the user who minted a token
// narrows the token at once.
func TestHTTP_MinterDemoted_TokenNarrowsAtOnce(t *testing.T) {
	env := newTestEnv(t, []string{"audit:read", "services:configure:own"}, "admin")
	cs := connectMCP(t, env.url, testBearer, "2026-07-28")
	const asAdmin = "audit_events_list,connection_logs_list,me_get,service_get,services_list"
	if got := sortedNames(listedTools(t, cs)); got != asAdmin {
		t.Fatalf("as admin: %s", got)
	}

	u := env.users.users[testUserID]
	u.Role = "user"
	env.users.users[testUserID] = u

	if got := sortedNames(listedTools(t, cs)); got != "me_get,service_get,services_list" {
		t.Errorf("after the demotion: %s", got)
	}
	_, err := callTool(t, cs, "audit_events_list", nil)
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("call after the demotion: %v", err)
	}
	rows := env.audit.all()
	if len(rows) != 1 || rows[0].SubjectLabel != "audit_events_list" || rows[0].Result != "denied" {
		t.Errorf("audit rows = %+v", rows)
	}
	if env.api.calls() != 0 {
		t.Errorf("the refused call reached the API")
	}
}

// ---------- limits ---------------------------------------------------------

func TestHTTP_BodyOverOneMiB_413(t *testing.T) {
	env := newTestEnv(t, []string{"audit:read"}, "admin")
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"audit_events_list","arguments":{"action":"` +
		strings.Repeat("a", MaxBodyBytes) + `"}}}`
	resp, _ := rawPOST(t, env.url, testBearer, legacyHeader, big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
	// Just under the limit is served.
	ok := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"audit_events_list","arguments":{"action":"` +
		strings.Repeat("a", MaxBodyBytes-200) + `"}}}`
	if resp, body := rawPOST(t, env.url, testBearer, legacyHeader, ok); resp.StatusCode != http.StatusOK {
		t.Fatalf("under the limit: %d %s", resp.StatusCode, body[:min(len(body), 200)])
	}
}

func TestHTTP_RateLimit_PerToken(t *testing.T) {
	env := newTestEnv(t, []string{"audit:read"}, "admin", WithRateLimit(3, time.Minute))
	env.bearer.put("bua_secondtoken123456", TokenInfo{ID: "tok2", UserID: testUserID, Permissions: []string{"audit:read"}})

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
	env := newTestEnv(t, []string{"audit:read"}, "admin", WithRateLimit(2, time.Minute))
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"audit_events_list","arguments":{}}}`
	for i := 1; i <= 2; i++ {
		if resp, body := rawPOST(t, env.url, testBearer, legacyHeader, call); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, resp.StatusCode, body)
		}
	}
	touches, rows, calls := env.bearer.touchCount(), len(env.audit.all()), env.api.calls()
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
	if got := env.api.calls(); got != calls {
		t.Errorf("a 429 ran the tool: %d -> %d", calls, got)
	}
}

func TestHTTP_DefaultRateLimit(t *testing.T) {
	s := newTestServer(t, nil, nil, quietLog())
	if s.rateLimit != DefaultRateLimit || s.rateWindow != DefaultRateWindow || DefaultRateLimit != 300 || DefaultRateWindow != time.Minute {
		t.Fatalf("default limit %d per %s", s.rateLimit, s.rateWindow)
	}
}

func TestHTTP_OnlyPOST(t *testing.T) {
	env := newTestEnv(t, []string{"audit:read"}, "admin")
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
	env := newTestEnv(t, []string{"audit:read"}, "admin")
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
	env := newTestEnv(t, []string{"audit:read"}, "admin")
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
// not a secret the API handed out. The session below covers a call that
// works, one the API refuses, one this server refuses, one the token may not
// make, and a rejected login.
func TestHTTP_SecretsStayOutOfLogsAndAudit(t *testing.T) {
	const handedOut = "bgk_SECRET-from-the-api"
	env := newTestEnv(t, []string{"services:configure:own", "services:configure:any"}, "admin")
	cs := connectMCP(t, env.url, testBearer, "2025-11-25")

	env.api.status, env.api.reply = http.StatusOK, `{"id":"svc1","key":"`+handedOut+`"}`
	if res, err := callTool(t, cs, "service_get", map[string]any{"serviceID": "svc1"}); err != nil || res.IsError {
		t.Fatalf("service_get: %v %+v", err, res)
	}
	env.api.status, env.api.reply = http.StatusNotFound, `{"error":"service not found"}`
	if res, err := callTool(t, cs, "service_get", map[string]any{"serviceID": "nope"}); err != nil || !res.IsError {
		t.Fatalf("service_get: %v %+v", err, res)
	}
	if res, err := callTool(t, cs, "service_get", map[string]any{"serviceID": ".."}); err != nil || !res.IsError {
		t.Fatalf("service_get ..: %v %+v", err, res)
	}
	_, _ = callTool(t, cs, "audit_events_list", nil) // not this token's tool
	rawPOST(t, env.url, "bua_wrongtoken12345", legacyHeader, rawToolsList)

	rows, _ := json.Marshal(env.audit.all())
	logs := env.logs.String()
	if !strings.Contains(logs, "service_get") {
		t.Fatalf("the failed call was not logged; the test would prove nothing: %q", logs)
	}
	for name, secret := range map[string]string{
		"bearer token":         testBearer,
		"rejected token":       "bua_wrongtoken12345",
		"secret of the answer": handedOut,
	} {
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
	if got, want := strings.Join(results, ","), "service_get=ok,service_get=error,service_get=error,audit_events_list=denied"; got != want {
		t.Errorf("audit rows %s, want %s", got, want)
	}
}

// ---------- the audit trail of a call --------------------------------------

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

// Who calls, from where, with which client and in which request travels
// through the SDK to the API request of a call, on every revision, so the
// API's own audit rows name the same; the mcp.tool.call row carries it too.
func TestHTTP_AuditContext_ReachesTheAPIAndTheRow(t *testing.T) {
	for _, rev := range testRevisions {
		t.Run(rev, func(t *testing.T) {
			env := newTestEnv(t, []string{"budgets:manage"}, "admin")
			env.api.status = http.StatusNoContent
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

			for _, usd := range []float64{1, 2} {
				if res, err := callTool(t, cs, "budget_create", map[string]any{
					"body": map[string]any{"scope": "global", "daily_usd": usd, "action_on_exceed": "throttle_zero"},
				}); err != nil || res.IsError {
					t.Fatalf("budget_create: %v %+v", err, res)
				}
			}
			_, _ = callTool(t, cs, "audit_events_list", nil) // not this token's tool

			rows := env.audit.all()
			var got []string
			for _, r := range rows {
				got = append(got, r.SubjectLabel+"="+r.Result)
				check("the "+r.SubjectLabel+" row", audit.LogContext{
					ActorID: r.ActorID, ActorEmail: r.ActorEmail,
					SourceIP: r.SourceIP, UserAgent: r.UserAgent, RequestID: r.RequestID,
				})
			}
			if want := "budget_create=ok,budget_create=ok,audit_events_list=denied"; strings.Join(got, ",") != want {
				t.Fatalf("audit rows %v, want %s", got, want)
			}
			// Each call is a request of its own.
			if rows[0].RequestID == rows[1].RequestID {
				t.Errorf("two calls share the request id %q", rows[0].RequestID)
			}
			// The API requests: the caller's address, user agent and request
			// id; no forwarding header.
			if env.api.calls() != 2 {
				t.Fatalf("%d API requests, want 2", env.api.calls())
			}
			for i, r := range env.api.seen {
				if !strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") || r.UserAgent() != testUserAgent ||
					r.Header.Get("X-Request-Id") != rows[i].RequestID || r.Header.Get("X-Forwarded-For") != "" {
					t.Errorf("API request %d: addr %q, user agent %q, request id %q (row %q), forwarded %q",
						i, r.RemoteAddr, r.UserAgent(), r.Header.Get("X-Request-Id"), rows[i].RequestID, r.Header.Get("X-Forwarded-For"))
				}
			}
			raw, _ := json.Marshal(rows)
			if strings.Contains(string(raw), testBearer) {
				t.Errorf("the audit rows carry the bearer token: %s", raw)
			}
		})
	}
}

// Without the request-id middleware and without an email on the user the
// row still names the actor; nothing is invented.
func TestHTTP_AuditRow_WithoutRequestID(t *testing.T) {
	env := newTestEnv(t, []string{"audit:read"}, "admin")
	cs := connectMCP(t, env.url, testBearer, "2026-07-28")
	if res, err := callTool(t, cs, "audit_events_list", nil); err != nil || res.IsError {
		t.Fatalf("audit_events_list: %v %+v", err, res)
	}
	rows := env.audit.all()
	if len(rows) != 1 || rows[0].ActorID != testUserID || rows[0].ActorEmail != "ci@x" ||
		rows[0].SourceIP != "127.0.0.1" || rows[0].RequestID != "" {
		t.Fatalf("audit rows = %+v", rows)
	}
	if id := env.api.seen[0].Header.Get("X-Request-Id"); id != "" {
		t.Errorf("the API request carries an invented request id %q", id)
	}
}

// Through the whole handler: arguments that do not fit the schema are a
// result the model can read, nothing is sent, and the value reaches neither
// the answer nor the log.
func TestHTTP_RefusedArguments_ValueStaysOut(t *testing.T) {
	env := newTestEnv(t, []string{"budgets:manage"}, "admin")
	cs := connectMCP(t, env.url, testBearer, "2025-11-25")
	res, err := callTool(t, cs, "budget_create", map[string]any{
		"body": map[string]any{"scope": "global", "daily_usd": 1, "action_on_exceed": "throttle_zero"}, "extra": "MARKERVALUE",
	})
	if err != nil || !res.IsError {
		t.Fatalf("err=%v result=%+v", err, res)
	}
	if got := resultText(t, res); !strings.Contains(got, "extra") || strings.Contains(got, "MARKERVALUE") {
		t.Errorf("result text %q", got)
	}
	if env.api.calls() != 0 {
		t.Errorf("refused arguments reached the API %d times", env.api.calls())
	}
	logs := env.logs.String()
	if !strings.Contains(logs, "budget_create") || strings.Contains(logs, "MARKERVALUE") {
		t.Errorf("log: %s", logs)
	}
	if rows := env.audit.all(); len(rows) != 1 || rows[0].Result != "error" {
		t.Errorf("audit rows %+v", rows)
	}
}

// ---------- what a caller can put into an audit row ------------------------

// The user agent, the request id and the address in an audit row are text the
// caller supplies, and a row is written for every call, reads and refused
// calls included. Each is cut to maxAuditTextBytes, between two characters,
// and is valid UTF-8 afterwards (a database that stores text as UTF-8 refuses
// the row otherwise, and the call would leave no trace).
func TestAuditText_IsBoundedAndValid(t *testing.T) {
	if maxAuditTextBytes != 256 {
		t.Fatalf("maxAuditTextBytes = %d, want 256", maxAuditTextBytes)
	}
	for _, c := range []struct {
		name, in, want string
	}{
		{"short", "curl/8.5.0", "curl/8.5.0"},
		{"empty", "", ""},
		{"exactly the limit", strings.Repeat("a", 256), strings.Repeat("a", 256)},
		{"one over", strings.Repeat("a", 257), strings.Repeat("a", 256)},
		{"900 KB", strings.Repeat("a", 900<<10), strings.Repeat("a", 256)},
		{"two-byte characters", strings.Repeat("é", 200), strings.Repeat("é", 128)},
		// The limit falls inside a character: the whole character goes.
		{"cut inside two bytes", "a" + strings.Repeat("é", 200), "a" + strings.Repeat("é", 127)},
		{"cut inside three bytes", "a" + strings.Repeat("€", 200), "a" + strings.Repeat("€", 85)},
		{"cut inside four bytes", "ab" + strings.Repeat("😀", 200), "ab" + strings.Repeat("😀", 63)},
		{"not UTF-8", "agent/\xff\xfe1", "agent/\uFFFD1"},
		// A run of bytes that are no UTF-8 becomes one replacement character.
		{"not UTF-8, long", strings.Repeat("\xff", 1000), "\uFFFD"},
		{"only continuation bytes", strings.Repeat("\x80", 1000), "\uFFFD"},
		// Replacing makes the text longer; it is cut again.
		{"every other byte is no UTF-8", strings.Repeat("a\xff", 500), strings.Repeat("a\uFFFD", 64)},
	} {
		got := auditText(c.in)
		if got != c.want {
			t.Errorf("%s: got %d bytes %.40q, want %d bytes %.40q", c.name, len(got), got, len(c.want), c.want)
		}
		if len(got) > maxAuditTextBytes || !utf8.ValidString(got) {
			t.Errorf("%s: %d bytes, valid UTF-8: %v", c.name, len(got), utf8.ValidString(got))
		}
	}
}

// Through the handler: a huge user agent and a huge request id reach neither
// the row of a refused call nor the row of a call that ran; the API request
// of the call carries the bounded request id.
func TestHTTP_AuditRow_CallerSuppliedTextIsBounded(t *testing.T) {
	env := newTestEnv(t, []string{"audit:read"}, "admin")
	hs := httptest.NewServer(middleware.RequestID(env.srv))
	t.Cleanup(hs.Close)
	hdr := map[string]string{
		"MCP-Protocol-Version": "2025-06-18",
		"User-Agent":           "agent/" + strings.Repeat("A", 600<<10),
		"X-Request-Id":         "req-" + strings.Repeat("R", 200<<10),
	}
	for _, call := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"providers_list","arguments":{}}}`, // not this token's tool
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"audit_events_list","arguments":{}}}`,
	} {
		if resp, body := rawPOST(t, hs.URL, testBearer, hdr, call); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %.200s", resp.StatusCode, body)
		}
	}
	rows := env.audit.all()
	if len(rows) != 2 || rows[0].Result != "denied" || rows[1].Result != "ok" {
		t.Fatalf("audit rows = %d, want a denied and an ok row", len(rows))
	}
	for _, r := range rows {
		if r.UserAgent != "agent/"+strings.Repeat("A", 250) {
			t.Errorf("the %s row: user agent has %d bytes: %.40q…", r.SubjectLabel, len(r.UserAgent), r.UserAgent)
		}
		if r.RequestID != "req-"+strings.Repeat("R", 252) {
			t.Errorf("the %s row: request id has %d bytes: %.40q…", r.SubjectLabel, len(r.RequestID), r.RequestID)
		}
		if raw, _ := json.Marshal(r); len(raw) > 2048 {
			t.Errorf("the %s row is %d bytes", r.SubjectLabel, len(raw))
		}
	}
	if id := env.api.seen[0].Header.Get("X-Request-Id"); id != rows[1].RequestID {
		t.Errorf("the API request carries a request id of %d bytes, want the row's", len(id))
	}
}
