package oauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

const testIssuer = "https://relay.test"

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type recAudit struct {
	mu sync.Mutex
	ev []audit.Event
}

func (a *recAudit) Append(_ context.Context, e audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev = append(a.ev, e)
	return nil
}

func (a *recAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.ev {
		out = append(out, e.Action)
	}
	return out
}

type harness struct {
	s     *Server
	x     *db.DB
	sqldb *sql.DB
	clk   *testClock
	audit *recAudit
	// client is a pre-registered client with a loopback and an https
	// redirect URI.
	client db.OAuthClient
}

func newHarness(t *testing.T, mods ...func(*Config)) *harness {
	t.Helper()
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	x := db.Wrap(sqldb)
	ctx := context.Background()
	for _, u := range []db.User{
		{ID: "u-admin", Email: "admin@x", PasswordHash: "h", Role: "admin"},
		{ID: "u-user", Email: "user@x", PasswordHash: "h", Role: "user"},
	} {
		if err := x.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	// A tool source to point a resource at.
	if err := x.CreateRemoteMCPSource(ctx, db.Service{ID: "svc-gh", UserID: "u-admin", Name: "mcp:github", AccessMode: "api_key"},
		db.MCPSource{Slug: "github", Name: "GitHub", URL: "https://mcp.example/mcp", TimeoutS: 60, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := x.CreateRemoteMCPSource(ctx, db.Service{ID: "svc-off", UserID: "u-admin", Name: "mcp:off", AccessMode: "api_key"},
		db.MCPSource{Slug: "off", Name: "Off", URL: "https://off.example/mcp", TimeoutS: 60, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	clk := &testClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	al := &recAudit{}
	cfg := Config{Store: x, Issuer: testIssuer, Audit: al, Now: clk.now}
	for _, m := range mods {
		m(&cfg)
	}
	s := New(cfg)
	c, err := s.CreateClient(ctx, NewClient{Name: "Test client", RedirectURIs: []string{"http://127.0.0.1:5000/callback", "https://app.example/cb"}, CreatedBy: "u-admin"})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{s: s, x: x, sqldb: sqldb, clk: clk, audit: al, client: c}
}

// verifier is a fixed PKCE pair.
const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

func challenge() string { return s256(verifier) }

// authParams are the parameters of a valid request of the harness's client
// for /mcp/burrow.
func (h *harness) authParams() url.Values {
	return url.Values{
		"client_id":             {h.client.ClientID},
		"redirect_uri":          {"http://127.0.0.1:5000/callback"},
		"response_type":         {"code"},
		"code_challenge":        {challenge()},
		"code_challenge_method": {"S256"},
		"resource":              {testIssuer + "/mcp/burrow"},
		"scope":                 {"burrow:read"},
		"state":                 {"st-1"},
	}
}

func (h *harness) authorize(t *testing.T, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.s.Authorize(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil))
	return rec
}

// requestID runs a valid authorization request and returns the pending id.
func (h *harness) requestID(t *testing.T, q url.Values) string {
	t.Helper()
	rec := h.authorize(t, q)
	loc, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || err != nil || loc.Path != "/oauth/consent" {
		t.Fatalf("authorize: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if got := loc.Scheme + "://" + loc.Host; got != testIssuer {
		t.Fatalf("consent page on %q, want the issuer", got)
	}
	return loc.Query().Get("request")
}

// code approves a request as the admin and returns the code.
func (h *harness) code(t *testing.T, q url.Values) string {
	t.Helper()
	got, err := h.s.Approve(context.Background(), h.requestID(t, q), Decision{UserID: "u-admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got.RedirectURL)
	if u.Query().Get("iss") != testIssuer || u.Query().Get("state") != q.Get("state") {
		t.Fatalf("approval redirect %q lacks iss or state", got.RedirectURL)
	}
	return u.Query().Get("code")
}

type tokenAnswer struct {
	status int
	body   map[string]any
}

func (a tokenAnswer) str(k string) string { s, _ := a.body[k].(string); return s }

func (h *harness) post(t *testing.T, handler http.HandlerFunc, form url.Values, mod ...func(*http.Request)) tokenAnswer {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, m := range mod {
		m(req)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	out := tokenAnswer{status: rec.Code, body: map[string]any{}}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	return out
}

func (h *harness) exchange(t *testing.T, code string, mods ...func(url.Values)) tokenAnswer {
	t.Helper()
	f := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {h.client.ClientID},
		"redirect_uri": {"http://127.0.0.1:5000/callback"}, "code_verifier": {verifier},
		"resource": {testIssuer + "/mcp/burrow"},
	}
	for _, m := range mods {
		m(f)
	}
	return h.post(t, h.s.Token, f)
}

func (h *harness) refresh(t *testing.T, rt string) tokenAnswer {
	t.Helper()
	return h.post(t, h.s.Token, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {h.client.ClientID}})
}

func wantTokenError(t *testing.T, what string, a tokenAnswer, status int, code string) {
	t.Helper()
	if a.status != status || a.str("error") != code {
		t.Errorf("%s: %d %v, want %d %s", what, a.status, a.body, status, code)
	}
}

// --- metadata ----------------------------------------------------------------

func TestMetadata(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	h.s.Metadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"issuer":                                         `"https://relay.test"`,
		"authorization_endpoint":                         `"https://relay.test/oauth/authorize"`,
		"token_endpoint":                                 `"https://relay.test/oauth/token"`,
		"revocation_endpoint":                            `"https://relay.test/oauth/revoke"`,
		"code_challenge_methods_supported":               `["S256"]`,
		"grant_types_supported":                          `["authorization_code","refresh_token"]`,
		"response_types_supported":                       `["code"]`,
		"token_endpoint_auth_methods_supported":          `["none"]`,
		"client_id_metadata_document_supported":          `true`,
		"authorization_response_iss_parameter_supported": `true`,
		"scopes_supported":                               `["burrow:read","burrow:write","mcp:tools"]`,
	}
	for k, v := range want {
		got, _ := json.Marshal(m[k])
		if string(got) != v {
			t.Errorf("%s = %s, want %s", k, got, v)
		}
	}
	if _, ok := m["registration_endpoint"]; ok {
		t.Error("registration_endpoint offered; DCR is not")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("metadata without CORS")
	}
}

// Without a configured issuer the server is off, except for a request to a
// loopback host (development), whose issuer is taken from it and not cached.
func TestIssuer_UnconfiguredIsOffExceptOnLoopback(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Issuer = "" })
	call := func(method, host, path string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = host
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rec := httptest.NewRecorder()
		switch {
		case strings.HasPrefix(path, "/.well-known"):
			h.s.Metadata(rec, req)
		case strings.HasPrefix(path, "/oauth/authorize"):
			h.s.Authorize(rec, req)
		case path == "/oauth/token":
			h.s.Token(rec, req)
		default:
			h.s.Revoke(rec, req)
		}
		return rec
	}
	for _, host := range []string{"burrow.example.com", "burrow.local:8080", "10.0.0.5"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		if got := h.s.Issuer(req); got != "" {
			t.Errorf("issuer for %s: %q, want none", host, got)
		}
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, "/.well-known/oauth-authorization-server", ""},
			{http.MethodGet, "/oauth/authorize?" + h.authParams().Encode(), ""},
			{http.MethodPost, "/oauth/token", "grant_type=refresh_token&refresh_token=bor_x&client_id=x"},
			{http.MethodPost, "/oauth/revoke", "token=x&client_id=x"},
		} {
			if rec := call(c.method, host, c.path, c.body); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s on %s: %d, want 404", c.method, c.path, host, rec.Code)
			}
		}
	}
	for host, iss := range map[string]string{
		"localhost:8080": "http://localhost:8080", "127.0.0.1:9": "http://127.0.0.1:9", "[::1]:80": "http://[::1]:80",
	} {
		rec := call(http.MethodGet, host, "/.well-known/oauth-authorization-server", "")
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		if rec.Code != http.StatusOK || m["issuer"] != iss || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("loopback %s: %d %v %q", host, rec.Code, m["issuer"], rec.Header().Get("Cache-Control"))
		}
	}
	// A configured issuer is used whatever the Host says.
	h2 := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "evil.example"
	if got := h2.s.Issuer(req); got != testIssuer {
		t.Fatalf("configured issuer: %q", got)
	}
}

// --- redirect URIs -----------------------------------------------------------

func TestRedirectURIRules(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://app.example/cb":               true,
		"http://127.0.0.1:8080/cb":             true,
		"http://localhost/cb":                  true,
		"http://[::1]:9/cb":                    true,
		"com.example.app:/oauth":               true,
		"http://app.example/cb":                false, // http off the loopback
		"https://app.example/cb#frag":          false,
		"https://user:pw@app.example/cb":       false,
		"javascript:alert(1)":                  false,
		"cursor://x":                           false, // a scheme without a dot
		"":                                     false,
		"https:///nohost":                      false,
		"https://app.example/a b":              false,
		"data:text/html,<script>":              false,
		"https://" + strings.Repeat("a", 3000): false,
	} {
		if got := ValidRedirectURI(raw); got != ok {
			t.Errorf("ValidRedirectURI(%q) = %v, want %v", raw, got, ok)
		}
	}
	reg := []string{"http://127.0.0.1:5000/callback", "https://app.example/cb"}
	for req, ok := range map[string]bool{
		"http://127.0.0.1:5000/callback":   true,
		"http://127.0.0.1:61234/callback":  true, // the port of a loopback redirect may vary
		"http://127.0.0.1/callback":        true,
		"http://localhost:5000/callback":   false, // another host name
		"http://127.0.0.1:5000/callback2":  false,
		"http://127.0.0.1:5000/callback?x": false,
		"https://app.example/cb":           true,
		"https://app.example:8443/cb":      false, // only loopback ports vary
		"https://app.example/cb/":          false,
		"https://evil.example/cb":          false,
	} {
		if got := redirectAllowed(reg, req); got != ok {
			t.Errorf("redirectAllowed(%q) = %v, want %v", req, got, ok)
		}
	}
}

// --- the authorization endpoint ------------------------------------------------

// Until the client and its redirect URI are known good, nothing is sent to
// the redirect URI.
func TestAuthorize_UntrustedRedirectGetsAPage(t *testing.T) {
	h := newHarness(t)
	cases := map[string]func(url.Values){
		"unknown client":        func(q url.Values) { q.Set("client_id", "bcl_nope") },
		"no client":             func(q url.Values) { q.Del("client_id") },
		"redirect mismatch":     func(q url.Values) { q.Set("redirect_uri", "https://evil.example/cb") },
		"no redirect":           func(q url.Values) { q.Del("redirect_uri") },
		"redirect repeated":     func(q url.Values) { q.Add("redirect_uri", "https://app.example/cb") },
		"loopback host differs": func(q url.Values) { q.Set("redirect_uri", "http://localhost:5000/callback") },
		"too long":              func(q url.Values) { q.Set("state", strings.Repeat("s", maxAuthorizeQuery)) },
	}
	for name, mod := range cases {
		q := h.authParams()
		mod(q)
		rec := h.authorize(t, q)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("%s: %d Location=%q, want 400 without a redirect", name, rec.Code, rec.Header().Get("Location"))
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: error page may be framed", name)
		}
	}
}

// After that, errors go back to the client with error, state and iss.
func TestAuthorize_ErrorsGoBackWithIss(t *testing.T) {
	h := newHarness(t)
	cases := map[string]struct {
		mod  func(url.Values)
		code string
	}{
		"no PKCE":           {func(q url.Values) { q.Del("code_challenge"); q.Del("code_challenge_method") }, "invalid_request"},
		"plain PKCE":        {func(q url.Values) { q.Set("code_challenge_method", "plain") }, "invalid_request"},
		"short challenge":   {func(q url.Values) { q.Set("code_challenge", "abc") }, "invalid_request"},
		"challenge charset": {func(q url.Values) { q.Set("code_challenge", strings.Repeat("a+/=", 11)) }, "invalid_request"},
		"disabled source": {func(q url.Values) {
			q.Set("resource", testIssuer+"/mcp/off")
			q.Set("scope", "mcp:tools")
		}, "invalid_target"},
		"token response":       {func(q url.Values) { q.Set("response_type", "token") }, "unsupported_response_type"},
		"no resource":          {func(q url.Values) { q.Del("resource") }, "invalid_target"},
		"foreign resource":     {func(q url.Values) { q.Set("resource", "https://other.example/mcp/burrow") }, "invalid_target"},
		"not an MCP endpoint":  {func(q url.Values) { q.Set("resource", testIssuer+"/api/v1") }, "invalid_target"},
		"unknown source":       {func(q url.Values) { q.Set("resource", testIssuer+"/mcp/nope") }, "invalid_target"},
		"unknown scope":        {func(q url.Values) { q.Set("scope", "burrow:root") }, "invalid_scope"},
		"tools at /mcp/burrow": {func(q url.Values) { q.Set("scope", "mcp:tools") }, "invalid_scope"},
		"burrow scope at /mcp": {func(q url.Values) { q.Set("resource", testIssuer+"/mcp"); q.Set("scope", "burrow:read") }, "invalid_scope"},
		"repeated parameter":   {func(q url.Values) { q.Add("scope", "burrow:write") }, "invalid_request"},
	}
	for name, tc := range cases {
		q := h.authParams()
		tc.mod(q)
		rec := h.authorize(t, q)
		loc, _ := url.Parse(rec.Header().Get("Location"))
		if rec.Code != http.StatusFound || loc == nil || loc.Host != "127.0.0.1:5000" {
			t.Errorf("%s: %d %q, want a redirect to the client", name, rec.Code, rec.Header().Get("Location"))
			continue
		}
		if got := loc.Query(); got.Get("error") != tc.code || got.Get("iss") != testIssuer || got.Get("state") != "st-1" {
			t.Errorf("%s: redirect %q, want error=%s with iss and state", name, loc, tc.code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: error redirect may be cached", name)
		}
	}
}

func TestAuthorize_ResourcesAndDefaults(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		resource, scope string
		kind, source    string
		scopes          string
	}{
		{testIssuer + "/mcp/burrow", "", ResourceBurrow, "", "burrow:read"},
		{testIssuer + "/mcp/burrow/", "burrow:write users:read offline_access", ResourceBurrow, "", "burrow:write users:read"},
		{testIssuer + "/mcp", "", ResourceGateway, "", "mcp:tools"},
		{testIssuer + "/mcp/github", "mcp:tools", ResourceSource, "github", "mcp:tools"},
	} {
		q := h.authParams()
		q.Set("resource", tc.resource)
		q.Set("scope", tc.scope)
		req, err := h.s.PendingRequest(context.Background(), h.requestID(t, q))
		if err != nil {
			t.Fatal(err)
		}
		if req.Resource != strings.TrimSuffix(tc.resource, "/") || req.ResourceKind != tc.kind || req.Source != tc.source || strings.Join(req.Scopes, " ") != tc.scopes {
			t.Errorf("%s %q: %+v", tc.resource, tc.scope, req)
		}
		if req.ClientName != "Test client" || req.ClientKind != "registered" || req.RedirectHost != "127.0.0.1" || !req.LocalhostRedirect {
			t.Errorf("client fields: %+v", req)
		}
		if tc.kind != ResourceBurrow && len(req.Sources) != 1 {
			t.Errorf("%s: sources %v", tc.resource, req.Sources)
		}
	}
}

func TestPendingRequestExpires(t *testing.T) {
	h := newHarness(t)
	id := h.requestID(t, h.authParams())
	h.clk.add(RequestTTL)
	if _, err := h.s.PendingRequest(context.Background(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after ten minutes: %v", err)
	}
	if _, err := h.s.Approve(context.Background(), id, Decision{UserID: "u-admin", Role: "admin"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approve after ten minutes: %v", err)
	}
}

func TestDeny(t *testing.T) {
	h := newHarness(t)
	id := h.requestID(t, h.authParams())
	got, err := h.s.Deny(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got.RedirectURL)
	if q := u.Query(); q.Get("error") != "access_denied" || q.Get("iss") != testIssuer || q.Get("state") != "st-1" || q.Get("code") != "" {
		t.Fatalf("deny redirect %q", got.RedirectURL)
	}
	if _, err := h.s.Approve(context.Background(), id, Decision{UserID: "u-admin", Role: "admin"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approve after deny: %v", err)
	}
}

// --- the consent decision ----------------------------------------------------

func TestApprove_ScopesWithinTheRole(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.requestID(t, h.authParams())
	if _, err := h.s.Approve(ctx, id, Decision{UserID: "u-user", Role: "user", Scopes: []string{"users:manage"}}); !errors.Is(err, ErrScopeNotInRole) {
		t.Fatalf("a user granting users:manage: %v", err)
	}
	if _, err := h.s.Approve(ctx, id, Decision{UserID: "u-user", Role: "user", Scopes: []string{"mcp:tools"}}); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("mcp:tools at /mcp/burrow: %v", err)
	}
	if _, err := h.s.Approve(ctx, id, Decision{UserID: "u-user", Role: "user", Scopes: []string{}}); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("no scope: %v", err)
	}
	// A refused decision leaves the request waiting; the user may switch
	// the read-only toggle off (burrow:write is the role's permissions).
	got, err := h.s.Approve(ctx, id, Decision{UserID: "u-user", Role: "user", Scopes: []string{"burrow:write", "tunnels:read:own"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Scopes, " ") != "burrow:write tunnels:read:own" {
		t.Fatalf("scopes %v", got.Scopes)
	}
}

func TestApprove_GatewayTools(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	authz.SetRoles(map[string][]authz.Permission{"viewer": {authz.PermTunnelsReadOwn}})
	t.Cleanup(func() { authz.SetRoles(nil) })
	gw := h.authParams()
	gw.Set("resource", testIssuer+"/mcp")
	gw.Set("scope", "mcp:tools")
	id := h.requestID(t, gw)
	if _, err := h.s.Approve(ctx, id, Decision{UserID: "u-user", Role: "viewer"}); !errors.Is(err, ErrToolsForbidden) {
		t.Fatalf("a role without ai:configure: %v", err)
	}
	bad := []string{"github"}
	if _, err := h.s.Approve(ctx, id, Decision{UserID: "u-user", Role: "user", AllowedTools: &bad}); !errors.Is(err, ErrInvalidTools) {
		t.Fatalf("malformed entry: %v", err)
	}
	// Defaults: every source, read-only.
	got, err := h.s.Approve(ctx, id, Decision{UserID: "u-user", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.AllowedTools, ",") != "github__*" || !got.ToolsReadOnly || strings.Join(got.Scopes, " ") != "mcp:tools" {
		t.Fatalf("defaults: %+v", got)
	}

	// At /mcp/<slug> every entry names that source.
	src := h.authParams()
	src.Set("resource", testIssuer+"/mcp/github")
	src.Set("scope", "")
	id = h.requestID(t, src)
	other := []string{"slack__*"}
	if _, err := h.s.Approve(ctx, id, Decision{UserID: "u-user", Role: "user", AllowedTools: &other}); !errors.Is(err, ErrInvalidTools) {
		t.Fatalf("another source at /mcp/github: %v", err)
	}
	one, rw := []string{"github__create_issue", "github__create_issue"}, false
	got, err = h.s.Approve(ctx, id, Decision{UserID: "u-user", Role: "user", AllowedTools: &one, ToolsReadOnly: &rw})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.AllowedTools, ",") != "github__create_issue" || got.ToolsReadOnly {
		t.Fatalf("chosen: %+v", got)
	}
	u, _ := url.Parse(got.RedirectURL)
	a := h.exchange(t, u.Query().Get("code"), func(f url.Values) { f.Set("resource", testIssuer+"/mcp/github") })
	if a.status != http.StatusOK || a.str("scope") != "mcp:tools" {
		t.Fatalf("exchange: %d %v", a.status, a.body)
	}
	tok, err := h.s.LookupAccessToken(ctx, a.str("access_token"))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Resource != testIssuer+"/mcp/github" || strings.Join(tok.AllowedTools, ",") != "github__create_issue" || tok.ToolsReadOnly || tok.UserID != "u-user" {
		t.Fatalf("token: %+v", tok)
	}
}

// --- the token endpoint ------------------------------------------------------

func TestToken_HappyPath(t *testing.T) {
	h := newHarness(t)
	a := h.exchange(t, h.code(t, h.authParams()))
	if a.status != http.StatusOK {
		t.Fatalf("exchange: %d %v", a.status, a.body)
	}
	if !strings.HasPrefix(a.str("access_token"), "boa_") || !strings.HasPrefix(a.str("refresh_token"), "bor_") ||
		a.str("token_type") != "Bearer" || a.body["expires_in"] != float64(3600) || a.str("scope") != "burrow:read" {
		t.Fatalf("answer %v", a.body)
	}
	tok, err := h.s.LookupAccessToken(context.Background(), a.str("access_token"))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Resource != testIssuer+"/mcp/burrow" || tok.ClientID != h.client.ClientID || tok.UserID != "u-admin" ||
		strings.Join(tok.Scopes, " ") != "burrow:read" || !tok.ExpiresAt.Equal(h.clk.now().Add(time.Hour)) {
		t.Fatalf("token %+v", tok)
	}
	// The refresh token is no access token, and nothing is stored in clear.
	if _, err := h.s.LookupAccessToken(context.Background(), a.str("refresh_token")); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("refresh token as access token: %v", err)
	}
	var n int
	if err := h.sqldb.QueryRow(`SELECT COUNT(*) FROM oauth_tokens WHERE token_hash IN (?, ?)`, a.str("access_token"), a.str("refresh_token")).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a token is stored in clear: %d %v", n, err)
	}
	grants, _ := h.s.ListGrants(context.Background(), "u-admin")
	if len(grants) != 1 || grants[0].ClientName != "Test client" || grants[0].LastUsed == nil {
		t.Fatalf("grants %+v", grants)
	}
}

func TestToken_WrongVerifierBurnsTheCode(t *testing.T) {
	h := newHarness(t)
	code := h.code(t, h.authParams())
	wantTokenError(t, "wrong verifier", h.exchange(t, code, func(f url.Values) {
		f.Set("code_verifier", strings.Repeat("x", 43))
	}), 400, "invalid_grant")
	wantTokenError(t, "right verifier after a wrong one", h.exchange(t, code), 400, "invalid_grant")
}

func TestToken_ReusedCodeRevokesItsGrant(t *testing.T) {
	h := newHarness(t)
	code := h.code(t, h.authParams())
	first := h.exchange(t, code)
	if first.status != http.StatusOK {
		t.Fatalf("first: %v", first.body)
	}
	wantTokenError(t, "second use", h.exchange(t, code), 400, "invalid_grant")
	if _, err := h.s.LookupAccessToken(context.Background(), first.str("access_token")); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("the first exchange's token after a reuse: %v", err)
	}
	wantTokenError(t, "the first exchange's refresh token", h.refresh(t, first.str("refresh_token")), 400, "invalid_grant")
}

func TestToken_CodeChecks(t *testing.T) {
	h := newHarness(t)
	cases := map[string]struct {
		mod    func(url.Values)
		status int
		code   string
	}{
		"redirect mismatch":     {func(f url.Values) { f.Set("redirect_uri", "https://app.example/cb") }, 400, "invalid_grant"},
		"another loopback port": {func(f url.Values) { f.Set("redirect_uri", "http://127.0.0.1:5001/callback") }, 400, "invalid_grant"},
		"another client":        {func(f url.Values) { f.Set("client_id", "bcl_other") }, 400, "invalid_grant"},
		"foreign resource":      {func(f url.Values) { f.Set("resource", testIssuer+"/mcp") }, 400, "invalid_target"},
		"no verifier":           {func(f url.Values) { f.Del("code_verifier") }, 400, "invalid_request"},
		"no client":             {func(f url.Values) { f.Del("client_id") }, 400, "invalid_request"},
		"client secret":         {func(f url.Values) { f.Set("client_secret", "s") }, 401, "invalid_client"},
		"unknown code":          {func(f url.Values) { f.Set("code", "nope") }, 400, "invalid_grant"},
		"implicit grant":        {func(f url.Values) { f.Set("grant_type", "password") }, 400, "unsupported_grant_type"},
	}
	for name, tc := range cases {
		code := h.code(t, h.authParams())
		wantTokenError(t, name, h.exchange(t, code, tc.mod), tc.status, tc.code)
	}
	// Expired: a code lives 60 seconds.
	code := h.code(t, h.authParams())
	h.clk.add(CodeTTL)
	wantTokenError(t, "expired code", h.exchange(t, code), 400, "invalid_grant")
	// resource may be left out of the exchange (RFC 8707); the code keeps it.
	h.clk.add(time.Second)
	code = h.code(t, h.authParams())
	if a := h.exchange(t, code, func(f url.Values) { f.Del("resource") }); a.status != http.StatusOK {
		t.Fatalf("without resource: %v", a.body)
	}
	// Not a form.
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(`{"grant_type":"authorization_code"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.s.Token(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Fatalf("JSON body: %d %s", rec.Code, rec.Body.String())
	}
}

// What an OAuth library sends when it detects the client authentication by
// itself: HTTP Basic with the client id and an empty password.
func TestToken_BasicWithEmptyPassword(t *testing.T) {
	h := newHarness(t)
	code := h.code(t, h.authParams())
	f := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"http://127.0.0.1:5000/callback"}, "code_verifier": {verifier}}
	a := h.post(t, h.s.Token, f, func(r *http.Request) { r.SetBasicAuth(h.client.ClientID, "") })
	if a.status != http.StatusOK {
		t.Fatalf("basic: %d %v", a.status, a.body)
	}
	code = h.code(t, h.authParams())
	f.Set("code", code)
	wantTokenError(t, "basic with a secret", h.post(t, h.s.Token, f, func(r *http.Request) { r.SetBasicAuth(h.client.ClientID, "s") }), 401, "invalid_client")
}

func TestRefresh_RotationAndReuse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := h.exchange(t, h.code(t, h.authParams()))
	second := h.refresh(t, first.str("refresh_token"))
	if second.status != http.StatusOK || second.str("refresh_token") == first.str("refresh_token") || second.str("scope") != "burrow:read" {
		t.Fatalf("rotation: %d %v", second.status, second.body)
	}
	// The access token of the first pair keeps working until it expires.
	if _, err := h.s.LookupAccessToken(ctx, first.str("access_token")); err != nil {
		t.Fatalf("first access token after a refresh: %v", err)
	}
	third := h.refresh(t, second.str("refresh_token"))
	if third.status != http.StatusOK {
		t.Fatalf("second rotation: %v", third.body)
	}
	// The first refresh token comes back: the grant ends, every token of it.
	wantTokenError(t, "reused refresh token", h.refresh(t, first.str("refresh_token")), 400, "invalid_grant")
	for _, tok := range []string{first.str("access_token"), second.str("access_token"), third.str("access_token")} {
		if _, err := h.s.LookupAccessToken(ctx, tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("an access token of the family after the reuse: %v", err)
		}
	}
	wantTokenError(t, "the newest refresh token after the reuse", h.refresh(t, third.str("refresh_token")), 400, "invalid_grant")
	if got := strings.Join(h.audit.actions(), ","); !strings.Contains(got, audit.ActionOAuthRefreshReuse) {
		t.Fatalf("audit: %s", got)
	}
	if grants, _ := h.s.ListGrants(ctx, ""); len(grants) != 0 {
		t.Fatalf("grants after the reuse: %+v", grants)
	}
}

func TestRefresh_Checks(t *testing.T) {
	h := newHarness(t)
	a := h.exchange(t, h.code(t, h.authParams()))
	rt := a.str("refresh_token")
	wantTokenError(t, "another client", h.post(t, h.s.Token, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {"bcl_x"}}), 400, "invalid_grant")
	wantTokenError(t, "access token as refresh token", h.refresh(t, a.str("access_token")), 400, "invalid_grant")
	wantTokenError(t, "widened scope", h.post(t, h.s.Token, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {h.client.ClientID}, "scope": {"burrow:write"}}), 400, "invalid_scope")
	wantTokenError(t, "another resource", h.post(t, h.s.Token, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {h.client.ClientID}, "resource": {testIssuer + "/mcp"}}), 400, "invalid_target")
	// None of the refused attempts used it up.
	if r := h.refresh(t, rt); r.status != http.StatusOK {
		t.Fatalf("after refused attempts: %v", r.body)
	}
}

func TestRefresh_SuspendedUser(t *testing.T) {
	h := newHarness(t)
	a := h.exchange(t, h.code(t, h.authParams()))
	if _, err := h.sqldb.Exec(`UPDATE users SET status='suspended' WHERE id='u-admin'`); err != nil {
		t.Fatal(err)
	}
	wantTokenError(t, "suspended user", h.refresh(t, a.str("refresh_token")), 400, "invalid_grant")
}

func TestAccessToken_ExpiresAndRevokes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.exchange(t, h.code(t, h.authParams()))
	h.clk.add(AccessTokenTTL - time.Second)
	if _, err := h.s.LookupAccessToken(ctx, a.str("access_token")); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	h.clk.add(time.Second)
	if _, err := h.s.LookupAccessToken(ctx, a.str("access_token")); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("after an hour: %v", err)
	}
	// The refresh token still gets a new one, until the grant is revoked.
	b := h.refresh(t, a.str("refresh_token"))
	grants, _ := h.s.ListGrants(ctx, "u-admin")
	if _, err := h.s.RevokeGrant(ctx, grants[0].ID, "u-user"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's grant: %v", err)
	}
	if _, err := h.s.RevokeGrant(ctx, grants[0].ID, "u-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.LookupAccessToken(ctx, b.str("access_token")); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("after the grant was revoked: %v", err)
	}
	wantTokenError(t, "refresh after revoke", h.refresh(t, b.str("refresh_token")), 400, "invalid_grant")
	for _, bad := range []string{"", "boa_nope", "bua_" + a.str("access_token")[4:]} {
		if _, err := h.s.LookupAccessToken(ctx, bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("LookupAccessToken(%q): %v", bad, err)
		}
	}
}

func TestRevokeEndpoint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	revoke := func(token, client string) int {
		return h.post(t, h.s.Revoke, url.Values{"token": {token}, "client_id": {client}}).status
	}
	a := h.exchange(t, h.code(t, h.authParams()))
	if c := revoke(a.str("refresh_token"), "bcl_other"); c != http.StatusOK {
		t.Fatalf("another client: %d", c)
	}
	if _, err := h.s.LookupAccessToken(ctx, a.str("access_token")); err != nil {
		t.Fatalf("another client revoked it: %v", err)
	}
	if c := revoke("bor_unknown", h.client.ClientID); c != http.StatusOK {
		t.Fatalf("unknown token: %d", c)
	}
	// An access token ends itself only.
	if c := revoke(a.str("access_token"), h.client.ClientID); c != http.StatusOK {
		t.Fatalf("access: %d", c)
	}
	if _, err := h.s.LookupAccessToken(ctx, a.str("access_token")); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked access token: %v", err)
	}
	b := h.refresh(t, a.str("refresh_token"))
	if b.status != http.StatusOK {
		t.Fatalf("refresh after the access token was revoked: %v", b.body)
	}
	// A refresh token ends the grant.
	if c := revoke(b.str("refresh_token"), h.client.ClientID); c != http.StatusOK {
		t.Fatalf("refresh: %d", c)
	}
	if _, err := h.s.LookupAccessToken(ctx, b.str("access_token")); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("after the refresh token was revoked: %v", err)
	}
	if !strings.Contains(strings.Join(h.audit.actions(), ","), audit.ActionOAuthGrantRevoke) {
		t.Fatalf("audit: %v", h.audit.actions())
	}
	if c := h.post(t, h.s.Revoke, url.Values{"client_id": {h.client.ClientID}}).status; c != http.StatusBadRequest {
		t.Fatalf("no token: %d", c)
	}
}

func TestDeleteClientRevokesItsGrants(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.exchange(t, h.code(t, h.authParams()))
	if _, err := h.s.DeleteClient(ctx, h.client.ClientID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.LookupAccessToken(ctx, a.str("access_token")); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("token of a deleted client: %v", err)
	}
	if rec := h.authorize(t, h.authParams()); rec.Code != http.StatusBadRequest {
		t.Fatalf("authorize with a deleted client: %d", rec.Code)
	}
	if _, err := h.s.DeleteClient(ctx, h.client.ClientID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestCreateClientValidation(t *testing.T) {
	h := newHarness(t)
	for name, in := range map[string]NewClient{
		"no name":         {RedirectURIs: []string{"https://a.example/cb"}},
		"no redirect":     {Name: "x"},
		"http redirect":   {Name: "x", RedirectURIs: []string{"http://a.example/cb"}},
		"fragment":        {Name: "x", RedirectURIs: []string{"https://a.example/cb#x"}},
		"control in name": {Name: "x\ny", RedirectURIs: []string{"https://a.example/cb"}},
	} {
		if _, err := h.s.CreateClient(context.Background(), in); !errors.Is(err, ErrInvalidClientInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	c, err := h.s.CreateClient(context.Background(), NewClient{Name: " App ", RedirectURIs: []string{"https://a.example/cb", "https://a.example/cb"}})
	if err != nil || c.Name != "App" || len(c.RedirectURIs) != 1 || !strings.HasPrefix(c.ClientID, ClientIDPrefix) {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestScopePermissions(t *testing.T) {
	read := ScopePermissions([]string{ScopeBurrowRead})
	for _, p := range read {
		if !strings.Contains(string(p), ":read") {
			t.Errorf("burrow:read holds %s", p)
		}
	}
	if len(read) == 0 || len(ScopePermissions([]string{ScopeBurrowWrite})) != len(authz.AllPermissions()) {
		t.Fatal("expansion sizes")
	}
	if got := ScopePermissions([]string{"users:manage", "bogus"}); len(got) != 1 || got[0] != authz.PermUsersManage {
		t.Fatalf("single: %v", got)
	}
}

// --- client metadata documents ---------------------------------------------

// cimdServer serves documents over TLS; the fetcher trusts its certificate.
type cimdServer struct {
	srv  *httptest.Server
	hits atomic.Int32
	docs map[string]func(base string) (status int, header http.Header, body string)
}

func newCIMDServer(t *testing.T) *cimdServer {
	c := &cimdServer{docs: map[string]func(string) (int, http.Header, string){}}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		f, ok := c.docs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		status, hdr, body := f(c.srv.URL)
		for k, v := range hdr {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func docJSON(id, name string, redirects ...string) string {
	b, _ := json.Marshal(map[string]any{"client_id": id, "client_name": name, "redirect_uris": redirects, "token_endpoint_auth_method": "none"})
	return string(b)
}

func TestCIMD(t *testing.T) {
	cs := newCIMDServer(t)
	ok := func(base string) (int, http.Header, string) {
		return 200, http.Header{"Cache-Control": {"max-age=60"}}, docJSON(base+"/client.json", "Doc client", "http://127.0.0.1:7000/cb")
	}
	cs.docs["/client.json"] = ok
	cs.docs["/mismatch.json"] = func(base string) (int, http.Header, string) {
		return 200, nil, docJSON(base+"/other.json", "X", "http://127.0.0.1:7000/cb")
	}
	cs.docs["/big.json"] = func(base string) (int, http.Header, string) {
		return 200, nil, `{"client_id":"` + base + `/big.json","client_name":"` + strings.Repeat("a", cimdMaxBytes) + `"}`
	}
	cs.docs["/redirect.json"] = func(base string) (int, http.Header, string) {
		return 302, http.Header{"Location": {base + "/client.json"}}, ""
	}
	cs.docs["/secret.json"] = func(base string) (int, http.Header, string) {
		b, _ := json.Marshal(map[string]any{"client_id": base + "/secret.json", "client_name": "S", "redirect_uris": []string{"https://a.example/cb"}, "token_endpoint_auth_method": "client_secret_basic"})
		return 200, nil, string(b)
	}
	cs.docs["/noredirects.json"] = func(base string) (int, http.Header, string) {
		return 200, nil, `{"client_id":"` + base + `/noredirects.json","client_name":"N"}`
	}
	cs.docs["/badredirect.json"] = func(base string) (int, http.Header, string) {
		return 200, nil, docJSON(base+"/badredirect.json", "B", "http://evil.example/cb")
	}
	cs.docs["/notjson.json"] = func(string) (int, http.Header, string) { return 200, nil, "<html>" }
	h := newHarness(t, func(c *Config) { c.CIMDClient = cs.srv.Client() })

	q := h.authParams()
	q.Set("client_id", cs.srv.URL+"/client.json")
	q.Set("redirect_uri", "http://127.0.0.1:7123/cb")
	id := h.requestID(t, q)
	req, err := h.s.PendingRequest(context.Background(), id)
	if err != nil || req.ClientName != "Doc client" || req.ClientKind != "metadata_document" || req.ClientID != cs.srv.URL+"/client.json" {
		t.Fatalf("%+v %v", req, err)
	}
	// Cached: a second request does not fetch again until max-age (held at
	// five minutes at least) is over.
	before := cs.hits.Load()
	h.requestID(t, q)
	if cs.hits.Load() != before {
		t.Fatal("fetched again within the cache time")
	}
	h.clk.add(5*time.Minute + time.Second)
	h.requestID(t, q)
	if cs.hits.Load() != before+1 {
		t.Fatal("not fetched again after the cache time")
	}
	// The whole flow works for a document client.
	got, err := h.s.Approve(context.Background(), id, Decision{UserID: "u-admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got.RedirectURL)
	a := h.exchange(t, u.Query().Get("code"), func(f url.Values) {
		f.Set("client_id", cs.srv.URL+"/client.json")
		f.Set("redirect_uri", "http://127.0.0.1:7123/cb")
	})
	if a.status != http.StatusOK {
		t.Fatalf("document client exchange: %d %v", a.status, a.body)
	}

	for path, want := range map[string]string{
		"/mismatch.json":    "does not equal",
		"/big.json":         "64 KiB",
		"/redirect.json":    "answered 302",
		"/secret.json":      "public clients only",
		"/noredirects.json": "redirect_uris",
		"/badredirect.json": "redirect URI",
		"/notjson.json":     "not valid JSON",
		"/missing.json":     "answered 404",
	} {
		q := h.authParams()
		q.Set("client_id", cs.srv.URL+path)
		q.Set("redirect_uri", "http://127.0.0.1:7000/cb")
		rec := h.authorize(t, q)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s, want a page that says %q", path, rec.Code, rec.Body.String(), want)
		}
	}
	// A redirect URI the document does not list.
	q.Set("client_id", cs.srv.URL+"/client.json")
	q.Set("redirect_uri", "http://127.0.0.1:7000/other")
	if rec := h.authorize(t, q); rec.Code != http.StatusBadRequest {
		t.Fatalf("unlisted redirect: %d", rec.Code)
	}
	// Not a usable URL.
	for _, bad := range []string{cs.srv.URL, cs.srv.URL + "/", cs.srv.URL + "/c.json?x=1", cs.srv.URL + "/a/../c.json"} {
		q.Set("client_id", bad)
		if rec := h.authorize(t, q); rec.Code != http.StatusBadRequest {
			t.Errorf("client_id %q: %d", bad, rec.Code)
		}
	}
}

// The default fetcher connects to public addresses only: a document on a
// loopback or private address is refused before any request is sent.
func TestCIMD_PrivateAddressRefused(t *testing.T) {
	cs := newCIMDServer(t)
	cs.docs["/client.json"] = func(base string) (int, http.Header, string) {
		return 200, nil, docJSON(base+"/client.json", "Local", "http://127.0.0.1:7000/cb")
	}
	h := newHarness(t) // the default, guarded client
	q := h.authParams()
	q.Set("client_id", cs.srv.URL+"/client.json")
	q.Set("redirect_uri", "http://127.0.0.1:7000/cb")
	rec := h.authorize(t, q)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not a public address") {
		t.Fatalf("loopback document: %d %s", rec.Code, rec.Body.String())
	}
	if cs.hits.Load() != 0 {
		t.Fatal("the private address was contacted")
	}
}

func TestCacheTTL(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for hdr, want := range map[string]time.Duration{
		"":                    cimdMinTTL,
		"max-age=60":          cimdMinTTL,
		"max-age=3600":        time.Hour,
		"public, max-age=900": 15 * time.Minute,
		"max-age=999999":      cimdMaxTTL,
		"no-store":            cimdMinTTL,
	} {
		h := http.Header{}
		if hdr != "" {
			h.Set("Cache-Control", hdr)
		}
		if got := cacheTTL(h, now); got != want {
			t.Errorf("%q: %v, want %v", hdr, got, want)
		}
	}
	h := http.Header{"Expires": {now.Add(2 * time.Hour).Format(http.TimeFormat)}}
	if got := cacheTTL(h, now); got != 2*time.Hour {
		t.Errorf("Expires: %v", got)
	}
}
