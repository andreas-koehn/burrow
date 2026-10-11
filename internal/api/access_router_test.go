package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
)

// gateRefused reports whether the answer is the 403 of the route's own gate,
// as opposed to a 403 its handler or the store gave.
func gateRefused(t *testing.T, a access, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		return false
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if a.kind == accessDashboard {
		return body.Error == msgDashboardSession
	}
	return body.Error == a.denied()
}

// An automation token that an admin minted without any permission opens
// nothing but the allow-list.
func TestTokenWithoutPermissionsIsRefusedEverywhere(t *testing.T) {
	h := newAuthzHarness(t)
	token := h.token(t, "admin")
	for _, r := range h.apiRoutes(t) {
		a := routeAccess[r.key()]
		rec := h.call(t, r, token)
		if a.open() {
			if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
				t.Errorf("%s is on the allow-list, yet answered %d", r.key(), rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: an admin's token without permissions got %d, want 403", r.key(), rec.Code)
		}
	}
}

// A token with exactly one permission passes the gate of exactly the routes
// that name that permission.
func TestTokenWithOnePermissionOpensItsRoutesOnly(t *testing.T) {
	h := newAuthzHarness(t)
	routes := h.apiRoutes(t)
	for _, p := range authz.AllPermissions() {
		token := h.token(t, "admin", string(p))
		opened := 0
		for _, r := range routes {
			a := routeAccess[r.key()]
			if a.open() {
				continue
			}
			refused := gateRefused(t, a, h.call(t, r, token))
			if a.covers(p) {
				opened++
			}
			if refused == a.covers(p) {
				t.Errorf("token with %s on %s: refused by the gate = %v, covered = %v", p, r.key(), refused, a.covers(p))
			}
		}
		t.Logf("%-32s opens %d routes", p, opened)
	}
}

// The permission a token declares counts only while its user's role grants
// it: a token minted by an admin who was demoted since opens nothing more
// than the demoted role does.
func TestTokenOfADemotedMinter(t *testing.T) {
	h := newAuthzHarness(t)
	token := h.token(t, "admin", string(authz.PermUsersManage), string(authz.PermTokensManageOwn))
	get := func(path string) int { return h.do(t, http.MethodGet, path, nil, token).Code }
	if code := get("/api/v1/users"); code != http.StatusOK {
		t.Fatalf("before the demotion: GET /users = %d", code)
	}
	if _, err := h.sqldb.Exec(`UPDATE users SET role='user' WHERE id='u-admin'`); err != nil {
		t.Fatal(err)
	}
	if code := get("/api/v1/users"); code != http.StatusForbidden {
		t.Errorf("after the demotion: GET /users = %d, want 403", code)
	}
	// What the new role still grants keeps working.
	if code := get("/api/v1/tokens"); code != http.StatusOK {
		t.Errorf("after the demotion: GET /tokens = %d, want 200", code)
	}
}
