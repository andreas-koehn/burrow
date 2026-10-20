package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loginReqWith is POST /auth/login with the given headers.
func loginReqWith(contentType string, headers map[string]string) *http.Request {
	req := httptest.NewRequest("POST", "http://dash.example.com/api/v1/auth/login", strings.NewReader(`{"email":"a@x","password":"pw"}`))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// Login CSRF: a page on another site (or on another host of the same site,
// a services host) must not sign the browser in to an account of its
// choosing. A cross-site request is refused before the password is checked.
func TestLogin_RefusesCrossSiteRequests(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"typed by the user", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"same-site (services host)", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://apps.example.com"}, http.StatusForbidden},
		{"old browser, foreign Origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"old browser, own Origin", map[string]string{"Origin": "http://dash.example.com"}, http.StatusOK},
		{"CLI (no browser headers)", nil, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verified := false
			au := &authUsers{verify: func(_, _ string) (bool, error) { verified = true; return true, nil }}
			rec := httptest.NewRecorder()
			NewRouter(Deps{Users: au, Log: discardLog()}).ServeHTTP(rec, loginReqWith("application/json", c.headers))
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
			if c.want == http.StatusForbidden && (verified || len(rec.Result().Cookies()) > 0) {
				t.Fatal("a refused login checked the password or set a cookie")
			}
		})
	}
}

// A text/plain body is what a cross-site form or a no-cors fetch can send
// without a preflight; login refuses it whatever it holds.
func TestLogin_RefusesTextPlain(t *testing.T) {
	for _, ct := range []string{"text/plain", "text/plain;charset=UTF-8", "TEXT/PLAIN"} {
		au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
		rec := httptest.NewRecorder()
		NewRouter(Deps{Users: au, Log: discardLog()}).ServeHTTP(rec, loginReqWith(ct, nil))
		if rec.Code != http.StatusUnsupportedMediaType || len(rec.Result().Cookies()) > 0 {
			t.Fatalf("%s: status %d cookies %v, want 415 and none", ct, rec.Code, rec.Result().Cookies())
		}
	}
}

// Sign-in takes application/json only: a form, multipart, any other media
// type and a body without a Content-Type are refused with 415 (no repo
// client signs in without one; the dashboard, the scripts and the e2e
// helpers all send application/json).
func TestLogin_RefusesOtherMediaTypes(t *testing.T) {
	for _, ct := range []string{"", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "text/html", "application/xml", "application/jsonp", "application/json-seq", "not a media type"} {
		au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
		rec := httptest.NewRecorder()
		NewRouter(Deps{Users: au, Log: discardLog()}).ServeHTTP(rec, loginReqWith(ct, nil))
		if rec.Code != http.StatusUnsupportedMediaType || len(rec.Result().Cookies()) > 0 {
			t.Errorf("%q: status %d cookies %v, want 415 and none", ct, rec.Code, rec.Result().Cookies())
		}
	}
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
		rec := httptest.NewRecorder()
		NewRouter(Deps{Users: au, Log: discardLog()}).ServeHTTP(rec, loginReqWith(ct, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%q: status %d %s, want 200", ct, rec.Code, rec.Body)
		}
	}
}

// POST /auth/reauth refuses a cross-site request as well, even one that
// carries the session and the CSRF token.
func TestReauth_RefusesCrossSiteRequests(t *testing.T) {
	d := testDeps(func(string) (string, error) { return "u1", nil })
	req := httptest.NewRequest("POST", "http://dash.example.com/api/v1/auth/reauth", strings.NewReader(`{"password":"pw"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "burrow_session=s1; burrow_csrf=t1")
	req.Header.Set("X-CSRF-Token", "t1")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	rec := httptest.NewRecorder()
	NewRouter(d).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-origin") {
		t.Fatalf("status %d %s, want 403 cross-origin", rec.Code, rec.Body)
	}
}
