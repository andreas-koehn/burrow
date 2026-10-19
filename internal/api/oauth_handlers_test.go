package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/oauth"
	"github.com/ankoehn/burrow/internal/store"
)

// oauthHarness is the router with an authorization server over a real store,
// an admin "admin@x" who can sign in, a tool source "github", and the
// management MCP server at /mcp/burrow taking the server's access tokens.
type oauthHarness struct {
	srv   *httptest.Server
	st    *store.Store
	x     *db.DB
	oauth *oauth.Server
	audit *loginAudit
	admin string
}

func newOAuthHarness(t *testing.T, cfg func(*oauth.Config), mod func(*Deps)) *oauthHarness {
	t.Helper()
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	st := store.New(sqldb)
	x := db.Wrap(sqldb)
	ctx := context.Background()
	if err := st.SeedAdmin(ctx, "admin@x", "password1"); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByEmail(ctx, "admin@x")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.CreateRemoteMCPSource(ctx, db.Service{ID: "svc-gh", UserID: u.ID, Name: "mcp:github", AccessMode: "api_key"},
		db.MCPSource{Slug: "github", Name: "GitHub", URL: "https://mcp.example/mcp", TimeoutS: 60, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	al := &loginAudit{}
	c := oauth.Config{Store: x, Audit: auditEvents{al}}
	if cfg != nil {
		cfg(&c)
	}
	as := oauth.New(c)
	deps := Deps{Users: st, Log: discardLog(), OAuth: as, AuditAppender: auditEvents{al}}
	if mod != nil {
		mod(&deps)
	}
	h := &oauthHarness{st: st, x: x, oauth: as, audit: al, admin: u.ID}
	// The management MCP server, taking the server's access tokens and
	// replaying through the router.
	mcpSrv := newMCPServer(t, mcpTokens{}, mcpTokens{})
	mcpSrv.SetOAuth(as)
	deps.MCP = MCPInfo{Enabled: true, Server: mcpSrv, Handler: mcpSrv}
	deps.Bearer = NewStoreBearerStore(st)
	router := NewRouter(deps)
	mcpSrv.SetAPIHandler(router)
	h.srv = httptest.NewServer(router)
	t.Cleanup(h.srv.Close)
	return h
}

// auditEvents adapts the recording appender of the login tests.
type auditEvents struct{ a *loginAudit }

func (e auditEvents) Append(ctx context.Context, ev audit.Event) error { return e.a.Append(ctx, ev) }

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// consent plays the browser and the user: it follows the authorization URL
// to the consent page, reads the request, approves it with body and returns
// the client's redirect.
func (h *oauthHarness) consent(t *testing.T, c *authClient, authURL string, body any) *url.URL {
	t.Helper()
	resp, err := noRedirect.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil || loc.Path != "/oauth/consent" {
		t.Fatalf("authorize: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	id := loc.Query().Get("request")
	r := c.get(t, "/api/v1/oauth/requests/"+id)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("request details: %d %s", r.StatusCode, readBody(t, r))
	}
	_ = r.Body.Close()
	r = c.post(t, "/api/v1/oauth/requests/"+id+"/approve", body)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d %s", r.StatusCode, readBody(t, r))
	}
	var out struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal([]byte(readBody(t, r)), &out); err != nil {
		t.Fatal(err)
	}
	back, err := url.Parse(out.RedirectURL)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

// sdkAuthorize runs the SDK's own client (auth.AuthorizationCodeHandler)
// against the relay, as an MCP client that was answered 401 by /mcp/burrow.
func (h *oauthHarness) sdkAuthorize(t *testing.T, cfg *auth.AuthorizationCodeHandlerConfig, c *authClient) *auth.AuthorizationCodeHandler {
	t.Helper()
	cfg.AuthorizationCodeFetcher = func(_ context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		back := h.consent(t, c, args.URL, map[string]any{})
		q := back.Query()
		return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
	}
	handler, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/mcp/burrow", nil)
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header: http.Header{"Www-Authenticate": {
			fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp/burrow", scope="burrow:read"`, h.srv.URL),
		}},
		Body: io.NopCloser(strings.NewReader("")),
	}
	if err := handler.Authorize(context.Background(), req, resp); err != nil {
		t.Fatalf("SDK authorization: %v", err)
	}
	return handler
}

func (h *oauthHarness) form(t *testing.T, path string, f url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(h.srv.URL+path, f)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &m)
	return resp.StatusCode, m
}

// checkSDKToken looks the SDK's token up as the resource side will.
func (h *oauthHarness) checkSDKToken(t *testing.T, handler *auth.AuthorizationCodeHandler, clientID string) (access, refresh string) {
	t.Helper()
	ts, err := handler.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatalf("token source: %v", err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.oauth.LookupAccessToken(context.Background(), tok.AccessToken)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.Resource != h.srv.URL+"/mcp/burrow" || got.ClientID != clientID || got.UserID != h.admin || strings.Join(got.Scopes, " ") != "burrow:read" {
		t.Fatalf("token %+v", got)
	}
	if tok.RefreshToken == "" {
		t.Fatal("no refresh token")
	}
	return tok.AccessToken, tok.RefreshToken
}

// The SDK's client completes the flow with a pre-registered client: PRM and
// AS discovery, PKCE, iss, resource; then the refresh token rotates through
// the relay's token endpoint.
func TestOAuth_SDKClient_Preregistered(t *testing.T) {
	h := newOAuthHarness(t, nil, nil)
	c := authedClient(t, h.srv)
	r := c.post(t, "/api/v1/oauth/clients", map[string]any{"name": "CLI", "redirect_uris": []string{"http://127.0.0.1:43210/callback"}})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create client: %d %s", r.StatusCode, readBody(t, r))
	}
	var cl struct {
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal([]byte(readBody(t, r)), &cl)
	handler := h.sdkAuthorize(t, &auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: &oauthex.ClientCredentials{ClientID: cl.ClientID, Issuer: h.srv.URL},
		RedirectURL:         "http://127.0.0.1:43210/callback",
	}, c)
	_, refresh := h.checkSDKToken(t, handler, cl.ClientID)

	code, body := h.form(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {cl.ClientID}})
	if code != http.StatusOK || body["refresh_token"] == refresh {
		t.Fatalf("refresh: %d %v", code, body)
	}
	code, body = h.form(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {cl.ClientID}})
	if code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("reused refresh: %d %v", code, body)
	}
	if len(h.audit.byAction(audit.ActionOAuthRefreshReuse)) != 1 || len(h.audit.byAction(audit.ActionOAuthGrantApprove)) != 1 ||
		len(h.audit.byAction(audit.ActionOAuthClientCreate)) != 1 {
		t.Fatalf("audit:\n%s", h.audit.dump())
	}
	for _, e := range h.audit.ev {
		if strings.Contains(string(e.Payload), "bor_") || strings.Contains(string(e.Payload), "boa_") {
			t.Fatalf("a token in the audit log: %s", e.Payload)
		}
	}
}

// The SDK's client with a Client ID Metadata Document.
func TestOAuth_SDKClient_MetadataDocument(t *testing.T) {
	var docURL string
	cimd := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client.json" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"client_id": docURL, "client_name": "Doc Agent", "redirect_uris": []string{"http://127.0.0.1:43210/callback"},
			"grant_types": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_method": "none",
		})
	}))
	t.Cleanup(cimd.Close)
	docURL = cimd.URL + "/client.json"
	h := newOAuthHarness(t, func(c *oauth.Config) { c.CIMDClient = cimd.Client() }, nil)
	c := authedClient(t, h.srv)
	handler := h.sdkAuthorize(t, &auth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: docURL},
		RedirectURL:                    "http://127.0.0.1:43210/callback",
	}, c)
	h.checkSDKToken(t, handler, docURL)
	// The connected app lists under the document's name.
	r := c.get(t, "/api/v1/oauth/grants")
	var grants []oauthGrantResp
	_ = json.Unmarshal([]byte(readBody(t, r)), &grants)
	if len(grants) != 1 || grants[0].ClientName != "Doc Agent" || grants[0].ClientID != docURL || grants[0].UserEmail != "admin@x" {
		t.Fatalf("grants %+v", grants)
	}
}

// authURL builds an authorization request of a pre-registered client.
func (h *oauthHarness) authURL(clientID, resource string) string {
	return h.srv.URL + "/oauth/authorize?" + url.Values{
		"client_id": {clientID}, "redirect_uri": {"http://127.0.0.1:43210/callback"}, "response_type": {"code"},
		"code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}, "code_challenge_method": {"S256"},
		"resource": {resource}, "state": {"s1"},
	}.Encode()
}

func (h *oauthHarness) newClient(t *testing.T) string {
	t.Helper()
	cl, err := h.oauth.CreateClient(context.Background(), oauth.NewClient{Name: "CLI", RedirectURIs: []string{"http://127.0.0.1:43210/callback"}})
	if err != nil {
		t.Fatal(err)
	}
	return cl.ClientID
}

// pendingID follows an authorization URL and returns the request id.
func pendingID(t *testing.T, authURL string) string {
	t.Helper()
	resp, err := noRedirect.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc == nil || loc.Query().Get("request") == "" {
		t.Fatalf("authorize: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	return loc.Query().Get("request")
}

// Without a services domain the approval takes the password: a recent
// confirmation or the inline field (D24).
func TestOAuth_ApproveNeedsThePassword(t *testing.T) {
	h := newOAuthHarness(t, nil, nil)
	cid := h.newClient(t)
	c := unconfirmedClient(t, h.srv)
	id := pendingID(t, h.authURL(cid, h.srv.URL+"/mcp/burrow"))
	approve := func(body any) (int, string) {
		r := c.post(t, "/api/v1/oauth/requests/"+id+"/approve", body)
		b := readBody(t, r)
		return r.StatusCode, b
	}
	if code, b := approve(map[string]any{}); code != http.StatusForbidden || !strings.Contains(b, "reauth_required") {
		t.Fatalf("without the password: %d %s", code, b)
	}
	if code, b := approve(map[string]any{"password": "wrong"}); code != http.StatusForbidden || !strings.Contains(b, "incorrect") {
		t.Fatalf("wrong password: %d %s", code, b)
	}
	// The request waits through refusals.
	if code, b := approve(map[string]any{"password": "password1"}); code != http.StatusOK || !strings.Contains(b, "code=") {
		t.Fatalf("inline password: %d %s", code, b)
	}
	// The details say the password is required.
	id = pendingID(t, h.authURL(cid, h.srv.URL+"/mcp"))
	r := c.get(t, "/api/v1/oauth/requests/"+id)
	var req oauthRequestResp
	_ = json.Unmarshal([]byte(readBody(t, r)), &req)
	if !req.PasswordRequired || req.ResourceKind != "gateway" || !req.CanGrantTools || len(req.Sources) != 1 || req.Sources[0].Slug != "github" ||
		req.RedirectHost != "127.0.0.1" || !req.LocalhostRedirect || strings.Join(req.Scopes, " ") != "mcp:tools" {
		t.Fatalf("details %+v", req)
	}
	// A confirmed session approves without the field.
	c.reauth(t, "password1")
	back := h.consent(t, c, h.authURL(cid, h.srv.URL+"/mcp"), map[string]any{"allowed_tools": []string{"github__list_issues"}})
	if back.Query().Get("code") == "" || back.Query().Get("iss") != h.srv.URL {
		t.Fatalf("redirect %s", back)
	}
}

func TestOAuth_ApproveWithServicesDomainNeedsNoPassword(t *testing.T) {
	h := newOAuthHarness(t, nil, func(d *Deps) { d.AuthDomain, d.ServicesDomain = "burrow.example.com", "apps.example.com" })
	cid := h.newClient(t)
	c := unconfirmedClient(t, h.srv)
	id := pendingID(t, h.authURL(cid, h.srv.URL+"/mcp/burrow"))
	r := c.post(t, "/api/v1/oauth/requests/"+id+"/approve", map[string]any{})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("approve with a services domain: %d %s", r.StatusCode, readBody(t, r))
	}
}

func TestOAuth_DenyAndDecidedOnce(t *testing.T) {
	h := newOAuthHarness(t, nil, nil)
	cid := h.newClient(t)
	c := authedClient(t, h.srv)
	id := pendingID(t, h.authURL(cid, h.srv.URL+"/mcp/burrow"))
	r := c.post(t, "/api/v1/oauth/requests/"+id+"/deny", nil)
	b := readBody(t, r)
	if r.StatusCode != http.StatusOK || !strings.Contains(b, "error=access_denied") || !strings.Contains(b, "iss=") {
		t.Fatalf("deny: %d %s", r.StatusCode, b)
	}
	for _, path := range []string{"", "/approve", "/deny"} {
		var r *http.Response
		if path == "" {
			r = c.get(t, "/api/v1/oauth/requests/"+id)
		} else {
			r = c.post(t, "/api/v1/oauth/requests/"+id+path, map[string]any{})
		}
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("%q after the deny: %d", path, r.StatusCode)
		}
		_ = r.Body.Close()
	}
}

func TestOAuth_ApproveErrors(t *testing.T) {
	h := newOAuthHarness(t, nil, nil)
	cid := h.newClient(t)
	c := authedClient(t, h.srv)
	id := pendingID(t, h.authURL(cid, h.srv.URL+"/mcp/github"))
	for _, tc := range []struct {
		body   any
		status int
	}{
		{map[string]any{"scopes": []string{"burrow:read"}}, http.StatusBadRequest},
		{map[string]any{"allowed_tools": []string{"slack__*"}}, http.StatusBadRequest},
		{map[string]any{"allowed_tools": []string{"github"}}, http.StatusBadRequest},
	} {
		r := c.post(t, "/api/v1/oauth/requests/"+id+"/approve", tc.body)
		if r.StatusCode != tc.status {
			t.Errorf("%v: %d %s", tc.body, r.StatusCode, readBody(t, r))
		}
		_ = r.Body.Close()
	}
	if r := c.post(t, "/api/v1/oauth/requests/"+id+"/approve", map[string]any{}); r.StatusCode != http.StatusOK {
		t.Fatalf("after refused bodies: %d", r.StatusCode)
	}
}

// Connected apps: the user's own; every user's with oauth:clients:manage;
// revoking ends the tokens at once.
func TestOAuth_GrantsAPI(t *testing.T) {
	h := newOAuthHarness(t, nil, nil)
	ctx := context.Background()
	cid := h.newClient(t)
	c := authedClient(t, h.srv)
	back := h.consent(t, c, h.authURL(cid, h.srv.URL+"/mcp/burrow"), map[string]any{})
	code, tok := h.form(t, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {cid},
		"redirect_uri": {"http://127.0.0.1:43210/callback"}, "code_verifier": {"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
	})
	if code != http.StatusOK {
		t.Fatalf("exchange: %d %v", code, tok)
	}
	access, _ := tok["access_token"].(string)

	// Another user's grant, made directly.
	if err := h.x.CreateUser(ctx, db.User{ID: "u-other", Email: "other@x", PasswordHash: "h", Role: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := h.x.CreateOAuthGrant(ctx, db.OAuthGrant{ID: "g-other", ClientID: cid, ClientName: "CLI", UserID: "u-other", Resource: h.srv.URL + "/mcp/burrow", Scopes: []string{"burrow:read"}}, ""); err != nil {
		t.Fatal(err)
	}
	list := func(q string) []oauthGrantResp {
		r := c.get(t, "/api/v1/oauth/grants"+q)
		var out []oauthGrantResp
		_ = json.Unmarshal([]byte(readBody(t, r)), &out)
		return out
	}
	if own := list(""); len(own) != 1 || own[0].UserID != h.admin || own[0].LastUsed != nil {
		t.Fatalf("own: %+v", own)
	}
	if all := list("?all=1"); len(all) != 2 {
		t.Fatalf("all: %+v", all)
	}
	own := list("")
	if r := c.delete(t, "/api/v1/oauth/grants/"+own[0].ID); r.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if _, err := h.oauth.LookupAccessToken(ctx, access); err == nil {
		t.Fatal("the token of a revoked grant still works")
	}
	if r := c.delete(t, "/api/v1/oauth/grants/"+own[0].ID); r.StatusCode != http.StatusNotFound {
		t.Fatalf("second revoke: %d", r.StatusCode)
	}
	// An admin revokes another user's grant.
	if r := c.delete(t, "/api/v1/oauth/grants/g-other"); r.StatusCode != http.StatusNoContent {
		t.Fatalf("admin revoking another user's grant: %d", r.StatusCode)
	}
	if n := len(h.audit.byAction(audit.ActionOAuthGrantRevoke)); n != 2 {
		t.Fatalf("revoke audits: %d", n)
	}
}

func TestOAuth_ClientsAPI(t *testing.T) {
	h := newOAuthHarness(t, nil, nil)
	c := authedClient(t, h.srv)
	if r := c.post(t, "/api/v1/oauth/clients", map[string]any{"name": "X", "redirect_uris": []string{"http://evil.example/cb"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("http redirect: %d", r.StatusCode)
	}
	r := c.post(t, "/api/v1/oauth/clients", map[string]any{"name": "X", "redirect_uris": []string{"https://app.example/cb"}})
	var cl oauthClientResp
	_ = json.Unmarshal([]byte(readBody(t, r)), &cl)
	if r.StatusCode != http.StatusCreated || !strings.HasPrefix(cl.ClientID, "bcl_") || cl.CreatedBy != h.admin {
		t.Fatalf("create: %d %+v", r.StatusCode, cl)
	}
	r = c.get(t, "/api/v1/oauth/clients")
	var list []oauthClientResp
	_ = json.Unmarshal([]byte(readBody(t, r)), &list)
	if len(list) != 1 || list[0].ClientID != cl.ClientID {
		t.Fatalf("list %+v", list)
	}
	if r := c.delete(t, "/api/v1/oauth/clients/"+cl.ClientID); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if r := c.delete(t, "/api/v1/oauth/clients/"+cl.ClientID); r.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: %d", r.StatusCode)
	}
	if len(h.audit.byAction(audit.ActionOAuthClientDelete)) != 1 {
		t.Fatalf("audit:\n%s", h.audit.dump())
	}
}

// The endpoints of an MCP client: mounted on the root, rate-limited, and
// 404 while no authorization server is configured.
func TestOAuth_RootEndpoints(t *testing.T) {
	h := newOAuthHarness(t, nil, nil)
	resp, err := http.Get(h.srv.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(readBody(t, resp)), &m)
	if resp.StatusCode != http.StatusOK || m["issuer"] != h.srv.URL || m["token_endpoint"] != h.srv.URL+"/oauth/token" {
		t.Fatalf("metadata: %d %v", resp.StatusCode, m)
	}
	code, body := h.form(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"x"}})
	if code != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("token: %d %v", code, body)
	}

	bare := httptest.NewServer(NewRouter(Deps{Log: discardLog()}))
	t.Cleanup(bare.Close)
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/oauth/authorize"} {
		resp, err := http.Get(bare.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s without a server: %d", p, resp.StatusCode)
		}
	}
	resp, err = http.PostForm(bare.URL+"/oauth/token", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("token without a server: %d", resp.StatusCode)
	}

	// The authorization endpoint is limited per address.
	limited := 0
	for i := 0; i < OAuthAuthorizeRatePerIP+5; i++ {
		resp, err := noRedirect.Get(h.srv.URL + "/oauth/authorize?client_id=bcl_nope")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("the authorization endpoint is not rate-limited")
	}
}

// While no issuer is configured, the consent and grant routes refuse too,
// unless the request is loopback by Host and by peer.
func TestOAuth_RoutesOffWithoutIssuer(t *testing.T) {
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "off.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	h := newAuthzHarness(t, func(d *Deps) { d.OAuth = oauth.New(oauth.Config{Store: db.Wrap(sqldb)}) })
	session := h.session(t, "admin")
	for _, r := range []apiRoute{
		{http.MethodGet, "/api/v1/oauth/requests/{id}"},
		{http.MethodPost, "/api/v1/oauth/requests/{id}/approve"},
		{http.MethodPost, "/api/v1/oauth/requests/{id}/deny"},
		{http.MethodGet, "/api/v1/oauth/grants"},
		{http.MethodDelete, "/api/v1/oauth/grants/{id}"},
	} {
		rec := h.call(t, r, func(req *http.Request) {
			session(req)
			req.Host, req.RemoteAddr = "localhost", "203.0.113.9:4000"
		})
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "oauth is not configured") {
			t.Errorf("%s with Host localhost from a remote peer: %d %s", r.key(), rec.Code, rec.Body.String())
		}
	}
	rec := h.call(t, apiRoute{http.MethodGet, "/api/v1/oauth/grants"}, func(req *http.Request) {
		session(req)
		req.Host, req.RemoteAddr = "localhost", "127.0.0.1:4000"
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback by Host and peer: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	req.Host, req.RemoteAddr = "localhost", "203.0.113.9:4000"
	out := httptest.NewRecorder()
	h.router.ServeHTTP(out, req)
	if out.Code != http.StatusNotFound {
		t.Fatalf("metadata with Host localhost from a remote peer: %d", out.Code)
	}
}

// The SDK's own MCP client, given only the URL of /mcp/burrow: the 401
// challenge leads it to the protected resource metadata, the authorization
// server and the consent; then its access token lists and calls the tools as
// the user who approved, and the same token is refused at the REST API and at
// another resource.
func TestOAuth_SDKClientReachesTheManagementServer(t *testing.T) {
	h := newOAuthHarness(t, nil, nil)
	c := authedClient(t, h.srv)
	r := c.post(t, "/api/v1/oauth/clients", map[string]any{"name": "Agent", "redirect_uris": []string{"http://127.0.0.1:43210/callback"}})
	var cl struct {
		ClientID string `json:"client_id"`
	}
	if r.StatusCode != http.StatusCreated || json.Unmarshal([]byte(readBody(t, r)), &cl) != nil {
		t.Fatalf("create client: %d", r.StatusCode)
	}
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: &oauthex.ClientCredentials{ClientID: cl.ClientID, Issuer: h.srv.URL},
		RedirectURL:         "http://127.0.0.1:43210/callback",
		AuthorizationCodeFetcher: func(_ context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			back := h.consent(t, c, args.URL, map[string]any{"scopes": []string{"burrow:write"}})
			q := back.Query()
			return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "oauth-e2e", Version: "0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: h.srv.URL + "/mcp/burrow", OAuthHandler: handler, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect with OAuth: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "me_get", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("me_get: %v %+v", err, res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, `"email":"admin@x"`) {
		t.Fatalf("me_get = %s", text)
	}
	ts, err := handler.TokenSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/me", "/mcp", "/mcp/github"} {
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if strings.HasPrefix(path, "/api/") {
			req, _ = http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		// /mcp and /mcp/github answer 503 here (no tool gateway is wired);
		// what matters is that nothing answers 2xx with this token.
		if resp.StatusCode/100 == 2 {
			t.Errorf("%s with the /mcp/burrow token: %d", path, resp.StatusCode)
		}
	}
	// Revoking the grant ends the token at once.
	grants, _ := h.oauth.ListGrants(ctx, h.admin)
	if len(grants) != 1 {
		t.Fatalf("grants: %+v", grants)
	}
	if _, err := h.oauth.RevokeGrant(ctx, grants[0].ID, h.admin); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/mcp/burrow", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("revoked grant: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

// The protected resource metadata through the router: the root document,
// /mcp/burrow, /mcp and a source; 404 for an unknown source and without an
// authorization server.
func TestOAuth_ProtectedResourceMetadataRoutes(t *testing.T) {
	h := newOAuthHarness(t, nil, nil)
	for path, resource := range map[string]string{
		"/.well-known/oauth-protected-resource":            h.srv.URL,
		"/.well-known/oauth-protected-resource/mcp/burrow": h.srv.URL + "/mcp/burrow",
		"/.well-known/oauth-protected-resource/mcp":        h.srv.URL + "/mcp",
		"/.well-known/oauth-protected-resource/mcp/github": h.srv.URL + "/mcp/github",
	} {
		resp, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(readBody(t, resp)), &m)
		if resp.StatusCode != http.StatusOK || m["resource"] != resource {
			t.Errorf("%s: %d %v", path, resp.StatusCode, m)
		}
	}
	resp, err := http.Get(h.srv.URL + "/.well-known/oauth-protected-resource/mcp/nope")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown source: %d", resp.StatusCode)
	}
	off := httptest.NewServer(NewRouter(Deps{Log: discardLog()}))
	t.Cleanup(off.Close)
	resp, err = http.Get(off.URL + "/.well-known/oauth-protected-resource/mcp/burrow")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("without an authorization server: %d", resp.StatusCode)
	}
}
