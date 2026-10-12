package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
)

// sessionRefused reports whether the answer is the 403 of the route's own
// gate for a dashboard session without any of the permissions.
func sessionRefused(a access, rec *httptest.ResponseRecorder) bool {
	if rec.Code != http.StatusForbidden {
		return false
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error == a.denied()
}

// A role's permissions bind its dashboard session: the session of a role that
// holds nothing is refused by the gate of every route but the allow-list.
func TestSessionOfARoleWithoutPermissionsIsRefusedEverywhere(t *testing.T) {
	h := newAuthzHarness(t)
	for _, r := range h.apiRoutes(t) {
		a := routeAccess[r.key()]
		// A session of its own for every call: logging out ends one.
		rec := h.call(t, r, h.session(t, "cr-none"))
		if a.open() {
			if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
				t.Errorf("%s is on the allow-list, yet answered %d", r.key(), rec.Code)
			}
			continue
		}
		if !sessionRefused(a, rec) {
			t.Errorf("%s: the session of a role without permissions got %d %s, want the gate's 403",
				r.key(), rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// An account whose role holds nothing gets no credential: a client token
// opens tunnels and a gateway key spends AI budget, and neither the control
// server nor the gateway looks at the role again.
func TestSessionOfARoleWithoutPermissionsGetsNoCredential(t *testing.T) {
	h := newAuthzHarness(t)
	calls := []struct {
		method, path string
		body         any
		user         int // what the built-in "user" role gets, as before
	}{
		{http.MethodPost, "/api/v1/tokens", map[string]string{"name": "laptop"}, http.StatusCreated},
		{http.MethodPost, "/api/v1/ai/keys", map[string]string{"name": "ci"}, http.StatusCreated},
		// One route each of five more areas.
		{http.MethodGet, "/api/v1/services", nil, http.StatusOK},
		{http.MethodGet, "/api/v1/sessions", nil, http.StatusOK},
		{http.MethodGet, "/api/v1/tunnels", nil, http.StatusOK},
		{http.MethodGet, "/api/v1/ai/models", nil, http.StatusOK},
		{http.MethodGet, "/api/v1/cost/pricing", nil, http.StatusOK},
		{http.MethodGet, "/api/v1/roles/permissions", nil, http.StatusOK},
		{http.MethodGet, "/api/v1/client/login/requests/ABCD-EFGH", nil, http.StatusNotFound},
	}
	for _, c := range calls {
		rec := h.do(t, c.method, c.path, c.body, h.session(t, "cr-none"))
		if rec.Code != http.StatusForbidden {
			t.Errorf("cr-none %s %s: %d, want 403", c.method, c.path, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "bur_") || strings.Contains(body, "bgw_") {
			t.Errorf("cr-none %s %s: the answer carries a credential: %s", c.method, c.path, body)
		}
		if rec := h.do(t, c.method, c.path, c.body, h.session(t, "user")); rec.Code != c.user {
			t.Errorf("user %s %s: %d, want %d", c.method, c.path, rec.Code, c.user)
		}
	}

	// What is about the account itself stays open to it.
	self := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/v1/me", http.StatusOK},
		{http.MethodPost, "/api/v1/auth/change-password", http.StatusBadRequest}, // {} names no password
		{http.MethodPost, "/api/v1/auth/logout", http.StatusNoContent},
	}
	for _, c := range self {
		var body any
		if c.method == http.MethodPost {
			body = map[string]string{}
		}
		if rec := h.do(t, c.method, c.path, body, h.session(t, "cr-none")); rec.Code != c.want {
			t.Errorf("cr-none %s %s: %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
}

// Every route that is not public answers 401 to a request without
// credentials, through the real router.
func TestEveryRouteWantsCredentials(t *testing.T) {
	h := newAuthzHarness(t)
	for _, r := range h.apiRoutes(t) {
		if routeAccess[r.key()].kind == accessPublic {
			continue
		}
		if rec := h.call(t, r, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials: %d, want 401", r.key(), rec.Code)
		}
	}
}

// The gate does not rely on the group a route is registered in: a request
// that reaches it without a user is not signed in.
func TestGateRefusesARequestWithoutAUser(t *testing.T) {
	// A store that would call anyone an admin.
	d := Deps{Users: &fakeUserStore{role: "admin"}, Log: discardLog()}
	audit := authz.PermAuditRead
	for name, a := range map[string]access{
		"self": self(), "needs": needs(audit), "dashboard": dashboard(audit),
		"every permission": needs(audit).andEveryPermission(),
	} {
		reached := false
		h := d.allow(a)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusUnauthorized || reached {
			t.Errorf("%s: status %d, handler reached %v; want 401 and false", name, rec.Code, reached)
		}
	}
	reached := false
	h := d.allow(public())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !reached {
		t.Error("public: the handler was not reached")
	}
}

// Restoring swaps an uploaded archive into the live database, so it takes
// every permission there is: backup:run alone does not restore.
func TestRestoreTakesEveryPermission(t *testing.T) {
	h := newAuthzHarness(t)
	ctx := context.Background()
	if err := h.st.CreateRole(ctx, "cr-backup", "backups only", []string{"backup:run"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sqldb.ExecContext(ctx,
		`INSERT INTO users(id, email, password_hash, role) VALUES('u-cr-backup','cr-backup@x','x','cr-backup')`); err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, p := range authz.AllPermissions() {
		all = append(all, string(p))
	}
	restore := apiRoute{method: http.MethodPost, pattern: "/api/v1/backups/restore"}
	// The harness uploads no archive: a caller who passes the gate gets the
	// handler's 400, one who does not gets 403 before the body is read.
	cases := []struct {
		name string
		auth func(*http.Request)
		want int
	}{
		{"an admin's session", h.session(t, "admin"), http.StatusBadRequest},
		{"an admin's token that declares every permission", h.token(t, "admin", all...), http.StatusBadRequest},
		{"an admin's token with backup:run only", h.token(t, "admin", "backup:run"), http.StatusForbidden},
		{"a session of a role with backup:run only", h.session(t, "cr-backup"), http.StatusForbidden},
		{"a session of a role that lacks the newest permissions", h.session(t, "cr-legacy"), http.StatusForbidden},
	}
	for _, c := range cases {
		rec := h.call(t, restore, c.auth)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
		if c.want == http.StatusForbidden && !strings.Contains(rec.Body.String(), "every permission") {
			t.Errorf("%s: the 403 does not say what is missing: %s", c.name, rec.Body)
		}
	}
	// backup:run still opens the other backup routes.
	list := apiRoute{method: http.MethodGet, pattern: "/api/v1/backups"}
	if rec := h.call(t, list, h.session(t, "cr-backup")); rec.Code != http.StatusOK {
		t.Errorf("GET /backups with backup:run: %d, want 200", rec.Code)
	}
}
