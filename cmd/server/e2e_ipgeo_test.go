package main

// e2e_ipgeo_test.go — Task 9 of the v0.4.0 integration plan. Real-stack
// e2e for the per-service IP allow/block CIDR enforcement + the global
// /api/v1/geo/status surface in the default (geo-tag-off) build.
//
// The shared bootE2EStack listener does NOT install the per-service
// IPGeo middleware (the aigw.Chain wires it as a stub today — see
// internal/aigw/chain.go Step 2). Per Wave 2 task spec we therefore
// stand up a SECOND proxy listener inside this file that wraps the
// existing proxy handler with an ipgeo middleware constructed from
// proxy.CompileIPGeoPolicy + proxy.IPGeoEngine.AllowRequest. The
// middleware honours X-Forwarded-For only when the immediate TCP peer
// is inside the trusted-proxy CIDR list — matching the production
// invariant that visitor-set XFF headers are ignored unless Burrow's
// own front gate set them.
//
// The country-code path is checked in TestE2EIPGeo_CountriesGated
// (//go:build geo, separate file e2e_ipgeo_geo_test.go), which uses the
// bundled MaxMind test mmdb to assert real lookups. The default build's
// noopGeoLookup makes country lists a no-op even when set on the service
// config — this file verifies that "geo OFF + CIDR-only" continues to
// enforce the CIDR allowlist.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
)

// ipGeoListener owns the test-only TLS listener that wraps the proxy
// handler with an ipgeo middleware. addr is "host:port", port is the
// bare port. The struct mirrors mtlsListener so the e2e fixtures look
// consistent.
type ipGeoListener struct {
	srv  *http.Server
	addr string
	port string
}

// startIPGeoListener spins up a fresh TLS ingress that wraps the
// bootE2EStack's proxy handler with a per-request IPGeo middleware. The
// middleware resolves the visitor IP via clientip.Resolve (trusted-proxy
// aware), loads service_ip_geo for the requested vhost via *db.DB,
// compiles the policy, and consults proxy.IPGeoEngine.Allow — exactly
// the production wiring the chain step is expected to gain. On deny we
// short-circuit 403 with the spec wording.
//
// trustedProxies wires CIDRs whose X-Forwarded-For is honoured. The
// test uses 127.0.0.1/32 (loopback) so the test client can spoof the
// visitor IP via XFF without colluding with the proxy.
func startIPGeoListener(t *testing.T, s *e2eStack, trustedProxies []*net.IPNet) *ipGeoListener {
	t.Helper()

	certPath, keyPath := generateWildcardCert(t, t.TempDir(), "*."+e2eAuthDomain, e2eAuthDomain)

	wrapped := db.Wrap(s.db)

	// The middleware: resolve subdomain → service_id → service_ip_geo
	// row → IPGeoEngine.AllowRequest. Calls the inner proxy handler on
	// allow, writes 403 on deny.
	inner := buildProxyForIPGeoTest(t, s, trustedProxies)
	ipgeoMW := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		suffix := "." + e2eAuthDomain
		if !strings.HasSuffix(host, suffix) {
			inner.ServeHTTP(w, r)
			return
		}
		label := strings.TrimSuffix(host, suffix)
		if label == "" || strings.Contains(label, ".") {
			inner.ServeHTTP(w, r)
			return
		}
		svc, err := s.store.ServiceForSubdomain(r.Context(), label)
		if err != nil {
			inner.ServeHTTP(w, r)
			return
		}
		cfg, err := wrapped.GetServiceIPGeo(r.Context(), svc.ID)
		if err != nil || !cfg.Enabled {
			inner.ServeHTTP(w, r)
			return
		}
		policy, err := proxy.CompileIPGeoPolicy(
			cfg.AllowCIDRs, cfg.BlockCIDRs, cfg.AllowCountries, cfg.BlockCountries,
		)
		if err != nil {
			inner.ServeHTTP(w, r)
			return
		}
		engine := proxy.NewIPGeoEngine(policy, nil) // nil geo = NoopGeoLookup
		if !engine.AllowRequest(w, r, trustedProxies) {
			return
		}
		inner.ServeHTTP(w, r)
	})

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load proxy cert: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("ipgeo listen: %v", err)
	}
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)

	srv := &http.Server{
		Handler:           ipgeoMW,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return &ipGeoListener{srv: srv, addr: addr, port: port}
}

// buildProxyForIPGeoTest constructs the inner proxy handler the ipgeo
// middleware delegates to on allow. It shares the bootE2EStack's store +
// *server.Server + gate so the same upstream tunnel handles the request.
// trustedProxies is also passed into the proxy via WithTrustedProxies so
// the inner proxy's own X-Forwarded-For rewriting stays consistent.
func buildProxyForIPGeoTest(t *testing.T, s *e2eStack, trustedProxies []*net.IPNet) http.Handler {
	t.Helper()
	checker := proxy.NewAccessCheckerWithSessionsAndLogger(
		s.store, s.store, e2eAuthDomain, s.log)
	dialer := proxyDialerAdapter{st: s.store, srv: s.server}
	return proxy.New(dialer, checker, e2eAuthDomain, s.log,
		proxy.WithGate(s.gate),
		proxy.WithTrustedProxies(trustedProxies),
	)
}

// visitorClient builds an *http.Client that dials the ipgeo listener
// on loopback and presents the requested SNI. TLS verify is skipped
// (the listener uses a self-signed test cert).
func (l *ipGeoListener) visitorClient(t *testing.T, hostname string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dialAddr := l.addr
	tlsCfg := &tls.Config{
		ServerName:         hostname,
		InsecureSkipVerify: true, //nolint:gosec // e2e test: self-signed listener cert
		MinVersion:         tls.VersionTLS12,
	}
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", dialAddr)
			if err != nil {
				return nil, err
			}
			tlsConn := tls.Client(conn, tlsCfg)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return tlsConn, nil
		},
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

// TestE2EIPGeo_CIDRAllowBlock verifies the default-build CIDR allow/block
// enforcement and the geo-OFF behaviour of the global /geo/status surface.
//
// Steps mirror the v0.4.0 integration plan, Task 9:
//  1. Seed service_ip_geo with allow_cidrs=["203.0.113.0/24"] (TEST-NET-3).
//  2. POST with X-Forwarded-For=203.0.113.7 (allowed) → 200.
//  3. POST with X-Forwarded-For=198.51.100.4 (TEST-NET-2, not in the
//     allowlist) → 403 {"error":"forbidden","reason":"ip_geo"}.
//  4. Add allow_countries=["DE"] while geo is not loaded; GET
//     /api/v1/geo/status returns {enabled:false}, and CIDR allow-only
//     enforcement continues to apply (the country list is a no-op).
func TestE2EIPGeo_CIDRAllowBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)

	// Upstream returns a known body so we know when the proxy lets the
	// request through.
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ipgeo-ok")
	})

	// Trust 127.0.0.1 as a forwarding proxy so X-Forwarded-For is
	// honoured for requests originating from the test client.
	_, loopback, _ := net.ParseCIDR("127.0.0.1/32")
	trusted := []*net.IPNet{loopback}

	// Stand up the new TLS listener with the ipgeo middleware wired.
	ln := startIPGeoListener(t, s, trusted)

	// ------------------------------------------------------------------
	// Step 1: seed service_ip_geo with allow_cidrs.
	// ------------------------------------------------------------------
	wrapped := db.Wrap(s.db)
	must(t, wrapped.SetServiceIPGeo(context.Background(), db.ServiceIPGeoConfig{
		ServiceID:      s.serviceID,
		Enabled:        true,
		AllowCIDRs:     []string{"203.0.113.0/24"},
		BlockCIDRs:     []string{},
		AllowCountries: []string{},
		BlockCountries: []string{},
	}), "SetServiceIPGeo(allow_cidrs)")

	hostname := s.hostname
	target := "https://" + hostname + ":" + ln.port + "/"

	// ------------------------------------------------------------------
	// Step 2: visitor IP inside allow_cidrs → 200.
	// ------------------------------------------------------------------
	hc := ln.visitorClient(t, hostname)
	req1, _ := http.NewRequest(http.MethodPost, target, strings.NewReader("payload"))
	req1.Header.Set("X-Forwarded-For", "203.0.113.7")
	r1, err := hc.Do(req1)
	must(t, err, "POST (allowed)")
	body1 := readAllString(t, r1)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("allowed XFF: want 200, got %d body=%s", r1.StatusCode, body1)
	}
	if body1 != "ipgeo-ok" {
		t.Errorf("allowed XFF: want upstream body 'ipgeo-ok', got %q", body1)
	}

	// ------------------------------------------------------------------
	// Step 3: visitor IP outside allow_cidrs → 403 ip_geo.
	// ------------------------------------------------------------------
	req2, _ := http.NewRequest(http.MethodPost, target, strings.NewReader("payload"))
	req2.Header.Set("X-Forwarded-For", "198.51.100.4")
	r2, err := hc.Do(req2)
	must(t, err, "POST (blocked)")
	body2 := readAllString(t, r2)
	if r2.StatusCode != http.StatusForbidden {
		t.Fatalf("blocked XFF: want 403, got %d body=%s", r2.StatusCode, body2)
	}
	// Spec Part J wording.
	if !strings.Contains(body2, `"error":"forbidden"`) ||
		!strings.Contains(body2, `"reason":"ip_geo"`) {
		t.Errorf("403 body shape: want forbidden+ip_geo, got %s", body2)
	}

	// ------------------------------------------------------------------
	// Step 4: add allow_countries while geo not loaded.
	//
	// In the default build NoopGeoLookup returns Enabled=false, so the
	// country list is accepted at write time but never enforced. CIDR
	// allow-only enforcement keeps working: 203.0.113.7 → 200,
	// 198.51.100.4 → 403.
	// ------------------------------------------------------------------
	must(t, wrapped.SetServiceIPGeo(context.Background(), db.ServiceIPGeoConfig{
		ServiceID:      s.serviceID,
		Enabled:        true,
		AllowCIDRs:     []string{"203.0.113.0/24"},
		BlockCIDRs:     []string{},
		AllowCountries: []string{"DE"},
		BlockCountries: []string{},
	}), "SetServiceIPGeo(+allow_countries)")

	// GET /api/v1/geo/status → enabled:false (default build).
	apiSrv, apiClient := startAPIForIPGeoTest(t, s)
	defer apiSrv.Close()

	statusReq, _ := http.NewRequest(http.MethodGet, apiSrv.URL+"/api/v1/geo/status", nil)
	statusResp, err := apiClient.hc.Do(statusReq)
	must(t, err, "GET /geo/status")
	statusBody := readAllString(t, statusResp)
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /geo/status: want 200, got %d body=%s", statusResp.StatusCode, statusBody)
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(statusBody), &status); err != nil {
		t.Fatalf("decode /geo/status: %v body=%s", err, statusBody)
	}
	if got, _ := status["enabled"].(bool); got {
		t.Errorf("/geo/status.enabled: want false in default build, got %v body=%s", got, statusBody)
	}

	// CIDR enforcement still works with allow_countries set + geo off.
	req3, _ := http.NewRequest(http.MethodPost, target, strings.NewReader("payload"))
	req3.Header.Set("X-Forwarded-For", "203.0.113.7")
	r3, err := hc.Do(req3)
	must(t, err, "POST (allowed, geo-off + countries)")
	body3 := readAllString(t, r3)
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("post-country-set allowed XFF: want 200, got %d body=%s", r3.StatusCode, body3)
	}
	if body3 != "ipgeo-ok" {
		t.Errorf("post-country-set body: want 'ipgeo-ok', got %q", body3)
	}
	req4, _ := http.NewRequest(http.MethodPost, target, strings.NewReader("payload"))
	req4.Header.Set("X-Forwarded-For", "198.51.100.4")
	r4, err := hc.Do(req4)
	must(t, err, "POST (blocked, geo-off + countries)")
	body4 := readAllString(t, r4)
	if r4.StatusCode != http.StatusForbidden {
		t.Fatalf("post-country-set blocked XFF: want 403, got %d body=%s", r4.StatusCode, body4)
	}
	if !strings.Contains(body4, `"reason":"ip_geo"`) {
		t.Errorf("403 body: want ip_geo reason, got %s", body4)
	}
}

// TestE2EIPGeo_BlockCIDR proves the production proxy (the one booted by
// bootE2EStack, wired via proxyDialerAdapter + SetServiceIPGeo) returns 403
// when the visitor's real TCP source IP is in a blocked CIDR and 200 once the
// block is cleared.
//
// The visitor always connects from 127.0.0.1 (loopback). No trusted-proxy list
// is set on the stack, so clientip.Resolve returns the raw TCP peer IP.
// Blocking 127.0.0.0/8 therefore blocks the test's own visitor requests.
func TestE2EIPGeo_BlockCIDR(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)

	// Upstream returns 200 with a known body when the proxy lets traffic through.
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok-ipgeo")
	})

	wrapped := db.Wrap(s.db)
	hc := s.visitorClient(t)
	target := "https://" + s.hostWithPort() + "/"

	// ------------------------------------------------------------------
	// Arm 1: block the loopback CIDR → proxy must return 403.
	// ------------------------------------------------------------------
	must(t, wrapped.SetServiceIPGeo(context.Background(), db.ServiceIPGeoConfig{
		ServiceID:      s.serviceID,
		Enabled:        true,
		AllowCIDRs:     []string{},
		BlockCIDRs:     []string{"127.0.0.0/8"},
		AllowCountries: []string{},
		BlockCountries: []string{},
	}), "SetServiceIPGeo(block loopback)")

	resp1, err := hc.Get(target)
	must(t, err, "GET (blocked)")
	body1 := readAllString(t, resp1)
	if resp1.StatusCode != http.StatusForbidden {
		t.Fatalf("arm1: want 403, got %d body=%s", resp1.StatusCode, body1)
	}

	// ------------------------------------------------------------------
	// Arm 2: clear the block (Enabled=false) → proxy must return 200.
	// ------------------------------------------------------------------
	must(t, wrapped.SetServiceIPGeo(context.Background(), db.ServiceIPGeoConfig{
		ServiceID:      s.serviceID,
		Enabled:        false,
		AllowCIDRs:     []string{},
		BlockCIDRs:     []string{},
		AllowCountries: []string{},
		BlockCountries: []string{},
	}), "SetServiceIPGeo(clear)")

	resp2, err := hc.Get(target)
	must(t, err, "GET (cleared)")
	body2 := readAllString(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("arm2: want 200, got %d body=%s", resp2.StatusCode, body2)
	}
	if body2 != "ok-ipgeo" {
		t.Errorf("arm2: upstream body: want ok-ipgeo, got %q", body2)
	}
}

// ---------------------------------------------------------------------------
// Minimal API harness for the /geo/status step.
// ---------------------------------------------------------------------------

// ipgeoAPIClient holds a logged-in http.Client for the JSON API. The
// session cookie is set by POST /auth/login; GET endpoints (like
// /geo/status) do not need CSRF.
type ipgeoAPIClient struct {
	hc *http.Client
}

// startAPIForIPGeoTest mounts api.NewRouter against the same store the
// e2e stack uses, with GeoLookup=NoopGeoLookup() so /geo/status reports
// the default-build enabled=false. Returns a logged-in client whose
// Jar carries burrow_session.
func startAPIForIPGeoTest(t *testing.T, s *e2eStack) (*httptest.Server, *ipgeoAPIClient) {
	t.Helper()
	deps := api.Deps{
		Users:      s.store,
		Services:   s.store,
		AuthDomain: e2eAuthDomain,
		Log:        s.log,
		GeoLookup:  proxy.NoopGeoLookup(),
	}
	apiSrv := httptest.NewServer(api.NewRouter(deps))

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Jar: jar, Timeout: 5 * time.Second}

	loginBody := strings.NewReader(`{"email":"` + e2eAdminEmail +
		`","password":"` + e2eAdminPassword + `"}`)
	resp, err := hc.Post(apiSrv.URL+"/api/v1/auth/login", "application/json", loginBody)
	must(t, err, "POST /auth/login")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}
	return apiSrv, &ipgeoAPIClient{hc: hc}
}
