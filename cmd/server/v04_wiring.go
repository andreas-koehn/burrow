// v04_wiring.go — Task 25: compose every v0.4.0 engine (audit, webhook
// dispatcher, quota, cost, cache, redact, guardrails, route, inspector,
// aimeter) into a single aigw.Chain wired into the proxy via
// proxy.WithAIChain. GeoDB lookup, MCP server, and every API-layer
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
	"strings"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/aimeter"
	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/auth"
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
//   - the management MCP server.
//
// Every field is non-nil when a working configuration was passed in. The
// inspector replayer is a thin closure that reuses the chain's Replay seam
// — see the inspectorReplayerAdapter below.
type v04Stack struct {
	AuditLogger   *audit.Logger
	AuditAppender api.AuditAppender
	AuditEvents   api.AuditQueryStore
	// AuditSessionRefCutoff: see audit.RedactSubject.
	AuditSessionRefCutoff string
	AuditChain            api.AuditChain
	WebhookDispatcher     *webhook.Dispatcher
	WebhookSecrets        *webhook.InMemorySecrets
	CostEngine            *cost.Engine
	QuotaEngine           *quota.Engine
	CacheEngine           *exact.Cache
	RedactEngine          *redact.Engine
	GuardrailsEngine      *guardrails.Engine
	// GlobalGuardrails returns the global guardrail settings (cached briefly).
	GlobalGuardrails func(context.Context) guardrails.Settings
	InspectorMgr     *inspector.Manager
	RouteRouter      *route.Router
	MeterSink        *aimeter.SQLSink
	Metrics          *metrics.Recorder
	GeoLookup        proxy.GeoLookup
	MCPServer        *mcpserv.Server
	AIChain          *aigw.Chain
	// GuardrailAudit writes the guardrail.refused entries; flushed at shutdown.
	GuardrailAudit *guardrailAuditor
	// Judge is the chain's way to the judge model: main hands it the AI
	// gateway once that is built (the chain is built first).
	Judge *judgeAdapter
	// Observer receives the events of the observation points (sessions,
	// traffic, model requests) and turns them into metrics, webhook events
	// and audit entries. Flushed at shutdown.
	Observer *observer
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
	// Session rows written by older versions hold the raw session id; the
	// first start of this version marks where they end (audit.RedactSubject).
	sessionRefCutoff, err := audit.EnsureSessionRefCutoff(ctx, st)
	if err != nil {
		return nil, err
	}
	auditLogger.SetSessionRefCutoff(sessionRefCutoff)
	// The first start of this version also marks the end of the bare
	// burrow_session cookie: with Secure cookies, a bare one counts only for
	// a session created before then (auth.BareCookieCutoffKey).
	bareCookieCutoff, err := auth.EnsureBareCookieCutoff(ctx, st)
	if err != nil {
		return nil, err
	}
	st.SetBareCookieCutoff(bareCookieCutoff)
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
	// The entries an admin saved with PUT /cost/pricing are merged over the
	// bundled or file table: a saved entry wins for its model, models only
	// the start-up table has keep their price. A stored row that does not
	// parse is logged and the start-up table kept.
	if n, version, err := api.ApplyStoredPricing(ctx, st, costEngine); err != nil {
		log.Warn("cost: stored price table not applied", slog.String("err", err.Error()))
	} else if n > 0 {
		log.Info("cost: saved prices applied over the start-up table",
			slog.Int("entries", n), slog.String("version", version))
	}
	meterSink.Budgets = costEngine

	// Webhook dispatcher — needed by the cost engine for budget.exceeded
	// alerts. Pair with an InMemorySecrets registry for plaintext lookup.
	webhookSecrets := webhook.NewInMemorySecrets()
	dispatcher := webhook.New(wrapped, webhookSecrets, auditLogger, log)
	costEngine.SetDispatcher(dispatcher)
	// The store announces a service an admin created and a change of the
	// retention settings.
	st.SetEvents(dispatcher)

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
	// The global guardrails, read at most every globalGuardrailsInterval: the
	// loader applies them to a service with an AI config, the chain to a
	// gateway service without one.
	globalGuard := newGlobalGuardrails(wrapped, globalGuardrailsInterval, log)
	aiChain.Loader = chainConfigLoader{db: wrapped, log: log, global: globalGuard}
	aiChain.GlobalGuardrails = globalGuard.get

	// Wire quota enforcement into the chain's rate-limit hook (Task 11 /
	// spec Part D). The previously-stubbed Chain.RateLimit field is assigned
	// here so every AI-proxied request is charged one RPM unit before steps
	// 4-9 run. On denial Charge returns Allow=false; the middleware writes 429
	// and emits a ratelimit.enforced audit row (sampled: one per limit per
	// hour) without calling next.
	aiChain.RateLimit = buildQuotaMiddleware(quotaEngine, auditLogger, newQuotaEvents(dispatcher, nil, log))

	// A guardrail refusal leaves an audit entry (guardrail.refused, sampled
	// by the audit logger), written off the request path.
	guardrailAudit := newGuardrailAuditor(auditLogger, log)
	aiChain.OnGuardrailRefuse = guardrailAudit.refused
	// An answer an answer pattern matched leaves one too (guardrail.flagged,
	// or guardrail.refused with direction "answer" when it was withheld).
	aiChain.OnGuardrailAnswer = guardrailAudit.answered
	// The judge model is asked through the AI gateway, built later in main
	// (judgeAdapter.set); its blocks and failures are audited like the
	// pattern hits (guardrail.refused with tier "judge",
	// guardrail.judge_failed).
	judge := &judgeAdapter{}
	aiChain.Judge = judge.judge
	aiChain.OnGuardrailJudge = guardrailAudit.judged

	// --- metrics recorder --------------------------------------------------
	metricsRec := metrics.New()

	// --- observer ------------------------------------------------------------
	// One value for all observation points: the chain, the control server
	// and the proxy hand it their events.
	obs := newObserver(observerParts{
		Recorder: metricsRec, Events: dispatcher, Audit: auditLogger,
		Cost: costEngine, Chain: aiChain, Log: log, Names: wrapped,
	})
	aiChain.Observer = obs

	// --- geo lookup (default build = noop; geo build tag = real MMDB) ------
	geoLookup, err := proxy.OpenGeoDB(cfg.GeoDBPath)
	if err != nil {
		// Geo init failure: fall back to noop so misconfigured operators
		// still get a running server.
		log.Warn("v0.4 geo db open failed; running without country lookup", "err", err)
		geoLookup = proxy.NoopGeoLookup()
	}

	// --- mcp server: built in main.go via BuildMCPServer, which hands it
	// the API router once that exists.
	var mcpServer *mcpserv.Server

	return &v04Stack{
		AuditLogger:           auditLogger,
		AuditAppender:         auditLogger,
		AuditEvents:           wrapped,
		AuditSessionRefCutoff: sessionRefCutoff,
		AuditChain:            exportAnnouncingChain{AuditChain: api.NewAuditChainAdapter(auditLogger), events: dispatcher},
		WebhookDispatcher:     dispatcher,
		WebhookSecrets:        webhookSecrets,
		CostEngine:            costEngine,
		QuotaEngine:           quotaEngine,
		CacheEngine:           cacheEngine,
		RedactEngine:          redactEngine,
		GuardrailsEngine:      guardrailsEngine,
		GlobalGuardrails:      globalGuard.get,
		InspectorMgr:          inspectorMgr,
		RouteRouter:           routeRouter,
		MeterSink:             meterSink,
		Metrics:               metricsRec,
		GeoLookup:             geoLookup,
		MCPServer:             mcpServer,
		AIChain:               aiChain,
		GuardrailAudit:        guardrailAudit,
		Judge:                 judge,
		Observer:              obs,
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
// mcpserv.TokenInfo shape the MCP server consumes.
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

// ---------------------------------------------------------------------------
// Inspector replayer adapter
// ---------------------------------------------------------------------------

// BuildMCPServer constructs the *mcpserv.Server the API router mounts at
// /mcp/burrow, its tools built from spec (api.OpenAPIDocument()). Every relay
// has one; there is nothing to configure. main.go hands it the router with
// SetAPIHandler once the router is built: a tool call is replayed through
// it with the caller's bearer, so the gate, the handlers and the audit rows
// are those of the REST API.
//
// The audit logger is wired into the server so every tools/call dispatch
// emits one mcp.tool.call audit row, and the observer counts it in
// burrow_mcp_tool_calls_total with source "burrow".
func BuildMCPServer(st *store.Store, stack *v04Stack, spec []byte, log *slog.Logger) (*mcpserv.Server, error) {
	srv, err := mcpserv.New(mcpBearerAdapter{s: st}, st, spec, log)
	if err != nil {
		return nil, err
	}
	if stack.AuditLogger != nil {
		srv.SetAuditAppender(stack.AuditLogger)
	}
	if stack.Observer != nil {
		srv.SetObserver(stack.Observer)
	}
	return srv, nil
}

// warnRetiredMCPConfig says once, at start, that the variables of the
// removed MCP listener do nothing any more. They are still parsed (the _FILE
// forms are not even read), so that a relay deployed with them keeps starting.
// Only the names of the variables that are set are logged, never a value or a
// path: one of them is a secret. It reports whether there was anything to say.
func warnRetiredMCPConfig(cfg *config.ServerConfig, log *slog.Logger) bool {
	var set []string
	if cfg.MCPListen != "" {
		set = append(set, "BURROW_MCP_LISTEN")
	}
	if cfg.BurrowMCPToken != "" {
		set = append(set, "BURROW_MCP_TOKEN")
	}
	set = append(set, cfg.RetiredMCPFileVars...)
	if len(set) == 0 {
		return false
	}
	log.Warn("BURROW_MCP_LISTEN and BURROW_MCP_TOKEN are no longer used; the MCP endpoint is "+
		mcpserv.EndpointPath+" on the main address", "set", strings.Join(set, ","))
	return true
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
