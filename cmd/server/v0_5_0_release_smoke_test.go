// v0_5_0_release_smoke_test.go — Release smoke test for v0.5.0 wiring.
//
// Boots a minimal httptest.Server with the full v0.5.0 Deps surface wired
// (all new Task 3–11 fields populated). Hits every new API endpoint once and
// asserts:
//   - No panics.
//   - Every response is status < 500 (may be 200, 204, 400, 403, 404 — all
//     acceptable; 500 means a missing/nil dependency crashed the handler).
//
// This is NOT an integration correctness test — correctness lives in the
// per-feature e2e suites. The goal here is the no-panic, no-5xx gate.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/metrics"
	"github.com/ankoehn/burrow/internal/store"
	"github.com/ankoehn/burrow/internal/webhook"
)

// smokeStack holds the running httptest.Server + an authenticated client with
// CSRF token ready for making authed requests.
type smokeStack struct {
	srv  *httptest.Server
	hc   *http.Client
	csrf string
}

// bootSmokeServer stands up a minimal API server with the full v0.5.0 Deps
// surface wired. The caller gets a smokeStack with an admin-authed http.Client.
func bootSmokeServer(t *testing.T) *smokeStack {
	t.Helper()
	dir := t.TempDir()
	sqldb, err := db.Open(filepath.Join(dir, "smoke.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })

	wrapped := db.Wrap(sqldb)
	st := store.New(sqldb)
	const adminEmail = "admin-smoke@test"
	const adminPass = "password1-very-strong"
	if err := st.SeedAdmin(context.Background(), adminEmail, adminPass); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Build audit logger (needed for webhook dispatcher).
	signingKey, err := audit.LoadOrGenerateSigningKey(context.Background(), st)
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	auditLogger := audit.NewLogger(wrapped, signingKey, log)
	st.SetAuditLogger(storeAuditAdapter{l: auditLogger})

	// Webhook dispatcher (needed for api.Deps.WebhookDispatcher).
	secrets := webhook.NewInMemorySecrets()
	dispatcher := webhook.New(wrapped, secrets, auditLogger, log)
	dispatcher.Start()
	t.Cleanup(dispatcher.Close)

	// Build v0.5.0 components using the same helpers as production.
	metricsRec := metrics.New()
	v05, err := buildV05Stack(context.Background(), wrapped, metricsRec, log)
	if err != nil {
		t.Fatalf("buildV05Stack: %v", err)
	}

	// Build the exact cache (normally from buildV04Stack).
	cacheEngine := exact.New(wrapped, log)

	cfg := &config.ServerConfig{}
	// Build the full v0.4.0 stack to get quota, cost, inspector etc.
	v04, err := buildV04Stack(context.Background(), cfg, sqldb, st, log)
	if err != nil {
		t.Fatalf("buildV04Stack: %v", err)
	}
	t.Cleanup(func() { v04.WebhookDispatcher.Close() })
	// Patch semantic + credinject into the chain (same as main.go does).
	v04.AIChain.Semantic = v05.SemanticCache
	v04.AIChain.CredInjector = v05.CredInjector
	_ = cacheEngine // already inside v04.CacheEngine; avoid unused-var lint

	deps := api.Deps{
		Users:             st,
		Roles:             st,
		Sessions:          st,
		Settings:          st,
		Clients:           noopClientLister{},
		AccessModes:       st,
		DB:                sqldb,
		Services:          st,
		Log:               log,
		AuditEvents:       wrapped,
		AuditChain:        api.NewAuditChainAdapter(auditLogger),
		AuditAppender:     auditLogger,
		Metrics:           api.NewMetricsRecorderAdapter(metricsRec),
		Webhooks:          wrapped,
		WebhookDispatcher: dispatcher,
		WebhookSecrets:    secrets,
		CacheEngine:       v04.CacheEngine,
		CacheServices:     cacheServiceLookupAdapter{db: wrapped},
		InspectorRings:    v04.InspectorMgr,
		InspectorServices: cacheServiceLookupAdapter{db: wrapped},
		InspectorReplayer: newInspectorReplayer(v04.AIChain, log),
		AIModels:          st,
		AIGatewayKeys:     st,
		IPGeo:             wrapped,
		IPGeoServices:     wrapped,
		GeoLookup:         v04.GeoLookup,
		RateLimitDB:       wrapped,
		RateLimits:        v04.QuotaEngine,
		Budgets:           wrapped,
		CostEngine:        v04.CostEngine,
		Bearer:            api.NewStoreBearerStore(st),
		Automation:        st,
		// v0.5.0 Task 17 wiring.
		ServiceAIConfigs:   wrapped,
		CredentialVault:    v05.CredVault,
		CredentialDB:       wrapped,
		CredentialServices: wrapped,
		CustomDomains:      wrapped,
		CustomDomainCache:  v05.CustomDomainStore,
		ConnLogDB:          v05.ConnLogDB,
		Database: api.DBInfo{
			Driver:      "sqlite",
			URLRedacted: "smoke-test.db",
		},
	}

	hsrv := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(hsrv.Close)

	// Login as admin to get session + CSRF.
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	body, _ := json.Marshal(map[string]string{"email": adminEmail, "password": adminPass})
	resp, err := hc.Post(hsrv.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status=%d body=%s", resp.StatusCode, string(b))
	}
	_ = resp.Body.Close()

	u, _ := url.Parse(hsrv.URL)
	var csrf string
	for _, ck := range jar.Cookies(u) {
		if ck.Name == "burrow_csrf" {
			csrf = ck.Value
		}
	}
	if csrf == "" {
		t.Fatal("no CSRF cookie after login")
	}

	return &smokeStack{srv: hsrv, hc: hc, csrf: csrf}
}

// doAuthedAdmin executes an authenticated JSON request using the admin session.
func (s *smokeStack) doAuthedAdmin(t *testing.T, method, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.srv.URL+path, body)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != nil || method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// noopClientLister satisfies api.ClientLister with empty results so the smoke
// server does not need a running *server.Server.
type noopClientLister struct{}

func (noopClientLister) ListClients() []api.ClientView { return nil }
func (noopClientLister) GetClient(_ string) (api.ClientDetail, bool) {
	return api.ClientDetail{}, false
}

// TestV050ReleaseSmoke boots the v0.5.0 wired API server and asserts every
// new endpoint returns < 500 (no panics, all deps wired correctly).
func TestV050ReleaseSmoke(t *testing.T) {
	srv := bootSmokeServer(t)

	type ep struct {
		method, path    string
		body            io.Reader
		wantStatusUnder int
	}

	endpoints := []ep{
		// --- Task 4: semantic cache API (spec A.4) ---
		{"GET", "/api/v1/cache/settings", nil, 500},
		{"GET", "/api/v1/cache/stats", nil, 500},
		{"DELETE", "/api/v1/cache/semantic/entries", nil, 500},
		{"PUT", "/api/v1/services/svc1/ai-config",
			strings.NewReader(`{"cache":{"semantic":{"enabled":false}}}`), 500},

		// --- Task 5: upstream-credential injection API (spec B.2) ---
		{"GET", "/api/v1/upstream-credentials/slots", nil, 500},
		{"GET", "/api/v1/services/svc1/upstream-credential", nil, 500},
		{"PUT", "/api/v1/services/svc1/upstream-credential",
			strings.NewReader(`{"slot":"OPENAI"}`), 500},
		{"DELETE", "/api/v1/services/svc1/upstream-credential", nil, 500},

		// --- Task 7: custom domains API (spec D.2) ---
		{"GET", "/api/v1/services/svc1/domains", nil, 500},

		// --- Task 8: connection logs API (spec E) ---
		{"GET", "/api/v1/connection-logs", nil, 500},
		{"GET", "/api/v1/connection-logs/rollups", nil, 500},
		{"GET", "/api/v1/connection-logs/export?format=ndjson", nil, 500},

		// --- Task 9: retention settings API (spec N) ---
		{"GET", "/api/v1/settings/retention", nil, 500},
		{"PUT", "/api/v1/settings/retention", strings.NewReader(`{}`), 500},

		// --- Task 10: webhook preview API (spec H.3) ---
		{"POST", "/api/v1/webhooks/wh1/preview",
			strings.NewReader(`{"event":"service.created","fields":{}}`), 500},

		// --- Task 11: OpenAPI viewer (spec J.2) ---
		{"GET", "/api/v1/openapi/viewer/", nil, 500},
		{"GET", "/api/v1/openapi/viewer/static/viewer.css", nil, 500},
		{"GET", "/api/v1/openapi/viewer/static/viewer.js", nil, 500},

		// --- Task 15: database status endpoint ---
		{"GET", "/api/v1/database", nil, 500},
	}

	for _, e := range endpoints {
		e := e
		t.Run(e.method+" "+e.path, func(t *testing.T) {
			resp := srv.doAuthedAdmin(t, e.method, e.path, e.body)
			defer resp.Body.Close()
			// Drain the body so the connection is reused.
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode >= e.wantStatusUnder {
				t.Errorf("%s %s: got status %d (want < %d)",
					e.method, e.path, resp.StatusCode, e.wantStatusUnder)
			}
		})
	}
}
