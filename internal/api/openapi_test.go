package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/inspector"
	"github.com/ankoehn/burrow/internal/mcpserv"
	"github.com/ankoehn/burrow/internal/quota"
	"github.com/ankoehn/burrow/internal/store"
	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// docsOpenAPIPath is the canonical hand-written OpenAPI v3 file relative to
// the package directory. The route-coverage test loads it directly from the
// source tree (not the embedded copy) so a stale embed never masks a missing
// path entry.
const docsOpenAPIPath = "../../docs/openapi.yaml"

// routeEntry is a single (method, path) the chi mux exposes.
type routeEntry struct {
	Method string
	Path   string
}

// enumerateRoutes walks the chi mux and returns every (method, path) the
// router answers. Health probes (/healthz, /readyz) are excluded by
// convention — they exist for k8s/load-balancer use, not for SDK consumers.
// The OpenAPI doc serve endpoints are also excluded: they describe the spec
// itself, not the JSON API, and pinning them inside the doc would create a
// self-referential surface that adds no SDK value. The client hand-out
// (/install.sh, /install.ps1, /download/…) is excluded as well.
func enumerateRoutes(t *testing.T, r chi.Router) []routeEntry {
	t.Helper()
	var out []routeEntry
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		switch route {
		case "/healthz", "/readyz":
			return nil
		case "/api/v1/openapi.yaml", "/api/v1/openapi.json":
			return nil
		case "/install.sh", "/install.ps1":
			// The client hand-out serves scripts and archives to a shell,
			// not JSON to an SDK; it is outside the documented API like the
			// probes. client_download_test.go pins its behaviour.
			return nil
		}
		if route == "/download" || strings.HasPrefix(route, "/download/") {
			return nil
		}
		out = append(out, routeEntry{Method: method, Path: route})
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// loadOpenAPIPaths parses docs/openapi.yaml and returns the set of
// (method, path) entries declared in its `paths:` mapping. Each path key
// is expected to be a literal OpenAPI path (e.g. /api/v1/services/{id});
// chi path params use the same `{id}` form, so no template conversion is
// needed.
func loadOpenAPIPaths(t *testing.T, path string) map[routeEntry]bool {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs(%s): %v", path, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %s: %v", abs, err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	out := make(map[routeEntry]bool, len(doc.Paths)*4)
	for p, methods := range doc.Paths {
		for m := range methods {
			// Spec methods are typically lowercase; chi reports uppercase.
			out[routeEntry{Method: strings.ToUpper(m), Path: p}] = true
		}
	}
	return out
}

// TestOpenAPI_RouteCoverage walks the chi mux and asserts every
// (method, path) it answers is documented in docs/openapi.yaml. Adding a
// new route without a corresponding YAML entry breaks the build — this is
// the lock that keeps the hand-written doc honest.
func TestOpenAPI_RouteCoverage(t *testing.T) {
	r := NewRouter(Deps{Log: discardLog()})
	mux, ok := r.(chi.Router)
	if !ok {
		t.Fatalf("NewRouter did not return chi.Router; got %T", r)
	}
	routes := enumerateRoutes(t, mux)
	docPaths := loadOpenAPIPaths(t, docsOpenAPIPath)

	var missing []string
	for _, e := range routes {
		// /api/v1/internal/* paths are test-only routes gated behind build
		// tags (e.g. -tags=integration mounts POST /api/v1/internal/test-reset
		// from router_integration.go). They are intentionally never in the
		// shipped openapi.yaml — they must not exist in release binaries.
		if strings.HasPrefix(e.Path, "/api/v1/internal/") {
			continue
		}
		if !docPaths[e] {
			missing = append(missing, fmt.Sprintf("%s %s", e.Method, e.Path))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d route(s) not documented in %s:\n  %s",
			len(missing), docsOpenAPIPath, strings.Join(missing, "\n  "))
	}
}

// TestOpenAPIRouteCoverage_FullIntegrationMux is the v0.4.0 Task 17 lock that
// catches a route added to the chi mux behind a non-nil Deps field. The
// minimal TestOpenAPI_RouteCoverage above uses Deps{Log: discardLog()} and so
// will not notice a future route that only registers when (say) Deps.Webhooks
// is non-nil. This test wires every field cmd/server/main.go populates with a
// throwaway stub that satisfies the relevant interface — chi.Walk never
// invokes the handlers, so the stubs panic on call to make any accidental
// dereference loud and obvious. A SPA catch-all is also installed so the
// "/*" registration branch is exercised; the walk filter drops it from the
// coverage assertion (the spec describes the JSON API, not the SPA).
func TestOpenAPIRouteCoverage_FullIntegrationMux(t *testing.T) {
	stub := &fullDepsStub{}
	deps := Deps{
		Users:          stub,
		Tunnels:        stub,
		Events:         stub,
		Log:            discardLog(),
		SecureCookies:  true,
		HTTPSEnabled:   true,
		SPA:            http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		TrustedProxies: []string{"127.0.0.1/32"},
		Roles:          stub,
		Sessions:       stub,
		Settings:       stub,
		Clients:        stub,
		AccessModes:    stub,
		DB:             stub,
		Services:       stub,
		LiveTunnels:    stub,
		AuthDomain:     "tunnels.example.com",
		// v0.4.0 surfaces.
		CacheEngine:       stub,
		CacheServices:     stub,
		AIModels:          stub,
		AIGatewayKeys:     stub,
		InspectorRings:    stub,
		InspectorServices: stub,
		InspectorReplayer: stub,
		RateLimitDB:       stub,
		RateLimits:        stub,
		Budgets:           stub,
		CostEngine:        stub,
		AuditEvents:       stub,
		AuditChain:        stub,
		Webhooks:          stub,
		WebhookDispatcher: stub,
		WebhookSecrets:    stub,
		Automation:        stub,
		Bearer:            stub,
		IPGeo:             stub,
		IPGeoServices:     stub,
		GeoLookup:         stub,
		BackupDir:         t.TempDir(),
		BackupRunner:      stub,
		RestoreRunner:     stub,
		RestoreTracker:    NewRestoreTracker(),
		AuditAppender:     stub,
		Metrics:           stub,
		MCP: MCPInfo{
			Enabled: true,
			Listen:  ":7800",
			TokenID: "atk_stub",
			Server:  stub,
		},
		// v0.5.0 surfaces.
		CredentialVault:    &stubCredVaultForOpenAPI{},
		CredentialDB:       stub,
		CredentialServices: stub,
		// v0.5.0 Task 7: custom domain store (nil OK — routes registered unconditionally).
		CustomDomains: stub,
		// v0.5.0 Task 8: connection log store.
		ConnLogDB: stub,
	}
	r := NewRouter(deps)
	mux, ok := r.(chi.Router)
	if !ok {
		t.Fatalf("NewRouter did not return chi.Router; got %T", r)
	}
	routes := enumerateRoutes(t, mux)
	// Drop the SPA catch-all from the assertion — the spec describes the
	// JSON API, not the embedded dashboard. chi reports the catch-all as
	// "/*"; filter it out here rather than inside enumerateRoutes so the
	// minimal-Deps test (no SPA) is unaffected.
	// Also drop /api/v1/internal/* — these are test-only routes gated behind
	// build tags (e.g. POST /test-reset from router_integration.go). They
	// must never appear in the shipped openapi.yaml.
	filtered := routes[:0]
	for _, e := range routes {
		if e.Path == "/*" || strings.HasPrefix(e.Path, "/api/v1/internal/") {
			continue
		}
		// /link is a dashboard page as well, routed by name (spa_test.go).
		if e.Path == "/link" || e.Path == "/link/" {
			continue
		}
		filtered = append(filtered, e)
	}
	routes = filtered
	docPaths := loadOpenAPIPaths(t, docsOpenAPIPath)

	var missing []string
	for _, e := range routes {
		if !docPaths[e] {
			missing = append(missing, fmt.Sprintf("%s %s", e.Method, e.Path))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d route(s) on the full integration mux not documented in %s:\n  %s",
			len(missing), docsOpenAPIPath, strings.Join(missing, "\n  "))
	}
}

// fullDepsStub satisfies every Deps interface in one type so the route
// enumeration walk has typed (non-nil) values for each field. None of the
// methods are invoked during chi.Walk — they panic if anything is ever
// dereferenced, making a future regression that calls into Deps from
// registration time impossible to miss.
type fullDepsStub struct{}

func stubPanic(name string) {
	panic("fullDepsStub: " + name + " should not be called during route enumeration")
}

// UserStore.
func (*fullDepsStub) VerifyUserPassword(context.Context, string, string) (bool, error) {
	stubPanic("VerifyUserPassword")
	return false, nil
}
func (*fullDepsStub) GetUserByEmail(context.Context, string) (db.User, error) {
	stubPanic("GetUserByEmail")
	return db.User{}, nil
}
func (*fullDepsStub) GetUserByID(context.Context, string) (db.User, error) {
	stubPanic("GetUserByID")
	return db.User{}, nil
}
func (*fullDepsStub) IssueClientToken(context.Context, string, string) (string, error) {
	stubPanic("IssueClientToken")
	return "", nil
}
func (*fullDepsStub) ListClientTokens(context.Context, string) ([]db.ClientToken, error) {
	stubPanic("ListClientTokens")
	return nil, nil
}
func (*fullDepsStub) RevokeClientToken(context.Context, string, string) error {
	stubPanic("RevokeClientToken")
	return nil
}
func (*fullDepsStub) CreateSession(context.Context, string, string, string) (string, error) {
	stubPanic("CreateSession")
	return "", nil
}
func (*fullDepsStub) ValidateSession(context.Context, string) (string, error) {
	stubPanic("ValidateSession")
	return "", nil
}
func (*fullDepsStub) DeleteSession(context.Context, string) error {
	stubPanic("DeleteSession")
	return nil
}
func (*fullDepsStub) ChangePassword(context.Context, string, string, string) error {
	stubPanic("ChangePassword")
	return nil
}
func (*fullDepsStub) ListUsersPage(context.Context, string, int, int) ([]db.User, int, error) {
	stubPanic("ListUsersPage")
	return nil, 0, nil
}
func (*fullDepsStub) CreateUser(context.Context, string, string, string) (db.User, error) {
	stubPanic("CreateUser")
	return db.User{}, nil
}
func (*fullDepsStub) DeleteUser(context.Context, string) error {
	stubPanic("DeleteUser")
	return nil
}
func (*fullDepsStub) UpdateUserRole(context.Context, string, string) error {
	stubPanic("UpdateUserRole")
	return nil
}
func (*fullDepsStub) SetUserStatus(context.Context, string, string) error {
	stubPanic("SetUserStatus")
	return nil
}
func (*fullDepsStub) TouchUserLastLogin(context.Context, string) error {
	stubPanic("TouchUserLastLogin")
	return nil
}

// TunnelLister.
func (*fullDepsStub) ListUserTunnels(string) []TunnelView {
	stubPanic("ListUserTunnels")
	return nil
}

// EventStream.
func (*fullDepsStub) Subscribe(string) (<-chan struct{}, func()) {
	stubPanic("Subscribe")
	return nil, nil
}

// RoleStore.
func (*fullDepsStub) ListRoles(context.Context) ([]db.Role, error) {
	stubPanic("ListRoles")
	return nil, nil
}
func (*fullDepsStub) GetRole(context.Context, string) (store.RoleDetail, error) {
	stubPanic("GetRole")
	return store.RoleDetail{}, nil
}
func (*fullDepsStub) CreateRole(context.Context, string, string, []string, bool) error {
	stubPanic("CreateRole")
	return nil
}
func (*fullDepsStub) UpdateRole(context.Context, string, store.RoleUpdate) error {
	stubPanic("UpdateRole")
	return nil
}
func (*fullDepsStub) DeleteRole(context.Context, string) ([]string, error) {
	stubPanic("DeleteRole")
	return nil, nil
}

// SessionStore.
func (*fullDepsStub) ListSessions(context.Context, string) ([]db.Session, error) {
	stubPanic("ListSessions")
	return nil, nil
}
func (*fullDepsStub) RevokeSession(context.Context, string, string) error {
	stubPanic("RevokeSession")
	return nil
}
func (*fullDepsStub) RevokeOtherSessions(context.Context, string, string) (int64, error) {
	stubPanic("RevokeOtherSessions")
	return 0, nil
}

// SettingsStore.
func (*fullDepsStub) GetSettings(context.Context) (map[string]string, error) {
	stubPanic("GetSettings")
	return nil, nil
}
func (*fullDepsStub) SaveSettings(context.Context, map[string]string) error {
	stubPanic("SaveSettings")
	return nil
}
func (*fullDepsStub) SendTestEmail(context.Context, string) error {
	stubPanic("SendTestEmail")
	return nil
}

// ClientLister.
func (*fullDepsStub) ListClients() []ClientView {
	stubPanic("ListClients")
	return nil
}
func (*fullDepsStub) GetClient(string) (ClientDetail, bool) {
	stubPanic("GetClient")
	return ClientDetail{}, false
}

// AccessModeSetter.
func (*fullDepsStub) SetTunnelAccessMode(context.Context, string, string, string) error {
	stubPanic("SetTunnelAccessMode")
	return nil
}

// Pinger (DB).
func (*fullDepsStub) PingContext(context.Context) error {
	stubPanic("PingContext")
	return nil
}

// ServiceStore.
func (*fullDepsStub) ListServices(context.Context, string, string) ([]store.ServiceView, error) {
	stubPanic("ListServices")
	return nil, nil
}
func (*fullDepsStub) GetService(context.Context, string, string, string) (store.ServiceDetail, error) {
	stubPanic("GetService")
	return store.ServiceDetail{}, nil
}
func (*fullDepsStub) SetServiceAccessMode(context.Context, string, string, string, string, string, []byte) error {
	stubPanic("SetServiceAccessMode")
	return nil
}
func (*fullDepsStub) ListAPIKeys(context.Context, string, string, string) ([]db.ServiceAPIKey, error) {
	stubPanic("ListAPIKeys")
	return nil, nil
}
func (*fullDepsStub) CreateAPIKey(context.Context, string, string, string, string) (string, string, error) {
	stubPanic("CreateAPIKey")
	return "", "", nil
}
func (*fullDepsStub) DeleteAPIKey(context.Context, string, string, string, string) error {
	stubPanic("DeleteAPIKey")
	return nil
}
func (*fullDepsStub) GetAccessPolicy(context.Context, string, string, string) ([]string, error) {
	stubPanic("GetAccessPolicy")
	return nil, nil
}
func (*fullDepsStub) SetAccessPolicy(context.Context, string, string, string, []string) error {
	stubPanic("SetAccessPolicy")
	return nil
}
func (*fullDepsStub) CreateService(context.Context, db.Service) error {
	stubPanic("CreateService")
	return nil
}
func (*fullDepsStub) SetServiceSlug(context.Context, string, string, string, string) (string, bool, error) {
	stubPanic("SetServiceSlug")
	return "", false, nil
}
func (*fullDepsStub) SetServiceGatewayOnly(context.Context, string, string, string, bool) error {
	stubPanic("SetServiceGatewayOnly")
	return nil
}
func (*fullDepsStub) SuggestSlug(context.Context) (string, error) {
	stubPanic("SuggestSlug")
	return "", nil
}

// LiveTunnelLookup.
func (*fullDepsStub) LookupByServiceID(string) (LiveTunnelSnapshot, bool) {
	stubPanic("LookupByServiceID")
	return LiveTunnelSnapshot{}, false
}
func (*fullDepsStub) LookupByTunnelID(string) (TunnelLocator, bool) {
	stubPanic("LookupByTunnelID")
	return TunnelLocator{}, false
}

// CacheEngine.
func (*fullDepsStub) Clear(context.Context, string) error {
	stubPanic("CacheEngine.Clear")
	return nil
}
func (*fullDepsStub) Stats(context.Context) (int, int64, float64, error) {
	stubPanic("CacheEngine.Stats")
	return 0, 0, 0, nil
}

// CacheServiceLookup + InspectorOwnerLookup share GetServiceOwner.
// ServiceOwnerLookup has GetServiceByID, distinct from GetServiceOwner — both
// live on the same stub type so the same value can be assigned to multiple
// Deps fields.
func (*fullDepsStub) GetServiceOwner(context.Context, string) (string, error) {
	stubPanic("GetServiceOwner")
	return "", nil
}
func (*fullDepsStub) GetServiceAIConfig(context.Context, string) ([]byte, error) {
	stubPanic("GetServiceAIConfig")
	return nil, nil
}
func (*fullDepsStub) ListAllServiceAIConfigs(context.Context) ([]CacheServiceConfigRow, error) {
	stubPanic("ListAllServiceAIConfigs")
	return nil, nil
}
func (*fullDepsStub) GetServiceByID(context.Context, string) (db.Service, error) {
	stubPanic("GetServiceByID")
	return db.Service{}, nil
}

// AIModelStore.
func (*fullDepsStub) ListModels(context.Context) ([]db.AIModel, error) {
	stubPanic("ListModels")
	return nil, nil
}
func (*fullDepsStub) ModelByName(context.Context, string) (db.AIModel, error) {
	stubPanic("ModelByName")
	return db.AIModel{}, nil
}
func (*fullDepsStub) CreateModel(context.Context, db.AIModel) (db.AIModel, error) {
	stubPanic("CreateModel")
	return db.AIModel{}, nil
}
func (*fullDepsStub) UpdateModel(context.Context, string, db.AIModel) (db.AIModel, error) {
	stubPanic("UpdateModel")
	return db.AIModel{}, nil
}
func (*fullDepsStub) DeleteModel(context.Context, string) error {
	stubPanic("DeleteModel")
	return nil
}

// AIGatewayKeyStore.
func (*fullDepsStub) CreateGatewayKey(context.Context, string, string, []string) (store.GatewayKey, string, error) {
	stubPanic("CreateGatewayKey")
	return store.GatewayKey{}, "", nil
}
func (*fullDepsStub) ListGatewayKeys(context.Context, string, string) ([]store.GatewayKey, error) {
	stubPanic("ListGatewayKeys")
	return nil, nil
}
func (*fullDepsStub) RevokeGatewayKey(context.Context, string, string, string) error {
	stubPanic("RevokeGatewayKey")
	return nil
}

// InspectorRings.
func (*fullDepsStub) GetOrCreate(string, int) *inspector.Ring {
	stubPanic("InspectorRings.GetOrCreate")
	return nil
}
func (*fullDepsStub) Get(string) *inspector.Ring {
	stubPanic("InspectorRings.Get")
	return nil
}

// InspectorReplayer.
func (*fullDepsStub) Replay(context.Context, string, *http.Request) (inspector.Entry, error) {
	stubPanic("Replay")
	return inspector.Entry{}, nil
}

// RateLimitStore.
func (*fullDepsStub) ListRateLimits(context.Context) ([]db.RateLimit, error) {
	stubPanic("ListRateLimits")
	return nil, nil
}
func (*fullDepsStub) GetRateLimit(context.Context, string) (db.RateLimit, error) {
	stubPanic("GetRateLimit")
	return db.RateLimit{}, nil
}
func (*fullDepsStub) CreateRateLimit(context.Context, db.RateLimit) error {
	stubPanic("CreateRateLimit")
	return nil
}
func (*fullDepsStub) UpdateRateLimit(context.Context, db.RateLimit) error {
	stubPanic("UpdateRateLimit")
	return nil
}
func (*fullDepsStub) DeleteRateLimit(context.Context, string) error {
	stubPanic("DeleteRateLimit")
	return nil
}

// QuotaEngine.
func (*fullDepsStub) Reload(context.Context) error {
	stubPanic("QuotaEngine.Reload")
	return nil
}
func (*fullDepsStub) UsageFor(context.Context, quota.Subjects) []quota.Usage {
	stubPanic("QuotaEngine.UsageFor")
	return nil
}
func (*fullDepsStub) Limits() []quota.Limit {
	stubPanic("QuotaEngine.Limits")
	return nil
}
func (*fullDepsStub) DropBucket(_, _, _, _ string) {
	stubPanic("QuotaEngine.DropBucket")
}

// BudgetStore.
func (*fullDepsStub) ListBudgets(context.Context) ([]db.Budget, error) {
	stubPanic("ListBudgets")
	return nil, nil
}
func (*fullDepsStub) GetBudget(context.Context, string) (db.Budget, error) {
	stubPanic("GetBudget")
	return db.Budget{}, nil
}
func (*fullDepsStub) CreateBudget(context.Context, db.Budget) error {
	stubPanic("CreateBudget")
	return nil
}
func (*fullDepsStub) UpdateBudget(context.Context, db.Budget) error {
	stubPanic("UpdateBudget")
	return nil
}
func (*fullDepsStub) DeleteBudget(context.Context, string) error {
	stubPanic("DeleteBudget")
	return nil
}
func (*fullDepsStub) ListUsageForWindow(context.Context, string) ([]db.UsageRow, error) {
	stubPanic("ListUsageForWindow")
	return nil, nil
}

// CostEngine.
func (*fullDepsStub) Pricing() cost.Pricing {
	stubPanic("Pricing")
	return cost.Pricing{}
}
func (*fullDepsStub) ReplacePricing(cost.Pricing) {
	stubPanic("ReplacePricing")
}
func (*fullDepsStub) Summary(context.Context, string) (cost.Summary, error) {
	stubPanic("Summary")
	return cost.Summary{}, nil
}
func (*fullDepsStub) BudgetUsages(context.Context, []db.Budget) ([]cost.BudgetUsage, error) {
	stubPanic("BudgetUsages")
	return nil, nil
}
func (*fullDepsStub) SummaryBy(context.Context, string, string) ([]cost.GroupRow, error) {
	stubPanic("SummaryBy")
	return nil, nil
}
func (*fullDepsStub) RowUSD(db.UsageRow) float64 {
	stubPanic("RowUSD")
	return 0
}
func (*fullDepsStub) UsdFor(string, int, int) float64 {
	stubPanic("UsdFor")
	return 0
}

// AuditQueryStore.
func (*fullDepsStub) ListAuditEvents(context.Context, db.AuditQuery) ([]db.AuditEvent, error) {
	stubPanic("ListAuditEvents")
	return nil, nil
}

// AuditChain.
func (*fullDepsStub) Verify(context.Context, string, string) (bool, string, error) {
	stubPanic("AuditChain.Verify")
	return false, "", nil
}
func (*fullDepsStub) ExportNDJSON(context.Context, io.Writer, audit.ExportQuery) error {
	stubPanic("ExportNDJSON")
	return nil
}
func (*fullDepsStub) PublicKey() []byte {
	stubPanic("PublicKey")
	return nil
}
func (*fullDepsStub) FingerprintHex() string {
	stubPanic("FingerprintHex")
	return ""
}

// WebhookStore.
func (*fullDepsStub) ListWebhooks(context.Context) ([]db.Webhook, error) {
	stubPanic("ListWebhooks")
	return nil, nil
}
func (*fullDepsStub) GetWebhook(context.Context, string) (db.Webhook, error) {
	stubPanic("GetWebhook")
	return db.Webhook{}, nil
}
func (*fullDepsStub) CreateWebhook(context.Context, db.Webhook) error {
	stubPanic("CreateWebhook")
	return nil
}
func (*fullDepsStub) UpdateWebhook(context.Context, db.Webhook) error {
	stubPanic("UpdateWebhook")
	return nil
}
func (*fullDepsStub) DeleteWebhook(context.Context, string) error {
	stubPanic("DeleteWebhook")
	return nil
}
func (*fullDepsStub) SetWebhookPaused(context.Context, string, bool) error {
	stubPanic("SetWebhookPaused")
	return nil
}
func (*fullDepsStub) ListWebhookDeliveries(context.Context, db.WebhookDeliveryQuery) ([]db.WebhookDelivery, error) {
	stubPanic("ListWebhookDeliveries")
	return nil, nil
}

// WebhookDispatcher.
func (*fullDepsStub) DeliverNow(context.Context, string, string, any) (int, int, error) {
	stubPanic("DeliverNow")
	return 0, 0, nil
}
func (*fullDepsStub) Publish(context.Context, string, any) {
	stubPanic("Publish")
}

// WebhookSecretRegistry.
func (*fullDepsStub) Set(string, string) {
	stubPanic("WebhookSecrets.Set")
}
func (*fullDepsStub) Delete(string) {
	stubPanic("WebhookSecrets.Delete")
}

// AutomationStore.
func (*fullDepsStub) MintAutomationToken(context.Context, string, string, string, []string, *time.Time) (store.AutomationTokenView, string, error) {
	stubPanic("MintAutomationToken")
	return store.AutomationTokenView{}, "", nil
}
func (*fullDepsStub) ListAutomationTokensForCaller(context.Context, string, string) ([]store.AutomationTokenView, error) {
	stubPanic("ListAutomationTokensForCaller")
	return nil, nil
}
func (*fullDepsStub) RevokeAutomationToken(context.Context, string, string, string) error {
	stubPanic("RevokeAutomationToken")
	return nil
}

// BearerStore.
func (*fullDepsStub) LookupBearer(context.Context, string) (AutomationTokenInfo, error) {
	stubPanic("LookupBearer")
	return AutomationTokenInfo{}, nil
}
func (*fullDepsStub) TouchBearer(context.Context, string) error {
	stubPanic("TouchBearer")
	return nil
}

// IPGeoStore.
func (*fullDepsStub) GetServiceIPGeo(context.Context, string) (db.ServiceIPGeoConfig, error) {
	stubPanic("GetServiceIPGeo")
	return db.ServiceIPGeoConfig{}, nil
}
func (*fullDepsStub) SetServiceIPGeo(context.Context, db.ServiceIPGeoConfig) error {
	stubPanic("SetServiceIPGeo")
	return nil
}

// GeoLookupSurface.
func (*fullDepsStub) Enabled() bool {
	stubPanic("GeoLookup.Enabled")
	return false
}
func (*fullDepsStub) DBPath() string {
	stubPanic("GeoLookup.DBPath")
	return ""
}
func (*fullDepsStub) DBAgeSeconds() int64 {
	stubPanic("GeoLookup.DBAgeSeconds")
	return 0
}

// BackupRunner.
func (*fullDepsStub) RunBackup(context.Context, string) error {
	stubPanic("RunBackup")
	return nil
}

// RestoreRunner.
func (*fullDepsStub) RunRestore(context.Context, string) error {
	stubPanic("RunRestore")
	return nil
}

// AuditAppender.
func (*fullDepsStub) Append(context.Context, audit.Event) error {
	stubPanic("Append")
	return nil
}

// MetricsRecorder.
func (*fullDepsStub) WriteText(http.ResponseWriter) error {
	stubPanic("WriteText")
	return nil
}
func (*fullDepsStub) SetCertExpiryDays(string, float64) {
	stubPanic("SetCertExpiryDays")
}

// MCPToolsLister.
func (*fullDepsStub) Tools() []mcpserv.ToolDescriptor {
	stubPanic("Tools")
	return nil
}

// stubCredVaultForOpenAPI satisfies CredentialVaultIface for the full-deps
// route-enumeration walk. chi.Walk does not invoke handlers, so these methods
// are never called; they satisfy the interface type-check only.
type stubCredVaultForOpenAPI struct{}

func (*stubCredVaultForOpenAPI) Get(string) (string, bool) { return "", false }
func (*stubCredVaultForOpenAPI) Slots() []string           { return nil }

// CredentialStore (v0.5.0 Task 5).
func (*fullDepsStub) GetUpstreamCredential(context.Context, string) (db.ServiceUpstreamCredential, error) {
	stubPanic("CredentialStore.GetUpstreamCredential")
	return db.ServiceUpstreamCredential{}, nil
}
func (*fullDepsStub) UpsertUpstreamCredential(context.Context, db.ServiceUpstreamCredential) error {
	stubPanic("CredentialStore.UpsertUpstreamCredential")
	return nil
}
func (*fullDepsStub) DeleteUpstreamCredential(context.Context, string) error {
	stubPanic("CredentialStore.DeleteUpstreamCredential")
	return nil
}

// ConnectionLogStore (v0.5.0 Task 8).
func (*fullDepsStub) ListConnectionLogs(context.Context, connlog.ConnLogQuery) ([]db.ConnectionLog, error) {
	stubPanic("ConnectionLogStore.ListConnectionLogs")
	return nil, nil
}
func (*fullDepsStub) ListConnectionLogRollups(context.Context, connlog.RollupQuery) ([]db.ConnectionLogRollup, error) {
	stubPanic("ConnectionLogStore.ListConnectionLogRollups")
	return nil, nil
}
func (*fullDepsStub) ListConnectionLogRollupTopIPs(context.Context, string, string, string) ([]connlog.TopIP, error) {
	stubPanic("ConnectionLogStore.ListConnectionLogRollupTopIPs")
	return nil, nil
}
func (*fullDepsStub) ListConnectionLogRollupTopIPsBatch(context.Context, []connlog.TopIPsGroup) (map[connlog.TopIPsGroup][]connlog.TopIP, error) {
	stubPanic("ConnectionLogStore.ListConnectionLogRollupTopIPsBatch")
	return nil, nil
}

// CustomDomainStore (v0.5.0 Task 7).
func (*fullDepsStub) InsertCustomDomain(context.Context, db.ServiceCustomDomain) (db.ServiceCustomDomain, error) {
	stubPanic("CustomDomainStore.InsertCustomDomain")
	return db.ServiceCustomDomain{}, nil
}
func (*fullDepsStub) UpdateCustomDomain(context.Context, db.ServiceCustomDomain) error {
	stubPanic("CustomDomainStore.UpdateCustomDomain")
	return nil
}
func (*fullDepsStub) GetCustomDomain(context.Context, string, string) (db.ServiceCustomDomain, error) {
	stubPanic("CustomDomainStore.GetCustomDomain")
	return db.ServiceCustomDomain{}, nil
}
func (*fullDepsStub) ListCustomDomains(context.Context, string) ([]db.ServiceCustomDomain, error) {
	stubPanic("CustomDomainStore.ListCustomDomains")
	return nil, nil
}
func (*fullDepsStub) DeleteCustomDomain(context.Context, string, string) error {
	stubPanic("CustomDomainStore.DeleteCustomDomain")
	return nil
}
func (*fullDepsStub) ListAllCustomDomains(context.Context) ([]db.ServiceCustomDomain, error) {
	stubPanic("CustomDomainStore.ListAllCustomDomains")
	return nil, nil
}

// TestOpenAPI_EmbedFresh asserts the embedded copy at
// internal/api/openapi.yaml matches the canonical docs/openapi.yaml byte-
// for-byte. The two files exist because go:embed cannot reach outside the
// package directory; this test is the contract that keeps them in sync.
// If you edit docs/openapi.yaml, run:
//
//	cp docs/openapi.yaml internal/api/openapi.yaml
func TestOpenAPI_EmbedFresh(t *testing.T) {
	canonical, err := os.ReadFile(docsOpenAPIPath)
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	mirror, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read embed mirror: %v", err)
	}
	if string(canonical) != string(mirror) {
		t.Fatalf("internal/api/openapi.yaml is stale; copy docs/openapi.yaml into internal/api/openapi.yaml")
	}
}

// TestOpenAPI_ServeYAML asserts GET /api/v1/openapi.yaml returns 200 with
// the embedded YAML bytes and `application/yaml` content-type. The route is
// public (no auth) so SDK tooling can curl it.
func TestOpenAPI_ServeYAML(t *testing.T) {
	srv := httptest.NewServer(NewRouter(Deps{Log: discardLog()}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Errorf("Content-Type=%q want application/yaml*", ct)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "openapi:") {
		t.Errorf("body missing openapi: header; got %q...", body[:min(120, len(body))])
	}
	if !strings.Contains(body, "paths:") {
		t.Errorf("body missing paths: section")
	}
}

// TestOpenAPI_ServeJSON asserts GET /api/v1/openapi.json returns 200 with
// the YAML converted to JSON at request time (no new dep — yaml.v3 →
// encoding/json). The content-type is application/json and the body parses
// as a generic JSON object with the same top-level keys as the YAML.
func TestOpenAPI_ServeJSON(t *testing.T) {
	srv := httptest.NewServer(NewRouter(Deps{Log: discardLog()}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type=%q want application/json*", ct)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(readBody(t, resp)), &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if _, ok := parsed["openapi"]; !ok {
		t.Errorf("JSON body missing openapi key: keys=%v", keysOf(t, parsed))
	}
	if _, ok := parsed["paths"]; !ok {
		t.Errorf("JSON body missing paths key: keys=%v", keysOf(t, parsed))
	}
}
