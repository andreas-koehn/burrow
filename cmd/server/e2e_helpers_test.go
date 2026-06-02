package main

// e2e_helpers_test.go — shared bootstrap for the v0.3.0 real-stack e2e tests.
//
// One bootE2EStack(t) call brings up:
//   - a temp sqlite DB (with migrations + a seeded admin + a minted client token)
//   - a real *server.Server (control, TLS, dev cert) on a random loopback port
//   - a real *internal/client.Client registering a single "http" tunnel against
//     a local test upstream
//   - a real *internal/proxy ingress on a random loopback port with a freshly
//     generated wildcard TLS cert for *.test.local + the burrow-login gate
//
// The visitor side is a single *http.Client whose Transport always dials the
// proxy port on loopback (so the test never needs a real DNS entry), with
// TLS verification skipped — exactly the "TLS verify disabled, SNI = the
// hostname" model the integration plan specifies.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/aimeter"
	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/cache/semantic"
	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/proxy/customdomain"
	"github.com/ankoehn/burrow/internal/quota"
	"github.com/ankoehn/burrow/internal/redact"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

const (
	e2eAuthDomain    = "test.local"
	e2eAdminEmail    = "admin@test.local"
	e2eAdminPassword = "password1-very-strong"
	e2eTunnelName    = "echo"
	e2eClientReady   = 5 * time.Second
)

// e2eStack is the bundle of live components owned by one bootE2EStack call.
type e2eStack struct {
	ctx      context.Context
	cancel   context.CancelFunc
	log      *slog.Logger
	db       *sql.DB
	store    *store.Store
	server   *server.Server
	client   *client.Client
	proxySrv *http.Server
	gate     http.Handler

	// proxyAddr is the "host:port" of the proxy ingress (loopback).
	proxyAddr string
	proxyPort string

	// upstream is the local-side test HTTP server the client tunnel forwards to.
	upstream     *http.Server
	upstreamLn   net.Listener
	upstreamAddr string

	// upstreamHandler is the per-test hook the upstream serves. Reassign before
	// the visitor sends a request. The mux dispatches every request to it.
	upstreamHandler atomic.Value // func(http.ResponseWriter, *http.Request)

	// service identity (resolved at registration time).
	serviceID string
	subdomain string
	hostname  string // "<sub>.test.local"
	userID    string

	// quotaEngine is non-nil when the stack was booted with withE2EQuota.
	// Tests that seed rate_limits rows via seedRateLimit may call
	// quotaEngine.Reload to pick up the new rows before firing requests.
	quotaEngine *quota.Engine

	cleanupFns []func()
}

// hostWithPort returns "<sub>.test.local:<proxy-port>" — the visitor's apparent
// destination. Combined with proxyDialer, that name is never DNS-resolved.
func (s *e2eStack) hostWithPort() string {
	return s.hostname + ":" + s.proxyPort
}

// setUpstreamHandler swaps the upstream's request handler for the next test.
func (s *e2eStack) setUpstreamHandler(h func(http.ResponseWriter, *http.Request)) {
	s.upstreamHandler.Store(h)
}

// visitorClient builds an *http.Client that always dials the proxy ingress on
// loopback regardless of the URL's hostname (so .test.local needs no DNS),
// trusts no CA (TLS verify disabled — InsecureSkipVerify per the plan), and
// sets SNI from the URL hostname so the wildcard cert is matched correctly.
// Each call returns an independent client (cookie jar is fresh).
func (s *e2eStack) visitorClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dialAddr := s.proxyAddr // closed over
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", dialAddr)
		},
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", dialAddr)
			if err != nil {
				return nil, err
			}
			// Use the requested addr's host for SNI so the wildcard cert is matched.
			serverName, _, _ := net.SplitHostPort(addr)
			if serverName == "" {
				serverName = addr
			}
			tlsConn := tls.Client(conn, &tls.Config{
				ServerName:         serverName,
				InsecureSkipVerify: true, //nolint:gosec // e2e test: TLS verify disabled per plan
				MinVersion:         tls.VersionTLS12,
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
		// Do not auto-follow redirects: tests need to inspect them.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 15 * time.Second,
	}
}

// bootE2EStackOption configures optional behaviour of bootE2EStack. The
// variadic pattern keeps the default zero-config call sites (most e2e tests)
// unchanged while letting a single test (F-13 connection-log seam) inject an
// extra proxy option.
type bootE2EStackOption func(*bootE2ECfg)

// bootE2ECfg accumulates the optional configuration. Tests rarely touch this
// directly — they pass option closures.
type bootE2ECfg struct {
	// extraProxyOpts are appended to the static proxy.Option list passed to
	// proxy.New (after WithGate / WithIngressPort / WithAIChain). Used by the
	// connection-log seam test to install proxy.WithConnLogSink.
	extraProxyOpts []proxy.Option
	// semCache, when non-nil, is installed as the aigw.Chain's Semantic field.
	// Default (nil) leaves the chain with no semantic tier — preserving the
	// existing e2e contract for tests that never enable cache.semantic. The
	// v0.5.0 semantic cache e2e test passes a chromem-backed cache here.
	semCache semantic.Cache
	// cdCertStore, when non-nil, is wired into the proxy ingress listener's
	// TLS config via customdomain.CertCallback(store, &wildcardCert). The
	// callback returns per-domain certs on an SNI hit and falls back to the
	// wildcard on a miss — mirroring the production wiring in
	// cmd/server/main.go. Default (nil) preserves the v0.3.0 listener shape
	// (static Certificates: []tls.Certificate{wildcard}).
	cdCertStore *customdomain.Store
	// quotaEnabled, when true, builds a real quota.Engine from the test DB and
	// assigns it to the AI chain's RateLimit middleware field.  Tests that need
	// enforcement call withE2EQuota() and then seed rate_limits rows via
	// seedRateLimit (which calls quotaEngine.Reload after each insert so the
	// in-memory snapshot is current before requests are fired).
	quotaEnabled bool
	// redactEnabled, when true, builds a real redact.Engine (built-in rules
	// plus any custom rules) passed as NewChain's redactEngine arg.
	redactEnabled bool
	redactRules   []redact.Rule
	// guardrailsEnabled, when true, installs a real guardrails.Engine as the
	// AI chain's guardrails engine (5th NewChain arg).
	guardrailsEnabled bool
}

// withConnLogSink returns a bootE2EStack option that registers a proxy
// ConnLogSink so the proxy hot-path records one connection_logs row per
// closed request. The adapter that converts proxy.ConnLogEntry into the
// concrete connlog.Entry lives in the calling test (kept out of the helper
// to avoid forcing connlog import into every other e2e file).
func withConnLogSink(sink proxy.ConnLogSink) bootE2EStackOption {
	return func(c *bootE2ECfg) {
		c.extraProxyOpts = append(c.extraProxyOpts, proxy.WithConnLogSink(sink))
	}
}

// withSemanticCache returns a bootE2EStack option that wires the given
// semantic.Cache into the aigw.Chain. Used by the v0.5.0 semantic cache e2e
// test to install a chromem-backed cache (under -tags=semantic_cache) so the
// chain's vector-similarity tier exercises real Lookup/Promote against the
// test DB. Default (no option) leaves Semantic=nil → tests that don't enable
// cache.semantic in service_ai_config see the v0.4.0 chain behaviour intact.
//
//nolint:unused // only referenced from e2e_v050_semantic_test.go (//go:build semantic_cache); appears unused in the default build.
func withSemanticCache(c semantic.Cache) bootE2EStackOption {
	return func(cfg *bootE2ECfg) {
		cfg.semCache = c
	}
}

// withCustomDomainLookup returns a bootE2EStack option that registers a
// proxy custom-domain routing closure. This is the test-side analogue of
// the production wiring in cmd/server/main.go that maps an inbound Host
// header (when not <label>.<authDomain>) to a serviceID via
// v05.CustomDomainStore. The F-14 seam test installs this option so the
// proxy's host != ".<authDomain>" branch actually routes through to a
// live tunnel instead of falling through to notFound.
func withCustomDomainLookup(fn func(ctx context.Context, host string) (string, bool, error)) bootE2EStackOption {
	return func(c *bootE2ECfg) {
		c.extraProxyOpts = append(c.extraProxyOpts, proxy.WithCustomDomainLookup(fn))
	}
}

// withCustomDomainCertStore returns a bootE2EStack option that wires the
// given customdomain.Store into the proxy ingress TLS listener. The proxy
// listener's tls.Config is built with GetCertificate =
// customdomain.CertCallback(store, &wildcardCert) — so SNI hits return the
// per-domain cert from the store and misses fall back to the wildcard.
// This is the test-side analogue of cmd/server/main.go's proxy listener wiring
// for v0.5.0 custom domains. Default (no option) preserves the static-cert
// listener shape used by every other e2e test.
func withCustomDomainCertStore(store *customdomain.Store) bootE2EStackOption {
	return func(c *bootE2ECfg) {
		c.cdCertStore = store
	}
}

// withE2EQuota enables a real quota.Engine in the booted stack, wiring it into
// the AI chain's RateLimit field (mirrors cmd/server/v04_wiring.go). The engine
// reads its rules from the test DB, so callers seed rules via seedRateLimit
// after bootE2EStack returns; seedRateLimit calls s.quotaEngine.Reload so the
// in-memory snapshot is current before requests are fired. The rules parameter
// is accepted for documentation/clarity but the engine always loads from DB —
// use seedRateLimit to insert the actual rule rows.
func withE2EQuota(_ ...quota.Limit) bootE2EStackOption {
	return func(c *bootE2ECfg) { c.quotaEnabled = true }
}

// withE2ERedaction builds a real redact.Engine and installs it as the AI
// chain's redaction engine, so redaction runs on the real proxy data path.
func withE2ERedaction(custom ...redact.Rule) bootE2EStackOption {
	return func(c *bootE2ECfg) { c.redactEnabled = true; c.redactRules = custom }
}

// withE2EGuardrails installs a real guardrails.Engine so prompt-injection /
// refuse policy runs on the real proxy data path.
func withE2EGuardrails() bootE2EStackOption {
	return func(c *bootE2ECfg) { c.guardrailsEnabled = true }
}

// withProxyIdleTimeout returns a bootE2EStack option that sets a per-request
// idle timeout on the proxy. Used by P2.3 tests (E2/E3) to exercise the
// closed_idle status path without waiting for the production-default timeout.
// A 2-second value is tight enough for a fast test but generous enough to
// survive CI scheduling jitter.
func withProxyIdleTimeout(d time.Duration) bootE2EStackOption {
	return func(c *bootE2ECfg) {
		c.extraProxyOpts = append(c.extraProxyOpts, proxy.WithProxyIdleTimeout(d))
	}
}

// bootE2EStack stands up the whole real-stack chain. Always uses TLS on the
// proxy (per plan: "dev wildcard cert + auth_domain test.local").
func bootE2EStack(t *testing.T, opts ...bootE2EStackOption) *e2eStack {
	t.Helper()
	cfg := &bootE2ECfg{}
	for _, o := range opts {
		o(cfg)
	}
	s := &e2eStack{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// 1. DB + store + admin + client token.
	dir := t.TempDir()
	// Register shutdown AFTER TempDir: t.Cleanup is LIFO, so we want to run
	// the DB close (inside shutdown) BEFORE TempDir's RemoveAll (Windows holds
	// a lock on open files and the unlink fails otherwise).
	t.Cleanup(s.shutdown)
	dbPath := filepath.Join(dir, "e2e.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	s.cleanupFns = append(s.cleanupFns, func() { _ = d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s.db = d
	st := store.New(d)
	s.store = st
	if err := st.SeedAdmin(context.Background(), e2eAdminEmail, e2eAdminPassword); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	u, err := st.GetUserByEmail(context.Background(), e2eAdminEmail)
	if err != nil {
		t.Fatalf("admin lookup: %v", err)
	}
	s.userID = u.ID
	tok, err := st.IssueClientToken(context.Background(), u.ID, "e2e")
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	// 2. Dev certs for the control listener.
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatalf("devcert: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caPEM)

	// 3. Wildcard cert for *.test.local (proxy ingress).
	proxyCertPath, proxyKeyPath := generateWildcardCert(t, dir, "*."+e2eAuthDomain, e2eAuthDomain)

	// 4. Upstream test HTTP server (the "local app" the client tunnel fronts).
	mux := http.NewServeMux()
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		// Default echo handler; tests override.
		fmt.Fprintf(w, "default upstream got %s %s", r.Method, r.URL.Path)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h := s.upstreamHandler.Load().(func(http.ResponseWriter, *http.Request))
		h(w, r)
	})
	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	s.upstreamLn = upstreamLn
	s.upstreamAddr = upstreamLn.Addr().String()
	s.upstream = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.upstream.Serve(upstreamLn) }()

	// 5. Real control server with v0.3.0 wiring.
	resolver := serviceResolverAdapter{db: db.Wrap(d)}
	srv, err := server.New(server.Options{
		Listen:     "127.0.0.1:0",
		TLSCert:    filepath.Join(dir, "dev-server.pem"),
		TLSKey:     filepath.Join(dir, "dev-server-key.pem"),
		Auth:       st,
		Tunnels:    tunnelStoreAdapter{st},
		Services:   resolver,
		AuthDomain: e2eAuthDomain,
		PublicBind: "127.0.0.1",
		Logger:     s.log,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	s.server = srv

	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	s.cancel = cancel

	go func() { _ = srv.Serve(ctx) }()

	// Wait for control listener to bind.
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		t.Fatal("control server never bound")
	}

	// 6. Real proxy ingress with gate, TLS wildcard cert. When a
	// customdomain.Store option is provided, wire GetCertificate so SNI
	// matched per-domain certs are served from the store with the wildcard
	// as fallback — mirroring cmd/server/main.go.
	proxyLn, err := tls.Listen("tcp", "127.0.0.1:0", func() *tls.Config {
		cert, err := tls.LoadX509KeyPair(proxyCertPath, proxyKeyPath)
		if err != nil {
			t.Fatalf("load proxy cert: %v", err)
		}
		tcfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.cdCertStore != nil {
			wildcard := cert
			tcfg.GetCertificate = customdomain.CertCallback(cfg.cdCertStore, &wildcard)
		} else {
			tcfg.Certificates = []tls.Certificate{cert}
		}
		return tcfg
	}())
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	s.proxyAddr = proxyLn.Addr().String()
	_, s.proxyPort, _ = net.SplitHostPort(s.proxyAddr)
	checker := proxy.NewAccessCheckerWithSessionsAndLogger(st, st, e2eAuthDomain, s.log)
	gate := proxy.NewGate(st, e2eAuthDomain, true, s.log)
	s.gate = gate
	dialer := proxyDialerAdapter{st: st, srv: srv}
	// Wire a minimal aigw.Chain with a DB-backed Loader so tests that seed
	// service_ai_config rows exercise the cache / redaction / guardrails
	// middleware. Tests that don't seed a row → Loader returns ok=false →
	// IsAIPassThrough → byte-for-byte v0.3.0 path (preserves the existing
	// e2e contract).
	cacheEngine := exact.New(db.Wrap(d), s.log)
	// Wire a real SQLSink so usage_events lands in the test DB — Task 2-4
	// real-stack tests assert per-key metering rows are written. The sink
	// is non-blocking (errors logged + swallowed) so unrelated tests that
	// never query usage_events are unaffected.
	meterSink := aimeter.NewSQLSink(db.Wrap(d))
	meterSink.Log = s.log
	var redactEngine *redact.Engine
	if cfg.redactEnabled {
		var err error
		redactEngine, err = redact.NewEngine(cfg.redactRules)
		if err != nil {
			t.Fatalf("redact.NewEngine: %v", err)
		}
	}
	var guardEngine *guardrails.Engine
	if cfg.guardrailsEnabled {
		guardEngine = guardrails.NewEngine()
	}
	aiChain := aigw.NewChain(cacheEngine, cfg.semCache, nil, redactEngine, guardEngine, nil, nil, meterSink, s.log)
	aiChain.Loader = chainConfigLoader{db: db.Wrap(d), log: s.log}
	if cfg.quotaEnabled {
		// Build a real quota.Engine backed by the test DB (mirrors
		// cmd/server/v04_wiring.go). quota.New loads the current rate_limits
		// snapshot and wires the same *db.DB as the dailyUsage store so
		// window=day checks query real usage_events rows. Tests call
		// seedRateLimit to insert rules after boot; seedRateLimit calls
		// s.quotaEngine.Reload to refresh the in-memory snapshot before
		// making requests.
		qe := quota.New(db.Wrap(d))
		s.quotaEngine = qe
		aiChain.RateLimit = buildQuotaMiddleware(qe, nil)
	}
	proxyOpts := []proxy.Option{
		proxy.WithGate(gate),
		proxy.WithIngressPort(s.proxyPort),
		proxy.WithAIChain(aiChain),
	}
	proxyOpts = append(proxyOpts, cfg.extraProxyOpts...)
	handler := proxy.New(
		dialer, checker, e2eAuthDomain, s.log,
		proxyOpts...,
	)
	s.proxySrv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = s.proxySrv.Serve(proxyLn) }()

	// 7. Real client with one http tunnel.
	c := client.New(client.Options{
		Server:     srv.Addr(),
		Token:      tok,
		RootCAs:    caPool,
		ServerName: "localhost",
		Tunnels: []client.TunnelSpec{
			{Name: e2eTunnelName, Type: "http", LocalAddr: s.upstreamAddr},
		},
		Logger: s.log,
	})
	s.client = c
	go func() { _ = c.Run(ctx) }()

	// 8. Wait for the client to register the http tunnel + service row to land.
	deadline = time.Now().Add(e2eClientReady)
	for time.Now().Before(deadline) {
		if c.Registered() {
			tns := srv.HTTPTunnels()
			if len(tns) > 0 && tns[0].Subdomain != "" {
				s.serviceID = tns[0].ServiceID
				s.subdomain = tns[0].Subdomain
				s.hostname = s.subdomain + "." + e2eAuthDomain
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.subdomain == "" {
		t.Fatal("http tunnel never resolved a subdomain")
	}

	return s
}

// shutdown reverses bootE2EStack. Idempotent.
func (s *e2eStack) shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.proxySrv != nil {
		_ = s.proxySrv.Shutdown(context.Background())
	}
	if s.upstream != nil {
		_ = s.upstream.Shutdown(context.Background())
	}
	if s.upstreamLn != nil {
		_ = s.upstreamLn.Close()
	}
	if s.server != nil {
		s.server.Wait()
	}
	for i := len(s.cleanupFns) - 1; i >= 0; i-- {
		s.cleanupFns[i]()
	}
}

// generateWildcardCert writes a wildcard cert + key to dir, returns their paths.
// SANs include both the wildcard and the bare auth domain (so the gate URL
// https://test.local/__burrow/login resolves to a valid cert too).
func generateWildcardCert(t *testing.T, dir, wildcardName, bareName string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: wildcardName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{wildcardName, bareName},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "proxy-cert.pem")
	keyPath = filepath.Join(dir, "proxy-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// must keeps the small assertion site tidy.
func must(t *testing.T, err error, msg string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}

// readAllString returns the response body string and closes the body.
func readAllString(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// _ keeps the imports honest under future refactors.
var _ = sync.WaitGroup{}

// _ silences "imported and not used" for api when individual test files do
// not import it directly (the helper above doesn't currently call api.X but
// keeps the import marker live for downstream test growth).
var _ = api.JSONHandlerTimeout

// _ silences strings if no downstream test imports it via the helper.
var _ = strings.Contains

// ---------------------------------------------------------------------------
// Security-focused stack (cross-cutting middleware tests)
// ---------------------------------------------------------------------------

// securityStack is the bundle of live components owned by one
// bootSecurityStack call. Lighter weight than e2eStack: no client, no
// tunnel, no proxy — only the API server's HTTP handler attached to a
// random loopback listener.
type securityStack struct {
	db         *sql.DB
	store      *store.Store
	apiSrv     *http.Server
	apiAddr    string // host:port the test should dial
	apiURL     string // "http://" or "https://" prefix + apiAddr
	httpsOn    bool
	cleanupFns []func()
}

// securityOpt configures the security stack.
type securityOpt func(*securityCfg)

type securityCfg struct {
	httpsEnabled     bool
	secureCookies    bool
	trustedProxies   []string
	loginPerIPLimit  int // 0 = use production default
	loginGlobalLimit int
}

// withSecHTTPS enables native TLS on the API listener (uses a self-signed
// cert generated in-process). Required to assert HSTS header presence.
func withSecHTTPS() securityOpt {
	return func(c *securityCfg) { c.httpsEnabled = true }
}

// withSecCookies forces SecureCookies on the Deps struct.
func withSecCookies(b bool) securityOpt {
	return func(c *securityCfg) { c.secureCookies = b }
}

// withSecTrustedProxies sets Deps.TrustedProxies.
func withSecTrustedProxies(cidrs ...string) securityOpt {
	return func(c *securityCfg) { c.trustedProxies = cidrs }
}

// withSecLoginRateLimit overrides the per-IP + global login rate-limit
// thresholds. Use small values (e.g., 3) so the test triggers the limiter
// without 11 actual requests.
func withSecLoginRateLimit(perIP, global int) securityOpt {
	return func(c *securityCfg) {
		c.loginPerIPLimit = perIP
		c.loginGlobalLimit = global
	}
}

// bootSecurityStack boots a minimal relay: temp DB, seeded admin, API server
// on a random loopback listener. Returns a stack the test can hit via
// http.Client. Used only by TestSec_* tests in e2e_security_test.go.
func bootSecurityStack(t *testing.T, opts ...securityOpt) *securityStack {
	t.Helper()
	cfg := &securityCfg{}
	for _, o := range opts {
		o(cfg)
	}
	s := &securityStack{httpsOn: cfg.httpsEnabled}

	// Register TempDir BEFORE shutdown so LIFO cleanup order is:
	// shutdown (DB close + server drain) → TempDir removal.
	// If shutdown is registered first, TempDir runs first and Windows
	// fails to unlink the open DB file.
	dir := t.TempDir()
	t.Cleanup(s.shutdown)
	dbPath := filepath.Join(dir, "sec.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	s.cleanupFns = append(s.cleanupFns, func() { _ = d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s.db = d

	st := store.New(d)
	s.store = st
	if err := st.SeedAdmin(context.Background(), e2eAdminEmail, e2eAdminPassword); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	// Pick a random loopback port for the API.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.apiAddr = ln.Addr().String()

	deps := api.Deps{
		Users:                        st,
		Sessions:                     st,
		Settings:                     st,
		Roles:                        st,
		Log:                          slog.New(slog.NewTextHandler(io.Discard, nil)),
		SecureCookies:                cfg.secureCookies,
		HTTPSEnabled:                 cfg.httpsEnabled,
		TrustedProxies:               cfg.trustedProxies,
		LoginRateLimitPerIPOverride:  cfg.loginPerIPLimit,
		LoginRateLimitGlobalOverride: cfg.loginGlobalLimit,
		DB:                           d,
	}
	apiSrv := &http.Server{
		Handler:           api.NewRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.apiSrv = apiSrv

	if cfg.httpsEnabled {
		s.apiURL = "https://" + s.apiAddr
		// Generate a temporary self-signed cert.
		certPEM, keyPEM := genSelfSignedCert(t, "127.0.0.1")
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatalf("x509 keypair: %v", err)
		}
		apiSrv.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		}
		go func() {
			_ = apiSrv.ServeTLS(ln, "", "")
		}()
	} else {
		s.apiURL = "http://" + s.apiAddr
		go func() {
			_ = apiSrv.Serve(ln)
		}()
	}

	// Wait for the listener to accept (poll healthz).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c := s.client()
		resp, err := c.Get(s.apiURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return s
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("api never became ready at %s", s.apiURL)
	return nil
}

// client returns an *http.Client with InsecureSkipVerify (the self-signed
// cert isn't trusted by the system root pool) and an isolated cookie jar.
func (s *securityStack) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec
	}
	return &http.Client{
		Transport:     tr,
		Jar:           jar,
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// shutdown drains the api server + closes the DB. Idempotent.
func (s *securityStack) shutdown() {
	if s.apiSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.apiSrv.Shutdown(ctx)
	}
	for _, f := range s.cleanupFns {
		f()
	}
}

// genSelfSignedCert generates an in-memory self-signed cert for the given
// host. Returns PEM-encoded cert + key. Used by bootSecurityStack when
// withSecHTTPS() is set.
func genSelfSignedCert(t *testing.T, host string) ([]byte, []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP(host)},
		DNSNames:     []string{host},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
