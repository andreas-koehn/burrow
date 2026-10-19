package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
)

// reauthGuarded are the routes behind RequireRecentAuth (T02b): the ones that
// mint a secret or change who may do what. The approve route of a client
// sign-in has tests of its own in client_login_handlers_test.go.
var reauthGuarded = []apiRoute{
	{http.MethodPost, "/api/v1/tokens"},
	{http.MethodPost, "/api/v1/automation/tokens"},
	{http.MethodPost, "/api/v1/services/{serviceID}/api-keys"},
	{http.MethodPost, "/api/v1/ai/keys"},
	{http.MethodPut, "/api/v1/ai/keys/{id}"},
	{http.MethodPost, "/api/v1/users"},
	{http.MethodPatch, "/api/v1/users/{id}"},
	{http.MethodDelete, "/api/v1/users/{id}"},
	{http.MethodPost, "/api/v1/roles"},
	{http.MethodPut, "/api/v1/roles/{name}"},
	{http.MethodDelete, "/api/v1/roles/{name}"},
	{http.MethodPut, "/api/v1/settings"},
	{http.MethodGet, "/api/v1/backups/{id}/download"},
	{http.MethodPost, "/api/v1/backups/restore"},
	{http.MethodDelete, "/api/v1/sessions/{id}"},
	{http.MethodPost, "/api/v1/sessions/revoke-all"},
	// Fix round 1.
	{http.MethodPost, "/api/v1/auth/change-password"},
	{http.MethodPost, "/api/v1/ai/providers"},
	{http.MethodPut, "/api/v1/ai/providers/{slug}"},
	{http.MethodPut, "/api/v1/ai/providers/{slug}/upstream"},
	{http.MethodPost, "/api/v1/ai/providers/{slug}/models/sync"},
	{http.MethodPut, "/api/v1/services/{serviceID}/upstream-credential"},
	{http.MethodDelete, "/api/v1/services/{serviceID}/upstream-credential"},
	{http.MethodPut, "/api/v1/tunnels/{id}/access-mode"},
	{http.MethodPut, "/api/v1/services/{serviceID}/access-mode"},
	{http.MethodPut, "/api/v1/services/{serviceID}/access-policy"},
	{http.MethodPut, "/api/v1/services/{serviceID}/ip-geo"},
	{http.MethodPut, "/api/v1/services/{serviceID}/gateway-only"},
	{http.MethodPut, "/api/v1/redaction/settings"},
	{http.MethodPost, "/api/v1/redaction/rules"},
	{http.MethodPut, "/api/v1/redaction/rules/{id}"},
	{http.MethodDelete, "/api/v1/redaction/rules/{id}"},
	{http.MethodPost, "/api/v1/webhooks"},
	{http.MethodPut, "/api/v1/webhooks/{id}"},
	{http.MethodPut, "/api/v1/settings/retention"},
	{http.MethodPost, "/api/v1/services/{serviceID}/domains"},
	{http.MethodPut, "/api/v1/services/{serviceID}/domains/{did}"},
	{http.MethodPost, "/api/v1/settings/test-email"},
	// Tool sources carry a URL and a credential slot name.
	{http.MethodPost, "/api/v1/mcp/sources"},
	{http.MethodPut, "/api/v1/mcp/sources/{slug}"},
	{http.MethodDelete, "/api/v1/mcp/sources/{slug}"},
	// A sync calls the upstream; approvals and overrides change what
	// clients are served.
	{http.MethodPost, "/api/v1/mcp/sources/{slug}/sync"},
	{http.MethodPut, "/api/v1/mcp/sources/{slug}/tools/{name}"},
	{http.MethodPost, "/api/v1/mcp/sources/{slug}/tools/{name}/approve"},
	{http.MethodPost, "/api/v1/mcp/sources/{slug}/approve-all"},
	// OAuth: a consent approval (the password inline or a recent
	// confirmation, like a client sign-in) and a new client's redirect URIs.
	{http.MethodPost, "/api/v1/oauth/requests/{id}/approve"},
	{http.MethodPost, "/api/v1/oauth/clients"},
}

func errorOf(rec *httptest.ResponseRecorder) string {
	var b struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return b.Error
}

// Without the window every guarded route answers 403 reauth_required, and
// inside it none does.
func TestReauth_GuardedRoutes(t *testing.T) {
	h := newAuthzHarness(t)
	for _, r := range reauthGuarded {
		// Fresh sessions per route: revoke-all and the password change end the
		// user's other sessions.
		cold := h.sessionUnconfirmed(t, "admin")
		warm := h.session(t, "admin")
		if _, ok := routeAccess[r.key()]; !ok {
			t.Fatalf("%s is not a route", r.key())
		}
		rec := h.call(t, r, cold)
		if rec.Code != http.StatusForbidden || errorOf(rec) != "reauth_required" {
			t.Errorf("%s without the window: %d %s, want 403 reauth_required", r.key(), rec.Code, rec.Body.String())
		}
		rec = h.call(t, r, warm)
		if errorOf(rec) == "reauth_required" {
			t.Errorf("%s inside the window: still reauth_required", r.key())
		}
	}
}

// A session of a role without the permission is told so, not asked for its
// password: the gate of the route runs first.
func TestReauth_PermissionGateComesFirst(t *testing.T) {
	h := newAuthzHarness(t)
	rec := h.call(t, apiRoute{http.MethodPost, "/api/v1/users"}, h.sessionUnconfirmed(t, "user"))
	if rec.Code != http.StatusForbidden || errorOf(rec) == "reauth_required" {
		t.Fatalf("user creating a user: %d %s, want the permission error", rec.Code, rec.Body.String())
	}
}

// An automation token carries no cookie a page could ride on: exempt.
func TestReauth_BearerIsExempt(t *testing.T) {
	h := newAuthzHarness(t)
	perms := make([]string, 0)
	for _, p := range authz.AllPermissions() {
		perms = append(perms, string(p))
	}
	token := h.token(t, "admin", perms...)
	for _, r := range reauthGuarded {
		if rec := h.call(t, r, token); errorOf(rec) == "reauth_required" {
			t.Errorf("%s with a bearer token: reauth_required", r.key())
		}
	}
}

// With a services domain no app runs on the dashboard's origin: guard off.
func TestReauth_OffWithServicesDomain(t *testing.T) {
	h := newLoginHarness(t, func(d *Deps) {
		d.AuthDomain, d.ServicesDomain = "burrow.example.com", "apps.example.com"
	})
	c := unconfirmedClient(t, h.srv)
	resp := c.post(t, "/api/v1/tokens", map[string]string{"name": "laptop"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint with a services domain and no confirmation: %d %s", resp.StatusCode, readBody(t, resp))
	}
}

// The window is ten minutes long, per session, and only the right password
// opens it.
func TestReauth_WindowLifecycle(t *testing.T) {
	clk := &loginTestClock{t: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)}
	h := newLoginHarness(t, func(d *Deps) { d.reauthClock = clk.now })
	c := unconfirmedClient(t, h.srv)
	other := unconfirmedClient(t, h.srv) // a second session of the same user
	mint := func(c *authClient) (int, string) {
		resp := c.post(t, "/api/v1/tokens", map[string]string{"name": "laptop"})
		defer resp.Body.Close()
		var b struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&b)
		return resp.StatusCode, b.Error
	}

	if code, e := mint(c); code != http.StatusForbidden || e != "reauth_required" {
		t.Fatalf("before: %d %q", code, e)
	}
	// A wrong password does not open it.
	resp := c.post(t, "/api/v1/auth/reauth", map[string]string{"password": "nope"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong password: %d, want 403", resp.StatusCode)
	}
	if resp := c.post(t, "/api/v1/auth/reauth", map[string]string{}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no password: %d, want 400", resp.StatusCode)
	}
	if code, _ := mint(c); code != http.StatusForbidden {
		t.Fatalf("after a wrong password: %d, want 403", code)
	}

	resp = c.post(t, "/api/v1/auth/reauth", map[string]string{"password": "password1"})
	var ok struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reauth: %d", resp.StatusCode)
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &ok); err != nil {
		t.Fatal(err)
	}
	if want := clk.now().Add(10 * time.Minute); !ok.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at %v, want %v", ok.ExpiresAt, want)
	}
	if code, e := mint(c); code != http.StatusCreated {
		t.Fatalf("inside the window: %d %q, want 201", code, e)
	}
	// Per session: the other one is still cold.
	if code, e := mint(other); code != http.StatusForbidden || e != "reauth_required" {
		t.Fatalf("other session: %d %q, want reauth_required", code, e)
	}
	clk.add(9*time.Minute + 59*time.Second)
	if code, _ := mint(c); code != http.StatusCreated {
		t.Fatalf("9m59s in: %d, want 201", code)
	}
	clk.add(2 * time.Second)
	if code, e := mint(c); code != http.StatusForbidden || e != "reauth_required" {
		t.Fatalf("after 10 minutes: %d %q, want reauth_required", code, e)
	}
	// Confirming again opens a new window.
	c.reauth(t, "password1")
	if code, _ := mint(c); code != http.StatusCreated {
		t.Fatalf("after confirming again: %d, want 201", code)
	}
}

// reauthAudit keeps what the handler appends.
type reauthAudit struct {
	mu   sync.Mutex
	rows []audit.Event
}

func (a *reauthAudit) Append(_ context.Context, e audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, e)
	return nil
}

// The token has no browser session to give a window to; the password is
// limited like the sign-in; both outcomes are audited without the password.
func TestReauth_BearerRefusedAuditedAndLimited(t *testing.T) {
	aud := &reauthAudit{}
	h := newAuthzHarness(t, func(d *Deps) {
		d.AuditAppender = aud
		d.LoginRateLimitPerIPOverride = 2
	})
	rec := h.call(t, apiRoute{http.MethodPost, "/api/v1/auth/reauth"}, h.token(t, "admin"))
	// No password in the body: 400 before anything else.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("token without a body: %d, want 400", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/reauth", strings.NewReader(`{"password":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	h.token(t, "admin")(req)
	rec = httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token with a password: %d, want 403", rec.Code)
	}
	// Third request from the same address: over the limit of 2 per minute.
	rec = h.call(t, apiRoute{http.MethodPost, "/api/v1/auth/reauth"}, h.sessionUnconfirmed(t, "admin"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third attempt: %d, want 429", rec.Code)
	}
}

func TestReauth_AuditedWithoutThePassword(t *testing.T) {
	aud := &reauthAudit{}
	h := newLoginHarness(t, func(d *Deps) { d.AuditAppender = aud })
	c := unconfirmedClient(t, h.srv)
	c.post(t, "/api/v1/auth/reauth", map[string]string{"password": "wrong-secret-1"}).Body.Close()
	c.reauth(t, "password1")
	if len(aud.rows) != 2 {
		t.Fatalf("%d audit rows, want 2: %+v", len(aud.rows), aud.rows)
	}
	for i, want := range []string{"denied", "ok"} {
		e := aud.rows[i]
		if e.Action != "auth.reauth" || e.Result != want || e.ActorEmail != "admin@x" {
			t.Errorf("row %d: %+v, want auth.reauth %s by admin@x", i, e, want)
		}
		if strings.Contains(string(e.Payload)+e.SubjectLabel, "secret") || strings.Contains(string(e.Payload), "password1") {
			t.Errorf("row %d carries a password: %+v", i, e)
		}
	}
}

// The sessions list names a session by a hash, never by the cookie value, and
// the Sessions page still revokes through that name.
func TestReauth_SessionsListHashesTheIDs(t *testing.T) {
	h := newLoginHarness(t, func(d *Deps) { d.Sessions = d.Users.(SessionStore) })
	a := authedClient(t, h.srv)
	b := unconfirmedClient(t, h.srv)
	cookieOf := func(c *authClient) string {
		u, _ := url.Parse(h.srv.URL)
		for _, ck := range c.hc.Jar.Cookies(u) {
			if ck.Name == sessionCookieName {
				return ck.Value
			}
		}
		t.Fatal("no session cookie")
		return ""
	}
	resp := a.get(t, "/api/v1/sessions")
	raw := readBody(t, resp)
	for _, c := range []*authClient{a, b} {
		if strings.Contains(raw, cookieOf(c)) {
			t.Fatalf("the list holds a session cookie value: %s", raw)
		}
	}
	var list []sessionResp
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("%d sessions, want 2: %s", len(list), raw)
	}
	var current, other string
	for _, s := range list {
		if s.Current {
			current = s.ID
		} else {
			other = s.ID
		}
	}
	if current != sessionRef(cookieOf(a)) || other != sessionRef(cookieOf(b)) {
		t.Fatalf("ids %q / %q do not match the hash of the sessions", current, other)
	}
	if resp := a.delete(t, "/api/v1/sessions/"+other); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke by hash: %d", resp.StatusCode)
	}
	if resp := b.get(t, "/api/v1/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked session still works: %d", resp.StatusCode)
	}
	if resp := a.get(t, "/api/v1/me"); resp.StatusCode != http.StatusOK {
		t.Fatalf("own session: %d", resp.StatusCode)
	}
	// The cookie value itself names nothing.
	if resp := a.delete(t, "/api/v1/sessions/"+cookieOf(a)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke by cookie value: %d, want 404", resp.StatusCode)
	}
}

// POST /auth/change-password is an account-takeover action: it is limited
// like sign-in, needs the confirmation window, audits wrong attempts, and a
// success ends the user's other sessions.
func TestChangePassword_GuessingIsLimitedAuditedAndConfirmed(t *testing.T) {
	aud := &reauthAudit{}
	h := newLoginHarness(t, func(d *Deps) {
		d.AuditAppender = aud
		d.Sessions = d.Users.(SessionStore)
		d.LoginRateLimitPerIPOverride = 6
	})
	c := unconfirmedClient(t, h.srv) // 1 of 6
	body := map[string]string{"current_password": "password1", "new_password": "newpassword1"}
	if resp := c.post(t, "/api/v1/auth/change-password", body); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unconfirmed: %d, want 403 reauth_required", resp.StatusCode) // 2 of 6
	}
	c.reauth(t, "password1") // 3 of 6
	wrong := map[string]string{"current_password": "guess", "new_password": "newpassword1"}
	for i := 0; i < 2; i++ { // 4, 5
		if resp := c.post(t, "/api/v1/auth/change-password", wrong); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong current password: %d, want 401", resp.StatusCode)
		}
	}
	denied := 0
	for _, e := range aud.rows {
		if e.Action == "auth.password_change" && e.Result == "denied" {
			denied++
		}
	}
	if denied != 2 {
		t.Fatalf("%d denied attempts audited, want 2: %+v", denied, aud.rows)
	}
	if resp := c.post(t, "/api/v1/auth/change-password", wrong); resp.StatusCode != http.StatusUnauthorized { // 6
		t.Fatalf("sixth: %d", resp.StatusCode)
	}
	if resp := c.post(t, "/api/v1/auth/change-password", body); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("guessing is not limited: %d, want 429", resp.StatusCode)
	}
}

func TestChangePassword_EndsOtherSessionsKeepsOwn(t *testing.T) {
	h := newLoginHarness(t, func(d *Deps) { d.Sessions = d.Users.(SessionStore) })
	mine := authedClient(t, h.srv)
	other := authedClient(t, h.srv)
	resp := mine.post(t, "/api/v1/auth/change-password", map[string]string{"current_password": "password1", "new_password": "newpassword1"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change: %d %s", resp.StatusCode, readBody(t, resp))
	}
	if r := other.get(t, "/api/v1/me"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other session after a password change: %d, want 401", r.StatusCode)
	}
	if r := mine.get(t, "/api/v1/me"); r.StatusCode != http.StatusOK {
		t.Fatalf("own session after a password change: %d, want 200", r.StatusCode)
	}
}

// Wrong passwords to /auth/reauth are counted per session, not per user: a
// script on the dashboard's origin can send wrong passwords as the victim, and
// must not be able to shut the victim out on every device.
func TestReauth_WrongPasswordsAreLimitedPerSession(t *testing.T) {
	clk := &loginTestClock{t: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)}
	h := newLoginHarness(t, func(d *Deps) {
		d.LoginRateLimitPerIPOverride = 1000
		d.LoginRateLimitGlobalOverride = 1000
		d.reauthClock = clk.now
	})
	a := unconfirmedClient(t, h.srv)
	b := unconfirmedClient(t, h.srv) // another session of the same user
	for i := 0; i < ReauthGuessLimit; i++ {
		if resp := a.post(t, "/api/v1/auth/reauth", map[string]string{"password": "nope"}); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("wrong %d: %d, want 403", i, resp.StatusCode)
		}
	}
	// A is shut out, the right password included ...
	if resp := a.post(t, "/api/v1/auth/reauth", map[string]string{"password": "password1"}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("A after %d wrong: %d, want 429", ReauthGuessLimit, resp.StatusCode)
	}
	// ... the user's other session is not.
	b.reauth(t, "password1")
	// And A may confirm again once its minute is over.
	clk.add(61 * time.Second)
	a.reauth(t, "password1")
}

// Wrong current passwords on /auth/change-password count against the same
// per-session allowance.
func TestChangePassword_WrongPasswordsShareTheSessionAllowance(t *testing.T) {
	h := newLoginHarness(t, func(d *Deps) {
		d.LoginRateLimitPerIPOverride = 1000
		d.LoginRateLimitGlobalOverride = 1000
	})
	c := authedClient(t, h.srv)
	wrong := map[string]string{"current_password": "guess", "new_password": "newpassword1"}
	for i := 0; i < ReauthGuessLimit; i++ {
		if resp := c.post(t, "/api/v1/auth/change-password", wrong); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong %d: %d, want 401", i, resp.StatusCode)
		}
	}
	right := map[string]string{"current_password": "password1", "new_password": "newpassword1"}
	if resp := c.post(t, "/api/v1/auth/change-password", right); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("change after %d wrong: %d, want 429", ReauthGuessLimit, resp.StatusCode)
	}
	if resp := c.post(t, "/api/v1/auth/reauth", map[string]string{"password": "password1"}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("reauth after %d wrong change attempts: %d, want 429", ReauthGuessLimit, resp.StatusCode)
	}
}

// A high per-user ceiling (wrong passwords per hour, all sessions) is audited
// but never refuses a correct password.
func TestReauth_UserCeilingAuditsAndNeverBlocks(t *testing.T) {
	aud := &reauthAudit{}
	h := newLoginHarness(t, func(d *Deps) {
		d.LoginRateLimitPerIPOverride = 10000
		d.LoginRateLimitGlobalOverride = 10000
		d.AuditAppender = aud
	})
	wrongSent := 0
	for wrongSent <= ReauthUserCeiling {
		c := unconfirmedClient(t, h.srv) // each session may send ReauthGuessLimit wrong ones
		for i := 0; i < ReauthGuessLimit && wrongSent <= ReauthUserCeiling; i++ {
			c.post(t, "/api/v1/auth/reauth", map[string]string{"password": "nope"}).Body.Close()
			wrongSent++
		}
	}
	fresh := unconfirmedClient(t, h.srv)
	fresh.reauth(t, "password1") // still accepted
	flagged := 0
	for _, e := range aud.rows {
		if e.Action == "auth.reauth" && strings.Contains(string(e.Payload), "over_user_ceiling") {
			flagged++
		}
	}
	if flagged == 0 {
		t.Fatalf("nothing audited past the ceiling: %d rows", len(aud.rows))
	}
	last := aud.rows[len(aud.rows)-1]
	if last.Result != "ok" || !strings.Contains(string(last.Payload), "over_user_ceiling") {
		t.Fatalf("the accepted confirmation past the ceiling is not flagged: %+v", last)
	}
}

// A request with a bearer token and a session cookie is the token's: its
// permissions decide, and the cookie's confirmation (or lack of it) does not.
func TestReauth_BearerWithCookieActsAsTheBearer(t *testing.T) {
	h := newAuthzHarness(t)
	cookie := h.sessionUnconfirmed(t, "admin")
	route := apiRoute{http.MethodPost, "/api/v1/users"}
	call := func(perms ...string) *httptest.ResponseRecorder {
		tok := h.token(t, "admin", perms...)
		return h.call(t, route, func(r *http.Request) { cookie(r); tok(r) })
	}
	// A token without the permission is refused by the gate, not asked to confirm.
	if rec := call(); rec.Code != http.StatusForbidden || errorOf(rec) == "reauth_required" {
		t.Fatalf("token without users:manage + cold cookie: %d %s", rec.Code, rec.Body.String())
	}
	// A token with it gets through the guard whatever the cookie did.
	if rec := call("users:manage"); errorOf(rec) == "reauth_required" || rec.Code == http.StatusForbidden {
		t.Fatalf("token with users:manage + cold cookie: %d %s", rec.Code, rec.Body.String())
	}
}

// Logging out closes the window of the session.
func TestReauth_LogoutClosesTheWindow(t *testing.T) {
	h := newAuthzHarness(t)
	auth := h.session(t, "admin")
	probe := httptest.NewRequest(http.MethodGet, "/", nil)
	auth(probe)
	ck, _ := probe.Cookie(sessionCookieName)
	ref := sessionRef(ck.Value)
	if !h.reauth.active(ref) {
		t.Fatal("the window should be open")
	}
	if rec := h.call(t, apiRoute{http.MethodPost, "/api/v1/auth/logout"}, auth); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	if h.reauth.active(ref) {
		t.Fatal("the window outlived the logout")
	}
}
