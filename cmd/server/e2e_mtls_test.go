package main

// e2e_mtls_test.go — Task 8 of the v0.4.0 integration plan. Real-stack e2e
// for mtls access mode + the PUT /access-mode 409 TCP guard.
//
// The shared bootE2EStack proxy listener does NOT wire
// proxy.GetConfigForClient (its TLS config carries Certificates only, no
// ClientAuth). That is fine for api_key / burrow_login tests but it means
// the TLS layer never asks for a client cert — so we cannot exercise the
// TLS handshake half of mtls through the shared listener.
//
// Resolution (per Wave 2 task plan: define new helpers inline, do NOT
// modify e2e_helpers_test.go): we stand up a SECOND TLS listener wrapping
// a FRESH proxy.Proxy + accessChecker that share the same store /
// *server.Server / gate. The new listener wires GetConfigForClient so
// per-vhost mtls verification engages at TLS-handshake time.
//
// The 409 step covers the API contract that PUT
// /api/v1/services/{id}/access-mode rejects mtls on a TCP service. We
// mount the real api.NewRouter on a tiny httptest.Server (no TLS — the
// JSON API is plain HTTP in tests), log in with the seeded admin, and
// PUT the access-mode change.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
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

// mtlsCA bundles a CA cert + key for issuing client certs inline.
type mtlsCA struct {
	cert     *x509.Certificate
	key      *ecdsa.PrivateKey
	pemBytes []byte
}

// newMTLSCA generates a fresh CA suitable for signing test client certs.
// Stdlib only — no openssl, no external dep.
func newMTLSCA(t *testing.T, cn string) *mtlsCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &mtlsCA{cert: cert, key: key, pemBytes: pemBytes}
}

// issueClient signs a client leaf cert with the CA; returned tls.Certificate
// is ready to drop into a Client TLS config.
func (ca *mtlsCA) issueClient(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}
}

// mtlsListener owns the test-only TLS listener wired with the proxy's
// per-vhost GetConfigForClient hook. addr is "host:port", port is the bare
// port number (matches the way bootE2EStack tracks its own listener).
type mtlsListener struct {
	srv  *http.Server
	addr string
	port string
}

// startMTLSListener spins up a fresh TLS ingress that shares the
// bootE2EStack's store + *server.Server + gate, but constructs its own
// *proxy.Proxy so we can wire GetConfigForClient at TLS-handshake time.
// The returned listener serves on a random loopback port; t.Cleanup tears
// it down deterministically.
//
// The base *tls.Config carries the same wildcard cert bootE2EStack uses,
// plus VerifyClientCertIfGiven so non-mtls vhosts still work; for the
// mtls SNI label, GetConfigForClient swaps in
// RequireAndVerifyClientCert + ClientCAs from services.mtls_ca_pem.
func startMTLSListener(t *testing.T, s *e2eStack) *mtlsListener {
	t.Helper()

	// Pull the same wildcard cert bootE2EStack generated. We don't have
	// access to its file paths so generate a fresh one for this listener.
	certPath, keyPath := generateWildcardCert(t, t.TempDir(), "*."+e2eAuthDomain, e2eAuthDomain)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load proxy cert: %v", err)
	}

	// Build a fresh checker + dialer using the same backing stores as
	// bootE2EStack. The proxy's mtls path needs the per-vhost CA from
	// services.mtls_ca_pem, which Lookup already plumbs through Resolved.
	checker := proxy.NewAccessCheckerWithSessionsAndLogger(s.store, s.store, e2eAuthDomain, s.log)
	dialer := proxyDialerAdapter{st: s.store, srv: s.server}
	base := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	p := proxy.New(dialer, checker, e2eAuthDomain, s.log,
		proxy.WithGate(s.gate),
		proxy.WithTLSBase(base),
	)
	// Wire the per-vhost client-cert verifier into the listener's TLS
	// handshake. The hook returns RequireAndVerifyClientCert + per-service
	// ClientCAs only for vhosts whose access_mode=mtls; non-mtls vhosts
	// continue to use the default config (no client cert required).
	base.GetConfigForClient = p.GetConfigForClient

	ln, err := tls.Listen("tcp", "127.0.0.1:0", base)
	if err != nil {
		t.Fatalf("mtls listen: %v", err)
	}
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)

	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
	})
	return &mtlsListener{srv: srv, addr: addr, port: port}
}

// dialMTLS builds an *http.Client that dials the mtls listener on
// loopback while presenting SNI for the test service vhost. When
// clientCert is the zero value, no client cert is presented (visitor
// "forgot" their cert). All test handshakes use TLS 1.2 minimum.
func (l *mtlsListener) dialMTLS(t *testing.T, hostname string, clientCert tls.Certificate) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dialAddr := l.addr
	tlsCfg := &tls.Config{
		ServerName:         hostname,
		InsecureSkipVerify: true, //nolint:gosec // e2e test: self-signed server cert
		MinVersion:         tls.VersionTLS12,
	}
	// Present a client cert only when one was issued (CN non-empty).
	if clientCert.Leaf != nil {
		tlsCfg.Certificates = []tls.Certificate{clientCert}
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

// TestE2EMTLS_AccessMode covers the four mtls scenarios end-to-end:
//
//  1. No client cert  → TLS handshake failure (the listener requires +
//     verifies a cert for the mtls vhost).
//  2. Wrong-CA cert   → TLS handshake failure (signed by an untrusted CA).
//  3. Correct cert    → 200 from the upstream echo handler.
//  4. PUT mtls on TCP → 409 with the spec wording from Part J.2.
//
// The first three exercise the data plane (TLS handshake + accessChecker
// re-verify); the fourth exercises the management API gate that prevents
// configuring mtls on a non-http service in the first place.
func TestE2EMTLS_AccessMode(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)
	ln := startMTLSListener(t, s)

	// Upstream returns a known body so the happy path is unambiguous.
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "mtls-ok")
	})

	// Generate a fresh CA + a client cert signed by it. Stdlib only.
	ca := newMTLSCA(t, "burrow-mtls-test-ca")
	goodCert := ca.issueClient(t, "alice@example.com")

	// Switch the HTTP service to mtls mode with our CA as the trust anchor.
	must(t, s.store.SetServiceAccessMode(
		context.Background(), s.userID, "admin", s.serviceID, "mtls", "", ca.pemBytes),
		"SetServiceAccessMode(mtls)")

	hostname := s.hostname
	target := "https://" + hostname + ":" + ln.port + "/"

	// ------------------------------------------------------------------
	// Scenario 1: no client cert → TLS handshake failure
	// ------------------------------------------------------------------
	hc1 := ln.dialMTLS(t, hostname, tls.Certificate{})
	resp1, err1 := hc1.Get(target)
	if err1 == nil {
		_ = resp1.Body.Close()
		// Some stdlib versions complete the handshake before sending the
		// alert; if the server reaches the handler it should respond 401
		// from the access checker. Either outcome is acceptable per spec.
		body := readAllString(t, resp1)
		if resp1.StatusCode != http.StatusUnauthorized {
			t.Fatalf("no-cert: want TLS handshake error OR 401, got %d body=%s", resp1.StatusCode, body)
		}
		if !strings.Contains(body, "client cert required") {
			t.Errorf("no-cert: body %q must contain 'client cert required'", body)
		}
	} else if !isTLSHandshakeError(err1) {
		t.Fatalf("no-cert: want TLS handshake error, got: %v", err1)
	}

	// ------------------------------------------------------------------
	// Scenario 2: cert signed by a DIFFERENT CA → handshake rejected
	// ------------------------------------------------------------------
	rogueCA := newMTLSCA(t, "burrow-rogue-ca")
	rogueCert := rogueCA.issueClient(t, "evil@example.com")
	hc2 := ln.dialMTLS(t, hostname, rogueCert)
	resp2, err2 := hc2.Get(target)
	if err2 == nil {
		_ = resp2.Body.Close()
		t.Fatalf("rogue-cert: want TLS handshake error, got status %d", resp2.StatusCode)
	}
	if !isTLSHandshakeError(err2) {
		t.Fatalf("rogue-cert: want TLS handshake error, got: %v", err2)
	}

	// ------------------------------------------------------------------
	// Scenario 3: cert signed by the CONFIGURED CA → 200 from upstream
	// ------------------------------------------------------------------
	hc3 := ln.dialMTLS(t, hostname, goodCert)
	resp3, err3 := hc3.Get(target)
	must(t, err3, "GET (good cert)")
	body3 := readAllString(t, resp3)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("good-cert: want 200, got %d body=%s", resp3.StatusCode, body3)
	}
	if body3 != "mtls-ok" {
		t.Errorf("good-cert: want body 'mtls-ok', got %q", body3)
	}

	// ------------------------------------------------------------------
	// Scenario 4: PUT mtls on a TCP service → 409 spec wording
	// ------------------------------------------------------------------
	// Seed a fresh TCP service (the bootE2EStack one is http). We talk
	// to the API on a plain-HTTP httptest.Server — the JSON API does not
	// require TLS in tests (cmd/server uses TLS for the control plane,
	// not for /api/v1).
	tcpSvc, err := db.Wrap(s.db).GetOrCreateService(
		context.Background(), s.userID, "tcp-mtls-409", "tcp")
	must(t, err, "create tcp service")
	if tcpSvc.Type != "tcp" {
		t.Fatalf("expected tcp service, got type=%q", tcpSvc.Type)
	}

	apiSrv, apiClient := startAPIForMTLSTest(t, s)
	defer apiSrv.Close()

	body := bytes.NewReader([]byte(`{"access_mode":"mtls","mtls_ca_pem":"` +
		strings.ReplaceAll(string(ca.pemBytes), "\n", `\n`) + `"}`))
	req, _ := http.NewRequest(http.MethodPut,
		apiSrv.URL+"/api/v1/services/"+tcpSvc.ID+"/access-mode",
		body)
	req.Header.Set("Content-Type", "application/json")
	apiClient.attachCSRF(req)
	resp4, err := apiClient.hc.Do(req)
	must(t, err, "PUT access-mode mtls")
	body4 := readAllString(t, resp4)
	if resp4.StatusCode != http.StatusConflict {
		t.Fatalf("PUT access-mode mtls on TCP: want 409, got %d body=%s", resp4.StatusCode, body4)
	}
	// Spec Part J.2 wording (also pinned by contract_v3_test.go).
	want := "api_key, burrow_login, and mtls require an http service"
	if !strings.Contains(body4, want) {
		t.Fatalf("409 body: want contains %q, got: %s", want, body4)
	}
}

// isTLSHandshakeError reports whether err looks like a TLS handshake
// failure. stdlib error wording varies by build / version so we accept
// any of a stable set of substrings — same approach the proxy package's
// mtls_test.go takes.
func isTLSHandshakeError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, needle := range []string{
		"tls",
		"handshake",
		"certificate",
		"alert",
		"bad certificate",
		"unknown certificate authority",
		"required",
		"EOF",
		"connection reset",
		"connection",
	} {
		if strings.Contains(strings.ToLower(msg), needle) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Minimal API harness for the 409 step.
// ---------------------------------------------------------------------------

// mtlsAPIClient holds a logged-in HTTP client and the CSRF token harvested
// from POST /api/v1/auth/login. The Jar carries the session cookie so
// subsequent requests authenticate automatically; the CSRF token must be
// echoed via the X-CSRF-Token header on every state-changing call
// (double-submit cookie pattern).
type mtlsAPIClient struct {
	hc   *http.Client
	csrf string
}

// attachCSRF echoes the CSRF token on a state-changing request so the
// api.RequireCSRF middleware passes.
func (c *mtlsAPIClient) attachCSRF(req *http.Request) {
	req.Header.Set("X-CSRF-Token", c.csrf)
}

// startAPIForMTLSTest mounts the real api.NewRouter against the same
// *store.Store the e2e stack uses, then logs in with the seeded admin
// account so the returned client carries a valid burrow_session cookie +
// CSRF token. The router is plain HTTP (no TLS); the data plane TLS work
// happens on the separate mtls listener.
func startAPIForMTLSTest(t *testing.T, s *e2eStack) (*httptest.Server, *mtlsAPIClient) {
	t.Helper()
	deps := api.Deps{
		Users:      s.store,
		Services:   s.store,
		AuthDomain: e2eAuthDomain,
		// This suite runs the host-routed ingress, so mTLS is on offer.
		HostRouting: true,
		Log:         s.log,
	}
	apiSrv := httptest.NewServer(api.NewRouter(deps))

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Jar: jar, Timeout: 5 * time.Second}

	// POST /auth/login → 200 + burrow_session + burrow_csrf cookies.
	loginBody := strings.NewReader(`{"email":"` + e2eAdminEmail +
		`","password":"` + e2eAdminPassword + `"}`)
	resp, err := hc.Post(apiSrv.URL+"/api/v1/auth/login", "application/json", loginBody)
	must(t, err, "POST /auth/login")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}

	// Extract the CSRF token from the cookie jar (Jar stores cookies by
	// the URL that set them; we just iterate every cookie for this host).
	u, _ := resp.Request.URL.Parse("/")
	var csrf string
	for _, c := range jar.Cookies(u) {
		if c.Name == "burrow_csrf" {
			csrf = c.Value
			break
		}
	}
	if csrf == "" {
		t.Fatal("login did not set burrow_csrf cookie")
	}
	return apiSrv, &mtlsAPIClient{hc: hc, csrf: csrf}
}

// _ keeps imports honest under future test growth (json is used by the
// 409 step's PEM-escape path indirectly; explicit silencer makes the
// imports list stable).
var _ = json.Marshal
