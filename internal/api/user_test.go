package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// userMgmtStore is a test UserStore implementation for user management tests.
// It embeds tokUsers (which embeds authUsers, which embeds fakeUsers) so all
// required interface methods are covered.
type userMgmtStore struct {
	tokUsers
	// change-password control
	changePwErr error
	// list users control
	listUsersResult []db.User
	listUsersErr    error
	// create user control
	createUserErr error
	// delete user control
	deleteUserErr error
	// role for GetUserByID (controls RequireAdmin)
	role string
}

func (u *userMgmtStore) GetUserByID(_ context.Context, id string) (db.User, error) {
	role := u.role
	if role == "" {
		role = "admin"
	}
	return db.User{ID: id, Email: "a@x", Role: role}, nil
}
func (u *userMgmtStore) ChangePassword(_ context.Context, _, _, _ string) error {
	return u.changePwErr
}
func (u *userMgmtStore) ListUsersPage(_ context.Context, _ string, _, _ int) ([]db.User, int, error) {
	return u.listUsersResult, len(u.listUsersResult), u.listUsersErr
}
func (u *userMgmtStore) CreateUser(_ context.Context, email, _, role string) (db.User, error) {
	if u.createUserErr != nil {
		return db.User{}, u.createUserErr
	}
	return db.User{ID: "u-new", Email: email, Role: role, CreatedAt: time.Now()}, nil
}
func (u *userMgmtStore) DeleteUser(_ context.Context, _ string) error {
	return u.deleteUserErr
}

// newUserMgmtServer builds a test server backed by userMgmtStore.
func newUserMgmtServer(t *testing.T, u *userMgmtStore) (*httptestServer, *http.Client, string) {
	t.Helper()
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	cl, csrf := loggedInClientWithCSRF(t, &httptestServer{ts})
	return &httptestServer{ts}, cl, csrf
}

// --- ChangePassword tests ---

func TestChangePasswordSuccess(t *testing.T) {
	u := &userMgmtStore{}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/change-password",
		strings.NewReader(`{"current_password":"oldpass1","new_password":"newpass1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}

func TestChangePasswordWrongCurrent(t *testing.T) {
	u := &userMgmtStore{changePwErr: store.ErrInvalidCredentials}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/change-password",
		strings.NewReader(`{"current_password":"wrong","new_password":"newpass1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong current password must return 401, got %d", resp.StatusCode)
	}
}

func TestChangePasswordTooShort(t *testing.T) {
	u := &userMgmtStore{changePwErr: store.ErrPasswordTooShort}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/change-password",
		strings.NewReader(`{"current_password":"oldpass1","new_password":"short"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short new password must return 400, got %d", resp.StatusCode)
	}
}

func TestChangePasswordMalformedJSON(t *testing.T) {
	u := &userMgmtStore{}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/change-password",
		strings.NewReader(`{bad json`))
	req.Header.Set("Content-Type", "application/json")
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON must return 400, got %d", resp.StatusCode)
	}
}

// TestChangePasswordRequiresCSRF proves that POST /auth/change-password is
// inside the CSRF-protected session group: a valid session without X-CSRF-Token
// must be rejected with 403 (not 204).
func TestChangePasswordRequiresCSRF(t *testing.T) {
	u := &userMgmtStore{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	jar, _ := cookiejarNew()
	cl := &http.Client{Jar: jar}
	// Login to get the session cookie (CSRF cookie set but we will NOT send the header).
	r, _ := cl.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	r.Body.Close()

	// Send change-password WITHOUT X-CSRF-Token header.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/change-password",
		strings.NewReader(`{"current_password":"oldpass1","new_password":"newpass1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("change-password without CSRF token must return 403, got %d", resp.StatusCode)
	}
}

// TestChangePasswordRequiresSession proves unauthenticated requests get 401.
func TestChangePasswordRequiresSession(t *testing.T) {
	u := &userMgmtStore{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/change-password",
		strings.NewReader(`{"current_password":"old","new_password":"newpass1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", "anytok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated change-password must return 401, got %d", resp.StatusCode)
	}
}

// --- Admin ListUsers tests ---

func TestAdminListUsersSuccess(t *testing.T) {
	u := &userMgmtStore{
		listUsersResult: []db.User{
			{ID: "u1", Email: "a@x", Role: "admin", CreatedAt: time.Now()},
			{ID: "u2", Email: "b@x", Role: "user", CreatedAt: time.Now()},
		},
	}
	ts, cl, _ := newUserMgmtServer(t, u)
	defer ts.Close()

	resp, err := cl.Get(ts.URL + "/api/v1/users")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"id":"u1"`) || !strings.Contains(body, `"id":"u2"`) {
		t.Fatalf("list users body missing users: %s", body)
	}
	// Must never include password_hash.
	if strings.Contains(body, "password_hash") || strings.Contains(body, "PasswordHash") {
		t.Fatalf("list users must not expose password_hash: %s", body)
	}
}

// TestAdminListUsersNonAdminForbidden proves 403 for a non-admin user.
func TestAdminListUsersNonAdminForbidden(t *testing.T) {
	u := &userMgmtStore{role: "user"}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	cl := loggedInClient(t, &httptestServer{ts})

	resp, err := cl.Get(ts.URL + "/api/v1/users")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin GET /users must return 403, got %d", resp.StatusCode)
	}
}

// TestAdminListUsersUnauthenticated proves 401-before-403 ordering: no session = 401.
func TestAdminListUsersUnauthenticated(t *testing.T) {
	u := &userMgmtStore{}
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/users")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /users must return 401, got %d", resp.StatusCode)
	}
}

// --- Admin CreateUser tests ---

func TestAdminCreateUserSuccess(t *testing.T) {
	u := &userMgmtStore{}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/users",
		strings.NewReader(`{"email":"new@x","password":"password1","role":"user"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := doWithCSRF(t, cl, req, csrf)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"email":"new@x"`) || !strings.Contains(body, `"role":"user"`) {
		t.Fatalf("create user body unexpected: %s", body)
	}
	if strings.Contains(body, "password") && strings.Contains(body, "hash") {
		t.Fatalf("create user must not expose password hash: %s", body)
	}
}

func TestAdminCreateUserDuplicateEmail(t *testing.T) {
	u := &userMgmtStore{createUserErr: store.ErrEmailConflict}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/users",
		strings.NewReader(`{"email":"dup@x","password":"password1","role":"user"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate email must return 409, got %d", resp.StatusCode)
	}
}

func TestAdminCreateUserPasswordTooShort(t *testing.T) {
	u := &userMgmtStore{createUserErr: store.ErrPasswordTooShort}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/users",
		strings.NewReader(`{"email":"x@x","password":"short","role":"user"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short password must return 400, got %d", resp.StatusCode)
	}
}

func TestAdminCreateUserBadRole(t *testing.T) {
	u := &userMgmtStore{createUserErr: store.ErrInvalidRole}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/users",
		strings.NewReader(`{"email":"x@x","password":"password1","role":"superuser"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad role must return 400, got %d", resp.StatusCode)
	}
}

// TestAdminCreateUserNonAdminForbidden proves 403 for a non-admin session.
func TestAdminCreateUserNonAdminForbidden(t *testing.T) {
	u := &userMgmtStore{role: "user"}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	cl, csrf := loggedInClientWithCSRF(t, &httptestServer{ts})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/users",
		strings.NewReader(`{"email":"x@x","password":"password1","role":"user"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin POST /users must return 403, got %d", resp.StatusCode)
	}
}

// TestAdminCreateUserUnauthenticated proves unauthenticated = 401.
func TestAdminCreateUserUnauthenticated(t *testing.T) {
	u := &userMgmtStore{}
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/users",
		strings.NewReader(`{"email":"x@x","password":"password1","role":"user"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /users must return 401, got %d", resp.StatusCode)
	}
}

// TestAdminCreateUserRequiresCSRF proves POST /users is CSRF-protected.
func TestAdminCreateUserRequiresCSRF(t *testing.T) {
	u := &userMgmtStore{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	jar, _ := cookiejarNew()
	cl := &http.Client{Jar: jar}
	r, _ := cl.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	r.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/users",
		strings.NewReader(`{"email":"x@x","password":"password1","role":"user"}`))
	req.Header.Set("Content-Type", "application/json")
	// NO X-CSRF-Token header.
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /users without CSRF token must return 403, got %d", resp.StatusCode)
	}
}

// --- Admin DeleteUser tests ---

func TestAdminDeleteUserSuccess(t *testing.T) {
	u := &userMgmtStore{}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/users/other-user-id", nil)
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
}

func TestAdminDeleteUserNotFound(t *testing.T) {
	u := &userMgmtStore{deleteUserErr: db.ErrNotFound}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/users/nonexistent", nil)
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("not found user must return 404, got %d", resp.StatusCode)
	}
}

// TestAdminDeleteUserSelf proves that an admin cannot delete their own account (lockout guard).
// The authed user is always "u1" (set by ValidateSession in fakeUsers).
func TestAdminDeleteUserSelf(t *testing.T) {
	u := &userMgmtStore{}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	// "u1" is the session user (ValidateSession returns "u1").
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/users/u1", nil)
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-delete must return 400, got %d", resp.StatusCode)
	}
}

func TestAdminDeleteUserNonAdminForbidden(t *testing.T) {
	u := &userMgmtStore{role: "user"}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	cl, csrf := loggedInClientWithCSRF(t, &httptestServer{ts})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/users/someone", nil)
	resp := doWithCSRF(t, cl, req, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin DELETE /users/{id} must return 403, got %d", resp.StatusCode)
	}
}

func TestAdminDeleteUserUnauthenticated(t *testing.T) {
	u := &userMgmtStore{}
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/users/someone", nil)
	req.Header.Set("X-CSRF-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated DELETE /users must return 401, got %d", resp.StatusCode)
	}
}

// TestAdminDeleteUserRequiresCSRF proves DELETE /users/{id} is CSRF-protected.
func TestAdminDeleteUserRequiresCSRF(t *testing.T) {
	u := &userMgmtStore{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	jar, _ := cookiejarNew()
	cl := &http.Client{Jar: jar}
	r, _ := cl.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	r.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/users/someone", nil)
	// NO X-CSRF-Token header.
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("DELETE /users without CSRF token must return 403, got %d", resp.StatusCode)
	}
}
