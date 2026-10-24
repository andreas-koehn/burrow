package main

// e2e_ipgeo_geo_test.go — Task 9 of the v0.4.0 integration plan (geo
// build-tag half). Smoke test that proves block_countries enforcement
// engages end-to-end when the binary is built with -tags geo and the
// MMDB-backed lookup is wired.
//
// The bundled MaxMind test database under internal/proxy/testdata/ is
// extremely sparse — it only covers a handful of networks (GB / SE / PH
// / NO / US / BT). The task plan suggests RU as the "blocked" country
// and DE as the "allowed" country, but neither is in the test fixture.
// We therefore adapt the spec's intent (block_countries enforcement +
// allow-by-default fallthrough) to the fixture: block SE
// (Sweden — 89.160.20.0/24 in the test data) and allow GB (Great
// Britain — 81.2.69.0/24). Same assertion shape, real countries the
// fixture actually carries.
//
// Path to the mmdb is relative to cmd/server's CWD at test time
// (../../internal/proxy/testdata/...). The test SKIPs when the file
// is missing so a CI mirror that purges vendor fixtures still goes
// green instead of bombing the suite.

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
)

// TestE2EIPGeo_CountriesGated proves block_countries enforcement engages
// when the geo build tag is on and the MMDB lookup is wired. SKIPs when
// the bundled test mmdb is missing — the country path is smoke-only by
// design (the heavy lifting is covered by the proxy package's own
// geo_test.go fixture probes).
func TestE2EIPGeo_CountriesGated(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}

	mmdbPath := filepath.Join("..", "..", "internal", "proxy", "testdata",
		"GeoLite2-Country-Test.mmdb")
	if _, err := os.Stat(mmdbPath); err != nil {
		t.Skip("no bundled test mmdb")
	}
	geoLookup, err := proxy.OpenGeoDB(mmdbPath)
	if err != nil {
		t.Fatalf("OpenGeoDB(%s): %v", mmdbPath, err)
	}
	if !geoLookup.Enabled() {
		t.Fatal("OpenGeoDB returned disabled lookup despite -tags geo build")
	}

	s := bootE2EStack(t)
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "country-ok")
	})

	// Trust 127.0.0.1 so X-Forwarded-For is honoured for the test client.
	_, loopback, _ := net.ParseCIDR("127.0.0.1/32")
	trusted := []*net.IPNet{loopback}

	// Stand up the geo-aware listener: same shape as the default-build
	// CIDR test but the engine is constructed with the live MMDB lookup
	// instead of NoopGeoLookup.
	ln := startGeoListener(t, s, trusted, geoLookup)

	// Seed: block_countries=["SE"] (Sweden — fixture carries 89.160.20.0/24).
	wrapped := db.Wrap(s.db)
	must(t, wrapped.SetServiceIPGeo(context.Background(), db.ServiceIPGeoConfig{
		ServiceID:      s.serviceID,
		Enabled:        true,
		AllowCIDRs:     []string{},
		BlockCIDRs:     []string{},
		AllowCountries: []string{},
		BlockCountries: []string{"SE"},
	}), "SetServiceIPGeo(block_countries=SE)")

	hostname := s.hostname
	target := "https://" + hostname + ":" + ln.port + "/"
	hc := geoVisitorClient(t, ln, hostname)

	// Known-SE IP from the test mmdb (89.160.20.112) → 403.
	reqSE, _ := http.NewRequest(http.MethodPost, target, strings.NewReader("p"))
	reqSE.Header.Set("X-Forwarded-For", "89.160.20.112")
	rSE, err := hc.Do(reqSE)
	must(t, err, "POST (SE)")
	bodySE := readAllString(t, rSE)
	if rSE.StatusCode != http.StatusForbidden {
		t.Fatalf("block_countries=SE on 89.160.20.112: want 403, got %d body=%s",
			rSE.StatusCode, bodySE)
	}
	if !strings.Contains(bodySE, `"reason":"ip_geo"`) {
		t.Errorf("SE 403 body shape: want ip_geo, got %s", bodySE)
	}

	// Known-GB IP (81.2.69.142) → 200 from upstream.
	reqGB, _ := http.NewRequest(http.MethodPost, target, strings.NewReader("p"))
	reqGB.Header.Set("X-Forwarded-For", "81.2.69.142")
	rGB, err := hc.Do(reqGB)
	must(t, err, "POST (GB)")
	bodyGB := readAllString(t, rGB)
	if rGB.StatusCode != http.StatusOK {
		t.Fatalf("block_countries=SE on 81.2.69.142: want 200, got %d body=%s",
			rGB.StatusCode, bodyGB)
	}
	if bodyGB != "country-ok" {
		t.Errorf("GB body: want upstream 'country-ok', got %q", bodyGB)
	}
}

// startGeoListener mirrors startIPGeoListener but constructs the
// IPGeoEngine with the supplied MMDB-backed lookup so country filters
// engage. Kept inline (not in e2e_helpers_test.go) per the Wave 2
// task spec.
func startGeoListener(t *testing.T, s *e2eStack, trustedProxies []*net.IPNet, geoLookup proxy.GeoLookup) *ipGeoListener {
	t.Helper()

	certPath, keyPath := generateWildcardCert(t, t.TempDir(), "*."+e2eAuthDomain, e2eAuthDomain)

	wrapped := db.Wrap(s.db)

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
		engine := proxy.NewIPGeoEngine(policy, geoLookup)
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
		t.Fatalf("geo listen: %v", err)
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

// geoVisitorClient builds an *http.Client wired to the geo listener.
// Same shape as the default-build helper; lives in this file so the
// non-geo build never compiles it.
func geoVisitorClient(t *testing.T, l *ipGeoListener, hostname string) *http.Client {
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
