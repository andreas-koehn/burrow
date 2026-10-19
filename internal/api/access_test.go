package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// gateRequest builds a request as the session middlewares leave it: the
// user id for everyone, and for an automation token its id, what it declares
// and the current role of its user.
func gateRequest(role string, token bool, declared []string) *http.Request {
	ctx := context.WithValue(context.Background(), userIDKey, "u1")
	if token {
		ctx = context.WithValue(ctx, bearerTokenIDKey, "tok1")
		ctx = context.WithValue(ctx, bearerPermsKey, declared)
		ctx = context.WithValue(ctx, callerRoleKey, role)
	}
	return httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
}

// gateRequestTo is a GET of a signed-in session for a router that has no
// session middleware in front of the gate.
func gateRequestTo(path string) *http.Request {
	ctx := context.WithValue(context.Background(), userIDKey, "u1")
	return httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
}

// everyPermission is what a token declares that declares everything.
func everyPermission() []string {
	var out []string
	for _, p := range authz.AllPermissions() {
		out = append(out, string(p))
	}
	return out
}

// TestGateKinds is the rule of the gate, one line per kind of route and
// caller. A session's role comes from the user store; a token's from the
// request.
func TestGateKinds(t *testing.T) {
	authz.SetRoles(map[string][]authz.Permission{"auditor": {authz.PermAuditRead}})
	defer authz.SetRoles(nil)
	audit := authz.PermAuditRead
	declares := func(p ...string) []string { return p }
	cases := []struct {
		name     string
		a        access
		role     string
		token    bool
		declared []string
		want     int
	}{
		{"public", public(), "user", false, nil, 200},
		{"self, session", self(), "user", false, nil, 200},
		{"self, token that declares nothing", self(), "user", true, declares(), 200},

		{"needs, admin's session", needs(audit), "admin", false, nil, 200},
		{"needs, user's session", needs(audit), "user", false, nil, 403},
		{"needs, session of a custom role that holds it", needs(audit), "auditor", false, nil, 200},
		{"needs, any of several", needs(authz.PermUsersRead, audit), "auditor", false, nil, 200},
		{"needs, admin's token that declares it", needs(audit), "admin", true, declares("audit:read"), 200},
		{"needs, admin's token that declares something else", needs(audit), "admin", true, declares("users:read"), 403},
		{"needs, admin's token that declares nothing", needs(audit), "admin", true, declares(), 403},
		{"needs, admin's token without a permission list", needs(audit), "admin", true, nil, 403},
		{"needs, token of a minter who was demoted", needs(audit), "user", true, declares("audit:read"), 403},

		// An empty list fails closed, for everyone and on every kind.
		{"needs nothing, user's session", needs(), "user", false, nil, 403},
		{"needs nothing, admin's session", needs(), "admin", false, nil, 403},
		{"needs nothing, admin's token", needs(), "admin", true, declares("audit:read"), 403},
		{"dashboard without a permission, admin's session", dashboard(), "admin", false, nil, 403},
		{"dashboard without a permission, token", dashboard(), "admin", true, declares("audit:read"), 403},

		{"dashboard, session without the permission", dashboard(audit), "user", false, nil, 403},
		{"dashboard, session with it", dashboard(audit), "auditor", false, nil, 200},
		{"dashboard, admin's session", dashboard(audit), "admin", false, nil, 200},
		{"dashboard, token with it", dashboard(audit), "admin", true, declares("audit:read"), 403},

		// Every permission: one of the route's own is not enough.
		{"every permission, admin's session", needs(audit).andEveryPermission(), "admin", false, nil, 200},
		{"every permission, session of a role with the route's own only", needs(audit).andEveryPermission(), "auditor", false, nil, 403},
		{"every permission, admin's token with the route's own only", needs(audit).andEveryPermission(), "admin", true, declares("audit:read"), 403},
		{"every permission, admin's token that declares them all", needs(audit).andEveryPermission(), "admin", true, everyPermission(), 200},
		{"every permission, such a token of a minter who was demoted", needs(audit).andEveryPermission(), "user", true, everyPermission(), 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Deps{Users: &fakeUserStore{role: c.role}, Log: discardLog()}
			reached := false
			h := d.allow(c.a)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, gateRequest(c.role, c.token, c.declared))
			if rec.Code != c.want || reached != (c.want == 200) {
				t.Fatalf("status %d, handler reached %v; want %d", rec.Code, reached, c.want)
			}
		})
	}
}

func TestGateAnswers(t *testing.T) {
	reached := false
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	d := Deps{Users: &fakeUserStore{role: "user"}, Log: discardLog()}
	defer func() {
		if reached {
			t.Error("a refused request reached the handler")
		}
	}()

	// An empty require fails closed.
	rec0 := httptest.NewRecorder()
	d.require()(ok).ServeHTTP(rec0, gateRequest("user", false, nil))
	if rec0.Code != 403 {
		t.Errorf("require(): %d, want 403", rec0.Code)
	}

	// The 403 names what would have opened the route.
	rec := httptest.NewRecorder()
	d.require(authz.PermUsersRead, authz.PermUsersManage)(ok).ServeHTTP(rec, gateRequest("user", false, nil))
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), `"users:read or users:manage required"`) {
		t.Errorf("require: %d %s", rec.Code, rec.Body)
	}

	// A token on a dashboard route: the same words as everywhere, not cacheable.
	rec = httptest.NewRecorder()
	d.allow(dashboard())(ok).ServeHTTP(rec, gateRequest("admin", true, nil))
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), msgDashboardSession) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("dashboard: %d %s %v", rec.Code, rec.Body, rec.Header())
	}

	// A session whose user is gone is not signed in.
	gone := Deps{Users: deletedUserFake{}, Log: discardLog()}
	rec = httptest.NewRecorder()
	gone.require(authz.PermUsersRead)(ok).ServeHTTP(rec, gateRequest("", false, nil))
	if rec.Code != 401 {
		t.Errorf("deleted user: %d, want 401", rec.Code)
	}

	// A store that fails is not a refusal of the caller: 500.
	broken := Deps{Users: failingUserFake{}, Log: discardLog()}
	rec = httptest.NewRecorder()
	broken.require(authz.PermUsersRead)(ok).ServeHTTP(rec, gateRequest("", false, nil))
	if rec.Code != 500 {
		t.Errorf("failing store: %d, want 500", rec.Code)
	}
}

// failingUserFake answers every user lookup with an error other than not-found.
type failingUserFake struct {
	fakeUsers
}

func (failingUserFake) GetUserByID(context.Context, string) (db.User, error) {
	return db.User{}, errors.New("disk on fire")
}

// TestEveryRouteDeclaresItsAccess keeps routeAccess equal to the router: a
// route under /api/v1 without an entry, an entry without a route, and an
// entry that makes no sense all fail.
func TestEveryRouteDeclaresItsAccess(t *testing.T) {
	h := newAuthzHarness(t)
	known := authz.AllPermissions()
	registered := map[string]bool{}
	for _, r := range h.apiRoutes(t) {
		registered[r.key()] = true
		a, ok := routeAccess[r.key()]
		if !ok {
			t.Errorf("%s: no entry in routeAccess", r.key())
			continue
		}
		switch a.kind {
		case accessPublic, accessSelf:
			if len(a.perms) != 0 {
				t.Errorf("%s: an allow-listed route names permissions %v", r.key(), a.perms)
			}
		case accessPerm, accessDashboard:
			if len(a.perms) == 0 {
				t.Errorf("%s: no permission named", r.key())
			}
		}
		for _, p := range a.perms {
			if !slices.Contains(known, p) {
				t.Errorf("%s: %q is not a permission of authz.AllPermissions()", r.key(), p)
			}
		}
	}
	for key := range routeAccess {
		if !registered[key] {
			t.Errorf("%s: declared in routeAccess, but the router has no such route", key)
		}
	}
}

// The allow-list is short and reviewed by hand: a route that joins it must be
// added here too.
func TestAllowListIsExactlyThis(t *testing.T) {
	want := []string{
		"GET /api/v1/client/discovery",
		"GET /api/v1/me",
		"GET /api/v1/openapi.json",
		"GET /api/v1/openapi.yaml",
		"POST /api/v1/auth/change-password",
		"POST /api/v1/auth/login",
		"POST /api/v1/auth/logout",
		"POST /api/v1/auth/reauth",
		"POST /api/v1/client/login/poll",
		"POST /api/v1/client/login/start",
	}
	var got []string
	for key, a := range routeAccess {
		if a.open() {
			got = append(got, key)
		}
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("allow-list:\n got %v\nwant %v", got, want)
	}
}

func TestUndeclaredRouteStopsTheRouter(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request) {}
	panics := func(name, wantMsg string, register func(r chi.Router)) {
		t.Helper()
		defer func() {
			msg, _ := recover().(string)
			if !strings.Contains(msg, wantMsg) {
				t.Errorf("%s: panic %q, want one that names %q", name, msg, wantMsg)
			}
		}()
		register(Deps{}.declared(chi.NewRouter(), "/api/v1"))
	}
	panics("plain", "GET /api/v1/nope", func(r chi.Router) { r.Get("/nope", ok) })
	panics("in a group", "POST /api/v1/nope", func(r chi.Router) {
		r.Group(func(r chi.Router) { r.Post("/nope", ok) })
	})
	panics("behind With", "DELETE /api/v1/nope/{id}", func(r chi.Router) {
		r.With(func(next http.Handler) http.Handler { return next }).Delete("/nope/{id}", ok)
	})
	panics("in a sub-route", "GET /api/v1/sub/nope", func(r chi.Router) {
		r.Route("/sub", func(r chi.Router) { r.Get("/nope", ok) })
	})
	// A router handed back by Group or Route registers through the gate too.
	panics("on the router Group returns", "GET /api/v1/nope", func(r chi.Router) {
		r.Group(func(chi.Router) {}).Get("/nope", ok)
	})
	panics("on the router Route returns", "DELETE /api/v1/roles/nope", func(r chi.Router) {
		r.Route("/roles", func(chi.Router) {}).Delete("/nope", ok)
	})
	panics("another method of a declared path", "PUT /api/v1/me", func(r chi.Router) { r.Put("/me", ok) })
	panics("Handle", "must name its method", func(r chi.Router) { r.Handle("/me", http.HandlerFunc(ok)) })
	panics("Mount", "Mount is not allowed", func(r chi.Router) { r.Mount("/me", http.HandlerFunc(ok)) })
}

// The middlewares of r.With run after the gate: a caller the gate refuses
// never reaches them.
func TestGateRunsBeforeRouteMiddlewares(t *testing.T) {
	reached := false
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			next.ServeHTTP(w, r)
		})
	}
	root := chi.NewRouter()
	d := Deps{Users: &fakeUserStore{role: "user"}, Log: discardLog()}
	root.Route("/api/v1", func(r chi.Router) {
		r = d.declared(r, "/api/v1")
		r.With(mw).Get("/users", func(http.ResponseWriter, *http.Request) {})
	})
	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, gateRequestTo("/api/v1/users"))
	if rec.Code != http.StatusForbidden || reached {
		t.Fatalf("status %d, middleware reached %v; want 403 and false", rec.Code, reached)
	}
}

// In r.With(mw).Route(...) the middleware runs after the gate of each route in
// the sub-router, as it does for a plain r.With(mw).Get.
func TestGateRunsBeforeMiddlewaresOfASubRoute(t *testing.T) {
	for _, role := range []string{"user", "admin"} {
		reached := false
		mw := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				next.ServeHTTP(w, r)
			})
		}
		root := chi.NewRouter()
		d := Deps{Users: &fakeUserStore{role: role}, Log: discardLog()}
		root.Route("/api/v1", func(r chi.Router) {
			r = d.declared(r, "/api/v1")
			r.With(mw).Route("/roles", func(r chi.Router) {
				r.Get("/{name}", func(http.ResponseWriter, *http.Request) {})
			})
		})
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, gateRequestTo("/api/v1/roles/x"))
		want := http.StatusForbidden
		if role == "admin" {
			want = http.StatusOK
		}
		if rec.Code != want || reached != (role == "admin") {
			t.Errorf("%s: status %d, middleware reached %v; want %d and %v", role, rec.Code, reached, want, role == "admin")
		}
	}
}
