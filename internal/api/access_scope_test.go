package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
)

// An admin's token that declares only the :own half of a permission reaches
// the admin's own resources and nobody else's; the :any half opens the rest.
// The harness's service belongs to the plain user, so for the admin's token
// it is somebody else's.
func TestTokenOwnScopeStopsAtOwnResources(t *testing.T) {
	cases := []struct {
		own, any authz.Permission
		route    apiRoute
	}{
		{authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny, apiRoute{http.MethodGet, "/api/v1/services/{serviceID}"}},
		{authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny, apiRoute{http.MethodGet, "/api/v1/services/{serviceID}/api-keys"}},
		{authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny, apiRoute{http.MethodGet, "/api/v1/services/{serviceID}/access-policy"}},
		{authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny, apiRoute{http.MethodPut, "/api/v1/services/{serviceID}/access-policy"}},
		{authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny, apiRoute{http.MethodGet, "/api/v1/services/{serviceID}/domains"}},
		{authz.PermIPGeoManageOwn, authz.PermIPGeoManageAny, apiRoute{http.MethodGet, "/api/v1/services/{serviceID}/ip-geo"}},
		{authz.PermIPGeoManageOwn, authz.PermIPGeoManageAny, apiRoute{http.MethodPut, "/api/v1/services/{serviceID}/ip-geo"}},
		{authz.PermAIConfigureOwn, authz.PermAIConfigureAny, apiRoute{http.MethodGet, "/api/v1/services/{serviceID}/ai-config"}},
		{authz.PermAIConfigureOwn, authz.PermAIConfigureAny, apiRoute{http.MethodPut, "/api/v1/services/{serviceID}/ai-config"}},
		{authz.PermAIConfigureOwn, authz.PermAIConfigureAny, apiRoute{http.MethodDelete, "/api/v1/services/{serviceID}/cache/entries"}},
		{authz.PermAIConfigureOwn, authz.PermAIConfigureAny, apiRoute{http.MethodGet, "/api/v1/services/{serviceID}/upstream-credential"}},
		{authz.PermInspectorReadOwn, authz.PermInspectorReadAny, apiRoute{http.MethodGet, "/api/v1/services/{serviceID}/inspector/requests"}},
	}
	h := newAuthzHarness(t)
	for _, c := range cases {
		ownTok := h.token(t, "admin", string(c.own))
		if rec := h.call(t, c.route, ownTok); rec.Code != http.StatusForbidden {
			t.Errorf("%s with %s on another user's service: %d, want 403", c.route.key(), c.own, rec.Code)
		}
		anyTok := h.token(t, "admin", string(c.any))
		if rec := h.call(t, c.route, anyTok); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("%s with %s: %d, want it to pass", c.route.key(), c.any, rec.Code)
		}
		// The owner's own token with the :own half does pass.
		mine := h.token(t, "user", string(c.own))
		if rec := h.call(t, c.route, mine); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("%s with the owner's %s: %d, want it to pass", c.route.key(), c.own, rec.Code)
		}
	}
}

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status %d, body %s: %v", rec.Code, rec.Body, err)
	}
	return out
}

// Lists follow the same rule: the :own token of an admin lists the admin's
// rows only.
func TestTokenOwnScopeLists(t *testing.T) {
	h := newAuthzHarness(t)
	services := apiRoute{http.MethodGet, "/api/v1/services"}
	if got := decodeList(t, h.call(t, services, h.token(t, "admin", "services:configure:own"))); len(got) != 0 {
		t.Errorf("services with :own: %v, want none (the admin owns no service)", got)
	}
	if got := decodeList(t, h.call(t, services, h.token(t, "admin", "services:configure:any"))); len(got) != 1 {
		t.Errorf("services with :any: %v, want the one service", got)
	}

	// Automation tokens: one of the plain user, then the admin's own.
	if _, _, err := h.st.MintAutomationToken(context.Background(), "u-user", "user", "theirs", []string{}, nil); err != nil {
		t.Fatal(err)
	}
	tokens := apiRoute{http.MethodGet, "/api/v1/automation/tokens"}
	for _, row := range decodeList(t, h.call(t, tokens, h.token(t, "admin", "automation:tokens:manage:own"))) {
		if row["user_id"] != "u-admin" {
			t.Errorf("automation tokens with :own lists a token of %v", row["user_id"])
		}
	}
	found := false
	for _, row := range decodeList(t, h.call(t, tokens, h.token(t, "admin", "automation:tokens:manage:any"))) {
		found = found || row["user_id"] == "u-user"
	}
	if !found {
		t.Error("automation tokens with :any does not list the other user's token")
	}
}

// A token mints nothing wider than itself: every permission it asks for must
// be one it declares.
func TestTokenCannotMintAWiderToken(t *testing.T) {
	h := newAuthzHarness(t)
	mint := func(auth func(*http.Request), perms ...string) int {
		body, _ := json.Marshal(map[string]any{"name": "child", "permissions": perms})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/automation/tokens", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		auth(req)
		rec := httptest.NewRecorder()
		h.router.ServeHTTP(rec, req)
		return rec.Code
	}
	narrow := h.token(t, "admin", "automation:tokens:manage:own")
	if code := mint(narrow, "users:manage"); code != http.StatusForbidden {
		t.Errorf("minting users:manage with a token that does not declare it: %d, want 403", code)
	}
	if code := mint(narrow, "automation:tokens:manage:own"); code != http.StatusCreated {
		t.Errorf("minting what the token itself declares: %d, want 201", code)
	}
	if code := mint(narrow); code != http.StatusCreated {
		t.Errorf("minting a token without permissions: %d, want 201", code)
	}
	// The token that declares users:manage and the mint permission only is
	// narrower than its admin: it mints nothing beyond what it declares, not
	// even every permission the admin role holds, and nothing is written.
	count := func() int {
		var n int
		if err := h.sqldb.QueryRow(`SELECT COUNT(*) FROM automation_tokens`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var all []string
	for _, p := range authz.AllPermissions() {
		all = append(all, string(p))
	}
	two := h.token(t, "admin", "users:manage", "automation:tokens:manage:own")
	before := count()
	if code := mint(two, all...); code != http.StatusForbidden {
		t.Errorf("minting every admin permission with a token that declares two: %d, want 403", code)
	}
	if n := count(); n != before {
		t.Errorf("a refused mint wrote %d token(s)", n-before)
	}
	if code := mint(two, "users:manage"); code != http.StatusCreated {
		t.Errorf("minting a subset of what the token declares: %d, want 201", code)
	}
	if code := mint(two, "users:manage", "automation:tokens:manage:own"); code != http.StatusCreated {
		t.Errorf("minting exactly what the token declares: %d, want 201", code)
	}
	// A dashboard session still mints whatever its role grants.
	if code := mint(h.session(t, "admin"), "users:manage"); code != http.StatusCreated {
		t.Errorf("an admin's session minting users:manage: %d, want 201", code)
	}
}

// Usage of a model is everyone's usage: quotas:read:own does not open it to a
// token, whoever minted it.
func TestTokenQuotaReadOwnDoesNotReadModelUsage(t *testing.T) {
	h := newAuthzHarness(t)
	usage := func(auth func(*http.Request)) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/rate-limits/usage?scope=model&subject=m", nil)
		auth(req)
		rec := httptest.NewRecorder()
		h.router.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := usage(h.token(t, "admin", "quotas:read:own")); code != http.StatusForbidden {
		t.Errorf("model usage with quotas:read:own: %d, want 403", code)
	}
	if code := usage(h.token(t, "admin", "quotas:read:any")); code != http.StatusOK {
		t.Errorf("model usage with quotas:read:any: %d, want 200", code)
	}
}

// Only access.go and bearer_middleware.go decide who may pass. A handler that
// compares a role name or asks authz.Can on its own would ignore what an
// automation token declares.
func TestNoHandlerGatesOnItsOwn(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "bearer_middleware.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			code, _, _ := strings.Cut(line, "//")
			for _, bad := range []string{"authz.Can(", `ole == "admin"`, `ole != "admin"`} {
				if strings.Contains(code, bad) {
					t.Errorf("%s:%d: %q: ask effectivePerms (or declare the route in access.go) instead", f, i+1, strings.TrimSpace(line))
					break
				}
			}
		}
	}
}
