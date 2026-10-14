package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/events"
	"github.com/ankoehn/burrow/internal/store"
)

// authzRoles are the roles the router-wide authorization tests walk: the two
// built-in ones, a custom role without any permission and a custom role that
// holds every permission that existed before the gate was unified.
var authzRoles = []string{"admin", "user", "cr-none", "cr-legacy"}

// legacyPerms is every permission of authz.AllPermissions() as of v0.9: what
// a custom role could hold before the permission gate. It is a literal on
// purpose; permissions added later must not widen "cr-legacy".
var legacyPerms = []string{
	"tunnels:read:own", "tunnels:read:any", "tunnels:manage:own", "tunnels:manage:any",
	"tokens:manage:own", "tokens:manage:any",
	"services:configure:own", "services:configure:any",
	"sessions:manage:own", "sessions:manage:any",
	"users:read", "users:manage", "roles:read", "settings:manage",
	"ai:read:own", "ai:read:any", "ai:configure:own", "ai:configure:any",
	"quotas:read:own", "quotas:read:any", "quotas:manage:any",
	"inspector:read:own", "inspector:read:any", "inspector:replay:own", "inspector:replay:any",
	"audit:read", "webhooks:manage", "roles:manage",
	"automation:tokens:manage:own", "automation:tokens:manage:any",
	"backup:run", "metrics:read", "mcp:tools:read",
	"mtls:manage:own", "mtls:manage:any", "ipgeo:manage:own", "ipgeo:manage:any",
}

// authzServiceID is the one service of the harness. It belongs to the user
// with the role "user".
const authzServiceID = "svc-user"

// authzHarness is the whole router over a real store (SQLite) with one user
// per role of authzRoles. The id of a role's user is "u-" + role. mods change
// the Deps before the router is built.
type authzHarness struct {
	router chi.Router
	st     *store.Store
	sqldb  *sql.DB
}

// harnessOwners answers the service lookups of the cache and inspector
// handlers from the services table, as cmd/server's adapter does.
type harnessOwners struct{ x *db.DB }

func (o harnessOwners) GetServiceOwner(ctx context.Context, serviceID string) (string, error) {
	svc, err := o.x.GetServiceByID(ctx, serviceID)
	if err != nil {
		return "", err
	}
	return svc.UserID, nil
}

func (o harnessOwners) GetServiceAIConfig(context.Context, string) ([]byte, error) {
	return nil, db.ErrNotFound
}

func (o harnessOwners) ListAllServiceAIConfigs(context.Context) ([]CacheServiceConfigRow, error) {
	return nil, nil
}

func newAuthzHarness(t *testing.T, mods ...func(*Deps)) *authzHarness {
	t.Helper()
	ctx := context.Background()
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "authz.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	st := store.New(sqldb)
	x := db.Wrap(sqldb)

	// The custom roles go through the store, which publishes them to authz.
	t.Cleanup(func() { authz.SetRoles(nil) })
	if err := st.CreateRole(ctx, "cr-none", "holds nothing", []string{}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRole(ctx, "cr-legacy", "holds every permission of v0.9", legacyPerms, false); err != nil {
		t.Fatal(err)
	}
	// Users are inserted directly: no password is ever checked here, and a
	// custom role is assigned without asking the store.
	for _, role := range authzRoles {
		if _, err := sqldb.ExecContext(ctx,
			`INSERT INTO users(id, email, password_hash, role) VALUES(?,?,?,?)`,
			"u-"+role, role+"@x", "x", role); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateService(ctx, db.Service{
		ID: authzServiceID, UserID: "u-user", Name: "web", Type: "http", Subdomain: "web", AccessMode: "open",
	}); err != nil {
		t.Fatal(err)
	}

	owners := harnessOwners{x: x}
	d := Deps{
		Log:   discardLog(),
		Users: st, Roles: st, Sessions: st, Settings: st, Services: st,
		AIProviders: st, AIAttempts: st, AIModels: st, AIGatewayKeys: st,
		ClientLogins: st, Automation: st, Bearer: NewStoreBearerStore(st),
		Events:   events.NewBus(),
		Webhooks: x, RateLimitDB: x, RateLimits: &fakeQuotaEngine{}, Budgets: x, AIMetrics: x,
		IPGeo: x, IPGeoServices: x, ServiceAIConfigs: x,
		CredentialDB: x, CredentialServices: x, CustomDomains: x,
		AuditEvents: x, ConnLogDB: NewConnLogDBAdapter(x),
		CacheServices: owners, InspectorServices: owners,
		BackupDir: t.TempDir(), RestoreTracker: NewRestoreTracker(),
		AuthDomain: "relay.test",
	}
	for _, mod := range mods {
		mod(&d)
	}
	return &authzHarness{router: NewRouter(d).(chi.Router), st: st, sqldb: sqldb}
}

// apiRoute is one route of the JSON API as chi reports it.
type apiRoute struct{ method, pattern string }

func (r apiRoute) key() string { return r.method + " " + r.pattern }

// apiRoutes returns every route under /api/v1, sorted. The test-only routes
// under /api/v1/internal/ exist under a build tag and are left out.
func (h *authzHarness) apiRoutes(t *testing.T) []apiRoute {
	t.Helper()
	var out []apiRoute
	err := chi.Walk(h.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/api/v1/") && !strings.HasPrefix(route, "/api/v1/internal/") {
			out = append(out, apiRoute{method: method, pattern: route})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

var routeParam = regexp.MustCompile(`\{[^}]+\}`)

// path fills the parameters of a route pattern: the harness's service for
// {serviceID}, and "nope" (nothing has that id) for every other one.
func (r apiRoute) path() string {
	return routeParam.ReplaceAllStringFunc(r.pattern, func(p string) string {
		if p == "{serviceID}" {
			return authzServiceID
		}
		return "nope"
	})
}

// stream reports whether the route answers with an event stream that only
// ends when the caller goes away.
func (r apiRoute) stream() bool {
	return strings.HasSuffix(r.pattern, "/events") || strings.HasSuffix(r.pattern, "/inspector/stream")
}

// hangUp cancels the request as soon as the handler starts to answer, so
// that an event stream ends.
type hangUp struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *hangUp) WriteHeader(code int) {
	w.ResponseRecorder.WriteHeader(code)
	w.cancel()
}

// call sends one request to the route and returns the answer. auth sets the
// credentials on the request. Every request that may carry a body sends {}.
func (h *authzHarness) call(t *testing.T, r apiRoute, auth func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if r.method == http.MethodGet || r.method == http.MethodHead {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader("{}")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(r.method, r.path(), body).WithContext(ctx)
	if body.Len() > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != nil {
		auth(req)
	}
	rec := httptest.NewRecorder()
	if r.stream() {
		h.router.ServeHTTP(&hangUp{ResponseRecorder: rec, cancel: cancel}, req)
	} else {
		h.router.ServeHTTP(rec, req)
	}
	return rec
}

// do sends one request with a JSON body (nil: none) and returns the answer.
func (h *authzHarness) do(t *testing.T, method, path string, body any, auth func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = mustJSON(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != nil {
		auth(req)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// session returns the credentials of a fresh dashboard session of the role's
// user: the session cookie and the CSRF pair.
func (h *authzHarness) session(t *testing.T, role string) func(*http.Request) {
	t.Helper()
	sid, err := h.st.CreateSession(context.Background(), "u-"+role, "authz-test", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	return func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
		req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf"})
		req.Header.Set("X-CSRF-Token", "csrf")
	}
}

// token mints an automation token for the role's user that declares perms,
// and returns its credentials.
func (h *authzHarness) token(t *testing.T, role string, perms ...string) func(*http.Request) {
	t.Helper()
	if perms == nil {
		perms = []string{}
	}
	_, plaintext, err := h.st.MintAutomationToken(context.Background(), "u-"+role, role, "authz-test", perms, nil)
	if err != nil {
		t.Fatal(err)
	}
	return func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+plaintext) }
}
