package mcpgw

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/inspector"
	"github.com/ankoehn/burrow/internal/mcptest"
	"github.com/ankoehn/burrow/internal/oauth"
	"github.com/ankoehn/burrow/internal/quota"
	"github.com/ankoehn/burrow/internal/store"
)

// fakeOAuth satisfies OAuthTokens: tokens by plaintext, each good at the
// endpoint path in its Resource field.
type fakeOAuth struct {
	mu     sync.Mutex
	tokens map[string]oauth.Verified
	asked  []string // "token@path"
}

func (f *fakeOAuth) VerifyAccessToken(_ *http.Request, plain, path string) (oauth.Verified, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, plain+"@"+path)
	v, ok := f.tokens[plain]
	if !ok || v.Resource != path {
		return oauth.Verified{}, oauth.ErrInvalidToken
	}
	return v, nil
}

func (f *fakeOAuth) Challenge(_ *http.Request, path string, invalid bool) string {
	v := `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource` + path + `", scope="mcp:tools"`
	if invalid {
		v += `, error="invalid_token"`
	}
	return v
}

// grant installs an OAuth access token of the fixture's user, good at path
// only, with a gateway key's allow-list and read-only switch.
func (g *gwFixture) grant(t *testing.T, token, grantID, path string, tools []string, readOnly bool) {
	t.Helper()
	if g.oauth == nil {
		g.oauth = &fakeOAuth{tokens: map[string]oauth.Verified{}}
		g.newGateway(t, nil)
	}
	fo := g.oauth.(*fakeOAuth)
	fo.mu.Lock()
	defer fo.mu.Unlock()
	fo.tokens[token] = oauth.Verified{
		AccessToken: oauth.AccessToken{GrantID: grantID, UserID: g.userID, Resource: path, AllowedTools: tools, ToolsReadOnly: readOnly},
		Kind:        oauth.ResourceGateway,
		User:        db.User{ID: g.userID, Role: "admin", Status: "active"},
	}
}

func post(t *testing.T, url, auth, apiKey string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// A gateway grant behaves like a gateway key: its allow-list and read-only
// switch choose the tools, on /mcp and on /mcp/<slug>.
func TestOAuthGrant_ToolsLikeAKey(t *testing.T) {
	g, _ := newAggFixture(t)
	g.grant(t, "boa_agg", "g-agg", "/mcp", []string{"gh__echo", "other__*"}, false)
	g.grant(t, "boa_gh", "g-gh", "/mcp/gh", []string{"gh__*"}, true)

	cs := g.connect(t, "boa_agg", "", "")
	if got := strings.Join(names(t, cs), ","); got != "gh__echo,other__echo,other__ping" {
		t.Fatalf("/mcp grant: tools = %s", got)
	}
	if res, err := call(t, cs, "gh__echo", map[string]any{"text": "hi"}); err != nil || resultText(res) != "echo: hi" {
		t.Fatalf("gh__echo: %v %v", res, err)
	}
	if _, err := call(t, cs, "gh__mutate", map[string]any{"text": "x"}); !isUnknownTool(err) {
		t.Fatalf("gh__mutate outside the grant: %v", err)
	}
	// The read-only grant at /mcp/gh sees the tools the operator classed
	// read-only (the mock marks big, echo and slow).
	if got := strings.Join(names(t, g.connect(t, "boa_gh", "gh", "")), ","); got != "big,echo,slow" {
		t.Fatalf("/mcp/gh read-only grant: tools = %s", got)
	}
}

// A token is good only at the endpoint it was issued for; a token presented
// elsewhere gets 401 with error="invalid_token" and the metadata of the
// endpoint it was presented at.
func TestOAuthGrant_OnlyAtItsOwnResource(t *testing.T) {
	g, _ := newAggFixture(t)
	g.grant(t, "boa_agg", "g-agg", "/mcp", []string{"gh__*", "other__*"}, false)
	g.grant(t, "boa_gh", "g-gh", "/mcp/gh", []string{"gh__*"}, false)
	for _, c := range []struct{ token, at string }{
		{"boa_agg", "/mcp/gh"}, {"boa_agg", "/mcp/other"}, {"boa_gh", "/mcp"}, {"boa_gh", "/mcp/other"},
	} {
		r := post(t, g.srv.URL+c.at, "Bearer "+c.token, "")
		want := `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource` + c.at + `", scope="mcp:tools", error="invalid_token"`
		if r.StatusCode != http.StatusUnauthorized || r.Header.Get("WWW-Authenticate") != want {
			t.Errorf("%s at %s: %d %q", c.token, c.at, r.StatusCode, r.Header.Get("WWW-Authenticate"))
		}
	}
	fo := g.oauth.(*fakeOAuth)
	fo.mu.Lock()
	asked := slices.Clone(fo.asked)
	fo.mu.Unlock()
	if !slices.Contains(asked, "boa_agg@/mcp/gh") || !slices.Contains(asked, "boa_gh@/mcp") {
		t.Fatalf("the verifier was not asked for the endpoint presented at: %v", asked)
	}
	// An OAuth token is a bearer token: not taken from x-api-key.
	if r := post(t, g.srv.URL+"/mcp", "", "boa_agg"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("boa_ in x-api-key: %d", r.StatusCode)
	}
	if r := post(t, g.srv.URL+"/mcp", "Bearer boa_agg", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("at its own endpoint: %d", r.StatusCode)
	}
}

// The 401 points to the protected resource metadata; error="invalid_token"
// only when a credential was presented.
func TestOAuthGrant_Challenges(t *testing.T) {
	g, _ := newAggFixture(t)
	g.grant(t, "boa_agg", "g-agg", "/mcp", []string{"gh__*"}, false)
	for _, c := range []struct{ at, auth, key, want string }{
		{"/mcp", "", "", `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource/mcp", scope="mcp:tools"`},
		{"/mcp/gh", "", "", `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource/mcp/gh", scope="mcp:tools"`},
		{"/mcp", "Bearer bgw_nope", "", `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource/mcp", scope="mcp:tools", error="invalid_token"`},
		{"/mcp", "", "bgw_nope", `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource/mcp", scope="mcp:tools", error="invalid_token"`},
		{"/mcp/gh", "Bearer boa_unknown", "", `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource/mcp/gh", scope="mcp:tools", error="invalid_token"`},
	} {
		r := post(t, g.srv.URL+c.at, c.auth, c.key)
		if r.StatusCode != http.StatusUnauthorized || r.Header.Get("WWW-Authenticate") != c.want {
			t.Errorf("%s %q %q: %d %q", c.at, c.auth, c.key, r.StatusCode, r.Header.Get("WWW-Authenticate"))
		}
	}
}

// A grant's call is attributed to the grant: the usage row, the inspector
// entry, the audit row and the observer name the grant and no gateway key.
func TestOAuthGrant_CallsAreAttributedToTheGrant(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	g := newGWFixture(t, up)
	g.setPolicy(Policy{Inspect: true})
	g.grant(t, "boa_gh", "g-gh", "/mcp/gh", []string{"gh__*"}, false)
	cs := g.connect(t, "boa_gh", "gh", "")
	if _, err := call(t, cs, "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, cs, "fail", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}
	samples, events, calls := g.rec.snapshot()
	if len(samples) != 2 || samples[0].GatewayKeyID != "" || samples[0].OAuthGrantID != "g-gh" {
		t.Fatalf("usage rows = %+v", samples)
	}
	if len(events) == 0 {
		t.Fatal("no audit row")
	}
	for _, e := range events {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if p["oauth_grant_id"] != "g-gh" || p["gateway_key_id"] != nil || e.ActorID != g.userID {
			t.Fatalf("audit row %+v payload %v", e, p)
		}
	}
	if len(calls) != 2 || calls[0].OAuthGrantID != "g-gh" || calls[0].GatewayKeyID != "" {
		t.Fatalf("observer = %+v", calls)
	}
	entries := g.insp.Get(g.source(t).ServiceID).List(inspector.ListQuery{})
	if len(entries) != 2 || entries[0].OAuthGrantID != "g-gh" || entries[0].GatewayKeyID != "" {
		t.Fatalf("inspector entries = %+v", entries)
	}
}

// A grant's calls count against the rate limits that are not a key's: the
// backing service's and global ones.
func TestOAuthGrant_RateLimits(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	g := newGWFixture(t, up)
	g.quota = quota.NewWithStores(limitRows{{ID: "rl", Scope: quota.ScopeGlobal, Dimension: quota.DimensionRPM, Lim: 1, Burst: 1, Window: quota.WindowMinute}}, nil)
	if err := g.quota.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.grant(t, "boa_gh", "g-gh", "/mcp/gh", []string{"gh__*"}, false)
	g.newGateway(t, nil)
	cs := g.connect(t, "boa_gh", "gh", "")
	if _, err := call(t, cs, "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}
	_, err := call(t, cs, "echo", map[string]any{"text": "x"})
	limitErr(t, err, "rate_limit")
}

// A key (or grant) that names only gh__* sees nothing of another source on
// /mcp, cannot call it, and the other source is never asked.
func TestAggregated_OnlyTheNamedSource(t *testing.T) {
	g, up2 := newAggFixture(t)
	key, _ := g.key(t, g.userID, []string{"gh__*"}, false)
	g.grant(t, "boa_gh_only", "g1", "/mcp", []string{"gh__*"}, false)
	for _, who := range []string{key, "boa_gh_only"} {
		cs := g.connect(t, who, "", "")
		got := names(t, cs)
		if len(got) == 0 {
			t.Fatalf("%s: no tools", who[:4])
		}
		for _, n := range got {
			if !strings.HasPrefix(n, "gh__") {
				t.Fatalf("%s: %s is listed", who[:4], n)
			}
		}
		for _, tool := range []string{"other__echo", "other__ping"} {
			if _, err := call(t, cs, tool, map[string]any{"text": "x"}); !isUnknownTool(err) {
				t.Fatalf("%s: %s callable: %v", who[:4], tool, err)
			}
		}
		if res, err := call(t, cs, "gh__echo", map[string]any{"text": "y"}); err != nil || resultText(res) != "echo: y" {
			t.Fatalf("%s: gh__echo: %v %v", who[:4], res, err)
		}
	}
	if n := len(up2.CallsTo("echo")) + len(up2.CallsTo("ping")); n != 0 {
		t.Fatalf("the other source was called %d times", n)
	}
}

// An exact allow-list entry names a renamed tool by its served name on /mcp:
// "gh__say" reaches the tool natively called echo; "gh__echo" no longer
// does.
func TestAggregated_ExactEntryNamesTheRenamedTool(t *testing.T) {
	g, _ := newAggFixture(t)
	ren := "say"
	if _, err := g.st.UpdateMCPToolOverrides(context.Background(), "gh", "echo", store.MCPToolOverridesUpdate{Rename: &ren}); err != nil {
		t.Fatal(err)
	}
	served, _ := g.key(t, g.userID, []string{"gh__say"}, false)
	cs := g.connect(t, served, "", "")
	if got := strings.Join(names(t, cs), ","); got != "gh__say" {
		t.Fatalf("gh__say key: tools = %s", got)
	}
	if res, err := call(t, cs, "gh__say", map[string]any{"text": "r"}); err != nil || resultText(res) != "echo: r" {
		t.Fatalf("gh__say: %v %v", res, err)
	}
	before := len(g.up.CallsTo("echo"))
	native, _ := g.key(t, g.userID, []string{"gh__echo"}, false)
	cs = g.connect(t, native, "", "")
	if got := names(t, cs); len(got) != 0 {
		t.Fatalf("gh__echo key after the rename: tools = %v", got)
	}
	for _, tool := range []string{"gh__echo", "gh__say"} {
		if _, err := call(t, cs, tool, map[string]any{"text": "x"}); !isUnknownTool(err) {
			t.Fatalf("gh__echo key calls %s: %v", tool, err)
		}
	}
	if len(g.up.CallsTo("echo")) != before {
		t.Fatal("the upstream was called for a key that names the native name")
	}
}
