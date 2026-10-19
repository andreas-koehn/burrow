package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/authz"
)

// tokenAt returns an access token of userID (acting with role) for the
// resource path, with the given scope parameter.
func (h *harness) tokenAt(t *testing.T, path, scope, userID, role string) string {
	t.Helper()
	q := h.authParams()
	q.Set("resource", testIssuer+path)
	q.Set("scope", scope)
	got, err := h.s.Approve(context.Background(), h.requestID(t, q), Decision{UserID: userID, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got.RedirectURL)
	a := h.exchange(t, u.Query().Get("code"), func(f url.Values) { f.Set("resource", testIssuer+path) })
	if a.status != http.StatusOK {
		t.Fatalf("exchange: %d %v", a.status, a.body)
	}
	return a.str("access_token")
}

func mcpRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "https://relay.test/mcp", nil)
}

// A token is good at the resource it was issued for, and nowhere else (D22).
func TestVerifyAccessToken_OnlyAtItsOwnResource(t *testing.T) {
	h := newHarness(t)
	burrow := h.tokenAt(t, "/mcp/burrow", "burrow:read", "u-admin", "admin")
	gateway := h.tokenAt(t, "/mcp", "mcp:tools", "u-user", "user")
	source := h.tokenAt(t, "/mcp/github", "mcp:tools", "u-user", "user")
	for _, c := range []struct {
		name, token string
		at          string
		ok          bool
	}{
		{"burrow at burrow", burrow, "/mcp/burrow", true},
		{"burrow at /mcp", burrow, "/mcp", false},
		{"burrow at a source", burrow, "/mcp/github", false},
		{"gateway at /mcp", gateway, "/mcp", true},
		{"gateway at a source", gateway, "/mcp/github", false},
		{"gateway at burrow", gateway, "/mcp/burrow", false},
		{"source at its source", source, "/mcp/github", true},
		{"source at another source", source, "/mcp/other", false},
		{"source at /mcp", source, "/mcp", false},
		{"source at no endpoint", source, "/api/v1/services", false},
		{"source with a trailing slash", source, "/mcp/github/", false},
	} {
		v, err := h.s.VerifyAccessToken(mcpRequest(), c.token, c.at)
		if c.ok && (err != nil || v.Resource != testIssuer+c.at) {
			t.Errorf("%s: %+v %v", c.name, v, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: accepted (%v)", c.name, err)
		}
	}
	for _, bad := range []string{"", "boa_nope", "bua_x", "bgw_x"} {
		if _, err := h.s.VerifyAccessToken(mcpRequest(), bad, "/mcp/burrow"); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// A token presented at the wrong resource does not count as a use of its
// grant.
func TestVerifyAccessToken_RefusedUseIsNotRecorded(t *testing.T) {
	h := newHarness(t)
	gateway := h.tokenAt(t, "/mcp", "mcp:tools", "u-user", "user")
	if _, err := h.s.VerifyAccessToken(mcpRequest(), gateway, "/mcp/burrow"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal(err)
	}
	grants, _ := h.s.ListGrants(context.Background(), "u-user")
	if len(grants) != 1 || grants[0].LastUsed != nil {
		t.Fatalf("grant after a refused use: %+v", grants)
	}
	if _, err := h.s.VerifyAccessToken(mcpRequest(), gateway, "/mcp"); err != nil {
		t.Fatal(err)
	}
	grants, _ = h.s.ListGrants(context.Background(), "u-user")
	if grants[0].LastUsed == nil {
		t.Fatal("an accepted use is not recorded")
	}
}

// The user is checked at every use: gone, suspended, or (for gateway tools)
// no longer allowed to configure the gateway (D28a).
func TestVerifyAccessToken_UserAsTheyAreNow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	gateway := h.tokenAt(t, "/mcp", "mcp:tools", "u-user", "user")
	burrow := h.tokenAt(t, "/mcp/burrow", "burrow:read", "u-user", "user")

	authz.SetRoles(map[string][]authz.Permission{"viewer": {authz.PermTunnelsReadOwn}})
	t.Cleanup(func() { authz.SetRoles(nil) })
	if err := h.x.UpdateUserRole(ctx, "u-user", "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.VerifyAccessToken(mcpRequest(), gateway, "/mcp"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("gateway grant of a user without ai:configure: %v", err)
	}
	// The management token keeps working, with what the new role grants.
	v, err := h.s.VerifyAccessToken(mcpRequest(), burrow, "/mcp/burrow")
	if err != nil || strings.Join(v.Permissions, ",") != string(authz.PermTunnelsReadOwn) {
		t.Fatalf("burrow token after demotion: %+v %v", v.Permissions, err)
	}
	if err := h.x.UpdateUserRole(ctx, "u-user", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.VerifyAccessToken(mcpRequest(), gateway, "/mcp"); err != nil {
		t.Fatalf("role restored: %v", err)
	}

	if err := h.x.UpdateUserStatus(ctx, "u-user", "suspended"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tok, at string }{{gateway, "/mcp"}, {burrow, "/mcp/burrow"}} {
		if _, err := h.s.VerifyAccessToken(mcpRequest(), c.tok, c.at); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("suspended user at %s: %v", c.at, err)
		}
	}
	if err := h.x.UpdateUserStatus(ctx, "u-user", "active"); err != nil {
		t.Fatal(err)
	}
	// Gone: the user row is deleted with foreign keys off, so the grant
	// stays and only the user lookup can refuse it.
	conn, err := h.sqldb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM users WHERE id = 'u-user'`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close() // the pool may hold a single connection
	var grants int
	if err := h.sqldb.QueryRow(`SELECT COUNT(*) FROM oauth_grants WHERE user_id = 'u-user'`).Scan(&grants); err != nil || grants == 0 {
		t.Fatalf("the grant went with the user (%d, %v): the check below proves nothing", grants, err)
	}
	if _, err := h.s.VerifyAccessToken(mcpRequest(), gateway, "/mcp"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("user gone: %v", err)
	}
}

// At /mcp/burrow the permissions are the scopes intersected with the role
// now (D23): burrow:write is the whole role, burrow:read its reads, a
// permission string itself.
func TestVerifyAccessToken_ScopesWithinTheRole(t *testing.T) {
	h := newHarness(t)
	role := func(name string) []string {
		var out []string
		for _, p := range authz.AllPermissions() {
			if authz.Can(name, p) {
				out = append(out, string(p))
			}
		}
		return out
	}
	userAll := role("user")
	for _, c := range []struct {
		scope, user, role string
		want              func(string) bool
	}{
		{"burrow:write", "u-user", "user", func(p string) bool { return slices.Contains(userAll, p) }},
		{"burrow:read", "u-user", "user", func(p string) bool { return slices.Contains(userAll, p) && strings.Contains(p, ":read") }},
		{"burrow:read", "u-admin", "admin", func(p string) bool { return strings.Contains(p, ":read") }},
		{"tunnels:read:own", "u-user", "user", func(p string) bool { return p == "tunnels:read:own" }},
	} {
		tok := h.tokenAt(t, "/mcp/burrow", c.scope, c.user, c.role)
		v, err := h.s.VerifyAccessToken(mcpRequest(), tok, "/mcp/burrow")
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, p := range authz.AllPermissions() {
			if c.want(string(p)) {
				want = append(want, string(p))
			}
		}
		got := slices.Clone(v.Permissions)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) || v.Kind != ResourceBurrow || v.User.ID != c.user {
			t.Errorf("%s as %s:\n got %v\nwant %v", c.scope, c.role, got, want)
		}
	}
	// A gateway token carries no management permissions.
	v, err := h.s.VerifyAccessToken(mcpRequest(), h.tokenAt(t, "/mcp", "mcp:tools", "u-user", "user"), "/mcp")
	if err != nil || v.Permissions != nil || v.Kind != ResourceGateway {
		t.Fatalf("gateway: %+v %v", v, err)
	}
}

// A revoked grant's token is refused at once.
func TestVerifyAccessToken_RevokedGrant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tok := h.tokenAt(t, "/mcp", "mcp:tools", "u-user", "user")
	grants, _ := h.s.ListGrants(ctx, "u-user")
	if _, err := h.s.RevokeGrant(ctx, grants[0].ID, "u-user"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.VerifyAccessToken(mcpRequest(), tok, "/mcp"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked: %v", err)
	}
}

// Without an issuer (and not loopback) the resource side is off too: no
// token is accepted, no metadata is served and the challenge is bare.
func TestResourceSide_OffWithoutIssuer(t *testing.T) {
	h := newHarness(t)
	tok := h.tokenAt(t, "/mcp", "mcp:tools", "u-user", "user")
	off := New(Config{Store: h.x, Now: h.clk.now})
	r := httptest.NewRequest(http.MethodPost, "https://relay.test/mcp", nil)
	if _, err := off.VerifyAccessToken(r, tok, "/mcp"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("off: %v", err)
	}
	if got := off.Challenge(r, "/mcp", true); got != "Bearer" {
		t.Fatalf("challenge when off: %q", got)
	}
	if off.ResourceMetadataURL(r, "/mcp") != "" {
		t.Fatal("metadata URL when off")
	}
	rec := httptest.NewRecorder()
	off.ProtectedResource(rec, httptest.NewRequest(http.MethodGet, "https://relay.test/.well-known/oauth-protected-resource/mcp", nil), "/mcp")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("metadata when off: %d", rec.Code)
	}
}

func TestChallenge(t *testing.T) {
	h := newHarness(t)
	r := mcpRequest()
	for _, c := range []struct {
		path    string
		invalid bool
		want    string
	}{
		{"/mcp/burrow", false, `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource/mcp/burrow", scope="burrow:read"`},
		{"/mcp/burrow", true, `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource/mcp/burrow", scope="burrow:read", error="invalid_token"`},
		{"/mcp", false, `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource/mcp", scope="mcp:tools"`},
		{"/mcp/github", true, `Bearer resource_metadata="https://relay.test/.well-known/oauth-protected-resource/mcp/github", scope="mcp:tools", error="invalid_token"`},
		{"/elsewhere", true, `Bearer`},
	} {
		if got := h.s.Challenge(r, c.path, c.invalid); got != c.want {
			t.Errorf("Challenge(%s, %v) = %s\nwant %s", c.path, c.invalid, got, c.want)
		}
	}
}

// The protected resource metadata (RFC 9728) of each MCP endpoint and of the
// origin; 404 for an unknown or disabled source and for any other path.
func TestProtectedResource(t *testing.T) {
	h := newHarness(t)
	get := func(path string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		h.s.ProtectedResource(rec, httptest.NewRequest(http.MethodGet, "https://relay.test"+ProtectedResourcePath+path, nil), path)
		m := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec, m
	}
	for _, c := range []struct{ path, scopes, name string }{
		{"", "burrow:read burrow:write mcp:tools", "Burrow"},
		{"/mcp/burrow", "burrow:read burrow:write", "Burrow management"},
		{"/mcp", "mcp:tools", "Burrow tools"},
		{"/mcp/github", "mcp:tools", "Burrow tools"},
	} {
		rec, m := get(c.path)
		var scopes []string
		for _, s := range m["scopes_supported"].([]any) {
			scopes = append(scopes, s.(string))
		}
		if rec.Code != http.StatusOK || m["resource"] != testIssuer+c.path || m["resource_name"] != c.name ||
			strings.Join(scopes, " ") != c.scopes || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%q: %d %v", c.path, rec.Code, m)
		}
		if as, _ := m["authorization_servers"].([]any); len(as) != 1 || as[0] != testIssuer {
			t.Errorf("%q: authorization_servers %v", c.path, m["authorization_servers"])
		}
	}
	for _, p := range []string{"/mcp/off", "/mcp/nope", "/mcp/Bad", "/api/v1", "/mcp/github/x"} {
		if rec, _ := get(p); rec.Code != http.StatusNotFound {
			t.Errorf("%q: %d", p, rec.Code)
		}
	}
}

// An expired access token is refused at its own resource.
func TestVerifyAccessToken_Expired(t *testing.T) {
	h := newHarness(t)
	tok := h.tokenAt(t, "/mcp", "mcp:tools", "u-user", "user")
	h.clk.add(AccessTokenTTL - time.Second)
	if _, err := h.s.VerifyAccessToken(mcpRequest(), tok, "/mcp"); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	h.clk.add(time.Second)
	if _, err := h.s.VerifyAccessToken(mcpRequest(), tok, "/mcp"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired: %v", err)
	}
}
