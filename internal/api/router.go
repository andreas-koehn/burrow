package api

import (
	"net/http"
	"time"

	"github.com/ankoehn/burrow/internal/openapi/viewer"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"
)

// JSONHandlerTimeout is the maximum duration the chi middleware.Timeout allows
// a JSON API handler to run. cmd/server uses this constant to set the HTTP
// server shutdown grace period to strictly exceed this value, ensuring every
// in-flight handler completes (or is chi-cancelled) before database.Close()
// runs.
const JSONHandlerTimeout = 30 * time.Second

// loginRateLimiters builds the two httprate middlewares applied only to
// POST /auth/login: one per-IP limiter and one global endpoint limiter.
// Both values are resolved from Deps overrides (for test injection) with
// fallback to the package-level constants.
//
// Per-IP accuracy is guaranteed by TrustedProxyMiddleware (C2), which runs
// before this limiter and only honors X-Forwarded-For when the TCP peer is
// within a trusted CIDR — preventing spoofed headers from bypassing the limit.
func (d Deps) loginRateLimiters() (perIP, global func(http.Handler) http.Handler) {
	limitPerIP := d.LoginRateLimitPerIPOverride
	if limitPerIP <= 0 {
		limitPerIP = LoginRateLimitPerIP
	}
	limitGlobal := d.LoginRateLimitGlobalOverride
	if limitGlobal <= 0 {
		limitGlobal = LoginRateLimitGlobal
	}
	rateLimitedHandler := httprate.WithLimitHandler(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusTooManyRequests, "too many login attempts")
	})
	perIP = httprate.Limit(limitPerIP, time.Minute,
		httprate.WithKeyFuncs(httprate.KeyByIP),
		rateLimitedHandler,
	)
	global = httprate.Limit(limitGlobal, time.Minute,
		httprate.WithKeyFuncs(httprate.KeyByEndpoint),
		rateLimitedHandler,
	)
	return perIP, global
}

// NewRouter builds the /api/v1 HTTP handler.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	// TrustedProxyMiddleware replaces the unconditional middleware.RealIP.
	// It only honors X-Forwarded-For / X-Real-IP when the immediate TCP peer
	// is within a trusted CIDR (Deps.TrustedProxies). When TrustedProxies is
	// empty (the safe default), forwarded headers are ignored entirely and the
	// raw TCP peer is used — preventing XFF spoofing from bypassing the per-IP
	// login rate-limiter or poisoning session.ip records.
	// MUST run before the per-IP rate-limiter and Login.
	r.Use(TrustedProxyMiddleware(d.TrustedProxies))
	// HSTSMiddleware adds Strict-Transport-Security only when the server is
	// serving native TLS (Deps.HTTPSEnabled). It intentionally does NOT read
	// X-Forwarded-Proto or any other spoofable header — the flag is set by
	// cmd/server from config, not from incoming request headers.
	r.Use(HSTSMiddleware(d.HTTPSEnabled))
	r.Use(middleware.RequestID)
	r.Use(d.requestLogger)
	r.Use(middleware.Recoverer)

	// Liveness/readiness probes: unauthenticated, no CSRF, no /api/v1 prefix
	// (k8s-style). Registered before the SPA catch-all so they are not
	// shadowed by client-side routing.
	r.Get("/healthz", d.Healthz)
	r.Get("/readyz", d.Readyz)

	// v0.4.0 Task 21: Prometheus /metrics endpoint (spec Part O). Lives
	// OUTSIDE /api/v1 (k8s/prometheus convention) but is NOT public —
	// gated by RequireBearerOrSession + RequireSession + requireMetricsRead
	// (admin OR authz.PermMetricsRead). The handler emits the closed metric
	// set in 0.0.4 text format; CSRF does not apply (GET is a safe method).
	// Registered before the SPA catch-all so /metrics is not shadowed.
	r.Group(func(r chi.Router) {
		r.Use(RequireBearerOrSession(d.Bearer, d.Users))
		r.Use(d.RequireSession)
		r.Use(d.requireMetricsRead)
		r.Get("/metrics", d.GetMetrics)
	})

	// Client hand-out: the install scripts and the client archives. Public —
	// a machine asks before it has anything — and registered before the SPA
	// catch-all; /install.sh, /install.ps1 and /download/ are reserved paths.
	// Everything under /download/ that is not a build answers a plain 404
	// here instead of the dashboard.
	for path, h := range map[string]http.HandlerFunc{
		"/install.sh":                    d.GetInstallSh,
		"/install.ps1":                   d.GetInstallPs1,
		"/download/burrow/checksums.txt": d.GetClientChecksums,
		"/download/burrow/{os}/{arch}":   d.GetClientDownload,
	} {
		// Handle, not Get: with the SPA's catch-all present chi would hand
		// any other method on these paths to the dashboard.
		r.Handle(path, getOrHead(h))
	}
	r.Handle("/download", http.HandlerFunc(d.ClientDownloadNotFound))
	r.Handle("/download/*", http.HandlerFunc(d.ClientDownloadNotFound))

	loginPerIP, loginGlobal := d.loginRateLimiters()
	loginStartLimit, loginPollLimit := d.clientLoginRateLimiters()
	// d is a copy: the counter of wrong user codes belongs to this router.
	d.clientLoginGuesses = newGuessLimiter(ClientLoginGuessLimit, time.Minute, d.clientLoginGuessClock)

	r.Route("/api/v1", func(r chi.Router) {
		// Test-only: under -tags=integration, registers
		// POST /api/v1/internal/test-reset (used by the test/e2e
		// e2e harness). Default builds compile a no-op stub — see
		// router_integration.go + router_integration_stub.go. The build tag
		// is the security boundary; release binaries never see this route.
		registerIntegrationRoutes(r, d)

		// v0.4.0 Task 22: OpenAPI spec discovery. Both routes are public
		// (no auth) so SDK code-generators can curl the canonical doc
		// without bootstrapping a session. They are excluded from
		// TestOpenAPI_RouteCoverage by convention (the spec describes the
		// JSON API; pinning its own discovery surface inside the spec
		// would be self-referential and adds no SDK value).
		r.Get("/openapi.yaml", d.GetOpenAPIYAML)
		r.Get("/openapi.json", d.GetOpenAPIJSON)

		// v0.5.0 Task 11: embedded OpenAPI browser (spec J.2).
		// Gated by admin OR metrics:read — the same gate as /metrics —
		// so any operator with the metrics:read permission can access the
		// API docs viewer without full admin escalation.
		// RequireBearerOrSession + RequireSession run first (outer group
		// above) so unauthenticated callers get 401 before the perm check.
		r.Group(func(r chi.Router) {
			r.Use(RequireBearerOrSession(d.Bearer, d.Users))
			r.Use(d.RequireSession)
			r.Use(d.requireMetricsRead)
			vh := viewer.New(d.Log)
			r.Route("/openapi/viewer", vh.Routes)
		})

		// v0.5.0 Task 15: database-backend status endpoint.
		// Gated by admin OR metrics:read (same as /metrics and /openapi/viewer).
		// Returns driver name, redacted URL, and postgres_alpha flag.
		r.Group(func(r chi.Router) {
			r.Use(RequireBearerOrSession(d.Bearer, d.Users))
			r.Use(d.RequireSession)
			r.Use(d.requireDatabaseRead)
			r.Get("/database", d.GetDatabaseStatus)
		})

		r.With(loginPerIP, loginGlobal).Post("/auth/login", d.Login)

		// Client discovery: public, like login, because a client asks before
		// it has a token. Rate-limited per source IP. It answers with the
		// public control endpoint and versions only (see ClientDiscovery).
		r.With(d.discoveryRateLimiter()).Get("/client/discovery", d.GetClientDiscovery)

		// Client sign-in, the client's side: public, because a client has no
		// token yet. start opens a request, poll collects its outcome with
		// the device code start returned. Both are rate-limited per source
		// IP and never cacheable. The dashboard's side (look at a request,
		// approve, deny) is in the session group below, behind the CSRF check.
		r.With(loginStartLimit, d.clientLoginAnon, middleware.Timeout(JSONHandlerTimeout)).
			Post("/client/login/start", d.PostClientLoginStart)
		r.With(loginPollLimit, d.clientLoginAnon, middleware.Timeout(JSONHandlerTimeout)).
			Post("/client/login/poll", d.PostClientLoginPoll)

		// JSON routes: session-protected + CSRF-protected + JSONHandlerTimeout.
		// RequireCSRF is placed after RequireSession so unauthenticated requests
		// get 401 before CSRF validation runs. Safe methods (GET/HEAD/OPTIONS)
		// pass through CSRF unconditionally — future state-changing routes added
		// to this group are automatically protected.
		//
		// v0.4.0 Task 18: RequireBearerOrSession runs FIRST in this group.
		// When the request carries Authorization: Bearer bua_<token> it ctx-
		// injects userID + bearerTokenID + bearerPerms + role; RequireSession
		// then short-circuits (sees the userID already set) and RequireCSRF
		// skips the double-submit check (bearer secret IS the CSRF defense).
		// When the header is absent, RequireBearerOrSession is a no-op and
		// the cookie flow proceeds unchanged.
		r.Group(func(r chi.Router) {
			r.Use(RequireBearerOrSession(d.Bearer, d.Users))
			r.Use(d.RequireSession)
			r.Use(d.withAuditContext)
			r.Use(RequireCSRF)
			r.Use(middleware.Timeout(JSONHandlerTimeout))
			r.Post("/auth/logout", d.Logout)
			r.Post("/auth/change-password", d.ChangePassword)
			// v0.4.0 Task 20: backup / restore JSON API (spec Part L.3).
			// All routes are gated by requireBackupRun (admin OR
			// authz.PermBackupRun). The download streams application/x-gzip;
			// every other route is JSON. POST /backups/restore is multipart.
			r.With(d.requireBackupRun).Get("/backups", d.GetBackups)
			r.With(d.requireBackupRun).Post("/backups", d.PostBackup)
			r.With(d.requireBackupRun).Get("/backups/{id}/download", d.GetBackupDownload)
			r.With(d.requireBackupRun).Post("/backups/{id}/verify", d.PostBackupVerify)
			r.With(d.requireBackupRun).Delete("/backups/{id}", d.DeleteBackup)
			r.With(d.requireBackupRun).Post("/backups/restore", d.PostBackupRestore)
			r.With(d.requireBackupRun).Get("/backups/restores/{id}", d.GetBackupRestoreStatus)
			r.Get("/me", d.Me)
			// /clients/connect-info is session-authed (NOT admin-gated like
			// the rest of /clients/*) so the "Connect a client" wizard works
			// for any user with the tokens:write permission to mint a
			// connect token.
			r.Get("/clients/connect-info", d.GetConnectInfo)
			r.Get("/tokens", d.ListTokens)
			r.Post("/tokens", d.CreateToken)
			r.Delete("/tokens/{id}", d.RevokeToken)
			// Client sign-in, the dashboard's side. State-changing session
			// requests like any other here: session, then CSRF. A cookie
			// session is required (no automation bearer token), and wrong
			// user codes are counted per user. Approving needs the permission
			// to manage client tokens, because it creates one.
			r.With(d.requireDashboardSession).
				Get("/client/login/requests/{user_code}", d.GetClientLoginRequest)
			r.With(d.requireClientTokensManage, d.requireDashboardSession).
				Post("/client/login/requests/{user_code}/approve", d.PostClientLoginApprove)
			r.With(d.requireDashboardSession).
				Post("/client/login/requests/{user_code}/deny", d.PostClientLoginDeny)
			r.Get("/tunnels", d.ListTunnels)
			r.Get("/sessions", d.ListSessions)
			r.Delete("/sessions/{id}", d.RevokeSession)
			r.Post("/sessions/revoke-all", d.RevokeAllSessions)
			r.Put("/tunnels/{id}/access-mode", d.SetAccessMode)
			// v0.3.0: service-scoped routes (owner-gated via store authz).
			r.Get("/services", d.ListServices)
			r.Get("/services/slug-suggestion", d.GetSlugSuggestion)
			r.Get("/services/{serviceID}", d.GetService)
			r.Put("/services/{serviceID}/slug", d.PutServiceSlug)
			// AI providers: the providers table joined with live data of each
			// provider's backing service. Reads follow service visibility;
			// writes are admin-only.
			r.Get("/ai/providers", d.GetAIProviders)
			r.Get("/ai/providers/{slug}", d.GetAIProvider)
			r.Get("/ai/providers/{slug}/metrics", d.GetAIProviderMetrics)
			r.With(d.RequireAdmin).Post("/ai/providers", d.PostAIProvider)
			r.With(d.RequireAdmin).Put("/ai/providers/{slug}", d.PutAIProvider)
			r.With(d.RequireAdmin).Delete("/ai/providers/{slug}", d.DeleteAIProvider)
			// Direct providers: upstream settings and the model catalog.
			// The sync makes an outbound call, so it is admin-only too.
			r.With(d.RequireAdmin).Put("/ai/providers/{slug}/upstream", d.PutAIProviderUpstream)
			r.Get("/ai/providers/{slug}/models", d.GetAIProviderModels)
			r.With(d.RequireAdmin).Post("/ai/providers/{slug}/models/sync", d.PostAIProviderModelsSync)
			r.With(d.RequireAdmin).Post("/ai/providers/{slug}/models", d.PostAIProviderModel)
			r.With(d.RequireAdmin).Delete("/ai/providers/{slug}/models", d.DeleteAIProviderModel)
			r.Put("/services/{serviceID}/access-mode", d.SetServiceAccessMode)
			r.Get("/services/{serviceID}/api-keys", d.ListAPIKeys)
			r.Post("/services/{serviceID}/api-keys", d.CreateAPIKey)
			r.Delete("/services/{serviceID}/api-keys/{id}", d.DeleteAPIKey)
			r.Get("/services/{serviceID}/access-policy", d.GetAccessPolicy)
			r.Put("/services/{serviceID}/access-policy", d.SetAccessPolicy)
			// v0.4.0 Task 4: exact-match prompt cache JSON API.
			// GET endpoints are session-authed only (any user may read
			// settings/stats); the PUT and global DELETE are gated by
			// requireAdminOrAIConfigureAny (admin OR ai:configure:any);
			// the per-service DELETE handler does its own ownership
			// check (owner OR ai:configure:own/any).
			r.Get("/cache/settings", d.GetCacheSettings)
			r.Get("/cache/stats", d.GetCacheStats)
			r.With(d.requireAdminOrAIConfigureAny).Put("/cache/settings", d.PutCacheSettings)
			r.With(d.requireAdminOrAIConfigureAny).Delete("/cache/entries", d.DeleteCacheEntries)
			r.Delete("/services/{serviceID}/cache/entries", d.DeleteServiceCacheEntries)
			// v0.5.0 Task 4: semantic cache JSON API (spec Part A.4).
			// DELETE /cache/semantic/entries clears only the semantic tier
			// (exact-match tier unaffected). Gated by
			// requireAdminOrAIConfigureAny (admin OR ai:configure:any).
			// PUT /services/{id}/ai-config stores the per-service AI config
			// blob, validating cache.semantic.{min_similarity,embedding_mode}.
			r.With(d.requireAdminOrAIConfigureAny).Delete("/cache/semantic/entries", d.DeleteSemanticCacheEntries)
			r.Get("/services/{serviceID}/ai-config", d.GetServiceAIConfig)
			r.Put("/services/{serviceID}/ai-config", d.PutServiceAIConfig)
			// v0.5.0 Task 5: upstream-credential injection (spec Part B.2).
			// GET slots is gated by admin OR ai:configure:any (global resource).
			// Per-service GET/PUT/DELETE gate on admin OR ai:configure:any OR
			// (owner AND ai:configure:own) — enforced inside the handlers via
			// ensureCredentialAccess.
			r.With(d.requireSlotsRead).Get("/upstream-credentials/slots", d.GetUpstreamCredentialSlots)
			r.Get("/services/{serviceID}/upstream-credential", d.GetServiceUpstreamCredential)
			r.Put("/services/{serviceID}/upstream-credential", d.PutServiceUpstreamCredential)
			r.Delete("/services/{serviceID}/upstream-credential", d.DeleteServiceUpstreamCredential)
			// v0.5.0 Task 7: custom domain CRUD (spec D.2 / D.3).
			// Permission: admin OR services:configure:any OR (owner AND
			// services:configure:own). Enforced inside handlers via
			// ensureCustomDomainAccess.
			r.Get("/services/{serviceID}/domains", d.ListCustomDomains)
			r.Post("/services/{serviceID}/domains", d.PostCustomDomain)
			r.Get("/services/{serviceID}/domains/{did}", d.GetCustomDomain)
			r.Put("/services/{serviceID}/domains/{did}", d.PutCustomDomain)
			r.Delete("/services/{serviceID}/domains/{did}", d.DeleteCustomDomain)
			// v0.5.0 Task 8: per-tunnel connection logs (spec E.2).
			// All three routes are gated by requireConnLogRead (admin OR
			// audit:read). /export streams ndjson or csv; no CSRF needed
			// for GET. No pagination cursor on /rollups (full result set
			// per filter window). /export uses a fixed max-limit (1000
			// rows); full streaming is deferred to Task 17.
			r.With(d.requireConnLogRead).Get("/connection-logs", d.GetConnectionLogs)
			r.With(d.requireConnLogRead).Get("/connection-logs/rollups", d.GetConnectionLogRollups)
			r.With(d.requireConnLogRead).Get("/connection-logs/export", d.GetConnectionLogsExport)
			// v0.4.0 Task 5: redaction rules + settings + preview JSON API.
			// GET endpoints are session-authed (any user may read); mutations
			// (POST/PUT/DELETE rules, PUT settings, POST preview) are gated
			// by requireAdminOrAIConfigureAny (admin OR ai:configure:any),
			// same pattern as the cache mutation routes.
			r.Get("/redaction/rules", d.GetRedactionRules)
			r.With(d.requireAdminOrAIConfigureAny).Post("/redaction/rules", d.PostRedactionRule)
			r.With(d.requireAdminOrAIConfigureAny).Put("/redaction/rules/{id}", d.PutRedactionRule)
			r.With(d.requireAdminOrAIConfigureAny).Delete("/redaction/rules/{id}", d.DeleteRedactionRule)
			r.Get("/redaction/settings", d.GetRedactionSettings)
			r.With(d.requireAdminOrAIConfigureAny).Put("/redaction/settings", d.PutRedactionSettings)
			r.With(d.requireAdminOrAIConfigureAny).Post("/redaction/preview", d.PostRedactionPreview)
			// v0.4.0 Task 6: prompt-injection guardrails JSON API.
			// GET endpoints are session-authed (any user may read settings
			// and the bundled-pattern list); PUT settings is gated by
			// requireAdminOrAIConfigureAny (admin OR ai:configure:any),
			// same pattern as the cache/redaction mutation routes.
			r.Get("/guardrails/settings", d.GetGuardrailSettings)
			r.With(d.requireAdminOrAIConfigureAny).Put("/guardrails/settings", d.PutGuardrailSettings)
			r.Get("/guardrails/patterns", d.GetGuardrailPatterns)
			// v0.4.0 Task 7: model alias registry (spec Part C.1).
			// GET is session-authed (any user may read); POST/PUT/DELETE are
			// admin OR ai:configure:any — same gating as cache/redaction/
			// guardrails mutations.
			r.Get("/models/aliases", d.GetModelAliases)
			r.With(d.requireAdminOrAIConfigureAny).Post("/models/aliases", d.PostModelAlias)
			r.With(d.requireAdminOrAIConfigureAny).Put("/models/aliases/{alias}", d.PutModelAlias)
			r.With(d.requireAdminOrAIConfigureAny).Delete("/models/aliases/{alias}", d.DeleteModelAlias)
			// v0.4.0 Task 11: rate-limit + quota CRUD (spec Part D.2).
			// Reads are gated by quotas:read:own/:any (admin always passes);
			// mutations require admin OR quotas:manage:any. The /usage
			// endpoint also reads — same gate as the list endpoint.
			r.With(d.requireQuotasReadOwnOrAny).Get("/rate-limits", d.GetRateLimits)
			r.With(d.requireQuotasReadOwnOrAny).Get("/rate-limits/usage", d.GetRateLimitUsage)
			r.With(d.requireQuotasManageAny).Post("/rate-limits", d.PostRateLimit)
			r.With(d.requireQuotasManageAny).Put("/rate-limits/{id}", d.PutRateLimit)
			r.With(d.requireQuotasManageAny).Delete("/rate-limits/{id}", d.DeleteRateLimit)
			// v0.4.0 Task 12: cost pricing + budgets + summary/export
			// (spec Part F). Permission tiers:
			//   - GET /cost/pricing — session-authed (every signed-in
			//     user may read the table; same as cache/redaction
			//     settings).
			//   - PUT /cost/pricing, POST/PUT/DELETE /budgets, GET
			//     /budgets — admin-only.
			//   - GET /cost/summary, GET /cost/export — admin OR
			//     quotas:read:any (matches the spec note that summary +
			//     export are a "cost dashboard" read surface).
			r.Get("/cost/pricing", d.GetCostPricing)
			r.With(d.RequireAdmin).Put("/cost/pricing", d.PutCostPricing)
			r.With(d.requireQuotasReadAnyOrAdmin).Get("/cost/summary", d.GetCostSummary)
			r.With(d.requireQuotasReadAnyOrAdmin).Get("/cost/export", d.GetCostExport)
			r.With(d.RequireAdmin).Get("/budgets", d.GetBudgets)
			r.With(d.RequireAdmin).Post("/budgets", d.PostBudget)
			r.With(d.RequireAdmin).Put("/budgets/{id}", d.PutBudget)
			r.With(d.RequireAdmin).Delete("/budgets/{id}", d.DeleteBudget)
			// v0.4.0 Task 8: request inspector ring buffer (spec Part E).
			// Read endpoints (list, get) are gated by inspector:read:own
			// (owner) or inspector:read:any (admin) — enforced inside the
			// handler. Replay + replay-compare are gated by
			// inspector:replay:own / :any. The SSE stream is registered
			// outside this group (no CSRF, no timeout).
			r.Get("/services/{serviceID}/inspector/requests", d.ListInspectorRequests)
			r.Get("/services/{serviceID}/inspector/requests/{rid}", d.GetInspectorRequest)
			r.Post("/services/{serviceID}/inspector/requests/{rid}/replay", d.ReplayInspectorRequest)
			r.Post("/services/{serviceID}/inspector/requests/{rid}/replay-compare", d.ReplayCompareInspectorRequest)
			// v0.4.0 Task 13: audit log JSON API (spec Part G.2).
			// Every audit route is gated by requireAdminOrAuditRead
			// (admin OR holds authz.PermAuditRead). The fingerprint
			// endpoint returns the public key only — the private signing
			// key lives in settings.audit.signing_key and is never
			// surfaced.
			r.With(d.requireAdminOrAuditRead).Get("/audit/events", d.GetAuditEvents)
			r.With(d.requireAdminOrAuditRead).Get("/audit/fingerprint", d.GetAuditFingerprint)
			r.With(d.requireAdminOrAuditRead).Get("/audit/export", d.GetAuditExport)
			r.With(d.requireAdminOrAuditRead).Post("/audit/verify", d.PostAuditVerify)
			// v0.4.0 Task 14: outbound HMAC webhook delivery JSON API
			// (spec Part H.1). Every route is gated by
			// requireWebhooksManage (admin OR webhooks:manage). The
			// signing_secret plaintext is returned exactly once in the
			// POST response — never in GET / list responses.
			r.With(d.requireWebhooksManage).Get("/webhooks", d.GetWebhooks)
			r.With(d.requireWebhooksManage).Post("/webhooks", d.PostWebhook)
			r.With(d.requireWebhooksManage).Put("/webhooks/{id}", d.PutWebhook)
			r.With(d.requireWebhooksManage).Delete("/webhooks/{id}", d.DeleteWebhook)
			r.With(d.requireWebhooksManage).Post("/webhooks/{id}/test", d.PostWebhookTest)
			r.With(d.requireWebhooksManage).Post("/webhooks/{id}/pause", d.PostWebhookPause)
			r.With(d.requireWebhooksManage).Post("/webhooks/{id}/resume", d.PostWebhookResume)
			r.With(d.requireWebhooksManage).Get("/webhooks/deliveries", d.GetWebhookDeliveries)
			// v0.5.0 Task 10: payload-template preview endpoint.
			r.With(d.requireWebhooksManage).Post("/webhooks/{id}/preview", d.PostWebhookPreview)
			// v0.4.0 Task 18: automation API + bearer-tokens (spec Part M).
			// Every route gates on admin OR
			// automation:tokens:manage:own / :any (the store narrows what
			// :own callers may list/revoke). The POST response carries the
			// plaintext bearer secret EXACTLY ONCE; GET responses redact it.
			r.With(d.requireAutomationTokensManage).Get("/automation/tokens", d.GetAutomationTokens)
			r.With(d.requireAutomationTokensManage).Post("/automation/tokens", d.PostAutomationToken)
			r.With(d.requireAutomationTokensManage).Delete("/automation/tokens/{id}", d.DeleteAutomationToken)
			// v0.4.0 Task 23: `burrowd mcp` dashboard surface (spec Part P).
			// Two read-only endpoints expose the listener config + closed
			// tool inventory. Both are gated by admin OR mcp:tools:read so
			// non-admin operators with the perm can render the page. The
			// actual MCP listener (port :7800) is wired by cmd/server in
			// Task 25; these endpoints have zero behavioural coupling to it.
			r.With(d.requireMCPRead).Get("/mcp/status", d.GetMCPStatus)
			r.With(d.requireMCPRead).Get("/mcp/tools", d.GetMCPTools)
			// v0.4.0 Task 16: per-service IP/Geo CRUD + global geo status
			// (spec Part J). The per-service routes gate on owner OR
			// ipgeo:manage:any (admin always passes via the role check
			// inside ensureIPGeoServiceAccess); /geo/status is session-
			// authed only (any signed-in user may read the global flag).
			r.Get("/services/{serviceID}/ip-geo", d.GetServiceIPGeo)
			r.Put("/services/{serviceID}/ip-geo", d.PutServiceIPGeo)
			r.Get("/geo/status", d.GetGeoStatus)
			// v0.4.0 Task 15: editable custom roles + permission matrix
			// (spec Part I). The permission catalog read is session-authed
			// (every signed-in user may see the list of capabilities); the
			// list / detail reads stay admin-only (registered below); the
			// POST/PUT/DELETE writes are gated by admin OR roles:manage so
			// a curator role can edit non-builtin definitions without full
			// admin escalation.
			r.Get("/roles/permissions", d.GetRolePermissions)
			r.With(d.requireRolesManage).Post("/roles", d.PostRole)
			r.With(d.requireRolesManage).Put("/roles/{name}", d.PutRole)
			r.With(d.requireRolesManage).Delete("/roles/{name}", d.DeleteRole)
			// Admin-only user management: RequireAdmin runs after RequireSession
			// (already applied by the outer Group), so unauthenticated requests
			// get 401 before RequireAdmin's 403 check runs.
			r.Group(func(r chi.Router) {
				r.Use(d.RequireAdmin)
				r.Get("/users", d.AdminListUsers)
				r.Post("/users", d.AdminCreateUser)
				r.Patch("/users/{id}", d.AdminUpdateUser)
				r.Delete("/users/{id}", d.AdminDeleteUser)
				r.Get("/roles", d.ListRoles)
				r.Get("/roles/{name}", d.GetRole)
				r.Get("/settings", d.GetSettings)
				r.Put("/settings", d.SaveSettings)
				r.Post("/settings/test-email", d.SendTestEmail)
				// v0.5.0 Task 9: retention settings (admin only).
				r.Get("/settings/retention", d.GetRetentionSettings)
				r.Put("/settings/retention", d.PutRetentionSettings)
				r.Get("/clients", d.ListClients)
				r.Get("/clients/{sessionID}", d.GetClient)
				// v0.5.2 P3.6 / Task 9: admin-only service pre-provisioning.
				// Lets dashboard / e2e flows POST a known service_id before any
				// client connects; client-side GetOrCreateService continues to
				// upsert against the same row when the client later connects
				// with the same service id.
				r.Post("/services", d.PostService)
			})
		})

		// SSE: session-protected, NO timeout (long-lived stream).
		// CSRF is not applied here: GET is a safe method and SSE clients cannot
		// send custom headers after the connection is upgraded.
		r.Group(func(r chi.Router) {
			r.Use(d.RequireSession)
			r.Get("/events", d.EventsStream)
			// v0.4.0 Task 8: per-service inspector SSE stream. Permission
			// gating (inspector:read:own/:any + ownership) is enforced
			// inside the handler before headers are written.
			r.Get("/services/{serviceID}/inspector/stream", d.InspectorStream)
		})
	})

	// The burrow_login gate. Path-routed services redirect visitors to
	// /__burrow/login on this origin; it sits outside /api/v1, so the CSRF
	// middleware does not apply to its form POST.
	if d.Gate != nil {
		r.Handle("/__burrow/*", d.Gate)
	}

	// AI data plane. Registered before the SPA catch-all. The dashboard's own
	// AI pages live under /gateway/, so /ai/ is free for it.
	if d.AIGateway != nil {
		ah := AIPathHandler(d.AIGateway)
		r.Handle("/ai", ah)
		r.Handle("/ai/", ah)
		r.Handle("/ai/{provider}", ah)
		r.Handle("/ai/{provider}/*", ah)
	}

	// Single-origin HTTP tunnel path routing (feat/builtin-acme B3): mount
	// /svc/{slug} and /svc/{slug}/* on the host-routing proxy handler so http
	// tunnels are reachable at https://<domain>/svc/<slug>/... on the same :443 origin,
	// even when the dedicated :8443 proxy listener is disabled. Registered
	// BEFORE the SPA catch-all so these routes take precedence. When either
	// TunnelProxy or AuthDomain is unset the routes are simply not registered
	// (behavior is unchanged from before this feature).
	if d.TunnelProxy != nil && d.AuthDomain != "" {
		sh := ServicePathHandler(d.TunnelProxy, d.AuthDomain)
		r.Handle("/svc/{slug}", sh)
		r.Handle("/svc/{slug}/*", sh)
	}

	if d.SPA != nil {
		// No dashboard page is shown inside a foreign frame, where a click
		// could be steered onto a button (Approve on /link above all). The
		// headers sit on every answer of the SPA handler and not on /link
		// alone: the dashboard's router matches paths without regard to
		// case, so /LINK renders the same page through the catch-all. The
		// session cookie is SameSite=Lax and is not sent to a cross-site
		// frame anyway; this is the second lock. Nothing frames the shell
		// itself (the OpenAPI viewer is an /api/v1 page framed by it), and
		// tunnelled /svc/ and /ai/ answers never pass through here.
		shell := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Frame-Options", "DENY")
			h.Set("Content-Security-Policy", "frame-ancestors 'none'")
			d.SPA.ServeHTTP(w, r)
		})
		// /link is where `burrow login` sends the browser to approve a
		// sign-in. It is a reserved path and the one of them that is a
		// dashboard page, so it is routed by name: reading only, answered
		// for a visitor without a session too (the page sends them through
		// the login and back), and the code in its address is not passed on
		// as a referrer.
		link := getOrHead(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Referrer-Policy", "no-referrer")
			shell.ServeHTTP(w, r)
		})
		r.Handle("/link", link)
		r.Handle("/link/", link)
		// Only a root catch-all: "/api/v1" is a mounted subrouter so chi
		// matches it first; unknown/unauth /api/v1/* stays in the API group's
		// own JSON 404/401 and never falls through here. (r.NotFound is NOT
		// used: chi propagates the root NotFound into the /api/v1 subrouter,
		// which would wrongly serve the SPA for /api/v1/nope.)
		r.Handle("/*", shell)
	}

	return r
}
