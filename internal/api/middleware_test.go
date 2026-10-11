package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

type fakeUsers struct {
	validate func(id string) (string, error)
}

func (f fakeUsers) VerifyUserPassword(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (f fakeUsers) GetUserByEmail(_ context.Context, _ string) (db.User, error) {
	return db.User{}, db.ErrNotFound
}
func (f fakeUsers) GetUserByID(_ context.Context, id string) (db.User, error) {
	return db.User{ID: id, Email: "a@x", Role: "admin"}, nil
}
func (f fakeUsers) IssueClientToken(_ context.Context, _, _ string) (string, error) {
	return "bur_x", nil
}
func (f fakeUsers) ListClientTokens(_ context.Context, _ string) ([]db.ClientToken, error) {
	return nil, nil
}
func (f fakeUsers) RevokeClientToken(_ context.Context, _, _ string) error { return nil }
func (f fakeUsers) CreateSession(_ context.Context, _, _, _ string) (string, error) {
	return "sid", nil
}
func (f fakeUsers) ValidateSession(_ context.Context, id string) (string, error) {
	return f.validate(id)
}
func (f fakeUsers) DeleteSession(_ context.Context, _ string) error        { return nil }
func (f fakeUsers) ChangePassword(_ context.Context, _, _, _ string) error { return nil }
func (f fakeUsers) ListUsersPage(_ context.Context, _ string, _, _ int) ([]db.User, int, error) {
	return nil, 0, nil
}
func (f fakeUsers) CreateUser(_ context.Context, _, _, _ string) (db.User, error) {
	return db.User{}, nil
}
func (f fakeUsers) DeleteUser(_ context.Context, _ string) error         { return nil }
func (f fakeUsers) UpdateUserRole(_ context.Context, _, _ string) error  { return nil }
func (f fakeUsers) SetUserStatus(_ context.Context, _, _ string) error   { return nil }
func (f fakeUsers) TouchUserLastLogin(_ context.Context, _ string) error { return nil }

func testDeps(v func(string) (string, error)) Deps {
	return Deps{Users: fakeUsers{validate: v}, Log: slog.Default()}
}

func TestRequireSessionNoCookie(t *testing.T) {
	d := testDeps(func(string) (string, error) { return "", store.ErrUnauthorized })
	h := d.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestRequireSessionValid(t *testing.T) {
	d := testDeps(func(id string) (string, error) {
		if id == "good" {
			return "u1", nil
		}
		return "", store.ErrUnauthorized
	})
	var seen string
	h := d.RequireSession(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = userID(r.Context())
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "good"})
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || seen != "u1" {
		t.Fatalf("want 200+u1, got %d / %q", rr.Code, seen)
	}
}

func TestRequireSessionInvalidCookie(t *testing.T) {
	d := testDeps(func(string) (string, error) { return "", store.ErrUnauthorized })
	h := d.RequireSession(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "bad"})
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestRequireSessionStoreError(t *testing.T) {
	d := testDeps(func(string) (string, error) { return "", errors.New("db down") })
	h := d.RequireSession(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "x"})
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rr.Code)
	}
}

func TestRequireSessionEmptyCookieValue(t *testing.T) {
	d := testDeps(func(string) (string, error) { return "", store.ErrUnauthorized })
	h := d.RequireSession(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: ""})
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for present-but-empty cookie, got %d", rr.Code)
	}
}

// capturingHandler is a slog.Handler that records every log record's level and
// message into a slice, protected by a mutex.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	return nil
}
func (h *capturingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(_ string) slog.Handler      { return h }

// TestRequestLoggerAssetPathsAreDebug asserts that the requestLogger logs
// static/SPA-asset paths at Debug and non-asset (API) paths at Info.
func TestRequestLoggerAssetPathsAreDebug(t *testing.T) {
	ch := &capturingHandler{}
	d := Deps{
		Users: fakeUsers{validate: func(string) (string, error) { return "u1", nil }},
		Log:   slog.New(ch),
	}

	noop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := d.requestLogger(noop)

	staticPaths := []string{"/", "/index.html", "/favicon.svg", "/assets/index-B2-B8H6c.js", "/assets/index-BAL-tmx1.css"}
	for _, p := range staticPaths {
		ch.mu.Lock()
		ch.records = ch.records[:0]
		ch.mu.Unlock()

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", p, nil))

		ch.mu.Lock()
		recs := ch.records
		ch.mu.Unlock()

		if len(recs) != 1 {
			t.Fatalf("path %q: expected 1 log record, got %d", p, len(recs))
		}
		if recs[0].Level != slog.LevelDebug {
			t.Errorf("path %q: want Debug, got %v", p, recs[0].Level)
		}
	}

	// Non-asset path must remain Info.
	ch.mu.Lock()
	ch.records = ch.records[:0]
	ch.mu.Unlock()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/me", nil))

	ch.mu.Lock()
	recs := ch.records
	ch.mu.Unlock()

	if len(recs) != 1 {
		t.Fatalf("/api/v1/me: expected 1 log record, got %d", len(recs))
	}
	if recs[0].Level != slog.LevelInfo {
		t.Errorf("/api/v1/me: want Info, got %v", recs[0].Level)
	}
}

// deletedUserFake embeds fakeUsers but overrides GetUserByID to return
// db.ErrNotFound, simulating a session whose backing user row was deleted.
type deletedUserFake struct {
	fakeUsers
}

func (d deletedUserFake) GetUserByID(_ context.Context, _ string) (db.User, error) {
	return db.User{}, db.ErrNotFound
}

// TestRequireAdminDeletedUserIs401 asserts that when RequireAdmin calls
// GetUserByID and the user row no longer exists (db.ErrNotFound), the response
// is 401 — not 500 (wrong status) and not 403 (wrong semantics).
func TestRequireAdminDeletedUserIs401(t *testing.T) {
	fake := deletedUserFake{fakeUsers: fakeUsers{validate: func(string) (string, error) { return "u1", nil }}}
	d := Deps{Users: fake, Log: slog.Default()}
	// Wrap: RequireSession injects uid into ctx, then RequireAdmin checks it.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := d.RequireSession(d.require(authz.PermUsersManage)(inner))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "any"})
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("deleted session user: want 401, got %d", rr.Code)
	}
}

// infraErrUsers is a UserStore whose ValidateSession always returns a
// non-ErrUnauthorized infrastructure error, while login still succeeds.
// It embeds authUsers so that VerifyUserPassword / GetUserByEmail / CreateSession
// work the same way as in the auth tests (ValidateSession is overridden here).
type infraErrUsers struct {
	authUsers
	sessionErr error
}

func (u *infraErrUsers) ValidateSession(_ context.Context, _ string) (string, error) {
	return "", u.sessionErr
}

// TestEventsStreamSessionInfraError500JSON asserts that when ValidateSession
// returns a non-ErrUnauthorized infrastructure error, GET /api/v1/events
// responds 500 with Content-Type application/json (not text/event-stream) and
// a JSON error body — the RequireSession middleware must short-circuit before
// any SSE headers are emitted.
func TestEventsStreamSessionInfraError500JSON(t *testing.T) {
	infraErr := errors.New("db connection lost")
	u := &infraErrUsers{sessionErr: infraErr}
	u.verify = func(_, _ string) (bool, error) { return true, nil }

	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()

	// Login to get a session cookie, then hit /events which will call ValidateSession.
	jar, _ := cookiejarNew()
	cl := &http.Client{Jar: jar}
	r, _ := cl.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	r.Body.Close()

	resp, err := cl.Get(ts.URL + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("want Content-Type application/json, got %q", ct)
	}
	if strings.Contains(ct, "text/event-stream") {
		t.Fatalf("must NOT be text/event-stream, got %q", ct)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("body must contain JSON error field, got %q", body)
	}
	if strings.Contains(body, infraErr.Error()) {
		t.Fatalf("internal error detail must not leak to client, got %q", body)
	}
}
