// v04_wiring.go — Task 25: compose every v0.4.0 engine (audit, webhook
// dispatcher, quota, cost, cache, redact, guardrails, route, inspector,
// aimeter) into a single aigw.Chain wired into the proxy via
// proxy.WithAIChain. GeoDB lookup, MCP listener, and every API-layer
// Deps field follow the same constructor seam.
//
// Everything in this file is additive over the v0.3.0 main.go: the build
// order is documented in the plan (Task 25, Step 3) and preserved here.

package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/aimeter"
	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/inspector"
	"github.com/ankoehn/burrow/internal/mcpserv"
	"github.com/ankoehn/burrow/internal/metrics"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/quota"
	"github.com/ankoehn/burrow/internal/redact"
	"github.com/ankoehn/burrow/internal/route"
	"github.com/ankoehn/burrow/internal/store"
	"github.com/ankoehn/burrow/internal/webhook"
)

// v04Stack is the bundle of v0.4.0 engines + adapters cmd/server constructs
// once and hands to:
//   - the AI chain (via aigw.NewChain),
//   - the proxy (via proxy.WithAIChain),
//   - the API Deps (every field listed in Task 25 of the plan),
//   - the optional MCP listener.
//
// Every field is non-nil when a working configuration was passed in. The
// inspector replayer is a thin closure that reuses the chain's Replay seam
// — see the inspectorReplayerAdapter below.
type v04Stack struct {
	AuditLogger       *audit.Logger
	AuditAppender     api.AuditAppender
	AuditEvents       api.AuditQueryStore
	AuditChain        api.AuditChain
	WebhookDispatcher *webhook.Dispatcher
	WebhookSecrets    *webhook.InMemorySecrets
	CostEngine        *cost.Engine
	QuotaEngine       *quota.Engine
	CacheEngine       *exact.Cache
	RedactEngine      *redact.Engine
	GuardrailsEngine  *guardrails.Engine
	InspectorMgr      *inspector.Manager
	RouteRouter       *route.Router
	MeterSink         *aimeter.SQLSink
	Metrics           *metrics.Recorder
	GeoLookup         proxy.GeoLookup
	MCPServer         *mcpserv.Server
	AIChain           *aigw.Chain
}

// buildV04Stack wires every v0.4.0 engine in dependency order. The order
// matches the build order in the plan (Task 25, Step 3):
//
//	audit → aimeter.SQLSink → cost → quota →
//	  cache → redact → guardrails → route → inspector →
//	  webhook dispatcher → aigw.Chain → mcp server
//
// The returned stack carries non-nil pointers for every dependency that
// could be constructed from the supplied config + database; transient
// errors (e.g. signing-key generation) are returned to the caller so the
// bootstrap can abort cleanly.
//
// IMPORTANT: this function does NOT start the dispatcher worker or the
// router health loop — main owns goroutine lifecycle, so those Start()
// calls live next to errc setup in main.go.
func buildV04Stack(
	ctx context.Context,
	cfg *config.ServerConfig,
	database *sql.DB,
	st *store.Store,
	log *slog.Logger,
) (*v04Stack, error) {
	wrapped := db.Wrap(database)

	// --- audit.Logger (signing key auto-gen + store back-fill) -------------
	signingKey, err := audit.LoadOrGenerateSigningKey(ctx, st)
	if err != nil {
		return nil, err
	}
	auditLogger := audit.NewLogger(wrapped, signingKey, log)
	// Back-fill the audit logger into the store so future store-side
	// mutations can chain (no current callers; the setter is the seam).
	st.SetAuditLogger(storeAuditAdapter{auditLogger})

	// --- aimeter.SQLSink + cost.Engine + dispatcher hookup ------------------
	// We construct the SQLSink first because cost.CheckBudgetsForSample is
	// attached to the SQLSink as its BudgetChecker after the cost engine is
	// built (sink → cost → sink loop closed here).
	meterSink := aimeter.NewSQLSink(wrapped)
	meterSink.Log = log

	// Cost engine: honour cfg.PricingPath when non-empty, else embedded.
	pricing, err := loadPricing(cfg.PricingPath)
	if err != nil {
		return nil, err
	}
	costEngine := cost.New(wrapped, pricing)
	meterSink.Budgets = costEngine

	// Webhook dispatcher — needed by the cost engine for budget.exceeded
	// alerts. Pair with an InMemorySecrets registry for plaintext lookup.
	webhookSecrets := webhook.NewInMemorySecrets()
	dispatcher := webhook.New(wrapped, webhookSecrets, auditLogger, log)
	costEngine.SetDispatcher(dispatcher)

	// --- quota.Engine ------------------------------------------------------
	quotaEngine := quota.New(wrapped)
	if err := quotaEngine.Reload(ctx); err != nil {
		// Reload failure is non-fatal: log and continue. The engine still
		// allows by default until the next Reload succeeds.
		log.Warn("v0.4 quota engine reload failed", "err", err)
	}

	// --- cache, redact, guardrails -----------------------------------------
	cacheEngine := exact.New(wrapped, log)
	redactEngine, err := redact.NewEngine(nil) // bundled rules only at startup
	if err != nil {
		return nil, err
	}
	guardrailsEngine := guardrails.NewEngine()

	// --- route.Router ------------------------------------------------------
	// The Router needs a Lookup that maps service-id → BackendRecord. v0.4.0
	// ships without a concrete DB.GetBackend method (deferred to v0.5 routing
	// strategies). Pass a nil Lookup: aigw.Chain only consults router.Pick
	// when service_ai_config carries a Routing policy, and in v0.4.0 that
	// happens only in tests. The nil Lookup makes Pick error out, which the
	// chain logs + ignores (the log-only seam preserves v0.3.0 behaviour).
	routeRouter := route.New(routeLookupNoop{}, nil, log)

	// --- inspector manager + replayer hookup -------------------------------
	inspectorMgr := inspector.NewManager()

	// --- aigw.Chain (composes 3–9) -----------------------------------------
	aiChain := aigw.NewChain(
		cacheEngine,
		nil, // semantic.Cache — wired in Task 17 (cmd/server startup)
		nil, // credinject.Injector — wired in Task 17 (cmd/server startup)
		redactEngine,
		guardrailsEngine,
		inspectorMgr,
		routeRouter,
		meterSink,
		log,
	)
	// Wire the DB-backed Loader (Deferral D1): on each proxied request
	// the chain reads service_ai_config to decide whether any AI feature
	// is enabled. Services without a row → ok=false → IsAIPassThrough →
	// straight to the v0.3.0 proxy handler (byte-for-byte preserved).
	// Services with a row → cache/redaction/guardrails/inspector/anthropic
	// pointers are populated and the chain runs the corresponding middleware.
	aiChain.Loader = chainConfigLoader{db: wrapped, log: log}

	// Wire quota enforcement into the chain's rate-limit hook (Task 11 /
	// spec Part D). The previously-stubbed Chain.RateLimit field is assigned
	// here so every AI-proxied request is charged one RPM unit before steps
	// 4-9 run. On denial Charge returns Allow=false; the middleware writes 429
	// and emits a ratelimit.enforced audit row without calling next.
	aiChain.RateLimit = buildQuotaMiddleware(quotaEngine, auditLogger, newQuotaEvents(dispatcher, nil))

	// A guardrail refusal leaves an audit entry (guardrail.refused, which the
	// audit logger keeps to one row per service per hour).
	aiChain.OnGuardrailRefuse = guardrailRefusalAudit(auditLogger)

	// --- metrics recorder --------------------------------------------------
	metricsRec := metrics.New()

	// --- geo lookup (default build = noop; geo build tag = real MMDB) ------
	geoLookup, err := proxy.OpenGeoDB(cfg.GeoDBPath)
	if err != nil {
		// Geo init failure: fall back to noop so misconfigured operators
		// still get a running server.
		log.Warn("v0.4 geo db open failed; running without country lookup", "err", err)
		geoLookup = proxy.NoopGeoLookup()
	}

	// --- mcp listener (deferred until caller supplies the live tunnel
	// registry; see BuildMCPServer below). When MCPListen is empty the
	// listener is disabled; main.go wires it after constructing *server.Server.
	var mcpServer *mcpserv.Server
	_ = mcpServer
	// We construct MCPServer lazily in main.go via BuildMCPServer so the
	// adapter sees the actual live tunnel registry (cycle-free at construction).

	return &v04Stack{
		AuditLogger:       auditLogger,
		AuditAppender:     auditLogger,
		AuditEvents:       wrapped,
		AuditChain:        api.NewAuditChainAdapter(auditLogger),
		WebhookDispatcher: dispatcher,
		WebhookSecrets:    webhookSecrets,
		CostEngine:        costEngine,
		QuotaEngine:       quotaEngine,
		CacheEngine:       cacheEngine,
		RedactEngine:      redactEngine,
		GuardrailsEngine:  guardrailsEngine,
		InspectorMgr:      inspectorMgr,
		RouteRouter:       routeRouter,
		MeterSink:         meterSink,
		Metrics:           metricsRec,
		GeoLookup:         geoLookup,
		MCPServer:         mcpServer,
		AIChain:           aiChain,
	}, nil
}

// loadPricing returns the operator-supplied pricing.yaml when path is
// non-empty, otherwise the embedded copy bundled with internal/cost.
func loadPricing(path string) (cost.Pricing, error) {
	if path != "" {
		return cost.LoadOverride(path)
	}
	return cost.LoadEmbedded()
}

// ---------------------------------------------------------------------------
// Adapters
// ---------------------------------------------------------------------------

// storeAuditAdapter narrows *audit.Logger to the store.AuditAppender
// surface (Append(ctx, any) error). The store stores it as a callback for
// future store-side audit emissions; today nothing on the store hot path
// calls it.
type storeAuditAdapter struct{ l *audit.Logger }

func (a storeAuditAdapter) Append(ctx context.Context, e any) error {
	if a.l == nil {
		return nil
	}
	ev, ok := e.(audit.Event)
	if !ok {
		return errors.New("storeAuditAdapter: expected audit.Event")
	}
	return a.l.Append(ctx, ev)
}

// routeLookupNoop is a stub Lookup that reports "unknown service" for
// every id. The v0.4.0 routing strategies are wired log-only inside the
// chain; a real Lookup will arrive with v0.5's backend probing.
type routeLookupNoop struct{}

func (routeLookupNoop) GetBackend(_ context.Context, _ string) (route.BackendRecord, bool, error) {
	return route.BackendRecord{}, false, nil
}

// mcpBearerAdapter narrows *store.Store's AutomationTokenView surface
// (LookupBearer/TouchBearer return store.AutomationTokenView) to the
// mcpserv.TokenInfo shape the MCP listener consumes.
type mcpBearerAdapter struct{ s *store.Store }

func (a mcpBearerAdapter) LookupBearer(ctx context.Context, hash string) (mcpserv.TokenInfo, error) {
	v, err := a.s.LookupBearer(ctx, hash)
	if err != nil {
		return mcpserv.TokenInfo{}, err
	}
	return mcpserv.TokenInfo{
		ID:          v.ID,
		UserID:      v.UserID,
		Permissions: v.Permissions,
		ExpiresAt:   v.ExpiresAt,
	}, nil
}

func (a mcpBearerAdapter) TouchBearer(ctx context.Context, id string) error {
	return a.s.TouchBearer(ctx, id)
}

// mcpToolStoreAdapter satisfies mcpserv.ToolStore by composing *store.Store,
// the live tunnel registry, the audit query store, and the metrics
// recorder. Every method maps to its REST equivalent — the MCP tools are
// thin wrappers over the same business logic.
type mcpToolStoreAdapter struct {
	st      *store.Store
	tunnels mcpTunnelSource // *server.Server adapter (live registry)
	audit   *db.DB          // SearchAuditEvents
	metrics *metrics.Recorder
}

// mcpTunnelSource is the narrow user-scoped tunnel-listing surface the MCP
// adapter needs from cmd/server's live tunnel registry. Defined here (rather
// than reusing api.TunnelLister) so the adapter has no dependency on the
// api package.
type mcpTunnelSource interface {
	ListUserTunnelsMCP(callerID string) []mcpserv.TunnelInfo
}

func (a mcpToolStoreAdapter) ListUserTunnels(_ context.Context, callerID, _ string) ([]mcpserv.TunnelInfo, error) {
	if a.tunnels == nil {
		return nil, nil
	}
	return a.tunnels.ListUserTunnelsMCP(callerID), nil
}

func (a mcpToolStoreAdapter) ListServices(ctx context.Context, callerID, callerRole string) ([]store.ServiceView, error) {
	return a.st.ListServices(ctx, callerID, callerRole)
}

func (a mcpToolStoreAdapter) GetService(ctx context.Context, callerID, callerRole, id string) (store.ServiceDetail, error) {
	return a.st.GetService(ctx, callerID, callerRole, id)
}

func (a mcpToolStoreAdapter) SetServiceAccessMode(ctx context.Context, callerID, callerRole, id, mode, header string, mtlsCAPEM []byte) error {
	return a.st.SetServiceAccessMode(ctx, callerID, callerRole, id, mode, header, mtlsCAPEM)
}

func (a mcpToolStoreAdapter) ListAPIKeys(ctx context.Context, callerID, callerRole, serviceID string) ([]db.ServiceAPIKey, error) {
	return a.st.ListAPIKeys(ctx, callerID, callerRole, serviceID)
}

func (a mcpToolStoreAdapter) CreateAPIKey(ctx context.Context, callerID, callerRole, serviceID, name string) (string, string, error) {
	return a.st.CreateAPIKey(ctx, callerID, callerRole, serviceID, name)
}

func (a mcpToolStoreAdapter) DeleteAPIKey(ctx context.Context, callerID, callerRole, serviceID, keyID string) error {
	return a.st.DeleteAPIKey(ctx, callerID, callerRole, serviceID, keyID)
}

func (a mcpToolStoreAdapter) ListClientTokens(ctx context.Context, userID string) ([]db.ClientToken, error) {
	return a.st.ListClientTokens(ctx, userID)
}

func (a mcpToolStoreAdapter) IssueClientToken(ctx context.Context, userID, name string) (string, error) {
	return a.st.IssueClientToken(ctx, userID, name)
}

func (a mcpToolStoreAdapter) ListUsers(ctx context.Context, q string, limit, offset int) ([]db.User, int, error) {
	return a.st.ListUsersPage(ctx, q, limit, offset)
}

func (a mcpToolStoreAdapter) SearchAuditEvents(ctx context.Context, q db.AuditQuery) ([]db.AuditEvent, error) {
	if a.audit == nil {
		return nil, nil
	}
	return a.audit.ListAuditEvents(ctx, q)
}

func (a mcpToolStoreAdapter) MetricsText() (string, error) {
	if a.metrics == nil {
		return "", nil
	}
	var buf metricsBuf
	if err := a.metrics.WriteText(&buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// cacheServiceLookupAdapter satisfies api.CacheServiceLookup. It is a
// thin wrapper over *db.DB so the api package stays decoupled from the
// db schema details. v0.4.0 ships without a real service_ai_config DB
// method — the adapter returns ErrNotFound so the per-service surfaces
// (DELETE /services/{id}/cache/entries, /cache/settings's per_service
// list, …) degrade to "no row" rather than panic. Task 24's typed AI
// config store will replace this with the real fetch.
type cacheServiceLookupAdapter struct{ db *db.DB }

func (a cacheServiceLookupAdapter) GetServiceOwner(ctx context.Context, serviceID string) (string, error) {
	svc, err := a.db.GetServiceByID(ctx, serviceID)
	if err != nil {
		return "", err
	}
	return svc.UserID, nil
}

func (a cacheServiceLookupAdapter) GetServiceAIConfig(_ context.Context, _ string) ([]byte, error) {
	// v0.4.0 ships without a typed service_ai_config fetch on *db.DB. The
	// per-service surfaces degrade to ErrNotFound — the handlers return
	// 404 ("no AI config for service"). Task 24 will plumb the real fetch.
	return nil, db.ErrNotFound
}

func (a cacheServiceLookupAdapter) ListAllServiceAIConfigs(_ context.Context) ([]api.CacheServiceConfigRow, error) {
	// See GetServiceAIConfig — empty list for now.
	return nil, nil
}

// mcpTunnelAdapter narrows *server.Server's ListUserTunnels to the
// mcpserv.TunnelInfo shape the MCP adapter needs. *server.Server is
// already usable directly, but the adapter exists to keep mcpserv free
// of any internal/server import.
type mcpTunnelAdapter struct{ s userTunnelLister }

func (a mcpTunnelAdapter) ListUserTunnelsMCP(callerID string) []mcpserv.TunnelInfo {
	if a.s == nil {
		return nil
	}
	tns := a.s.ListUserTunnels(callerID)
	out := make([]mcpserv.TunnelInfo, 0, len(tns))
	for _, t := range tns {
		out = append(out, mcpserv.TunnelInfo{
			ID: t.ID, Name: t.Name, Type: t.Type,
			RemotePort: t.RemotePort, LocalAddr: t.LocalAddr,
			BytesIn: t.BytesIn, BytesOut: t.BytesOut,
			Connected: t.Connected,
		})
	}
	return out
}

// metricsBuf is a tiny strings.Builder-shaped http.ResponseWriter so we
// can capture WriteText into a string without spinning up an httptest
// server. Implements the minimal http.ResponseWriter surface (Header,
// WriteHeader, Write) the recorder uses.
type metricsBuf struct {
	hdr http.Header
	b   []byte
}

func (m *metricsBuf) Header() http.Header {
	if m.hdr == nil {
		m.hdr = http.Header{}
	}
	return m.hdr
}
func (m *metricsBuf) WriteHeader(int)             {}
func (m *metricsBuf) Write(p []byte) (int, error) { m.b = append(m.b, p...); return len(p), nil }
func (m *metricsBuf) String() string              { return string(m.b) }

// ---------------------------------------------------------------------------
// Inspector replayer adapter
// ---------------------------------------------------------------------------

// BuildMCPServer constructs the *mcpserv.Server for the optional :7800
// listener. It is called from main.go after the live tunnel registry has
// been constructed (so the ToolStore adapter can reach into it). Returns
// nil when MCPListen is empty (disabled).
//
// The audit logger is wired into the server so every tools/call dispatch
// emits one mcp.tool.call audit row (Task 15 — closes spec Part P's audit
// contract).
func BuildMCPServer(
	cfg *config.ServerConfig,
	st *store.Store,
	stack *v04Stack,
	tunnels mcpTunnelSource,
	wrapped *db.DB,
	log *slog.Logger,
) *mcpserv.Server {
	if cfg.MCPListen == "" {
		return nil
	}
	toolStore := mcpToolStoreAdapter{
		st:      st,
		tunnels: tunnels,
		audit:   wrapped,
		metrics: stack.Metrics,
	}
	srv := mcpserv.New(
		mcpBearerAdapter{s: st},
		st,
		toolStore,
		log,
	)
	if stack.AuditLogger != nil {
		srv.SetAuditAppender(stack.AuditLogger)
	}
	return srv
}

// inspectorReplayerAdapter satisfies api.InspectorReplayer by re-dispatching
// the captured request through the AI chain. The chain owns the per-service
// inspector ring; replay populates it the same way a live request would.
//
// The api.InspectorReplayer surface takes (serviceID, *http.Request); the
// underlying aigw.Chain.Replay takes (aigw.Service, *http.Request,
// http.Handler). The adapter resolves serviceID → Service via the chain's
// loader (when set) and supplies a no-op proxy handler so replayed
// requests never touch upstream — the inspector capture from the request
// path is what the caller actually consumes.
type inspectorReplayerAdapter struct {
	chain *aigw.Chain
	log   *slog.Logger
}

// newInspectorReplayer wraps a chain into the api.InspectorReplayer shape.
func newInspectorReplayer(chain *aigw.Chain, log *slog.Logger) api.InspectorReplayer {
	return inspectorReplayerAdapter{chain: chain, log: log}
}

// Replay re-fires r through the chain. Returns the resulting Entry. The
// proxyHandler is a no-op: replay does not hit upstream — we want the
// inspector capture from the chain itself, not a live response.
//
// The chain's Loader resolves the service's AIConfig so the replay path
// exercises the same middleware chain (cache/redaction/guardrails/
// inspector/anthropic) as a live request. Without this, svc.AIConfig
// would be empty and Chain.run's captureEntry would silently no-op
// because cfg.Inspector is nil — meaning replay never adds a new entry
// to the per-service ring and the API caller sees a synthesised
// minimal entry instead of one that actually flowed through the chain.
// Loader errors are non-fatal: we fall back to an empty AIConfig so
// the replay still produces a response (with the same fail-open
// semantics as Chain.Dispatch).
func (a inspectorReplayerAdapter) Replay(ctx context.Context, serviceID string, r *http.Request) (inspector.Entry, error) {
	if a.chain == nil {
		return inspector.Entry{}, errors.New("aigw chain not wired")
	}
	svc := aigw.Service{ID: serviceID}
	if a.chain.Loader != nil {
		loaded, ok, err := a.chain.Loader.LoadAIConfig(ctx, serviceID)
		if err != nil && a.log != nil {
			a.log.Warn("inspector replay: load ai config failed",
				slog.String("service_id", serviceID),
				slog.String("err", err.Error()))
		}
		if ok {
			if loaded.ID == "" {
				loaded.ID = serviceID
			}
			svc = loaded
		}
	}
	noUpstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return a.chain.Replay(ctx, svc, r, noUpstream)
}
