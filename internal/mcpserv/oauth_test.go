package mcpserv

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/oauth"
)

// fakeOAuth satisfies OAuthTokens: tokens by plaintext, each good at one path.
type fakeOAuth struct {
	mu     sync.Mutex
	tokens map[string]oauth.Verified // plaintext -> token; Resource holds the path
	asked  []string                  // "token@path"
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
	v := `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource` + path + `", scope="burrow:read"`
	if invalid {
		v += `, error="invalid_token"`
	}
	return v
}

const testAccess = "boa_testaccesstoken"

func newOAuthEnv(t *testing.T, perms []string, role string) (*testEnv, *fakeOAuth) {
	t.Helper()
	env := newTestEnv(t, []string{"audit:read"}, "admin")
	expires := time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC)
	fo := &fakeOAuth{tokens: map[string]oauth.Verified{
		testAccess: {
			AccessToken: oauth.AccessToken{GrantID: "grant1", ClientName: "Agent", UserID: "u2", Resource: EndpointPath, ExpiresAt: expires},
			Kind:        oauth.ResourceBurrow,
			User:        db.User{ID: "u2", Email: "agent-user@x", Role: role, Status: "active"},
			Permissions: perms,
		},
		"boa_gatewaytoken": {AccessToken: oauth.AccessToken{GrantID: "grant2", UserID: "u2", Resource: "/mcp"}},
	}}
	env.srv.SetOAuth(fo)
	return env, fo
}

// An OAuth access token for /mcp/burrow is accepted: the tools follow its
// permissions, and a call is replayed through the API without any
// Authorization header, the identity riding in the in-process marker.
func TestOAuth_AccessTokenAtTheManagementServer(t *testing.T) {
	env, fo := newOAuthEnv(t, []string{"audit:read"}, "admin")
	env.api.reply = `[]`
	cs := connectMCP(t, env.url, testAccess, testRevisions[0])
	if got := sortedNames(listedTools(t, cs)); got != "audit_events_list,connection_logs_list,me_get" {
		t.Fatalf("tools = %s", got)
	}
	res, err := callTool(t, cs, "audit_events_list", nil)
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	r := env.api.seen[len(env.api.seen)-1]
	if got := r.Header.Get("Authorization"); got != "" {
		t.Fatalf("the replay carries Authorization %q", got)
	}
	id, ok := ReplayIdentityFrom(r.Context())
	if !ok || id.GrantID != "grant1" || id.UserID != "u2" || !slices.Equal(id.Permissions, []string{"audit:read"}) ||
		!id.ExpiresAt.Equal(time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("replay identity = %+v, %v", id, ok)
	}
	// Round 2: the rows the replayed request writes name the grant too.
	if c, ok := audit.CredentialFrom(r.Context()); !ok || c != (audit.Credential{OAuthGrantID: "grant1"}) {
		t.Fatalf("replay audit credential = %+v, %v", c, ok)
	}
	fo.mu.Lock()
	asked := slices.Clone(fo.asked)
	fo.mu.Unlock()
	if len(asked) == 0 || asked[0] != testAccess+"@"+EndpointPath {
		t.Fatalf("verifier asked %v", asked)
	}
	// No automation token was looked up or touched.
	if env.bearer.touchCount() != 0 {
		t.Fatalf("an automation token was touched: %v", env.bearer.touched)
	}
	rows := env.audit.all()
	if len(rows) != 1 || rows[0].ActorID != "u2" || rows[0].ActorEmail != "agent-user@x" || rows[0].Result != "ok" {
		t.Fatalf("audit rows = %+v", rows)
	}
	// Final review: the row names the grant whose token made the call.
	if p := string(rows[0].Payload); p != `{"oauth_grant_id":"grant1"}` {
		t.Fatalf("audit payload = %s", p)
	}
}

// The token's permissions are intersected with the user's role (mayUse), as
// for an automation token.
func TestOAuth_PermissionsWithinTheRole(t *testing.T) {
	env, _ := newOAuthEnv(t, []string{"audit:read", "users:read"}, "user")
	cs := connectMCP(t, env.url, testAccess, testRevisions[0])
	if got := sortedNames(listedTools(t, cs)); strings.Contains(got, "audit_events_list") || strings.Contains(got, "users_list") {
		t.Fatalf("a user's token sees admin tools: %s", got)
	}
}

// An automation token's call still sends its own bearer and no marker.
func TestOAuth_AutomationTokenCarriesNoMarker(t *testing.T) {
	env, _ := newOAuthEnv(t, []string{"audit:read"}, "admin")
	cs := connectMCP(t, env.url, testBearer, testRevisions[0])
	if res, err := callTool(t, cs, "audit_events_list", nil); err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	r := env.api.seen[len(env.api.seen)-1]
	if _, ok := ReplayIdentityFrom(r.Context()); ok || r.Header.Get("Authorization") != "Bearer "+testBearer {
		t.Fatalf("automation replay: marker %v, Authorization %q", ok, r.Header.Get("Authorization"))
	}
	// Round 2: the rows the replayed request writes name the token.
	if c, ok := audit.CredentialFrom(r.Context()); !ok || c != (audit.Credential{TokenID: "tok1"}) {
		t.Fatalf("replay audit credential = %+v, %v", c, ok)
	}
	if _, ok := ReplayIdentityFrom(context.Background()); ok {
		t.Fatal("a plain context carries a replay identity")
	}
	// Final review: the audit row names the automation token.
	rows := env.audit.all()
	if len(rows) != 1 || string(rows[0].Payload) != `{"token_id":"tok1"}` {
		t.Fatalf("audit rows = %+v", rows)
	}
}

// A 401 points to the protected resource metadata: without a credential
// plainly, with a refused one (an OAuth token of another resource, an
// unknown token, an unknown automation token) with error="invalid_token".
func TestOAuth_Challenges(t *testing.T) {
	env, _ := newOAuthEnv(t, []string{"audit:read"}, "admin")
	plain := `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource/mcp/burrow", scope="burrow:read"`
	for _, c := range []struct{ bearer, want string }{
		{"", plain},
		{"boa_gatewaytoken", plain + `, error="invalid_token"`},
		{"boa_unknown", plain + `, error="invalid_token"`},
		{"bua_unknowntoken12345", plain + `, error="invalid_token"`},
		{"garbage", plain + `, error="invalid_token"`},
	} {
		resp, body := rawPOST(t, env.url, c.bearer, legacyHeader, rawToolsList)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != c.want {
			t.Errorf("%q: %d %q", c.bearer, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
		if strings.TrimSpace(body) != `{"error":"invalid bearer token"}` {
			t.Errorf("%q: body %s", c.bearer, body)
		}
	}
}

// Without an authorization server an OAuth token is just an invalid bearer.
func TestOAuth_NoServerNoOAuth(t *testing.T) {
	env := newTestEnv(t, []string{"audit:read"}, "admin")
	resp, _ := rawPOST(t, env.url, testAccess, legacyHeader, rawToolsList)
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}
