package main

// e2e_v050_default_test.go — Integration Task 2 (v0.5.0).
//
// One default-build end-to-end smoke that proves the v0.5.0 wiring in
// cmd/server actually composes the new handlers under the headline build
// flavour (NoopCache + SQLite, no build tags).
//
// Unlike v0_5_0_release_smoke_test.go (which asserts only "no 5xx" across
// every endpoint), this file exercises ONE meaningful request per v0.5.0
// spec family (A–H, J) with a real assertion on the returned shape or
// state mutation. The goal is the SEAM — proving the wiring in
// cmd/server/main.go and v05_wiring.go actually composes the handlers
// against real SQLite + real store + real router. Per-feature
// correctness already lives in internal/api/*_test.go.
//
// Family coverage:
//
//	A semantic cache         — GET /api/v1/cache/settings: assert semantic
//	                           block present + enabled=false in default build
//	B upstream credentials   — PUT /api/v1/services/{svc}/upstream-credential
//	                           after seeding env BURROW_UPSTREAM_KEY_OPENAI
//	C multi-provider         — a direct provider, POST /api/v1/ai/models with
//	                           a target on it and POST /api/v1/ai/keys;
//	                           assert round-trip (see v050CheckSyntheticModel)
//	D custom domains         — POST /api/v1/services/{svc}/domains with a
//	                           CA-signed leaf covering "foo.example.com"
//	E connection logs        — see comment on TestV050DefaultBuildE2E/E
//	F retention              — PUT then GET /api/v1/settings/retention; the
//	                           PUT'd usage_retention_days round-trips
//	G database backend       — GET /api/v1/database: driver == "sqlite"
//	H webhook templates      — POST /api/v1/webhooks (with template) then
//	                           POST /api/v1/webhooks/{id}/preview; assert
//	                           "rendered" key present in the JSON response
//	J OpenAPI viewer         — GET /api/v1/openapi/viewer/ → 200 text/html
//
// Part E note (F-13 — wiring gap closed): The earlier shape of this test
// called connlog.NewSQLSink(...).Record(...) directly because the proxy
// never invoked the sink. F-13 closed that gap — internal/proxy/proxy.go
// now records one connection_logs row on each request close, and
// cmd/server/main.go installs the real sink via proxy.WithConnLogSink at
// startup. Part E now drives a visitor request through bootE2EStack's
// real proxy + real tunnel + real upstream and asserts a row landed —
// a true production-shaped seam, not a direct sink call.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/metrics"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/proxy/customdomain"
	"github.com/ankoehn/burrow/internal/store"
	"github.com/ankoehn/burrow/internal/webhook"
)

// v050Env is the bundle of long-lived objects owned by one bootV050E2E call.
type v050Env struct {
	dbPath    string
	sqldb     *db.DB
	srv       *httptest.Server
	hc        *http.Client
	csrf      string
	adminID   string
	serviceID string
	caPEM     string // PEM of the test CA, kept for diagnostic logging only
}

// bootV050E2E stands up the full v0.5.0 API surface against real SQLite +
// real store + real router. Pattern mirrors bootSmokeServer but adds:
//
//   - sets BURROW_UPSTREAM_KEY_OPENAI BEFORE buildV05Stack so the EnvVault
//     scan picks up the slot (Part B needs a known slot to bind),
//   - generates a self-signed test CA + plumbs it into Deps.CertValidationRoots
//     so Part D's POST /domains chain validation succeeds with the leaf cert,
//   - creates a real services row (admin-owned) so the per-service routes
//     (B, C, D) don't 404,
//   - logs in once and shares one CSRF/cookie jar across every subtest.
func bootV050E2E(t *testing.T) *v050Env {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "v050-e2e.db")
	sqldb, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })

	return buildV050EnvOnDB(t, sqldb, "sqlite", dbPath)
}

// buildV050EnvOnDB factors the post-DB-open construction of bootV050E2E out
// so a parallel boot (e.g. Postgres under the `postgres` build tag) can drive
// the same router stack. driver is "sqlite" or "postgres" — surfaced via
// Deps.Database for the Part G assertion. urlRedacted is the value reported
// in GET /api/v1/database.url_redacted (the on-disk path for SQLite, a
// redacted DSN for Postgres).
//
// The caller is responsible for: (1) opening sqldb, (2) running migrations,
// (3) ensuring the schema is empty if reuse-across-runs is undesirable, (4)
// registering close on cleanup.
func buildV050EnvOnDB(t *testing.T, sqldb *sql.DB, driver, urlRedacted string) *v050Env {
	t.Helper()

	// Slot must be visible to EnvVault at construction time (NewEnvVault
	// scans os.Environ() once). t.Setenv resets after the test.
	t.Setenv("BURROW_UPSTREAM_KEY_OPENAI", "sk-test-1")

	wrapped := db.Wrap(sqldb)
	st := store.New(sqldb)

	const adminEmail = "admin-v050@test"
	const adminPass = "password1-very-strong"
	if err := st.SeedAdmin(context.Background(), adminEmail, adminPass); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	adminUser, err := st.GetUserByEmail(context.Background(), adminEmail)
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}

	// Create a real service owned by admin (Parts B/C/D/E need a service id).
	svc, err := wrapped.GetOrCreateService(context.Background(), adminUser.ID, "echo", "http")
	if err != nil {
		t.Fatalf("get-or-create service: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Audit logger (needed by handlers that record per-mutation events).
	signingKey, err := audit.LoadOrGenerateSigningKey(context.Background(), st)
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	auditLogger := audit.NewLogger(wrapped, signingKey, log)
	st.SetAuditLogger(storeAuditAdapter{l: auditLogger})

	// Webhook dispatcher (Part H, plus cert.expiring publish in Part D).
	secrets := webhook.NewInMemorySecrets()
	dispatcher := webhook.New(wrapped, secrets, auditLogger, log)
	dispatcher.Start()
	t.Cleanup(dispatcher.Close)

	// v0.5.0 + v0.4.0 wiring — same construction path as cmd/server/main.go.
	metricsRec := metrics.New()
	v05, err := buildV05Stack(context.Background(), wrapped, metricsRec, log)
	if err != nil {
		t.Fatalf("buildV05Stack: %v", err)
	}
	cfg := &config.ServerConfig{}
	v04, err := buildV04Stack(context.Background(), cfg, sqldb, st, log)
	if err != nil {
		t.Fatalf("buildV04Stack: %v", err)
	}
	t.Cleanup(func() { v04.WebhookDispatcher.Close() })
	v04.AIChain.Semantic = v05.SemanticCache
	v04.AIChain.CredInjector = v05.CredInjector

	// Test CA + root pool so Part D's POST /domains can pass chain validation.
	caCert, caKey, caPool, caPEM := generateTestCA(t)

	deps := api.Deps{
		Users:               st,
		Roles:               st,
		Sessions:            st,
		Settings:            st,
		Clients:             noopClientLister{},
		AccessModes:         st,
		DB:                  sqldb,
		Services:            st,
		Log:                 log,
		AuditEvents:         wrapped,
		AuditChain:          api.NewAuditChainAdapter(auditLogger),
		AuditAppender:       auditLogger,
		Metrics:             api.NewMetricsRecorderAdapter(metricsRec),
		Webhooks:            wrapped,
		WebhookDispatcher:   dispatcher,
		WebhookSecrets:      secrets,
		CacheEngine:         v04.CacheEngine,
		CacheServices:       cacheServiceLookupAdapter{db: wrapped},
		InspectorRings:      v04.InspectorMgr,
		InspectorServices:   cacheServiceLookupAdapter{db: wrapped},
		InspectorReplayer:   newInspectorReplayer(v04.AIChain, log),
		AIProviders:         st,
		AIModels:            st,
		AIGatewayKeys:       st,
		IPGeo:               wrapped,
		IPGeoServices:       wrapped,
		GeoLookup:           v04.GeoLookup,
		RateLimitDB:         wrapped,
		RateLimits:          v04.QuotaEngine,
		Budgets:             wrapped,
		CostEngine:          v04.CostEngine,
		Bearer:              api.NewStoreBearerStore(st),
		Automation:          st,
		ServiceAIConfigs:    wrapped,
		CredentialVault:     v05.CredVault,
		CredentialDB:        wrapped,
		CredentialServices:  wrapped,
		CustomDomains:       wrapped,
		CustomDomainCache:   v05.CustomDomainStore,
		ConnLogDB:           v05.ConnLogDB,
		CertValidationRoots: caPool,
		Database: api.DBInfo{
			Driver:      driver,
			URLRedacted: urlRedacted,
		},
	}

	hsrv := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(hsrv.Close)

	// Log in as admin.
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

	env := &v050Env{
		dbPath:    urlRedacted,
		sqldb:     wrapped,
		srv:       hsrv,
		hc:        hc,
		csrf:      csrf,
		adminID:   adminUser.ID,
		serviceID: svc.ID,
		caPEM:     string(caPEM),
	}
	// Stash CA cert+key for Part D leaf signing. Cleared on test cleanup
	// so a sequential parallel test doesn't see a stale entry.
	testCAByEnv[env] = &testCAPair{cert: caCert, key: caKey}
	t.Cleanup(func() { delete(testCAByEnv, env) })
	return env
}

// v050CheckSyntheticModel is Part C, shared with the Postgres flavour.
// Synthetic models replaced the model aliases of v0.5.0: a model is created
// through the API with a target on a direct provider, a gateway key is
// created for it, and the provider cannot be deleted while the model targets
// it. Everything it creates is removed again.
func v050CheckSyntheticModel(t *testing.T, env *v050Env) {
	t.Helper()
	code, body := env.do(t, http.MethodPost, "/api/v1/ai/providers", map[string]any{
		"slug": "e2e-direct", "name": "E2E direct", "kind": "direct",
		"base_url": "https://api.example.com/v1", "credential_slot": "OPENAI",
	})
	if code != http.StatusCreated {
		t.Fatalf("create provider: status=%d body=%s", code, body)
	}
	t.Cleanup(func() {
		env.do(t, http.MethodDelete, "/api/v1/ai/models/fast", nil)
		env.do(t, http.MethodDelete, "/api/v1/ai/providers/e2e-direct", nil)
	})

	code, body = env.do(t, http.MethodPost, "/api/v1/ai/models", map[string]any{
		"name":    "fast",
		"targets": []map[string]string{{"provider": "e2e-direct", "model": "llama3.1:8b"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create model: status=%d body=%s", code, body)
	}
	var model struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
		Targets []struct {
			Dialect, Provider, Model string
		} `json:"targets"`
		Dialects []string `json:"dialects"`
	}
	if err := json.Unmarshal(body, &model); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if model.Name != "fast" || !model.Enabled || len(model.Targets) != 1 || len(model.Dialects) != 1 || model.Dialects[0] != "openai" {
		t.Fatalf("model = %+v (body=%s)", model, body)
	}
	if tg := model.Targets[0]; tg.Dialect != "openai" || tg.Provider != "e2e-direct" || tg.Model != "llama3.1:8b" {
		t.Errorf("target = %+v, want the provider's format filled in", tg)
	}

	// The same target twice is refused, through the real store.
	code, body = env.do(t, http.MethodPost, "/api/v1/ai/models", map[string]any{
		"name": "twice",
		"targets": []map[string]string{
			{"provider": "e2e-direct", "model": "m"},
			{"dialect": "openai", "provider": "e2e-direct", "model": "m"},
		},
	})
	if code != http.StatusBadRequest || !strings.Contains(string(body), "a target is listed twice") {
		t.Errorf("duplicate targets: status=%d body=%s", code, body)
	}

	// Two creates of one name at the same moment: one wins, the other is
	// told the name is taken. Neither is a 500.
	for round := 0; round < 5; round++ {
		name := fmt.Sprintf("race-%d", round)
		// env.tryDo, not env.do: t.Fatalf must not be called from a goroutine
		// other than the test's own. A request that failed reports -1 and the
		// test fails below, from the main goroutine.
		codes := make(chan int, 2)
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			go func() {
				<-start
				c, _, err := env.tryDo(http.MethodPost, "/api/v1/ai/models", map[string]any{
					"name":    name,
					"targets": []map[string]string{{"provider": "e2e-direct", "model": "m"}},
				})
				if err != nil {
					t.Errorf("round %d: %v", round, err)
					c = -1
				}
				codes <- c
			}()
		}
		close(start)
		a, b := <-codes, <-codes
		if a < 0 || b < 0 {
			t.Fatalf("round %d: a concurrent create did not get an answer", round)
		}
		if a > b {
			a, b = b, a
		}
		if a != http.StatusCreated || b != http.StatusConflict {
			t.Errorf("round %d: concurrent creates answered %d and %d, want 201 and 409", round, a, b)
		}
		// Gone again before the provider is deleted below.
		env.do(t, http.MethodDelete, "/api/v1/ai/models/"+name, nil)
	}

	// A gateway key restricted to the model: the key comes back once.
	code, body = env.do(t, http.MethodPost, "/api/v1/ai/keys", map[string]any{"name": "e2e", "allowed_models": []string{"fast"}})
	if code != http.StatusCreated {
		t.Fatalf("create key: status=%d body=%s", code, body)
	}
	var key struct{ ID, Key string }
	if err := json.Unmarshal(body, &key); err != nil || !strings.HasPrefix(key.Key, "bgw_") || len(key.Key) != 47 {
		t.Fatalf("key response: %v body=%s", err, body)
	}
	code, body = env.do(t, http.MethodGet, "/api/v1/ai/keys", nil)
	if code != http.StatusOK || !strings.Contains(string(body), key.ID) || strings.Contains(string(body), key.Key) {
		t.Errorf("key list: status=%d body=%s", code, body)
	}
	if code, body = env.do(t, http.MethodDelete, "/api/v1/ai/keys/"+key.ID, nil); code != http.StatusNoContent {
		t.Errorf("revoke key: status=%d body=%s", code, body)
	}

	// The provider is in use: 409 naming the model, and it stays.
	code, body = env.do(t, http.MethodDelete, "/api/v1/ai/providers/e2e-direct", nil)
	if code != http.StatusConflict || !strings.Contains(string(body), "provider is used by model(s): fast") {
		t.Fatalf("delete provider in use: status=%d body=%s", code, body)
	}
	if code, body = env.do(t, http.MethodDelete, "/api/v1/ai/models/fast", nil); code != http.StatusNoContent {
		t.Fatalf("delete model: status=%d body=%s", code, body)
	}
	if code, body = env.do(t, http.MethodDelete, "/api/v1/ai/providers/e2e-direct", nil); code != http.StatusNoContent {
		t.Fatalf("delete provider: status=%d body=%s", code, body)
	}
}

// do executes an authenticated JSON request with the CSRF header on
// mutating methods. Returns (status, body bytes). It fails the test when the
// request cannot be made, so it is for the test's own goroutine; tryDo is the
// same without the test.
func (e *v050Env) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	code, b, err := e.tryDo(method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	return code, b
}

// tryDo is do with the failure returned: safe to call from any goroutine.
func (e *v050Env) tryDo(method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", e.csrf)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// rawGet performs an authenticated GET and returns the *http.Response WITHOUT
// reading the body (the J subtest checks Content-Type + status only).
func (e *v050Env) rawGet(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// TestV050DefaultBuildE2E exercises one request per v0.5.0 spec family
// against the full real API binary under the default build flavour
// (no build tags → NoopCache + SQLite). This is the seam gate complementing
// the per-handler unit tests in internal/api/.
func TestV050DefaultBuildE2E(t *testing.T) {
	env := bootV050E2E(t)

	// --- Part A — semantic cache (NoopCache: enabled=false) ----------------
	t.Run("A_semantic_cache_settings", func(t *testing.T) {
		code, body := env.do(t, http.MethodGet, "/api/v1/cache/settings", nil)
		if code != http.StatusOK {
			t.Fatalf("status=%d body=%s", code, body)
		}
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("decode: %v body=%s", err, body)
		}
		sem, ok := obj["semantic"].(map[string]any)
		if !ok {
			t.Fatalf("response missing semantic block: %s", body)
		}
		enabled, _ := sem["enabled"].(bool)
		if enabled {
			t.Errorf("default build must report semantic.enabled=false (NoopCache); got %s", body)
		}
	})

	// --- Part B — upstream credential PUT ---------------------------------
	t.Run("B_upstream_credential_put", func(t *testing.T) {
		code, body := env.do(t, http.MethodPut,
			"/api/v1/services/"+env.serviceID+"/upstream-credential",
			map[string]string{
				"slot":          "OPENAI",
				"header_name":   "Authorization",
				"header_format": "Bearer {key}",
			})
		if code != http.StatusNoContent {
			t.Fatalf("status=%d body=%s", code, body)
		}
		// Read-back confirms the binding landed.
		code2, body2 := env.do(t, http.MethodGet,
			"/api/v1/services/"+env.serviceID+"/upstream-credential", nil)
		if code2 != http.StatusOK {
			t.Fatalf("readback status=%d body=%s", code2, body2)
		}
		var got map[string]any
		_ = json.Unmarshal(body2, &got)
		if got["slot"] != "OPENAI" {
			t.Errorf("readback slot=%v want OPENAI (body=%s)", got["slot"], body2)
		}
		if got["slot_present"] != true {
			t.Errorf("readback slot_present=%v want true (env BURROW_UPSTREAM_KEY_OPENAI set; body=%s)", got["slot_present"], body2)
		}
	})

	// --- Part C — multi-provider: a synthetic model and a gateway key ----
	t.Run("C_synthetic_model_and_gateway_key", func(t *testing.T) { v050CheckSyntheticModel(t, env) })

	// --- Part D — custom domain POST --------------------------------------
	t.Run("D_custom_domain_post", func(t *testing.T) {
		// The CA + leaf are signed with the same private key chain plumbed
		// into Deps.CertValidationRoots during boot, so the handler's chain
		// validation step accepts the leaf.
		ca, caKey := mustReuseTestCA(t, env)
		certPEM, keyPEM := genTestLeaf(t, ca, caKey, []string{"foo.example.com"})
		code, body := env.do(t, http.MethodPost,
			"/api/v1/services/"+env.serviceID+"/domains",
			map[string]any{
				"hostname": "foo.example.com",
				"cert_pem": certPEM,
				"key_pem":  keyPEM,
			})
		if code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", code, body)
		}
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("decode: %v body=%s", err, body)
		}
		if obj["hostname"] != "foo.example.com" {
			t.Errorf("hostname=%v want foo.example.com (body=%s)", obj["hostname"], body)
		}
	})

	// --- Part D2 — custom domain proxy routing seam (F-14) ----------------
	//
	// Drives a real visitor request through the full ingress proxy + tunnel
	// + upstream chain (bootE2EStack) with a proxy.WithCustomDomainLookup
	// closure installed via withCustomDomainLookup. The closure maps
	// "foo.example.com" -> s.serviceID, mirroring how cmd/server/main.go
	// adapts v05.CustomDomainStore.LookupBySNI into the proxy hook.
	//
	// This is the genuine wiring seam for F-14: it proves the proxy's
	// host-not-a-subdomain branch (proxy.go:285) actually routes through
	// the registered lookup and serves the upstream's body. Without F-14's
	// fix in cmd/server/main.go, the lookup field is nil and the dead-code
	// branch returns notFound — every custom-domain visitor gets a 404.
	//
	// The companion subtest D_custom_domain_post exercises the API write
	// path (POST /domains + chain validation) on the v050Env, which is
	// where v05.CustomDomainStore is fed. This subtest exercises the read
	// path through the proxy data plane on bootE2EStack.
	t.Run("D_custom_domain_route", func(t *testing.T) {
		const customHost = "foo.example.com"

		// Fresh test CA + leaf for customHost. The CA is pooled into the
		// visitor client's RootCAs so the handshake actually verifies the
		// per-domain cert presented by the proxy via GetCertificate — no
		// InsecureSkipVerify. The leaf is parsed into a tls.Certificate and
		// served from a stub customdomain.DBStore.
		caCert, caKey, caPool, _ := generateTestCA(t)
		leafCertPEM, leafKeyPEM := genTestLeaf(t, caCert, caKey, []string{customHost})
		leafTLS, err := tls.X509KeyPair([]byte(leafCertPEM), []byte(leafKeyPEM))
		if err != nil {
			t.Fatalf("X509KeyPair: %v", err)
		}

		// Stub DBStore: returns the leaf row for customHost, miss otherwise.
		// The customdomain.Store re-parses CertPEM/KeyPEM internally via
		// tls.X509KeyPair, so we hand it the same PEM bytes we just signed.
		cdDB := stubCustomDomainDB{
			rows: map[string]customdomain.DomainRow{
				customHost: {
					ID:        "test-row-1",
					ServiceID: "late-bound-after-boot",
					Hostname:  customHost,
					CertPEM:   leafCertPEM,
					KeyPEM:    leafKeyPEM,
					NotAfter:  time.Now().Add(24 * time.Hour),
				},
			},
		}
		cdStore := customdomain.New(cdDB)
		_ = leafTLS // kept around in case future assertions want the parsed cert directly

		// In-test lookup: map customHost -> serviceID. The real production
		// adapter calls v05.CustomDomainStore.LookupBySNI; this stub mirrors
		// its (serviceID, ok, err) contract verbatim. Misses return ok=false.
		var lookupServiceID string
		lookup := func(_ context.Context, host string) (string, bool, error) {
			if strings.EqualFold(host, customHost) {
				return lookupServiceID, true, nil
			}
			return "", false, nil
		}

		s := bootE2EStack(t,
			withCustomDomainLookup(lookup),
			withCustomDomainCertStore(cdStore),
		)
		lookupServiceID = s.serviceID // late-bound: bootE2EStack assigns serviceID

		const body = "ok-from-custom-domain"
		s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, body)
		})

		// Build a dedicated verifying client for this subtest — pools the
		// fresh test CA into RootCAs and sets SNI = customHost. The dial
		// indirection still targets the proxy's loopback port so no DNS
		// lookup occurs. This proves the SNI handshake selected the per-domain
		// cert (any failure here = handshake didn't pick up the store cert).
		hc := verifyingVisitorClient(t, s.proxyAddr, customHost, caPool)
		url := "https://" + customHost + ":" + s.proxyPort + "/custom-domain-probe"
		resp, err := hc.Get(url)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: want 200, got %d body=%s", resp.StatusCode, got)
		}
		if string(got) != body {
			t.Errorf("body: want %q, got %q", body, string(got))
		}
	})

	// --- Part E — connection log proxy roundtrip seam (F-13) --------------
	//
	// Drives a real visitor request through the full ingress proxy + tunnel
	// + upstream chain (bootE2EStack) with a connlog.NewSQLSink installed
	// via proxy.WithConnLogSink, and asserts a connection_logs row landed.
	//
	// This is the genuine wiring seam for F-13: it proves the proxy hot-path
	// actually invokes sink.Record(...) when a request closes. The previous
	// shape of this subtest called sink.Record directly — which silently
	// passed even when the proxy field was never read. The current shape
	// fails (no rows) if proxy.serveResolved doesn't call recordOnClose.
	t.Run("E_connection_logs", func(t *testing.T) {
		// Local sub-stack so this subtest doesn't share state with the
		// TestV050DefaultBuildE2E env (which uses a stub stack without a
		// real proxy listener). bootE2EStack already runs a real proxy +
		// real tunnel + real upstream; we just plug a connlog sink into it.
		//
		// Adapter is a forward-reference: the sink itself can't be built
		// until bootE2EStack has provisioned the test DB, but the proxy
		// option list is consumed inside bootE2EStack. We resolve this
		// with a thin indirection — the adapter calls through a sinkFn
		// closure that returns the late-bound *connlog.SQLSink. The
		// proxy never invokes the sink during bootE2EStack itself; only
		// once a visitor request is dispatched, at which point realSink
		// is non-nil.
		var realSink *connlog.SQLSink
		adapter := proxyConnLogTestAdapter{sinkFn: func() *connlog.SQLSink { return realSink }}

		s := bootE2EStack(t, withConnLogSink(adapter))
		realSink = connlog.NewSQLSink(db.Wrap(s.db), slog.New(slog.NewTextHandler(io.Discard, nil)))

		// Upstream returns a small known body so bytes_out > 0 is provable.
		const body = "ok-from-upstream"
		s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, body)
		})

		hc := s.visitorClient(t)
		url := "https://" + s.hostWithPort() + "/conn-log-probe"
		resp, err := hc.Get(url)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: want 200, got %d", resp.StatusCode)
		}

		// Poll the connection_logs table directly (the read-surface seam
		// already lives in part E's prior life; here we're testing the
		// write side of the seam — does the proxy actually write rows?).
		wrapped := db.Wrap(s.db)
		deadline := time.Now().Add(5 * time.Second)
		var rows []db.ConnectionLog
		for time.Now().Before(deadline) {
			r, err := connlog.ListConnectionLogs(context.Background(), wrapped, connlog.ConnLogQuery{
				ServiceID: s.serviceID,
				Kind:      string(connlog.KindHTTPProxy),
				Limit:     10,
			})
			if err != nil {
				t.Fatalf("ListConnectionLogs: %v", err)
			}
			rows = r
			if len(rows) >= 1 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if len(rows) < 1 {
			t.Fatalf("no rows found in connection_logs for service_id=%s after 5s — proxy did not call sink.Record", s.serviceID)
		}
		got := rows[0]
		if got.Kind != string(connlog.KindHTTPProxy) {
			t.Errorf("kind=%q want %q", got.Kind, connlog.KindHTTPProxy)
		}
		if got.ServiceID != s.serviceID {
			t.Errorf("service_id=%q want %q", got.ServiceID, s.serviceID)
		}
		if got.Status != string(connlog.StatusClosedClean) {
			t.Errorf("status=%q want %q", got.Status, connlog.StatusClosedClean)
		}
		if got.BytesIn < 0 {
			t.Errorf("bytes_in=%d want >= 0", got.BytesIn)
		}
		if got.BytesOut <= 0 {
			t.Errorf("bytes_out=%d want > 0 (upstream wrote %d bytes)", got.BytesOut, len(body))
		}
		if got.DurationMs < 0 {
			t.Errorf("duration_ms=%d want >= 0", got.DurationMs)
		}
		if got.StartedAt.IsZero() {
			t.Errorf("started_at is zero")
		}
		if got.StartedAt.After(got.EndedAt) {
			t.Errorf("started_at=%v after ended_at=%v", got.StartedAt, got.EndedAt)
		}
		// v0.5.1 P2.4: TunnelID / UserID / ClientSessionID must be threaded
		// through the Resolved struct and into the connection-log row.
		if got.TunnelID == "" {
			t.Error("tunnel_id missing")
		}
		if got.UserID == "" {
			t.Error("user_id missing")
		}
		if got.ClientSessionID == "" {
			t.Error("client_session_id missing")
		}
	})

	// --- Part E2 — connection log: closed_error (upstream returns 500) -----
	//
	// P2.3 seam test: visitor request whose upstream returns 500. The proxy's
	// ErrorHandler (or post-ServeHTTP status capture) must set logStatus to
	// "closed_error" before recordOnClose fires.
	t.Run("E2_connection_log_status_closed_error", func(t *testing.T) {
		var realSink *connlog.SQLSink
		adapter := proxyConnLogTestAdapter{sinkFn: func() *connlog.SQLSink { return realSink }}

		s := bootE2EStack(t, withConnLogSink(adapter))
		realSink = connlog.NewSQLSink(db.Wrap(s.db), slog.New(slog.NewTextHandler(io.Discard, nil)))

		// Upstream returns 500.
		s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "upstream error", http.StatusInternalServerError)
		})

		hc := s.visitorClient(t)
		url := "https://" + s.hostWithPort() + "/error-probe"
		resp, err := hc.Get(url)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		// The proxy surfaces the upstream 500 (or a 502 bad gateway); either is acceptable —
		// what we care about is the logged status, not the visitor response code.

		wrapped := db.Wrap(s.db)
		deadline := time.Now().Add(5 * time.Second)
		var rows []db.ConnectionLog
		for time.Now().Before(deadline) {
			r, err := connlog.ListConnectionLogs(context.Background(), wrapped, connlog.ConnLogQuery{
				ServiceID: s.serviceID,
				Kind:      string(connlog.KindHTTPProxy),
				Limit:     10,
			})
			if err != nil {
				t.Fatalf("ListConnectionLogs: %v", err)
			}
			rows = r
			if len(rows) >= 1 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if len(rows) < 1 {
			t.Fatalf("no rows in connection_logs for service_id=%s after 5s", s.serviceID)
		}
		got := rows[0]
		if got.Status != string(connlog.StatusClosedError) {
			t.Errorf("status=%q want %q (upstream returned 500)", got.Status, connlog.StatusClosedError)
		}
	})

	// --- Part E3 — connection log: closed_idle (upstream hangs past timeout) -
	//
	// P2.3 seam test: visitor request whose upstream hangs for 10 s. With the
	// proxy idle timeout set to 2 s, the proxy's context deadline fires first.
	// The proxy must record logStatus == "closed_idle".
	t.Run("E3_connection_log_status_closed_idle", func(t *testing.T) {
		var realSink *connlog.SQLSink
		adapter := proxyConnLogTestAdapter{sinkFn: func() *connlog.SQLSink { return realSink }}

		// 2s idle timeout: tight enough for a fast test, wide enough to survive CI jitter.
		s := bootE2EStack(t, withConnLogSink(adapter), withProxyIdleTimeout(2*time.Second))
		realSink = connlog.NewSQLSink(db.Wrap(s.db), slog.New(slog.NewTextHandler(io.Discard, nil)))

		// Upstream hangs (well past the 2s idle timeout).
		s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(10 * time.Second):
				w.WriteHeader(http.StatusOK)
			case <-r.Context().Done():
				// Context cancelled — request timed out from proxy side.
			}
		})

		hc := s.visitorClient(t)
		url := "https://" + s.hostWithPort() + "/idle-probe"
		resp, err := hc.Get(url)
		// The visitor will receive an error (proxy timeout) or a 502; either is fine.
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}

		wrapped := db.Wrap(s.db)
		deadline := time.Now().Add(8 * time.Second)
		var rows []db.ConnectionLog
		for time.Now().Before(deadline) {
			r, err := connlog.ListConnectionLogs(context.Background(), wrapped, connlog.ConnLogQuery{
				ServiceID: s.serviceID,
				Kind:      string(connlog.KindHTTPProxy),
				Limit:     10,
			})
			if err != nil {
				t.Fatalf("ListConnectionLogs: %v", err)
			}
			rows = r
			if len(rows) >= 1 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if len(rows) < 1 {
			t.Fatalf("no rows in connection_logs for service_id=%s after 8s", s.serviceID)
		}
		got := rows[0]
		if got.Status != string(connlog.StatusClosedIdle) {
			t.Errorf("status=%q want %q (upstream hung past idle timeout)", got.Status, connlog.StatusClosedIdle)
		}
	})

	// --- Part E4 — connection log: rejected (access deny regression pin) ----
	//
	// P2.3 regression pin: early access-deny path must still record "rejected".
	// This was already green before P2.3; pinning it here ensures the new
	// closed_error / closed_idle wiring doesn't accidentally override it.
	//
	// The test uses a service registered as "api_key" access mode and sends a
	// request with no key → AccessChecker denies → logStatus stays "rejected".
	t.Run("E4_connection_log_status_rejected", func(t *testing.T) {
		var realSink *connlog.SQLSink
		adapter := proxyConnLogTestAdapter{sinkFn: func() *connlog.SQLSink { return realSink }}

		s := bootE2EStack(t, withConnLogSink(adapter))
		realSink = connlog.NewSQLSink(db.Wrap(s.db), slog.New(slog.NewTextHandler(io.Discard, nil)))

		// bootE2EStack registers the tunnel as "open" access mode. To exercise
		// the rejection path we need to update the service's access mode to
		// "api_key" so the AccessChecker denies a key-less request.
		// Note: the API key itself is stored in service_api_keys (not a column
		// on services), so we only update access_mode here. A request with no
		// Authorization header is rejected before any key lookup.
		if _, err := s.db.ExecContext(context.Background(),
			`UPDATE services SET access_mode='api_key' WHERE id=?`,
			s.serviceID,
		); err != nil {
			t.Fatalf("update access_mode: %v", err)
		}

		// Visitor sends request with no API key → access denied.
		hc := s.visitorClient(t)
		url := "https://" + s.hostWithPort() + "/reject-probe"
		resp, err := hc.Get(url)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		wrapped := db.Wrap(s.db)
		deadline := time.Now().Add(5 * time.Second)
		var rows []db.ConnectionLog
		for time.Now().Before(deadline) {
			r, err := connlog.ListConnectionLogs(context.Background(), wrapped, connlog.ConnLogQuery{
				ServiceID: s.serviceID,
				Kind:      string(connlog.KindHTTPProxy),
				Limit:     10,
			})
			if err != nil {
				t.Fatalf("ListConnectionLogs: %v", err)
			}
			rows = r
			if len(rows) >= 1 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if len(rows) < 1 {
			t.Fatalf("no rows in connection_logs for service_id=%s after 5s", s.serviceID)
		}
		got := rows[0]
		if got.Status != string(connlog.StatusRejected) {
			t.Errorf("status=%q want %q (access denied by api_key mode)", got.Status, connlog.StatusRejected)
		}
	})

	// --- Part F — retention settings PUT then GET --------------------------
	t.Run("F_retention_put_get", func(t *testing.T) {
		code, body := env.do(t, http.MethodPut, "/api/v1/settings/retention",
			map[string]any{"usage_retention_days": 60})
		if code != http.StatusNoContent {
			t.Fatalf("PUT status=%d body=%s", code, body)
		}
		code2, body2 := env.do(t, http.MethodGet, "/api/v1/settings/retention", nil)
		if code2 != http.StatusOK {
			t.Fatalf("GET status=%d body=%s", code2, body2)
		}
		var obj map[string]any
		if err := json.Unmarshal(body2, &obj); err != nil {
			t.Fatalf("decode: %v body=%s", err, body2)
		}
		if v, _ := obj["usage_retention_days"].(float64); int(v) != 60 {
			t.Errorf("usage_retention_days=%v want 60 (body=%s)", obj["usage_retention_days"], body2)
		}
	})

	// --- Part G — database backend status ---------------------------------
	t.Run("G_database_status", func(t *testing.T) {
		code, body := env.do(t, http.MethodGet, "/api/v1/database", nil)
		if code != http.StatusOK {
			t.Fatalf("status=%d body=%s", code, body)
		}
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("decode: %v body=%s", err, body)
		}
		if obj["driver"] != "sqlite" {
			t.Errorf("driver=%v want sqlite (body=%s)", obj["driver"], body)
		}
	})

	// --- Part H — webhook payload template preview ------------------------
	t.Run("H_webhook_template_preview", func(t *testing.T) {
		// Create a webhook carrying a tiny template.
		code, body := env.do(t, http.MethodPost, "/api/v1/webhooks", map[string]any{
			"name":             "ops",
			"url":              "https://example.com/hook",
			"events":           []string{"ai.upstream_error"},
			"payload_template": `svc={{.service_id}}`,
		})
		if code != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", code, body)
		}
		var created map[string]any
		if err := json.Unmarshal(body, &created); err != nil {
			t.Fatalf("decode created: %v body=%s", err, body)
		}
		whID, _ := created["id"].(string)
		if whID == "" {
			t.Fatalf("created webhook missing id: %s", body)
		}
		// Preview the template with a field map.
		code2, body2 := env.do(t, http.MethodPost,
			"/api/v1/webhooks/"+whID+"/preview",
			map[string]any{
				"event":  "ai.upstream_error",
				"fields": map[string]any{"service_id": env.serviceID},
			})
		if code2 != http.StatusOK {
			t.Fatalf("preview status=%d body=%s", code2, body2)
		}
		var prev map[string]any
		if err := json.Unmarshal(body2, &prev); err != nil {
			t.Fatalf("decode preview: %v body=%s", err, body2)
		}
		if _, ok := prev["rendered"]; !ok {
			t.Errorf("preview missing rendered key (body=%s)", body2)
		}
	})

	// --- Part J — embedded OpenAPI viewer ---------------------------------
	t.Run("J_openapi_viewer", func(t *testing.T) {
		// Trailing slash is required (spec J reconciliation) — chi's mounted
		// subrouter answers /openapi/viewer/ with 200 text/html; the bare
		// /openapi/viewer redirects 301.
		resp := env.rawGet(t, "/api/v1/openapi/viewer/")
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type=%q want prefix text/html", ct)
		}
	})
}

// ---------------------------------------------------------------------------
// Test-only TLS helpers — independent of e2e_helpers_test.go because this
// file is build-tag-less and bootE2EStack does not provide a CA-rooted
// chain (its wildcard cert is self-signed).
// ---------------------------------------------------------------------------

// testCAStash holds the test CA cert + private key for one v050Env. It is
// populated by generateTestCA the first time it runs in this test process
// and reused so that Part D's leaf cert chains to the CA pool we plumbed
// into Deps.CertValidationRoots during bootV050E2E.
//
// We can't pass the CA out of bootV050E2E without growing the env struct
// surface; instead generateTestCA stores the pair in a process-wide map
// keyed by the env pointer. (One env per test run; t.Parallel is not used
// here.)
type testCAPair struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

var testCAByEnv = map[*v050Env]*testCAPair{}

// generateTestCA returns a self-signed CA cert + key + CertPool + PEM bytes.
// The CA expires in 24h (sufficient for any test run).
func generateTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, *x509.CertPool, []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generateTestCA: gen key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "burrow-v050-e2e-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("generateTestCA: create cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert, priv, pool, pemBytes
}

// mustReuseTestCA returns the CA pair generated for this env by
// bootV050E2E. It panics t.Fatal if generateTestCA was never called for
// this env — the contract is that bootV050E2E calls it once.
func mustReuseTestCA(t *testing.T, env *v050Env) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	// The simplest approach: regenerate a CA here is wrong (the new CA
	// would NOT be in Deps.CertValidationRoots). Instead, generate the CA
	// once at boot, stash it by env pointer, and look it up here.
	pair, ok := testCAByEnv[env]
	if !ok {
		t.Fatal("mustReuseTestCA: bootV050E2E did not stash a CA — test wiring bug")
	}
	return pair.cert, pair.key
}

// proxyConnLogTestAdapter satisfies proxy.ConnLogSink by forwarding each
// proxy.ConnLogEntry to a (late-bound) *connlog.SQLSink, mirroring the
// production adapter shape in cmd/server/main.go. The sinkFn indirection
// keeps the adapter constructable before the test DB exists — the proxy
// option list must be passed to bootE2EStack at boot, but the SQLSink
// itself can only be built after the test DB is opened. The proxy never
// invokes the sink until a real visitor request lands, by which time
// sinkFn returns the live sink.
type proxyConnLogTestAdapter struct {
	sinkFn func() *connlog.SQLSink
}

func (a proxyConnLogTestAdapter) Record(ctx context.Context, e proxy.ConnLogEntry) error {
	s := a.sinkFn()
	if s == nil {
		return nil
	}
	return s.Record(ctx, connlog.Entry{
		Kind:            connlog.Kind(e.Kind),
		ServiceID:       e.ServiceID,
		TunnelID:        e.TunnelID,
		UserID:          e.UserID,
		ClientSessionID: e.ClientSessionID,
		SourceIP:        e.SourceIP,
		UserAgent:       e.UserAgent,
		StartedAt:       e.StartedAt,
		EndedAt:         e.EndedAt,
		BytesIn:         e.BytesIn,
		BytesOut:        e.BytesOut,
		Status:          connlog.Status(e.Status),
		Reason:          e.Reason,
	})
}

// genTestLeaf returns PEM bytes for a leaf cert signed by ca, carrying
// the requested DNS SANs. The leaf expires in 24h.
func genTestLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dnsSANs []string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genTestLeaf: gen key: %v", err)
	}
	cn := dnsSANs[0]
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsSANs,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &priv.PublicKey, caKey)
	if err != nil {
		t.Fatalf("genTestLeaf: create cert: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("genTestLeaf: marshal key: %v", err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// stubCustomDomainDB satisfies customdomain.DBStore for the per-domain cert
// seam test. It returns rows from an in-memory map keyed by lowercase
// hostname; misses return ok=false (the contract LookupBySNI expects).
type stubCustomDomainDB struct {
	rows map[string]customdomain.DomainRow
}

func (s stubCustomDomainDB) LookupCustomDomainByHostname(_ context.Context, hostname string) (customdomain.DomainRow, bool, error) {
	row, ok := s.rows[strings.ToLower(hostname)]
	if !ok {
		return customdomain.DomainRow{}, false, nil
	}
	return row, true, nil
}

// verifyingVisitorClient is the verifying counterpart to e2eStack.visitorClient.
// It pools the supplied roots into the TLS config (no InsecureSkipVerify) and
// sets SNI = serverName. The dial closure still targets proxyAddr (loopback)
// regardless of the URL host so no DNS resolution occurs. Used by the custom-
// domain route subtest to prove the SNI handshake actually selected the per-
// domain cert returned by customdomain.CertCallback.
func verifyingVisitorClient(t *testing.T, proxyAddr, serverName string, roots *x509.CertPool) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", proxyAddr)
		},
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", proxyAddr)
			if err != nil {
				return nil, err
			}
			tlsConn := tls.Client(conn, &tls.Config{
				ServerName: serverName,
				RootCAs:    roots,
				MinVersion: tls.VersionTLS12,
			})
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return tlsConn, nil
		},
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Jar:       jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 15 * time.Second,
	}
}
