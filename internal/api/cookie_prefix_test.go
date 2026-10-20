package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/store"
)

// cookieAPI sends one request to the router with a raw Cookie header.
func cookieAPI(t *testing.T, d Deps, method, path, cookie, csrf, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	NewRouter(d).ServeHTTP(rec, req)
	return rec
}

func setCookies(rec *httptest.ResponseRecorder) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		out[c.Name] = c
	}
	return out
}

// With Secure cookies the dashboard's cookies carry the __Host- prefix:
// Secure, Path=/ and no Domain, so that no other host of the site can set a
// cookie of the same name. The bare names of earlier versions are cleared,
// never written.
func TestLogin_SecureCookiesUseHostPrefix(t *testing.T) {
	au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
	rec := cookieAPI(t, Deps{Users: au, Log: discardLog(), SecureCookies: true}, "POST", "/api/v1/auth/login", "", "", `{"email":"a@x","password":"pw"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	got := setCookies(rec)
	for _, name := range []string{"__Host-burrow_session", "__Host-burrow_csrf"} {
		c := got[name]
		if c == nil || c.Value == "" || c.MaxAge <= 0 {
			t.Fatalf("no %s set: %v", name, rec.Header().Values("Set-Cookie"))
		}
		if !c.Secure || c.Path != "/" || c.Domain != "" {
			t.Errorf("%s: Secure=%v Path=%q Domain=%q, want Secure, / and no Domain", name, c.Secure, c.Path, c.Domain)
		}
	}
	if got["__Host-burrow_session"].Value != "sid-1" || !got["__Host-burrow_session"].HttpOnly {
		t.Errorf("session cookie: %+v", got["__Host-burrow_session"])
	}
	for _, name := range []string{"burrow_session", "burrow_csrf"} {
		if c := got[name]; c != nil && (c.Value != "" || c.MaxAge >= 0) {
			t.Errorf("the bare %s was written: %+v", name, c)
		}
	}
}

// A bare burrow_session that a services-host or upstream script tossed with
// Domain= cannot shadow the dashboard's __Host- session: the prefixed cookie
// is the one read, whatever order the browser sends them in.
func TestSession_HostCookieCannotBeShadowed(t *testing.T) {
	d := testDeps(func(id string) (string, error) {
		if id == "sid-real" {
			return "u-real", nil
		}
		if id == "sid-planted" {
			return "u-attacker", nil
		}
		return "", store.ErrUnauthorized
	})
	d.SecureCookies = true
	rec := cookieAPI(t, d, "GET", "/api/v1/me", "burrow_session=sid-planted; __Host-burrow_session=sid-real", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"u-real"`) {
		t.Fatalf("me: %d %s, want the __Host- session's user", rec.Code, rec.Body)
	}
	// A browser still holding only the bare cookie of an earlier version is
	// not signed out by the upgrade.
	rec = cookieAPI(t, d, "GET", "/api/v1/me", "burrow_session=sid-real", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy cookie: %d, want 200", rec.Code)
	}
}

// The CSRF cookie is read in the name family of the session cookie: a bare
// burrow_csrf planted next to a __Host- session does not pass the
// double-submit check.
func TestCSRF_FollowsTheSessionCookieName(t *testing.T) {
	au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
	d := Deps{Users: au, Log: discardLog(), SecureCookies: true}
	rec := cookieAPI(t, d, "POST", "/api/v1/auth/logout", "__Host-burrow_session=sid-1; burrow_csrf=planted", "planted", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("planted bare CSRF cookie: %d, want 403", rec.Code)
	}
	rec = cookieAPI(t, d, "POST", "/api/v1/auth/logout", "burrow_csrf=planted; __Host-burrow_session=sid-1; __Host-burrow_csrf=real", "real", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("__Host- pair: %d, want 204", rec.Code)
	}
	// Logout clears both name families.
	got := setCookies(rec)
	for _, name := range []string{"__Host-burrow_session", "__Host-burrow_csrf", "burrow_session", "burrow_csrf"} {
		if c := got[name]; c == nil || c.MaxAge >= 0 {
			t.Errorf("logout did not clear %s: %v", name, rec.Header().Values("Set-Cookie"))
		}
	}
	// The bare pair of an earlier version still works for one release.
	au.deleted = ""
	rec = cookieAPI(t, d, "POST", "/api/v1/auth/logout", "burrow_session=sid-1; burrow_csrf=old", "old", "")
	if rec.Code != http.StatusNoContent || au.deleted != "sid-1" {
		t.Fatalf("legacy pair: %d deleted=%q", rec.Code, au.deleted)
	}
}
