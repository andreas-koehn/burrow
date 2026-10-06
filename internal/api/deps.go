// Package api is the Burrow HTTP JSON API (chi router, cookie sessions, SSE).
package api

import (
	"context"
	"crypto/x509"
	"log/slog"
	"net/http"
	"time"

	"github.com/ankoehn/burrow/internal/aigateway"
	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// ServiceStore is the subset of internal/store the service handlers need.
// *store.Store satisfies it implicitly.
type ServiceStore interface {
	ListServices(ctx context.Context, callerID, callerRole string) ([]store.ServiceView, error)
	GetService(ctx context.Context, callerID, callerRole, serviceID string) (store.ServiceDetail, error)
	// SetServiceAccessMode applies the new (mode, header) pair to the
	// service. mtlsCAPEM is meaningful only when mode == "mtls"; it carries
	// the operator-supplied PEM-encoded trust anchor for client cert
	// verification (Burrow does NOT sign certs). For any other mode the
	// argument is ignored.
	SetServiceAccessMode(ctx context.Context, callerID, callerRole, serviceID, mode, header string, mtlsCAPEM []byte) error
	ListAPIKeys(ctx context.Context, callerID, callerRole, serviceID string) ([]db.ServiceAPIKey, error)
	CreateAPIKey(ctx context.Context, callerID, callerRole, serviceID, name string) (id, plaintext string, err error)
	DeleteAPIKey(ctx context.Context, callerID, callerRole, serviceID, keyID string) error
	GetAccessPolicy(ctx context.Context, callerID, callerRole, serviceID string) ([]string, error)
	SetAccessPolicy(ctx context.Context, callerID, callerRole, serviceID string, roles []string) error
	// v0.5.2: admin-only pre-provisioning. Inserts a service row exactly as
	// supplied. Returns db.ErrDuplicateService on UNIQUE-constraint violations
	// (mapped to HTTP 409). *db.DB satisfies this directly via CreateService.
	CreateService(ctx context.Context, s db.Service) error
	SetServiceSlug(ctx context.Context, callerID, callerRole, serviceID, slug string) (old string, err error)
	SuggestSlug(ctx context.Context) (string, error)
}

// LiveTunnelSnapshot is the live/runtime subset of a tunnel that the API
// composes into service responses. It deliberately contains only the fields
// the handlers need, keeping the interface narrow and test-friendly.
type LiveTunnelSnapshot struct {
	LocalAddr  string
	Connected  bool
	RemotePort int // non-zero for tcp services; 0 for http services
}

// TunnelLocator holds the service and user association for a live tunnel,
// used by the v0.2 PUT /tunnels/{id}/access-mode back-compat path to resolve
// a tunnelID → serviceID before delegating to the service store.
type TunnelLocator struct {
	ServiceID string
	UserID    string
}

// LiveTunnelLookup is the narrow interface the service handlers use to query
// the in-memory tunnel registry for live runtime state. The concrete
// *server.Server (or an adapter) satisfies this; tests provide a fake.
// Task 12 (cmd/server wiring) will inject the real implementation.
type LiveTunnelLookup interface {
	// LookupByServiceID returns the live snapshot for the service with the
	// given serviceID. ok is false when no connected tunnel is registered for
	// that service.
	LookupByServiceID(serviceID string) (LiveTunnelSnapshot, bool)
	// LookupByTunnelID resolves a tunnel's runtime ID to its service + user.
	// ok is false when no live tunnel with that ID is registered.
	LookupByTunnelID(tunnelID string) (TunnelLocator, bool)
}

// UserStore is the subset of internal/store the API needs. *store.Store satisfies it.
type UserStore interface {
	VerifyUserPassword(ctx context.Context, email, password string) (bool, error)
	// GetUserByEmail is used by the login handler after VerifyUserPassword (spec F3).
	GetUserByEmail(ctx context.Context, email string) (db.User, error)
	GetUserByID(ctx context.Context, id string) (db.User, error)
	IssueClientToken(ctx context.Context, userID, name string) (string, error)
	ListClientTokens(ctx context.Context, userID string) ([]db.ClientToken, error)
	RevokeClientToken(ctx context.Context, id, userID string) error
	CreateSession(ctx context.Context, userID, ua, ip string) (string, error)
	ValidateSession(ctx context.Context, id string) (string, error)
	DeleteSession(ctx context.Context, id string) error
	// Multi-user methods.
	ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error
	ListUsersPage(ctx context.Context, q string, limit, offset int) ([]db.User, int, error)
	CreateUser(ctx context.Context, email, password, role string) (db.User, error)
	DeleteUser(ctx context.Context, id string) error
	UpdateUserRole(ctx context.Context, id, role string) error
	SetUserStatus(ctx context.Context, id, status string) error
	TouchUserLastLogin(ctx context.Context, id string) error
}

// RoleStore is the roles surface: read for the list/get endpoints, and the
// v0.4.0 Task 15 CRUD methods for custom-roles. *store.Store satisfies it.
// The CRUD methods are nil-safe at the route level (the handlers dispatch
// to them only when the route is registered, and Deps.Roles is set).
type RoleStore interface {
	ListRoles(ctx context.Context) ([]db.Role, error)
	GetRole(ctx context.Context, name string) (store.RoleDetail, error)
	// v0.4.0: editable custom roles.
	CreateRole(ctx context.Context, name, description string, permissions []string, defaultForNewUsers bool) error
	UpdateRole(ctx context.Context, name string, u store.RoleUpdate) error
	DeleteRole(ctx context.Context, name string) (affectedUserIDs []string, err error)
}

// SessionStore is the per-user session list/revoke surface.
type SessionStore interface {
	ListSessions(ctx context.Context, userID string) ([]db.Session, error)
	RevokeSession(ctx context.Context, id, userID string) error
	RevokeOtherSessions(ctx context.Context, userID, keepID string) (int64, error)
}

// SettingsStore is the admin settings + SMTP test surface.
type SettingsStore interface {
	GetSettings(ctx context.Context) (map[string]string, error)
	SaveSettings(ctx context.Context, kv map[string]string) error
	SendTestEmail(ctx context.Context, to string) error
}

// Pinger lets /readyz verify the database is reachable.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// ClientView is one live client (control session) for the overview.
type ClientView struct {
	SessionID     string `json:"session_id"`
	UserID        string `json:"user_id"`
	TokenName     string `json:"token_name"`
	RemoteAddr    string `json:"remote_addr"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	ClientVersion string `json:"client_version"`
	ServiceCount  int    `json:"service_count"`
	TotalBytesIn  int64  `json:"total_bytes_in"`
	TotalBytesOut int64  `json:"total_bytes_out"`
}

// ClientServiceView is one service (tunnel) under a client: live + persisted
// byte counters and the per-service access mode.
type ClientServiceView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// ServiceID is the durable-service id for http services (omitted for tcp).
	// The dashboard links to /services/{service_id} with it — the per-session
	// tunnel ID in "id" is not routable there.
	ServiceID     string `json:"service_id,omitempty"`
	RemotePort    int    `json:"remote_port"`
	LocalAddr     string `json:"local_addr"`
	AccessMode    string `json:"access_mode"`
	BytesIn       uint64 `json:"bytes_in"`        // live, since connect
	BytesOut      uint64 `json:"bytes_out"`       // live, since connect
	TotalBytesIn  int64  `json:"total_bytes_in"`  // persisted, survives reconnect
	TotalBytesOut int64  `json:"total_bytes_out"` // persisted
}

// ClientDetail is a client plus its services.
type ClientDetail struct {
	ClientView
	Services []ClientServiceView `json:"services"`
}

// ClientLister exposes live client sessions + their services (cmd/server
// adapts the registry + store totals). See client_handlers.go.
type ClientLister interface {
	ListClients() []ClientView
	GetClient(sessionID string) (ClientDetail, bool)
}

// AccessModeSetter sets a tunnel's per-service access mode (scoped to owner).
type AccessModeSetter interface {
	SetTunnelAccessMode(ctx context.Context, id, userID, mode string) error
}

// TunnelView is one live tunnel as exposed by the API.
type TunnelView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	RemotePort int    `json:"remote_port"`
	LocalAddr  string `json:"local_addr"`
	BytesIn    uint64 `json:"bytes_in"`
	BytesOut   uint64 `json:"bytes_out"`
	Connected  bool   `json:"connected"`
	// ServiceID is the durable-service id for http tunnels (omitted for tcp).
	// The dashboard's Configure dialog routes per-service endpoints
	// (/services/{service_id}/access-mode, /api-keys) through this ID — using
	// the tunnel.id for http tunnels yields 404 because per-session tunnel
	// UUIDs differ from the persisted service UUID.
	ServiceID string `json:"service_id,omitempty"`
	// URL is the public address of an http tunnel
	// (https://<auth_domain>/svc/<slug>/). Omitted for tcp tunnels and when
	// the relay has no auth domain.
	URL string `json:"url,omitempty"`
	// AccessMode is the per-service access mode currently in effect
	// ("open"/"api_key"/"burrow_login"/"mtls"). Omitted for tcp tunnels and
	// when no durable services row is wired. The Tunnels page falls back to
	// the "Open" badge as a sensible default when this is empty (P0-5).
	AccessMode string `json:"access_mode,omitempty"`
}

// TunnelLister returns the live tunnels owned by a user (cmd/server adapts the registry).
type TunnelLister interface {
	ListUserTunnels(userID string) []TunnelView
}

// EventStream is the SSE subscription side of the events bus. *events.Bus satisfies it.
type EventStream interface {
	Subscribe(userID string) (<-chan struct{}, func())
}

// LoginRateLimitPerIP is the default maximum login attempts per IP per minute.
// Per-IP accuracy is guaranteed by TrustedProxyMiddleware (C2), which runs
// before the limiter and gates XFF trust behind a trusted-CIDR allowlist.
const LoginRateLimitPerIP = 10

// LoginRateLimitGlobal is the default maximum login attempts across all IPs
// per minute. This global cap bounds concurrent argon2id CPU/RAM cost.
const LoginRateLimitGlobal = 60

// Deps are the API's injected dependencies.
// AIMetricsStore is the read surface for AI-endpoint usage aggregation over the
// usage_events table (populated by the proxy hot path). *db.DB satisfies it.
// Nil-safe: when unset, the AI-endpoint handlers report zeroed metering.
type AIMetricsStore interface {
	AIEndpointCounts24h(ctx context.Context) (map[string]db.AIEndpointCount, error)
	AIEndpointMetrics24h(ctx context.Context, serviceID string) (db.AIEndpointAgg, error)
}

// AIProviderStore is the provider surface the management API needs.
// *store.Store satisfies it.
type AIProviderStore interface {
	ListProviders(ctx context.Context) ([]db.AIProvider, error)
	ProviderBySlug(ctx context.Context, slug string) (db.AIProvider, error)
	CreateTunnelProvider(ctx context.Context, slug, name, serviceID string) (db.AIProvider, error)
	UpdateProvider(ctx context.Context, slug, newSlug, name string) (db.AIProvider, error)
	DeleteProvider(ctx context.Context, slug string) error
	CreateDirectProvider(ctx context.Context, ownerID string, in store.DirectProviderInput) (db.AIProvider, error)
	UpdateProviderUpstream(ctx context.Context, slug string, in store.DirectProviderInput) (db.AIProvider, error)
	SetProviderResponses(ctx context.Context, slug string, on bool) error
	ListProviderModels(ctx context.Context, slug string) ([]db.AIProviderModel, error)
	ReplaceProviderModels(ctx context.Context, slug string, models []db.AIProviderModel) error
	AddProviderModel(ctx context.Context, slug, modelID string) error
	RemoveProviderModel(ctx context.Context, slug, modelID string) error
}

type Deps struct {
	Users         UserStore
	Tunnels       TunnelLister
	Events        EventStream
	Log           *slog.Logger
	SecureCookies bool
	// HTTPSEnabled signals that the server itself is serving TLS natively
	// (BURROW_HTTP_TLS_CERT/KEY both set). When true:
	//   - The HSTSMiddleware adds Strict-Transport-Security to every response.
	//   - SecureCookies is forced on, regardless of the http_secure_cookies setting.
	// This flag is set by cmd/server and MUST NOT be derived from spoofable
	// headers such as X-Forwarded-Proto.
	HTTPSEnabled bool
	// SPA, if non-nil, serves the embedded dashboard for any non-/api/v1 path
	// (client-side routing). Nil keeps pure-API behavior (Phase 4b).
	SPA http.Handler
	// TrustedProxies is the list of CIDRs/IPs whose X-Forwarded-For /
	// X-Real-IP headers the server will honor. Empty means no forwarded
	// headers are trusted (safe default). Populated from config.ServerConfig
	// by cmd/server. See TrustedProxyMiddleware.
	TrustedProxies []string
	// LoginRateLimitPerIP overrides LoginRateLimitPerIP for tests; zero uses the const.
	LoginRateLimitPerIPOverride int
	// LoginRateLimitGlobalOverride overrides LoginRateLimitGlobal for tests; zero uses the const.
	LoginRateLimitGlobalOverride int
	// v0.2.0 surfaces.
	Roles       RoleStore
	Sessions    SessionStore
	Settings    SettingsStore
	Clients     ClientLister
	AccessModes AccessModeSetter
	DB          Pinger
	// v0.3.0 surfaces.
	// Services is the durable service store (api keys, access mode, policy).
	Services ServiceStore
	// AIProviders is the providers table behind /ai/providers.
	AIProviders AIProviderStore
	// HostCheck vets a direct provider's host when it is saved. nil skips
	// the check. The provider API finds out which credential slots are set
	// from CredentialVault; it never returns a slot's value.
	HostCheck func(ctx context.Context, host string) error
	// AllowPrivateUpstreams mirrors cfg.AIAllowPrivateUpstreams: when set,
	// HostCheck is not consulted.
	AllowPrivateUpstreams bool
	// FetchProviderModels reads a direct provider's model list from its
	// upstream, through the guarded upstream transport. nil = no model sync.
	FetchProviderModels func(ctx context.Context, p db.AIProvider) ([]aiprovider.Model, error)
	// LiveTunnels allows the service handlers to compose live/runtime fields
	// (connected, local_addr) into service responses. May be nil before
	// Task 12 wires the concrete server.Server implementation.
	LiveTunnels LiveTunnelLookup
	// AuthDomain is the configured auth domain (e.g. "tunnels.example.com").
	// Empty means no auth domain is configured; burrow_login mode is rejected
	// with 409 when this field is empty.
	AuthDomain string
	// HostRouting reports whether the host-routed ingress is enabled. mTLS
	// access mode needs it (a client certificate is checked during the TLS
	// handshake of a dedicated host).
	HostRouting bool
	// TunnelProxy, when non-nil, is the host-routing proxy handler. The router
	// mounts /svc/{slug} and /svc/{slug}/* on it (via ServicePathHandler) for
	// single-origin path routing. AuthDomain (above) is the base domain used
	// to synthesize the upstream host "<slug>.<AuthDomain>".
	TunnelProxy http.Handler
	// AIGateway, when non-nil, serves the /ai/{provider}/… data plane.
	AIGateway *aigateway.Gateway
	// Gate, when non-nil, serves /__burrow/* (the burrow_login form) on this
	// origin. Path-routed services redirect visitors here.
	Gate http.Handler
	// ControlListen is the relay's control-plane listen address — the value
	// `burrow connect --server …` must point at. Surfaced by the
	// /api/v1/clients/connect-info endpoint so the "Connect a client" wizard
	// can print a copy-pasteable command instead of guessing that the API
	// dashboard host:port is also the control endpoint (P1-2). Falls back to
	// the request Host header when empty.
	ControlListen string
	// MinClientVersion is the oldest client version this relay accepts, as the
	// public discovery endpoint reports it. Empty means no minimum.
	MinClientVersion string
	// DiscoveryRateLimitPerIPOverride overrides DiscoveryRateLimitPerIP for
	// tests; zero uses the const.
	DiscoveryRateLimitPerIPOverride int
	// ClientLogins is the browser-approved client sign-in (the five
	// /client/login routes). *store.Store satisfies it. Nil makes the routes
	// answer 404, as a relay without the feature does.
	ClientLogins ClientLoginStore
	// ClientLoginStartLimitOverride and ClientLoginPollLimitOverride override
	// the per-IP limits of start and poll for tests; zero uses the consts.
	ClientLoginStartLimitOverride int
	ClientLoginPollLimitOverride  int
	// clientLoginGuesses counts wrong user codes per user. NewRouter creates
	// it; clientLoginGuessClock is its clock in tests (nil: time.Now).
	clientLoginGuesses    *guessLimiter
	clientLoginGuessClock func() time.Time
	// ClientDownloadBase is where /download/ redirects to:
	// <base>/<tag>/<archive>. Empty means config.DefaultClientDownloadBase.
	ClientDownloadBase string
	// ClientDownloadDir, when set, is a directory of client archives and
	// their checksums.txt that /download/ serves itself instead of
	// redirecting, for installations without access to the release host.
	ClientDownloadDir string

	// v0.4.0 surfaces — additive; nil-safe handlers degrade gracefully.
	// CacheEngine is the exact-match prompt cache (clear/stats surface for
	// the JSON API; Lookup/Store live on the proxy hot path wired separately).
	// May be nil before Task 12 wires the concrete *exact.Cache.
	CacheEngine CacheEngine
	// CacheServices is the per-service ai-config lookup surface used by the
	// per-service cache endpoints (DELETE /services/{id}/cache/entries,
	// GET /cache/settings per_service list). May be nil before Task 24 wires
	// the typed ServiceAIConfig store.
	CacheServices CacheServiceLookup
	// ModelAliases is the CRUD surface for the model_aliases table (spec
	// Part C.1: GET/POST/PUT/DELETE /api/v1/models/aliases). *db.DB satisfies
	// it; nil disables the four routes (handlers return 500 with a clear
	// "alias store unavailable" body).
	ModelAliases ModelAliasStore
	// InspectorRings is the per-service in-memory ring-buffer manager
	// (spec Part E). *inspector.Manager satisfies it. When nil, the
	// list/get routes degrade to empty / 404 and the stream returns 500.
	InspectorRings InspectorRings
	// InspectorServices is the narrow service-ownership lookup used by the
	// inspector handlers' :own permission gate. *store.Store satisfies it
	// via GetServiceOwner; *db.DB exposes the same name. Nil disables :own
	// checks (handlers return 500 "service lookup unavailable" for :own
	// callers; :any callers still pass).
	InspectorServices InspectorOwnerLookup
	// InspectorReplayer re-fires an inspector entry's request through the
	// proxy chain. Wired in Task 10/25; nil keeps the routes registered
	// but the POST .../replay and POST .../replay-compare handlers return
	// 503 "replay engine unavailable".
	InspectorReplayer InspectorReplayer
	// RateLimitDB is the CRUD surface for the rate_limits table (spec
	// Part D.2: GET/POST/PUT/DELETE /api/v1/rate-limits). *db.DB satisfies
	// it; nil disables the four routes (handlers return 500 with a clear
	// "rate-limit store unavailable" body).
	RateLimitDB RateLimitStore
	// RateLimits is the runtime engine consulted by the proxy chain and
	// (read-side) by the /rate-limits/usage endpoint. Mutations to the
	// configured rule set call Reload synchronously so the next charge
	// sees the new shape. Wired in cmd/server (Task 12); nil keeps the
	// API routes functional but Charge always allows.
	RateLimits QuotaEngine
	// Budgets is the CRUD + usage-aggregation surface for the budgets
	// table (spec Part F). *db.DB satisfies it; nil disables all of
	// /api/v1/budgets and /api/v1/cost/export (handlers return 500 with a
	// clear "budget store unavailable" body).
	Budgets BudgetStore
	// CostEngine is the in-process pricing-table + budget-trigger engine
	// (spec Part F). Backs GET/PUT /api/v1/cost/pricing and the live
	// current_usd field returned by GET /budgets. Wired in cmd/server
	// (Task 25); nil makes GET /cost/pricing return an empty table and
	// PUT return 500, while GET /budgets still returns the configured
	// rows with current_usd=0.
	CostEngine CostEngine
	// AIMetrics aggregates usage_events into the per-endpoint request/token/
	// cache numbers shown by GET /ai/providers and .../{slug}/metrics. *db.DB
	// satisfies it; nil reports zeroed metering (early-wiring/test default).
	AIMetrics AIMetricsStore
	// AuditEvents is the read surface backing GET /audit/events (the
	// admin UI's audit log table) and POST /audit/verify (for resolving
	// first_id/last_id within a range). *db.DB satisfies it. Nil disables
	// the routes (handlers return 500 "audit store unavailable").
	AuditEvents AuditQueryStore
	// AuditChain is the action surface: verify, export, public-key. Wraps
	// an *audit.Logger via NewAuditChainAdapter. cmd/server constructs it
	// after LoadOrGenerateSigningKey wires the key. Nil disables verify /
	// export / fingerprint.
	AuditChain AuditChain
	// Webhooks is the CRUD + deliveries read surface backing every route
	// under /api/v1/webhooks (Task 14, spec Part H). *db.DB satisfies it.
	// Nil makes GET /webhooks + GET /webhooks/deliveries return [], and
	// mutating routes return 500 "webhook store unavailable".
	Webhooks WebhookStore
	// WebhookDispatcher is the singleton outbound delivery engine; the
	// handlers consume the synchronous DeliverNow seam used by
	// POST /webhooks/{id}/test. *webhook.Dispatcher satisfies it. Nil
	// makes POST .../test return 500 "dispatcher unavailable" (the
	// CRUD routes still work — the dispatcher is only required for the
	// test+deliver path).
	WebhookDispatcher WebhookDispatcher
	// WebhookSecrets is the in-memory plaintext registry the POST
	// handler populates and DELETE cleans up. *webhook.InMemorySecrets
	// satisfies it. Nil silently skips the registration call (delivery
	// will then fail signature checks for that webhook — acceptable
	// degraded mode for early wiring stages).
	WebhookSecrets WebhookSecretRegistry

	// v0.4.0 Task 18: automation token API + bearer middleware.
	// Automation is the mint/list/revoke surface (*store.Store satisfies it).
	// Bearer is the lookup + touch surface used by RequireBearerOrSession
	// (NewStoreBearerStore adapts *store.Store). When either is nil the
	// JSON routes return 500 "automation store unavailable" and the bearer
	// middleware (if registered) treats any "Authorization: Bearer …"
	// request as invalid.
	Automation AutomationStore
	Bearer     BearerStore

	// v0.4.0 Task 16: per-service IP/Geo CRUD + the global geo status
	// surface. IPGeo is the CRUD surface (db.DB satisfies it).
	// IPGeoServices is the service-ownership lookup used by the :own gate
	// (db.DB satisfies it via GetServiceByID). GeoLookup is the runtime
	// geo-resolver — proxy.NoopGeoLookup() in the default build, the
	// MMDB-backed lookup in the geo-tag-ON build (Task 17). All three may
	// be nil; the handlers degrade to 500 / enabled:false accordingly.
	IPGeo         IPGeoStore
	IPGeoServices ServiceOwnerLookup
	GeoLookup     GeoLookupSurface

	// v0.4.0 Task 20: backup / restore JSON API (spec Part L).
	// BackupDir is the on-disk directory the GET /backups handler scans and
	// where POST /backups writes new archives. Empty disables every
	// /backups route (handlers return 500 "backup directory not
	// configured"). cmd/server defaults this to <DatabasePath>.backups/.
	BackupDir string
	// BackupRunner is invoked by POST /api/v1/backups to actually produce
	// the .tar.gz at the supplied path. cmd/server wires a thin adapter
	// over runBackup; tests substitute a fake. Nil makes POST return 500
	// "backup runner unavailable".
	BackupRunner BackupRunner
	// RestoreRunner is invoked by POST /api/v1/backups/restore to extract
	// the uploaded archive and swap it into the live database. cmd/server
	// wires a thin adapter over runRestore. Nil keeps the upload working
	// (the staged file lands on disk) but the tracker reports failure with
	// a "restore runner unavailable" message, instructing the operator to
	// run `burrowd restore` against the staged file.
	RestoreRunner RestoreRunner
	// RestoreTracker is the in-memory ULID → status map keyed by
	// restore_id. cmd/server constructs a singleton with NewRestoreTracker.
	// Nil makes the POST + status routes return 500 "restore tracker
	// unavailable".
	RestoreTracker *restoreTracker
	// AuditAppender writes the audit.backup.run / audit.backup.restore
	// rows. *audit.Logger satisfies it directly. Nil silently skips the
	// audit append (the JSON route still succeeds; the chain just doesn't
	// reflect the API-driven mutation).
	AuditAppender AuditAppender

	// v0.4.0 Task 21: Prometheus /metrics endpoint.
	// Metrics is the closed-set metric recorder (spec Part O). cmd/server
	// (Task 25) constructs *metrics.Recorder, wraps it via
	// NewMetricsRecorderAdapter and assigns it here. Nil disables /metrics
	// (the handler returns 500 "metrics recorder unavailable"); the gate
	// (admin OR metrics:read) still runs first so unauthenticated callers
	// still get 401 / 403 — never a leak of internal state.
	Metrics MetricsRecorder

	// v0.4.0 Task 23: `burrowd mcp` configuration + tool registry surface.
	// The actual MCP listener (default :7800) is wired by cmd/server (Task
	// 25); the Deps surface only carries the metadata GET /api/v1/mcp/*
	// reads. Zero value (Enabled=false, Server=nil) is safe: status renders
	// {enabled:false,…} and tools/inventory renders []. Operators NEVER see
	// the BURROW_MCP_TOKEN plaintext via the API — only the row id.
	MCP MCPInfo

	// v0.5.0 Task 4: semantic cache JSON API (spec Part A.4).
	// SemanticEngine is the aggregate semantic cache surface consumed by the
	// JSON API (global clear + global stats). The concrete implementation is
	// build-tag-gated; in the default build a NoopSemanticEngine satisfying
	// the interface returns zeros and nil errors. May be nil before Task 12
	// wires the concrete implementation; all handlers degrade gracefully.
	SemanticEngine SemanticEngine
	// ServiceAIConfigs is the read/write surface for the service_ai_config
	// table, consumed by PUT /services/{id}/ai-config. *db.DB satisfies it
	// via GetServiceAIConfigRaw + UpsertServiceAIConfig. Nil disables the
	// PUT route (handler returns 500 "ai config store unavailable").
	ServiceAIConfigs ServiceAIConfigStore

	// v0.5.0 Task 5: upstream-credential injection (spec Part B.2).
	// CredentialVault is the env-only vault scanned once at startup. When nil
	// the GET /upstream-credentials/slots route returns []; PUT binding always
	// returns 400 "unknown slot" (every slot is unknown when there is no vault).
	CredentialVault CredentialVaultIface
	// CredentialDB is the CRUD surface for the service_upstream_credentials
	// table. *db.DB satisfies it. Nil degrades gracefully: GET returns
	// {slot_present:false}; PUT/DELETE return 204 (no-op).
	CredentialDB CredentialStore
	// CredentialServices is the service-ownership lookup used by the :own
	// permission gate for the per-service credential routes. Falls back to
	// IPGeoServices when nil (both point to *db.DB in production).
	CredentialServices ServiceOwnerLookup
	// NOTE: audit events for bind/unbind use the existing Deps.AuditAppender
	// field (same *audit.Logger adapter used by backup_handlers.go).

	// v0.5.0 Task 7: custom domain CRUD (spec D.2 / D.3).
	// CustomDomains is the CRUD surface for the service_custom_domains table.
	// *db.DB satisfies it. Nil degrades gracefully: GET returns []; POST/PUT
	// return 500 "custom domain store unavailable".
	CustomDomains CustomDomainStore
	// CustomDomainCache is the in-memory cert cache invalidation surface.
	// *customdomain.Store satisfies it. Nil is safe (cache simply not
	// invalidated on mutation — correct after the next process restart).
	// cmd/server wiring is deferred to Task 17.
	CustomDomainCache CustomDomainCacheInvalidator
	// CertValidationRoots overrides the system root pool used by
	// validateCertAndKey when non-nil. In production this is always nil
	// (system roots are used). Tests inject a pool containing the test CA
	// so the chain validation step trusts the test-issued cert without
	// requiring a real CA-signed certificate.
	CertValidationRoots *x509.CertPool

	// v0.5.0 Task 8: per-tunnel connection logs (spec E).
	// ConnLogDB is the read surface for GET /api/v1/connection-logs and the
	// two sub-routes (/rollups, /export). NewConnLogDBAdapter wraps *db.DB.
	// Nil disables all three routes (handlers return 500 "connection log store
	// unavailable"). cmd/server wiring is deferred to Task 17.
	ConnLogDB ConnectionLogStore

	// v0.5.0 Task 15: database backend status surface.
	// Database carries the driver name, redacted URL, and alpha flag populated
	// by cmd/server at startup (from the Backend selected by openBackend).
	// Zero value is safe: GET /api/v1/database returns {driver:"", ...} rather
	// than 500, which makes the endpoint available even before Task 17 wires
	// the full v0.5 stack in the integration compose job.
	Database DBInfo
}

// GeoLookupSurface is the Deps-facing interface that proxy.GeoLookup
// satisfies — it's a structural subset of proxy.GeoLookup (no Country method)
// so the handler can render /geo/status without importing crypto/net. The
// concrete proxy.NoopGeoLookup() and (in Task 17) MMDBGeoLookup both
// satisfy this implicitly.
type GeoLookupSurface interface {
	Enabled() bool
	DBPath() string
	DBAgeSeconds() int64
}

type ctxKey int

const (
	userIDKey ctxKey = iota
	// bearerTokenIDKey carries the automation_tokens.id of an authenticated
	// bearer token (set by RequireBearerOrSession). Its presence in the
	// context is the canonical signal that a request was bearer-authed:
	//   - RequireSession sees the userID already set and skips the cookie check.
	//   - RequireCSRF sees the token id and skips double-submit validation
	//     (the bearer secret IS the CSRF defense — it lives in a header).
	//   - Per-endpoint permission gates AND the bearer's declared permission
	//     set with the user's current role (effectivePerms helper).
	bearerTokenIDKey
	// bearerPermsKey carries the []string permission set declared at mint
	// time. The intersection with the user's current role is checked by
	// effectivePerms — role demotion immediately narrows reach.
	bearerPermsKey
	// callerRoleKey carries the user's CURRENT role (not role_at_mint).
	// Set by RequireBearerOrSession so the request hot path does not need
	// a second GetUserByID; the bearer middleware already did one.
	callerRoleKey
)

// userID returns the authenticated user id stored by RequireSession (or
// RequireBearerOrSession on bearer-authed requests), or "" if the context
// has none (safe default; every authenticated route MUST be guarded by
// RequireSession — public handlers calling this get "").
func userID(ctx context.Context) string {
	v, _ := ctx.Value(userIDKey).(string)
	return v
}

// bearerTokenID returns the automation_tokens.id of the authenticated
// bearer token for this request, or "" if the request was cookie-authed.
// Used by the CSRF middleware to skip its check on bearer-authed calls,
// and by future audit hookups to record the source token of a mutation.
func bearerTokenID(ctx context.Context) string {
	v, _ := ctx.Value(bearerTokenIDKey).(string)
	return v
}

// bearerPerms returns the closed permission set declared at mint time for
// the current bearer token, or nil for cookie-authed requests. Permission
// gates AND this slice with the user's current role.
func bearerPerms(ctx context.Context) []string {
	v, _ := ctx.Value(bearerPermsKey).([]string)
	return v
}

// callerRole returns the role attached to the request context by the
// bearer middleware. Empty when the request is cookie-authed (the existing
// callerRole(r) helper in service_handlers.go does a fresh GetUserByID for
// that path).
func callerRoleFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(callerRoleKey).(string)
	return v
}
