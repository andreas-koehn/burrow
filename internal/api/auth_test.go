package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

type authUsers struct {
	fakeUsers
	verify  func(email, pw string) (bool, error)
	created string
	deleted string
}

func (a *authUsers) VerifyUserPassword(_ context.Context, e, p string) (bool, error) {
	return a.verify(e, p)
}
func (a *authUsers) GetUserByEmail(_ context.Context, e string) (db.User, error) {
	return db.User{ID: "u1", Email: e, Role: "admin"}, nil
}
func (a *authUsers) CreateSession(_ context.Context, uid, _, _ string) (string, error) {
	a.created = uid
	return "sid-1", nil
}
func (a *authUsers) DeleteSession(_ context.Context, id string) error { a.deleted = id; return nil }
func (a *authUsers) ValidateSession(_ context.Context, id string) (string, error) {
	if id == "sid-1" {
		return "u1", nil
	}
	return "", store.ErrUnauthorized
}
func (a *authUsers) ValidateBareSession(ctx context.Context, id string) (string, error) {
	return a.ValidateSession(ctx, id)
}

func newTestServer(d Deps) *httptest.Server { return httptest.NewServer(NewRouter(d)) }

func TestLoginSuccessSetsCookie(t *testing.T) {
	au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
	ts := newTestServer(Deps{Users: au, Log: discardLog()})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got bool
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value == "sid-1" && c.HttpOnly {
			got = true
		}
	}
	if !got || au.created != "u1" {
		t.Fatalf("cookie/session not set: cookies=%v created=%q", resp.Cookies(), au.created)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	au := &authUsers{verify: func(_, _ string) (bool, error) { return false, nil }}
	ts := newTestServer(Deps{Users: au, Log: discardLog()})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"bad"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("no cookie on failed login")
	}
}

func TestMeAndLogout(t *testing.T) {
	au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
	ts := newTestServer(Deps{Users: au, Log: discardLog()})
	defer ts.Close()
	jar, _ := cookiejarNew()
	cl := &http.Client{Jar: jar}
	loginResp, _ := cl.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	// Extract the CSRF token set by Login so Logout can send it.
	var csrfToken string
	for _, c := range loginResp.Cookies() {
		if c.Name == csrfCookieName {
			csrfToken = c.Value
		}
	}
	loginResp.Body.Close()

	r, err := cl.Get(ts.URL + "/api/v1/me")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("me: %v status=%v", err, r.StatusCode)
	}
	r.Body.Close()

	loReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/logout", nil)
	loReq.Header.Set("X-CSRF-Token", csrfToken)
	lo, _ := cl.Do(loReq)
	lo.Body.Close()
	if lo.StatusCode != http.StatusNoContent || au.deleted != "sid-1" {
		t.Fatalf("logout status=%d deleted=%q", lo.StatusCode, au.deleted)
	}
}

// TestLoginSetsBothCookies asserts that a successful login sets both the
// burrow_session (HttpOnly) and burrow_csrf (NOT HttpOnly, Secure mirrors
// session) cookies. This verifies the bootstrap of the double-submit pattern.
func TestLoginSetsBothCookies(t *testing.T) {
	au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
	// SecureCookies=false (default for plain-HTTP MVP).
	ts := newTestServer(Deps{Users: au, Log: discardLog()})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var gotSession, gotCSRF bool
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			if !c.HttpOnly {
				t.Error("burrow_session must be HttpOnly")
			}
			if c.Secure { // SecureCookies=false in test
				t.Error("burrow_session must NOT be Secure when SecureCookies=false")
			}
			gotSession = true
		}
		if c.Name == csrfCookieName {
			if c.HttpOnly {
				t.Error("burrow_csrf must NOT be HttpOnly (JS must be able to read it)")
			}
			if c.Secure { // SecureCookies=false in test
				t.Error("burrow_csrf must NOT be Secure when SecureCookies=false")
			}
			if c.Value == "" {
				t.Error("burrow_csrf must have a non-empty value")
			}
			gotCSRF = true
		}
	}
	if !gotSession {
		t.Error("missing burrow_session cookie on successful login")
	}
	if !gotCSRF {
		t.Error("missing burrow_csrf cookie on successful login")
	}
}

// TestLoginDoesNotRequireCSRFToken asserts that POST /auth/login succeeds
// WITHOUT any X-CSRF-Token header (it is the bootstrap — no token exists yet).
func TestLoginDoesNotRequireCSRFToken(t *testing.T) {
	au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
	ts := newTestServer(Deps{Users: au, Log: discardLog()})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Must succeed (200) without any CSRF header — the login endpoint is exempt.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login must not require CSRF token, want 200 got %d", resp.StatusCode)
	}
}

// TestLogoutClearsBothCookies asserts that Logout sets expired (MaxAge=-1)
// Set-Cookie headers for both burrow_session and burrow_csrf.
func TestLogoutClearsBothCookies(t *testing.T) {
	au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
	ts := newTestServer(Deps{Users: au, Log: discardLog()})
	defer ts.Close()
	jar, _ := cookiejarNew()
	cl := &http.Client{Jar: jar}
	loginResp, _ := cl.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	var csrfToken string
	for _, c := range loginResp.Cookies() {
		if c.Name == csrfCookieName {
			csrfToken = c.Value
		}
	}
	loginResp.Body.Close()

	loReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/logout", nil)
	loReq.Header.Set("X-CSRF-Token", csrfToken)
	lo, err := cl.Do(loReq)
	if err != nil {
		t.Fatalf("logout request: %v", err)
	}
	defer lo.Body.Close()
	if lo.StatusCode != http.StatusNoContent {
		t.Fatalf("logout want 204, got %d", lo.StatusCode)
	}
	var clearedSession, clearedCSRF bool
	for _, c := range lo.Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			clearedSession = true
		}
		if c.Name == csrfCookieName && c.MaxAge < 0 {
			clearedCSRF = true
		}
	}
	if !clearedSession {
		t.Error("Logout must clear burrow_session cookie (MaxAge<0)")
	}
	if !clearedCSRF {
		t.Error("Logout must clear burrow_csrf cookie (MaxAge<0)")
	}
}

func TestLoginMalformedJSON(t *testing.T) {
	au := &authUsers{verify: func(_, _ string) (bool, error) { return true, nil }}
	ts := newTestServer(Deps{Users: au, Log: discardLog()})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{bad json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON want 400, got %d", resp.StatusCode)
	}
}

func TestLoginInfraErrorIs500(t *testing.T) {
	au := &authUsers{verify: func(_, _ string) (bool, error) { return false, errors.New("db down") }}
	ts := newTestServer(Deps{Users: au, Log: discardLog()})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("infra error want 500, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "db down") {
		t.Fatalf("internal error detail leaked to client: %s", body)
	}
}
