package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// fakeLive resolves every tunnel id to one service.
type fakeLive struct{ serviceID string }

func (f fakeLive) LookupByServiceID(string) (LiveTunnelSnapshot, bool) {
	return LiveTunnelSnapshot{}, false
}

func (f fakeLive) LookupByTunnelID(string) (TunnelLocator, bool) {
	return TunnelLocator{ServiceID: f.serviceID, UserID: "u-user"}, true
}

func (h *authzHarness) service(t *testing.T, id string) db.Service {
	t.Helper()
	svc, err := db.Wrap(h.sqldb).GetServiceByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func (h *authzHarness) keyCount(t *testing.T, serviceID string) int {
	t.Helper()
	keys, err := db.Wrap(h.sqldb).ListServiceAPIKeys(context.Background(), serviceID)
	if err != nil {
		t.Fatal(err)
	}
	return len(keys)
}

func (h *authzHarness) rows(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := h.sqldb.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The write routes behind actingRole: an admin's token that declares only the
// :own half is refused on another user's service and nothing changes; with
// the :any half it works.
func TestTokenOwnScopeWrites(t *testing.T) {
	const svc = "/api/v1/services/" + authzServiceID
	h := newAuthzHarness(t, func(d *Deps) { d.LiveTunnels = fakeLive{serviceID: authzServiceID} })
	ctx := context.Background()
	keyID, _, err := h.st.CreateAPIKey(ctx, "u-user", "user", authzServiceID, "k0")
	if err != nil {
		t.Fatal(err)
	}
	own := h.token(t, "admin", "services:configure:own")
	anyTok := h.token(t, "admin", "services:configure:any")

	cases := []struct {
		name    string
		method  string
		path    string
		body    any
		wantAny int
		changed func() bool
	}{
		{"access-mode", http.MethodPut, svc + "/access-mode", map[string]string{"access_mode": "api_key"}, http.StatusNoContent,
			func() bool { return h.service(t, authzServiceID).AccessMode != "open" }},
		{"tunnel access-mode", http.MethodPut, "/api/v1/tunnels/t1/access-mode", map[string]string{"access_mode": "burrow_login"}, http.StatusNoContent,
			func() bool { return h.service(t, authzServiceID).AccessMode == "burrow_login" }},
		{"create api key", http.MethodPost, svc + "/api-keys", map[string]string{"name": "k1"}, http.StatusCreated,
			func() bool { return h.keyCount(t, authzServiceID) > 1 }},
		{"delete api key", http.MethodDelete, svc + "/api-keys/" + keyID, nil, http.StatusNoContent,
			func() bool {
				keys, err := db.Wrap(h.sqldb).ListServiceAPIKeys(ctx, authzServiceID)
				if err != nil {
					t.Fatal(err)
				}
				for _, k := range keys {
					if k.ID == keyID {
						return false
					}
				}
				return true
			}},
		{"slug", http.MethodPut, svc + "/slug", map[string]string{"slug": "renamed"}, http.StatusOK,
			func() bool { return h.service(t, authzServiceID).Subdomain == "renamed" }},
		{"gateway-only", http.MethodPut, svc + "/gateway-only", map[string]bool{"gateway_only": true}, http.StatusOK,
			func() bool { return h.service(t, authzServiceID).GatewayOnly }},
	}
	// Refusals first, so that none of them sees the change of a success.
	for _, c := range cases {
		rec := h.do(t, c.method, c.path, c.body, own)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Errorf("%s with :own on another user's service: %d, want 403/404 (%s)", c.name, rec.Code, rec.Body)
		}
		if c.changed() {
			t.Errorf("%s with :own changed another user's service", c.name)
		}
	}
	for _, c := range cases {
		if rec := h.do(t, c.method, c.path, c.body, anyTok); rec.Code != c.wantAny {
			t.Errorf("%s with :any: %d, want %d (%s)", c.name, rec.Code, c.wantAny, rec.Body)
		}
		if !c.changed() {
			t.Errorf("%s with :any changed nothing", c.name)
		}
	}
}

// Revoking: the :own token of an admin does not reach another user's token.
func TestTokenOwnScopeRevoke(t *testing.T) {
	h := newAuthzHarness(t)
	view, _, err := h.st.MintAutomationToken(context.Background(), "u-user", "user", "theirs", []string{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/automation/tokens/" + view.ID
	own := h.token(t, "admin", "automation:tokens:manage:own")
	before := h.rows(t, "automation_tokens")
	if rec := h.do(t, http.MethodDelete, path, nil, own); rec.Code != http.StatusNotFound {
		t.Errorf("revoking another user's token with :own: %d, want 404", rec.Code)
	}
	if n := h.rows(t, "automation_tokens"); n != before {
		t.Errorf("a refused revoke changed the rows: %d, want %d", n, before)
	}
	anyTok := h.token(t, "admin", "automation:tokens:manage:any")
	if rec := h.do(t, http.MethodDelete, path, nil, anyTok); rec.Code != http.StatusNoContent {
		t.Errorf("revoking with :any: %d, want 204", rec.Code)
	}
}

// providerFixture adds an api-key service of owner with a tunnel provider on it.
func (h *authzHarness) providerFixture(t *testing.T, owner, serviceID, slug string) {
	t.Helper()
	ctx := context.Background()
	if err := h.st.CreateService(ctx, db.Service{
		ID: serviceID, UserID: owner, Name: serviceID, Type: "http", Subdomain: serviceID, AccessMode: "api_key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.CreateTunnelProvider(ctx, slug, slug, serviceID, "openai"); err != nil {
		t.Fatal(err)
	}
}

// Providers are seen by who sees the backing service.
func TestTokenOwnScopeProviders(t *testing.T) {
	h := newAuthzHarness(t)
	h.providerFixture(t, "u-user", "svc-prov", "prov")
	own := h.token(t, "admin", "ai:read:own")
	if got := decodeList(t, h.do(t, http.MethodGet, "/api/v1/ai/providers", nil, own)); len(got) != 0 {
		t.Errorf("providers with ai:read:own: %v, want none", got)
	}
	for _, p := range []string{"", "/metrics", "/models"} {
		if rec := h.do(t, http.MethodGet, "/api/v1/ai/providers/prov"+p, nil, own); rec.Code != http.StatusNotFound {
			t.Errorf("provider%s of another user with ai:read:own: %d, want 404", p, rec.Code)
		}
	}
	anyTok := h.token(t, "admin", "ai:read:any")
	if got := decodeList(t, h.do(t, http.MethodGet, "/api/v1/ai/providers", nil, anyTok)); len(got) != 1 {
		t.Errorf("providers with ai:read:any: %v, want one", got)
	}
	for _, p := range []string{"", "/metrics", "/models"} {
		if rec := h.do(t, http.MethodGet, "/api/v1/ai/providers/prov"+p, nil, anyTok); rec.Code != http.StatusOK {
			t.Errorf("provider%s with ai:read:any: %d, want 200", p, rec.Code)
		}
	}
}

// A token never gets more than its minter's role grants: a custom role that
// holds ai:read:own and not services:configure:own does not see the metrics
// of its own provider as a session, so its token does not either.
func TestTokenOfCustomRoleGetsNoMoreThanItsSession(t *testing.T) {
	h := newAuthzHarness(t)
	ctx := context.Background()
	if err := h.st.CreateRole(ctx, "cr-airead", "reads own ai", []string{"ai:read:own"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sqldb.ExecContext(ctx,
		`INSERT INTO users(id, email, password_hash, role) VALUES('u-cr-airead','cr@x','x','cr-airead')`); err != nil {
		t.Fatal(err)
	}
	h.providerFixture(t, "u-cr-airead", "svc-cr", "crprov")
	h.providerFixture(t, "u-user", "svc-own", "ownprov")
	metrics := "/api/v1/ai/providers/crprov/metrics"
	sessionCode := h.do(t, http.MethodGet, metrics, nil, h.session(t, "cr-airead")).Code
	if sessionCode != http.StatusNotFound {
		t.Fatalf("the custom role's session: %d, want 404", sessionCode)
	}
	_, plaintext, err := h.st.MintAutomationToken(ctx, "u-cr-airead", "cr-airead", "t", []string{"ai:read:own"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+plaintext) }
	if code := h.do(t, http.MethodGet, metrics, nil, tok).Code; code != sessionCode {
		t.Errorf("the custom role's token: %d, want %d like its session", code, sessionCode)
	}
	// The built-in user role does hold services:configure:own and keeps it.
	if code := h.do(t, http.MethodGet, "/api/v1/ai/providers/ownprov/metrics", nil, h.token(t, "user", "ai:read:own")).Code; code != http.StatusOK {
		t.Errorf("a user's token on its own provider: %d, want 200", code)
	}
}

// gateway_only is a security flag: a caller that may not configure the service
// is refused before anything is created.
func TestPostProviderGatewayOnlyNeedsConfigure(t *testing.T) {
	h := newAuthzHarness(t)
	ctx := context.Background()
	for _, id := range []string{"svc-a", "svc-b", "svc-c", "svc-d", "svc-e"} {
		owner := "u-user"
		if id == "svc-d" || id == "svc-e" {
			owner = "u-admin"
		}
		if err := h.st.CreateService(ctx, db.Service{
			ID: id, UserID: owner, Name: id, Type: "http", Subdomain: id, AccessMode: "api_key",
		}); err != nil {
			t.Fatal(err)
		}
	}
	post := func(auth func(*http.Request), svc, slug string) int {
		return h.do(t, http.MethodPost, "/api/v1/ai/providers",
			map[string]any{"name": slug, "slug": slug, "service_id": svc, "gateway_only": true}, auth).Code
	}
	provs := h.rows(t, "ai_providers")
	if code := post(h.token(t, "admin", "ai:providers:manage"), "svc-a", "prov-a"); code != http.StatusForbidden {
		t.Errorf("gateway_only with a token that may not configure the service: %d, want 403", code)
	}
	if n := h.rows(t, "ai_providers"); n != provs {
		t.Errorf("the refused request created %d provider(s)", n-provs)
	}
	if h.service(t, "svc-a").GatewayOnly {
		t.Error("the flag is set on svc-a")
	}
	if code := post(h.token(t, "admin", "ai:providers:manage", "services:configure:any"), "svc-b", "prov-b"); code != http.StatusCreated {
		t.Errorf("gateway_only with a token that may configure: %d, want 201", code)
	}
	if !h.service(t, "svc-b").GatewayOnly {
		t.Error("the flag is not set on svc-b")
	}
	// The admin's own service (svc-d, svc-e): the token still needs to declare
	// that it configures services.
	provs = h.rows(t, "ai_providers")
	if code := post(h.token(t, "admin", "ai:providers:manage"), "svc-d", "prov-d"); code != http.StatusForbidden {
		t.Errorf("gateway_only on the user's own service, token without configure: %d, want 403", code)
	}
	if n := h.rows(t, "ai_providers"); n != provs {
		t.Errorf("the refused request created %d provider(s)", n-provs)
	}
	if h.service(t, "svc-d").GatewayOnly {
		t.Error("the flag is set on svc-d")
	}
	if code := post(h.token(t, "admin", "ai:providers:manage", "services:configure:own"), "svc-e", "prov-e"); code != http.StatusCreated {
		t.Errorf("gateway_only on the user's own service, token with configure:own: %d, want 201", code)
	}
	if !h.service(t, "svc-e").GatewayOnly {
		t.Error("the flag is not set on svc-e")
	}
	if code := post(h.session(t, "admin"), "svc-c", "prov-c"); code != http.StatusCreated {
		t.Errorf("gateway_only by an admin session: %d, want 201", code)
	}
	if !h.service(t, "svc-c").GatewayOnly {
		t.Error("the flag is not set on svc-c")
	}
}

// A token that expires mints no token that outlives it.
func TestTokenCannotMintALongerLivedToken(t *testing.T) {
	h := newAuthzHarness(t)
	ctx := context.Background()
	exp := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	perms := []string{string(authz.PermAutomationTokensManageOwn)}
	_, pt, err := h.st.MintAutomationToken(ctx, "u-admin", "admin", "expiring", perms, &exp)
	if err != nil {
		t.Fatal(err)
	}
	expiring := func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+pt) }
	forever := h.token(t, "admin", perms...)
	mint := func(auth func(*http.Request), at *time.Time) int {
		return h.do(t, http.MethodPost, "/api/v1/automation/tokens",
			map[string]any{"name": "child", "permissions": []string{}, "expires_at": at}, auth).Code
	}
	later, earlier := exp.Add(time.Minute), exp.Add(-time.Minute)
	if code := mint(expiring, nil); code != http.StatusForbidden {
		t.Errorf("expiring parent, child without expiry: %d, want 403", code)
	}
	if code := mint(expiring, &later); code != http.StatusForbidden {
		t.Errorf("expiring parent, later child: %d, want 403", code)
	}
	if code := mint(expiring, &earlier); code != http.StatusCreated {
		t.Errorf("expiring parent, earlier child: %d, want 201", code)
	}
	if code := mint(expiring, &exp); code != http.StatusCreated {
		t.Errorf("expiring parent, child of equal expiry: %d, want 201", code)
	}
	if code := mint(forever, nil); code != http.StatusCreated {
		t.Errorf("parent without expiry, child without: %d, want 201", code)
	}
	if code := mint(forever, &later); code != http.StatusCreated {
		t.Errorf("parent without expiry, child with expiry: %d, want 201", code)
	}
	if code := mint(h.session(t, "admin"), nil); code != http.StatusCreated {
		t.Errorf("a session, child without expiry: %d, want 201", code)
	}
}
