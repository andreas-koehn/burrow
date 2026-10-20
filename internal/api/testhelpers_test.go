package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func cookiejarNew() (*cookiejar.Jar, error) { return cookiejar.New(nil) }

type httptestServer struct{ *httptest.Server }

func readBody(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// mustJSON marshals v and returns it as an io.Reader request body.
func mustJSON(v any) *bytes.Reader {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bytes.NewReader(b)
}

// fakeUserStore is the shared UserStore fake for the v0.2.0 handler tests. It
// embeds fakeUsers (full UserStore default impl) and overrides the auth +
// management methods so an authedClient can log in as a configurable role.
type fakeUserStore struct {
	fakeUsers
	role             string // role reported by GetUserByID/GetUserByEmail (default "admin")
	selfID           string // id reported for the logged-in user (default "u-self")
	page             []db.User
	total            int
	lastRole         string
	lastStatus       string
	loginEmail       string // if set, VerifyUserPassword requires this email
	loginPass        string // if set, VerifyUserPassword requires this password
	suspended        bool   // GetUserByEmail/ByID report status=suspended
	lastLoginTouched bool
}

func (u *fakeUserStore) id() string {
	if u.selfID != "" {
		return u.selfID
	}
	return "u-self"
}
func (u *fakeUserStore) roleOrAdmin() string {
	if u.role != "" {
		return u.role
	}
	return "admin"
}
func (u *fakeUserStore) statusStr() string {
	if u.suspended {
		return "suspended"
	}
	return "active"
}

func (u *fakeUserStore) VerifyUserPassword(_ context.Context, e, p string) (bool, error) {
	if u.loginEmail != "" && (e != u.loginEmail || p != u.loginPass) {
		return false, nil
	}
	return true, nil
}
func (u *fakeUserStore) GetUserByEmail(_ context.Context, e string) (db.User, error) {
	return db.User{ID: u.id(), Email: e, Role: u.roleOrAdmin(), Status: u.statusStr()}, nil
}
func (u *fakeUserStore) GetUserByID(_ context.Context, id string) (db.User, error) {
	return db.User{ID: id, Email: "self@x", Role: u.roleOrAdmin(), Status: u.statusStr()}, nil
}
func (u *fakeUserStore) CreateSession(_ context.Context, _, _, _ string) (string, error) {
	return "sid-" + u.id(), nil
}
func (u *fakeUserStore) ValidateSession(_ context.Context, _ string) (string, error) {
	return u.id(), nil
}
func (u *fakeUserStore) ValidateBareSession(ctx context.Context, id string) (string, error) {
	return u.ValidateSession(ctx, id)
}
func (u *fakeUserStore) TouchUserLastLogin(_ context.Context, _ string) error {
	u.lastLoginTouched = true
	return nil
}
func (u *fakeUserStore) ListUsersPage(_ context.Context, _ string, _, _ int) ([]db.User, int, error) {
	return u.page, u.total, nil
}
func (u *fakeUserStore) UpdateUserRole(_ context.Context, _, role string) error {
	u.lastRole = role
	return nil
}
func (u *fakeUserStore) SetUserStatus(_ context.Context, _, status string) error {
	u.lastStatus = status
	return nil
}

// authClient is a logged-in HTTP client that carries the session + CSRF cookies
// and echoes the burrow_csrf value in X-CSRF-Token on state-changing requests.
type authClient struct {
	base string
	hc   *http.Client
	csrf string
}

// authedClient logs in against srv and confirms the password (POST
// /auth/reauth), so the routes behind RequireRecentAuth are open to it. Use
// unconfirmedClient for a session that has not confirmed. Deps.Users must be a
// fakeUserStore (or anything that lets login succeed).
func authedClient(t *testing.T, srv *httptest.Server) *authClient {
	t.Helper()
	c := unconfirmedClient(t, srv)
	c.reauth(t, "password1")
	return c
}

// unconfirmedClient logs in against srv (POST /api/v1/auth/login) and returns a
// client whose jar holds the session/CSRF cookies, without confirming the
// password.
func unconfirmedClient(t *testing.T, srv *httptest.Server) *authClient {
	t.Helper()
	jar, err := cookiejarNew()
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Jar: jar}
	resp, err := hc.Post(srv.URL+"/api/v1/auth/login", "application/json",
		mustJSON(map[string]string{"email": "admin@x", "password": "password1"}))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	_ = resp.Body.Close()
	c := &authClient{base: srv.URL, hc: hc}
	u, _ := url.Parse(srv.URL)
	for _, ck := range jar.Cookies(u) {
		if ck.Name == csrfCookieName {
			c.csrf = ck.Value
		}
	}
	if c.csrf == "" {
		t.Fatal("authedClient: no CSRF cookie after login")
	}
	return c
}

// reauth confirms the password; it fails the test when the relay refuses.
func (c *authClient) reauth(t *testing.T, password string) {
	t.Helper()
	resp := c.do(t, http.MethodPost, "/api/v1/auth/reauth", map[string]string{"password": password})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reauth status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	_ = resp.Body.Close()
}

func (c *authClient) do(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = mustJSON(body)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (c *authClient) get(t *testing.T, path string) *http.Response {
	return c.do(t, http.MethodGet, path, nil)
}
func (c *authClient) post(t *testing.T, path string, body any) *http.Response {
	return c.do(t, http.MethodPost, path, body)
}
func (c *authClient) put(t *testing.T, path string, body any) *http.Response {
	return c.do(t, http.MethodPut, path, body)
}
func (c *authClient) patch(t *testing.T, path string, body any) *http.Response {
	return c.do(t, http.MethodPatch, path, body)
}
func (c *authClient) delete(t *testing.T, path string) *http.Response {
	return c.do(t, http.MethodDelete, path, nil)
}
