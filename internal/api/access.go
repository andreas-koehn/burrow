// access.go — who may call each route of the JSON API.
//
// Every route under /api/v1 has one entry in routeAccess. The router is built
// through declared(), which looks the entry up when a route is registered and
// puts its gate in front of the handler; a route without an entry stops the
// router from being built. The gate is the only place that decides whether a
// caller may pass. Outside the allow-list every caller needs a permission: a
// dashboard session holds what its user's role holds, and for an automation
// token the gate asks effectivePerms: the token must declare the permission
// and its user's role must still grant it.

package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// accessKind says which callers a route is open to.
type accessKind int

const (
	// accessPublic: no credentials. The route is outside every session group.
	accessPublic accessKind = iota
	// accessSelf: any signed-in caller, session or token. The route is about
	// the caller itself and nothing else.
	accessSelf
	// accessPerm: every caller needs one of the permissions. A session holds
	// it through its user's role, a token through effectivePerms.
	accessPerm
	// accessDashboard: a dashboard session only, whose role needs one of the
	// permissions; an automation token gets 403 whatever it declares.
	accessDashboard
)

// access is the declaration of one route. perms is any-of.
type access struct {
	kind  accessKind
	perms []authz.Permission
	// every: the caller must hold every permission there is (which includes
	// perms). For what amounts to owning the relay.
	every bool
}

func public() access                                 { return access{kind: accessPublic} }
func self() access                                   { return access{kind: accessSelf} }
func needs(p ...authz.Permission) access             { return access{kind: accessPerm, perms: p} }
func dashboard(p ...authz.Permission) access         { return access{kind: accessDashboard, perms: p} }
func anyOf(p ...authz.Permission) []authz.Permission { return p }

// andEveryPermission asks the caller for every permission of
// authz.AllPermissions() on top of the route's own.
func (a access) andEveryPermission() access {
	a.every = true
	return a
}

// open reports whether the route is on the allow-list: it asks for no
// permission and no dashboard session.
func (a access) open() bool { return a.kind == accessPublic || a.kind == accessSelf }

// covers reports whether an automation token that declares exactly p (and
// whose user's role grants it) passes the route's gate.
func (a access) covers(p authz.Permission) bool {
	return a.kind == accessPerm && !a.every && slices.Contains(a.perms, p)
}

// denied is the 403 message of the route's gate for a caller without any of
// the permissions: "users:read or users:manage required".
func (a access) denied() string {
	if a.every {
		return "every permission required"
	}
	names := make([]string, len(a.perms))
	for i, p := range a.perms {
		names[i] = string(p)
	}
	return strings.Join(names, " or ") + " required"
}

// Permission sets that several routes share. A read is open to the caller who
// may write the same thing.
var (
	servicesConfigure = anyOf(authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	clientTokens      = anyOf(authz.PermTokensManageOwn, authz.PermTokensManageAny)
	browserSessions   = anyOf(authz.PermSessionsManageOwn, authz.PermSessionsManageAny)
	aiRead            = anyOf(authz.PermAIReadOwn, authz.PermAIReadAny, authz.PermAIConfigureOwn, authz.PermAIConfigureAny)
	aiConfigure       = anyOf(authz.PermAIConfigureOwn, authz.PermAIConfigureAny)
	providersRead     = anyOf(authz.PermAIReadOwn, authz.PermAIReadAny, authz.PermAIProvidersManage)
	quotasRead        = anyOf(authz.PermQuotasReadOwn, authz.PermQuotasReadAny)
	inspectorRead     = anyOf(authz.PermInspectorReadOwn, authz.PermInspectorReadAny)
	inspectorReplay   = anyOf(authz.PermInspectorReplayOwn, authz.PermInspectorReplayAny)
	ipGeoManage       = anyOf(authz.PermIPGeoManageOwn, authz.PermIPGeoManageAny)
	automationTokens  = anyOf(authz.PermAutomationTokensManageOwn, authz.PermAutomationTokensManageAny)
	tunnelsRead       = anyOf(authz.PermTunnelsReadOwn, authz.PermTunnelsReadAny)
	// eventsRead: the events stream refreshes the Services, Tunnels,
	// Providers, Requests and Inspector pages, so any role that reads one.
	eventsRead = anyOf(authz.PermTunnelsReadOwn, authz.PermTunnelsReadAny,
		authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny,
		authz.PermAIReadOwn, authz.PermAIReadAny,
		authz.PermInspectorReadOwn, authz.PermInspectorReadAny)
	rolesRead = anyOf(authz.PermRolesRead, authz.PermRolesManage)
	usersRead = anyOf(authz.PermUsersRead, authz.PermUsersManage)
)

// routeAccess declares every route under /api/v1, keyed by "<METHOD> <chi
// pattern>". TestEveryRouteDeclaresItsAccess keeps it equal to the router.
var routeAccess = map[string]access{
	// --- The allow-list -------------------------------------------------
	// Public: called before the caller has any credential.
	"POST /api/v1/auth/login":         public(), // rate-limited per IP
	"GET /api/v1/client/discovery":    public(), // the control endpoint and versions only
	"POST /api/v1/client/login/start": public(), // a client opens a sign-in request
	"POST /api/v1/client/login/poll":  public(), // and collects its outcome with the device code
	"GET /api/v1/openapi.yaml":        public(), // the API description, for SDK generators
	"GET /api/v1/openapi.json":        public(),
	// Self-service: about the caller's own account, with nothing to scope.
	"POST /api/v1/auth/logout":          self(),
	"POST /api/v1/auth/change-password": self(), // asks for the current password
	"GET /api/v1/me":                    self(),

	// --- Dashboard sessions only ----------------------------------------
	"GET /api/v1/events": dashboard(eventsRead...), // SSE of content-free "refetch" notices; the handler scopes to the caller
	"GET /api/v1/services/{serviceID}/inspector/stream":      dashboard(inspectorRead...), // SSE; the handler checks the owner
	"GET /api/v1/client/login/requests/{user_code}":          dashboard(clientTokens...),  // the request that approve decides
	"POST /api/v1/client/login/requests/{user_code}/approve": dashboard(clientTokens...),  // mints a client token
	"POST /api/v1/client/login/requests/{user_code}/deny":    dashboard(clientTokens...),
	"POST /api/v1/ai/keys":                                   dashboard(aiConfigure...), // no token mints a credential that outlives it
	"GET /api/v1/ai/requests/{requestID}/attempts":           dashboard(authz.PermAIReadAny),

	// --- Client tokens, browser sessions, tunnels -----------------------
	"GET /api/v1/clients/connect-info":     needs(clientTokens...),
	"GET /api/v1/tokens":                   needs(clientTokens...),
	"POST /api/v1/tokens":                  needs(clientTokens...),
	"DELETE /api/v1/tokens/{id}":           needs(clientTokens...),
	"GET /api/v1/sessions":                 needs(browserSessions...),
	"DELETE /api/v1/sessions/{id}":         needs(browserSessions...),
	"POST /api/v1/sessions/revoke-all":     needs(browserSessions...),
	"GET /api/v1/tunnels":                  needs(tunnelsRead...),
	"PUT /api/v1/tunnels/{id}/access-mode": needs(servicesConfigure...),
	"GET /api/v1/clients":                  needs(authz.PermTunnelsReadAny),
	"GET /api/v1/clients/known":            needs(authz.PermTunnelsReadAny),
	"GET /api/v1/clients/{sessionID}":      needs(authz.PermTunnelsReadAny),

	// --- Services -------------------------------------------------------
	"POST /api/v1/services":                                                     needs(authz.PermServicesConfigureAny), // pre-provisioning
	"GET /api/v1/services":                                                      needs(servicesConfigure...),
	"GET /api/v1/services/slug-suggestion":                                      needs(servicesConfigure...),
	"GET /api/v1/services/{serviceID}":                                          needs(servicesConfigure...),
	"PUT /api/v1/services/{serviceID}/slug":                                     needs(servicesConfigure...),
	"PUT /api/v1/services/{serviceID}/access-mode":                              needs(servicesConfigure...),
	"PUT /api/v1/services/{serviceID}/gateway-only":                             needs(servicesConfigure...),
	"GET /api/v1/services/{serviceID}/api-keys":                                 needs(servicesConfigure...),
	"POST /api/v1/services/{serviceID}/api-keys":                                needs(servicesConfigure...),
	"DELETE /api/v1/services/{serviceID}/api-keys/{id}":                         needs(servicesConfigure...),
	"GET /api/v1/services/{serviceID}/access-policy":                            needs(servicesConfigure...),
	"PUT /api/v1/services/{serviceID}/access-policy":                            needs(servicesConfigure...),
	"GET /api/v1/services/{serviceID}/domains":                                  needs(servicesConfigure...),
	"POST /api/v1/services/{serviceID}/domains":                                 needs(servicesConfigure...),
	"GET /api/v1/services/{serviceID}/domains/{did}":                            needs(servicesConfigure...),
	"PUT /api/v1/services/{serviceID}/domains/{did}":                            needs(servicesConfigure...),
	"DELETE /api/v1/services/{serviceID}/domains/{did}":                         needs(servicesConfigure...),
	"GET /api/v1/services/{serviceID}/ip-geo":                                   needs(ipGeoManage...),
	"PUT /api/v1/services/{serviceID}/ip-geo":                                   needs(ipGeoManage...),
	"GET /api/v1/geo/status":                                                    needs(ipGeoManage...),
	"GET /api/v1/services/{serviceID}/ai-config":                                needs(aiConfigure...),
	"PUT /api/v1/services/{serviceID}/ai-config":                                needs(aiConfigure...),
	"DELETE /api/v1/services/{serviceID}/cache/entries":                         needs(aiConfigure...),
	"GET /api/v1/services/{serviceID}/upstream-credential":                      needs(aiConfigure...),
	"PUT /api/v1/services/{serviceID}/upstream-credential":                      needs(aiConfigure...),
	"DELETE /api/v1/services/{serviceID}/upstream-credential":                   needs(aiConfigure...),
	"GET /api/v1/upstream-credentials/slots":                                    needs(authz.PermAIConfigureAny),
	"GET /api/v1/services/{serviceID}/inspector/requests":                       needs(inspectorRead...),
	"GET /api/v1/ai/requests":                                                   needs(inspectorRead...), // :own lists the caller's own providers only
	"GET /api/v1/services/{serviceID}/inspector/requests/{rid}":                 needs(inspectorRead...),
	"POST /api/v1/services/{serviceID}/inspector/requests/{rid}/replay":         needs(inspectorReplay...),
	"POST /api/v1/services/{serviceID}/inspector/requests/{rid}/replay-compare": needs(inspectorReplay...),

	// --- AI gateway -----------------------------------------------------
	"GET /api/v1/ai/providers":                     needs(providersRead...),
	"GET /api/v1/ai/providers/{slug}":              needs(providersRead...),
	"GET /api/v1/ai/providers/{slug}/metrics":      needs(providersRead...),
	"GET /api/v1/ai/providers/{slug}/models":       needs(providersRead...),
	"POST /api/v1/ai/providers":                    needs(authz.PermAIProvidersManage),
	"PUT /api/v1/ai/providers/{slug}":              needs(authz.PermAIProvidersManage),
	"DELETE /api/v1/ai/providers/{slug}":           needs(authz.PermAIProvidersManage),
	"PUT /api/v1/ai/providers/{slug}/upstream":     needs(authz.PermAIProvidersManage),
	"POST /api/v1/ai/providers/{slug}/models/sync": needs(authz.PermAIProvidersManage), // an outbound call
	"POST /api/v1/ai/providers/{slug}/models":      needs(authz.PermAIProvidersManage),
	"DELETE /api/v1/ai/providers/{slug}/models":    needs(authz.PermAIProvidersManage),
	"GET /api/v1/ai/gateway":                       needs(aiRead...),
	"GET /api/v1/ai/models":                        needs(aiRead...),
	"GET /api/v1/ai/models/{name}":                 needs(aiRead...),
	"POST /api/v1/ai/models":                       needs(authz.PermAIConfigureAny),
	"PUT /api/v1/ai/models/{name}":                 needs(authz.PermAIConfigureAny),
	"DELETE /api/v1/ai/models/{name}":              needs(authz.PermAIConfigureAny),
	"GET /api/v1/ai/keys":                          needs(aiRead...),      // a token sees its user's own keys
	"DELETE /api/v1/ai/keys/{id}":                  needs(aiConfigure...), // and revokes only those
	"GET /api/v1/cache/settings":                   needs(aiRead...),
	"GET /api/v1/cache/stats":                      needs(aiRead...),
	"PUT /api/v1/cache/settings":                   needs(authz.PermAIConfigureAny),
	"DELETE /api/v1/cache/entries":                 needs(authz.PermAIConfigureAny),
	"DELETE /api/v1/cache/semantic/entries":        needs(authz.PermAIConfigureAny),
	"GET /api/v1/redaction/rules":                  needs(aiRead...),
	"GET /api/v1/redaction/settings":               needs(aiRead...),
	"POST /api/v1/redaction/rules":                 needs(authz.PermAIConfigureAny),
	"PUT /api/v1/redaction/rules/{id}":             needs(authz.PermAIConfigureAny),
	"DELETE /api/v1/redaction/rules/{id}":          needs(authz.PermAIConfigureAny),
	"PUT /api/v1/redaction/settings":               needs(authz.PermAIConfigureAny),
	"POST /api/v1/redaction/preview":               needs(authz.PermAIConfigureAny),
	"GET /api/v1/guardrails/settings":              needs(aiRead...),
	"GET /api/v1/guardrails/patterns":              needs(aiRead...),
	"PUT /api/v1/guardrails/settings":              needs(authz.PermAIConfigureAny),

	// --- Quotas and cost --------------------------------------------------
	"GET /api/v1/rate-limits":         needs(quotasRead...),
	"GET /api/v1/rate-limits/usage":   needs(quotasRead...),
	"POST /api/v1/rate-limits":        needs(authz.PermQuotasManageAny),
	"PUT /api/v1/rate-limits/{id}":    needs(authz.PermQuotasManageAny),
	"DELETE /api/v1/rate-limits/{id}": needs(authz.PermQuotasManageAny),
	"GET /api/v1/cost/pricing":        needs(authz.PermQuotasReadOwn, authz.PermQuotasReadAny, authz.PermCostPricingManage),
	"PUT /api/v1/cost/pricing":        needs(authz.PermCostPricingManage),
	"GET /api/v1/cost/summary":        needs(authz.PermQuotasReadAny),
	"GET /api/v1/cost/export":         needs(authz.PermQuotasReadAny),
	"GET /api/v1/budgets":             needs(authz.PermBudgetsManage),
	"POST /api/v1/budgets":            needs(authz.PermBudgetsManage),
	"PUT /api/v1/budgets/{id}":        needs(authz.PermBudgetsManage),
	"DELETE /api/v1/budgets/{id}":     needs(authz.PermBudgetsManage),

	// --- Audit, logs, webhooks, backups, operations -------------------------
	"GET /api/v1/audit/events":                 needs(authz.PermAuditRead),
	"GET /api/v1/audit/fingerprint":            needs(authz.PermAuditRead),
	"GET /api/v1/audit/export":                 needs(authz.PermAuditRead),
	"POST /api/v1/audit/verify":                needs(authz.PermAuditRead),
	"GET /api/v1/connection-logs":              needs(authz.PermAuditRead),
	"GET /api/v1/connection-logs/rollups":      needs(authz.PermAuditRead),
	"GET /api/v1/connection-logs/export":       needs(authz.PermAuditRead),
	"GET /api/v1/webhooks":                     needs(authz.PermWebhooksManage),
	"POST /api/v1/webhooks":                    needs(authz.PermWebhooksManage),
	"PUT /api/v1/webhooks/{id}":                needs(authz.PermWebhooksManage),
	"DELETE /api/v1/webhooks/{id}":             needs(authz.PermWebhooksManage),
	"POST /api/v1/webhooks/{id}/test":          needs(authz.PermWebhooksManage),
	"POST /api/v1/webhooks/{id}/pause":         needs(authz.PermWebhooksManage),
	"POST /api/v1/webhooks/{id}/resume":        needs(authz.PermWebhooksManage),
	"POST /api/v1/webhooks/{id}/preview":       needs(authz.PermWebhooksManage),
	"GET /api/v1/webhooks/deliveries":          needs(authz.PermWebhooksManage),
	"GET /api/v1/backups":                      needs(authz.PermBackupRun),
	"POST /api/v1/backups":                     needs(authz.PermBackupRun),
	"GET /api/v1/backups/{id}/download":        needs(authz.PermBackupRun),
	"POST /api/v1/backups/{id}/verify":         needs(authz.PermBackupRun),
	"DELETE /api/v1/backups/{id}":              needs(authz.PermBackupRun),
	"POST /api/v1/backups/restore":             needs(authz.PermBackupRun).andEveryPermission(), // swaps the database
	"GET /api/v1/backups/restores/{id}":        needs(authz.PermBackupRun),
	"GET /api/v1/database":                     needs(authz.PermMetricsRead),
	"GET /api/v1/openapi/viewer/":              needs(authz.PermMetricsRead),
	"GET /api/v1/openapi/viewer/static/{file}": needs(authz.PermMetricsRead),
	"GET /api/v1/mcp/status":                   needs(authz.PermMcpToolsRead),
	"GET /api/v1/mcp/tools":                    needs(authz.PermMcpToolsRead),

	// --- Automation tokens, roles, users, settings --------------------------
	"GET /api/v1/automation/presets":        needs(automationTokens...),
	"GET /api/v1/automation/tokens":         needs(automationTokens...),
	"POST /api/v1/automation/tokens":        needs(automationTokens...),
	"DELETE /api/v1/automation/tokens/{id}": needs(automationTokens...),
	"GET /api/v1/roles/permissions":         needs(authz.PermRolesRead, authz.PermRolesManage, authz.PermAutomationTokensManageOwn, authz.PermAutomationTokensManageAny),
	"GET /api/v1/roles":                     needs(rolesRead...),
	"GET /api/v1/roles/{name}":              needs(rolesRead...),
	"POST /api/v1/roles":                    needs(authz.PermRolesManage),
	"PUT /api/v1/roles/{name}":              needs(authz.PermRolesManage),
	"DELETE /api/v1/roles/{name}":           needs(authz.PermRolesManage),
	"GET /api/v1/users":                     needs(usersRead...),
	"POST /api/v1/users":                    needs(authz.PermUsersManage),
	"PATCH /api/v1/users/{id}":              needs(authz.PermUsersManage),
	"DELETE /api/v1/users/{id}":             needs(authz.PermUsersManage),
	"GET /api/v1/settings":                  needs(authz.PermSettingsManage),
	"PUT /api/v1/settings":                  needs(authz.PermSettingsManage),
	"POST /api/v1/settings/test-email":      needs(authz.PermSettingsManage),
	"GET /api/v1/settings/retention":        needs(authz.PermSettingsManage),
	"PUT /api/v1/settings/retention":        needs(authz.PermSettingsManage),
}

// allow returns the gate of a declaration. It runs after the session
// middlewares, which answer an unauthenticated request with 401; the gate
// does not rely on them and refuses a request without a user itself.
func (d Deps) allow(a access) func(http.Handler) http.Handler {
	if a.kind == accessPublic {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if userID(r.Context()) == "" {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			if a.kind == accessSelf {
				next.ServeHTTP(w, r)
				return
			}
			// A token is told from a session by its id, never by what it
			// declares: a token that declares nothing is still a token.
			if bearerTokenID(r.Context()) != "" && a.kind == accessDashboard {
				// What these routes answer is not for a cache to keep.
				w.Header().Set("Cache-Control", "no-store")
				writeErr(w, http.StatusForbidden, msgDashboardSession)
				return
			}
			role, err := d.callerRoleForAuth(r)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					writeErr(w, http.StatusUnauthorized, "unauthorized")
					return
				}
				writeErr(w, http.StatusInternalServerError, "lookup failed")
				return
			}
			if a.every {
				// Measured like mayGrant: a session by its role, a token by
				// what it declares and its user's role grants.
				for _, p := range authz.AllPermissions() {
					if !effectivePerms(r.Context(), role, p) {
						writeErr(w, http.StatusForbidden, a.denied())
						return
					}
				}
			}
			for _, p := range a.perms {
				if effectivePerms(r.Context(), role, p) {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeErr(w, http.StatusForbidden, a.denied())
		})
	}
}

// require is the gate for a route outside /api/v1: every caller needs one of
// the permissions. Routes under /api/v1 get theirs from routeAccess.
func (d Deps) require(p ...authz.Permission) func(http.Handler) http.Handler {
	return d.allow(needs(p...))
}

// declaredRouter registers routes with the gate of their declaration.
type declaredRouter struct {
	chi.Router
	d      Deps
	prefix string
	// with holds the middlewares of r.With(...): they run after the gate.
	with []func(http.Handler) http.Handler
}

// declared wraps the /api/v1 router. Every route registered through the
// result, in any group or sub-route of it, needs an entry in routeAccess.
func (d Deps) declared(r chi.Router, prefix string) chi.Router {
	return declaredRouter{Router: r, d: d, prefix: prefix}
}

func (g declaredRouter) With(mw ...func(http.Handler) http.Handler) chi.Router {
	g.with = append(slices.Clone(g.with), mw...)
	return g
}

func (g declaredRouter) Group(fn func(chi.Router)) chi.Router {
	inner := g
	inner.Router = g.Router.Group(func(r chi.Router) {
		in := g
		in.Router = r
		fn(in)
	})
	return inner
}

func (g declaredRouter) Route(pattern string, fn func(chi.Router)) chi.Router {
	sub := declaredRouter{d: g.d, prefix: g.prefix + pattern, with: g.with}
	sub.Router = g.Router.Route(pattern, func(r chi.Router) {
		in := sub
		in.Router = r
		fn(in)
	})
	return sub
}

func (g declaredRouter) Method(method, pattern string, h http.Handler) {
	key := method + " " + g.prefix + pattern
	a, ok := routeAccess[key]
	if !ok {
		panic("api: route " + key + " has no entry in routeAccess (internal/api/access.go)")
	}
	mws := append([]func(http.Handler) http.Handler{g.d.allow(a)}, g.with...)
	g.Router.With(mws...).Method(method, pattern, h)
}

func (g declaredRouter) MethodFunc(method, pattern string, h http.HandlerFunc) {
	g.Method(method, pattern, h)
}

func (g declaredRouter) Connect(pattern string, h http.HandlerFunc) {
	g.Method(http.MethodConnect, pattern, h)
}
func (g declaredRouter) Delete(pattern string, h http.HandlerFunc) {
	g.Method(http.MethodDelete, pattern, h)
}
func (g declaredRouter) Get(pattern string, h http.HandlerFunc) { g.Method(http.MethodGet, pattern, h) }
func (g declaredRouter) Head(pattern string, h http.HandlerFunc) {
	g.Method(http.MethodHead, pattern, h)
}
func (g declaredRouter) Options(pattern string, h http.HandlerFunc) {
	g.Method(http.MethodOptions, pattern, h)
}
func (g declaredRouter) Patch(pattern string, h http.HandlerFunc) {
	g.Method(http.MethodPatch, pattern, h)
}
func (g declaredRouter) Post(pattern string, h http.HandlerFunc) {
	g.Method(http.MethodPost, pattern, h)
}
func (g declaredRouter) Put(pattern string, h http.HandlerFunc) { g.Method(http.MethodPut, pattern, h) }
func (g declaredRouter) Trace(pattern string, h http.HandlerFunc) {
	g.Method(http.MethodTrace, pattern, h)
}

// A route that answers every method has no declaration to look up.
func (g declaredRouter) Handle(pattern string, _ http.Handler) {
	panic("api: route " + g.prefix + pattern + " must name its method (Handle is not allowed under /api/v1)")
}

func (g declaredRouter) HandleFunc(pattern string, _ http.HandlerFunc) {
	g.Handle(pattern, nil)
}

func (g declaredRouter) Mount(pattern string, _ http.Handler) {
	panic("api: " + g.prefix + pattern + " must be registered with Route (Mount is not allowed under /api/v1)")
}
