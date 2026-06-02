package proxy_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proxy"
)

// caBundle is a CA cert + its private key, used to sign client certs for
// the mtls tests.
type caBundle struct {
	cert     *x509.Certificate
	priv     *ecdsa.PrivateKey
	pemBytes []byte
}

// generateCA creates a self-signed CA suitable for issuing client certs.
func generateCA(t *testing.T) *caBundle {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "burrow-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &caBundle{cert: cert, priv: priv, pemBytes: pemBytes}
}

// issueClientCert signs a leaf certificate with the CA, returning a
// tls.Certificate the visitor can present.
func issueClientCert(t *testing.T, ca *caBundle, cn string) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &priv.PublicKey, ca.priv)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
		Leaf:        leaf,
	}
}

// --------------------------------------------------------------------------
// Unit tests: accessChecker.Allow for mtls mode
// --------------------------------------------------------------------------

// TestMTLS_AccessChecker_MissingCert verifies that a request without a TLS
// client cert is rejected with 401 client cert required.
func TestMTLS_AccessChecker_MissingCert(t *testing.T) {
	ac := proxy.NewAccessChecker(&fakeValidator{}, testAuthDomain)
	ca := generateCA(t)
	res := &proxy.Resolved{
		ServiceID:  "svc-mtls",
		AccessMode: proxy.AccessModeMTLS,
		MTLSCAPEM:  ca.pemBytes,
	}

	// r.TLS is nil → no cert presented.
	req := httptest.NewRequest("GET", "http://svc.example.com/", nil)
	ok, status, body, hdr := ac.Allow(context.Background(), res, req)
	if ok {
		t.Fatal("missing cert: want ok=false")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", status)
	}
	if body != `{"error":"client cert required"}` {
		t.Errorf("body mismatch: %q", body)
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Errorf("want json content-type, got %q", hdr.Get("Content-Type"))
	}
}

// TestMTLS_AccessChecker_TLSNoPeerCert verifies that a TLS connection that
// somehow has no peer certs (e.g. test fakery / VerifyClientCertIfGiven path)
// is also rejected.
func TestMTLS_AccessChecker_TLSNoPeerCert(t *testing.T) {
	ac := proxy.NewAccessChecker(&fakeValidator{}, testAuthDomain)
	ca := generateCA(t)
	res := &proxy.Resolved{
		ServiceID:  "svc-mtls",
		AccessMode: proxy.AccessModeMTLS,
		MTLSCAPEM:  ca.pemBytes,
	}
	req := httptest.NewRequest("GET", "http://svc.example.com/", nil)
	req.TLS = &tls.ConnectionState{} // present but empty
	ok, status, _, _ := ac.Allow(context.Background(), res, req)
	if ok {
		t.Fatal("empty peer certs: want ok=false")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", status)
	}
}

// TestMTLS_AccessChecker_ValidCert verifies that a request with a TLS cert
// signed by the service's CA is allowed.
func TestMTLS_AccessChecker_ValidCert(t *testing.T) {
	ac := proxy.NewAccessChecker(&fakeValidator{}, testAuthDomain)
	ca := generateCA(t)
	clientCert := issueClientCert(t, ca, "alice@example.com")

	res := &proxy.Resolved{
		ServiceID:  "svc-mtls",
		AccessMode: proxy.AccessModeMTLS,
		MTLSCAPEM:  ca.pemBytes,
	}
	req := httptest.NewRequest("GET", "http://svc.example.com/", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{clientCert.Leaf},
	}
	ok, status, body, _ := ac.Allow(context.Background(), res, req)
	if !ok {
		t.Errorf("valid cert: want ok=true, got status=%d body=%q", status, body)
	}
}

// TestMTLS_AccessChecker_UntrustedCert verifies defense-in-depth: even when
// r.TLS.PeerCertificates is set (so the TLS layer was happy), the access
// checker re-verifies against the per-service CA and rejects an untrusted
// cert. Guards against future wiring mistakes (mtls without
// GetConfigForClient set), tests that fake r.TLS, or a misconfigured TLS
// listener.
func TestMTLS_AccessChecker_UntrustedCert(t *testing.T) {
	ac := proxy.NewAccessChecker(&fakeValidator{}, testAuthDomain)
	expected := generateCA(t)
	rogue := generateCA(t)
	rogueClient := issueClientCert(t, rogue, "evil@example.com")

	res := &proxy.Resolved{
		ServiceID:  "svc-mtls",
		AccessMode: proxy.AccessModeMTLS,
		MTLSCAPEM:  expected.pemBytes, // service trusts CA #1...
	}
	req := httptest.NewRequest("GET", "http://svc.example.com/", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{rogueClient.Leaf}, // ...visitor signed by CA #2
	}
	ok, status, body, _ := ac.Allow(context.Background(), res, req)
	if ok {
		t.Fatal("untrusted cert must be rejected by defense-in-depth re-verify")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", status)
	}
	if body != `{"error":"client cert required"}` {
		t.Errorf("body mismatch: %q", body)
	}
}

// TestMTLS_AccessChecker_InvalidCAPEM verifies that an mtls Resolved with a
// non-PEM CA blob is rejected with 500 (operator-config bug) rather than
// allowing through.
func TestMTLS_AccessChecker_InvalidCAPEM(t *testing.T) {
	ac := proxy.NewAccessChecker(&fakeValidator{}, testAuthDomain)
	ca := generateCA(t)
	clientCert := issueClientCert(t, ca, "alice")

	res := &proxy.Resolved{
		ServiceID:  "svc-mtls",
		AccessMode: proxy.AccessModeMTLS,
		MTLSCAPEM:  []byte("not a pem block"),
	}
	req := httptest.NewRequest("GET", "http://svc.example.com/", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{clientCert.Leaf},
	}
	ok, status, body, _ := ac.Allow(context.Background(), res, req)
	if ok {
		t.Fatal("invalid CA PEM must not allow")
	}
	if status != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", status)
	}
	if body != `{"error":"invalid CA configuration"}` {
		t.Errorf("body mismatch: %q", body)
	}
}

// --------------------------------------------------------------------------
// Integration test: full TLS handshake round-trip through Proxy.ServeHTTP
//
// Uses httptest.NewUnstartedServer + a TLS config set up by the proxy's
// GetConfigForClient SNI lookup.
// --------------------------------------------------------------------------

// startMTLSTestServer builds an httptest TLS server with the proxy's
// GetConfigForClient hook wired against the listener's base TLS config.
// Returns the running server + an *http.Client that trusts the server cert
// and skips SNI/hostname mismatch (test cert is for example.com, but we
// dial via httptest's 127.0.0.1 with our own SNI / Host).
func startMTLSTestServer(t *testing.T, p *proxy.Proxy, domain string) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(p)
	// httptest.StartTLS lazily populates ts.TLS; we set GetConfigForClient
	// pre-Start so it's picked up by the listener.
	ts.TLS = &tls.Config{
		GetConfigForClient: p.GetConfigForClient,
	}
	ts.StartTLS()
	// Now that StartTLS has populated the listener's base config with the
	// test certificates, register that base on the proxy so the per-vhost
	// hook can clone Certificates from it.
	proxy.SetTLSBaseForTest(p, ts.TLS)
	return ts
}

// TestMTLS_FullHandshake_RejectsNoCert verifies that a TLS client that does
// not present a certificate is rejected at the TLS layer when the per-vhost
// config requires one.
func TestMTLS_FullHandshake_RejectsNoCert(t *testing.T) {
	const domain = "tunnels.example.com"
	ca := generateCA(t)

	d := newFakeDialer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream reached despite missing client cert")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	d.register("app", &proxy.Resolved{
		ServiceID:  "svc-mtls",
		AccessMode: proxy.AccessModeMTLS,
		LocalHost:  "127.0.0.1:3000",
		MTLSCAPEM:  ca.pemBytes,
	})

	ac := proxy.NewAccessChecker(&fakeValidator{}, domain)
	p := proxy.New(d, ac, domain, testLog())

	ts := startMTLSTestServer(t, p, domain)
	defer ts.Close()

	// Client trusts the test server's cert (skip hostname verify — the
	// httptest cert is for example.com, but we use SNI=app.tunnels.example.com).
	rootPool := x509.NewCertPool()
	rootPool.AddCert(ts.Certificate())
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:            rootPool,
				ServerName:         "app." + domain,
				InsecureSkipVerify: true, //nolint:gosec // test cert is for example.com
			},
		},
	}
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "app." + domain

	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("want TLS handshake error, got status=%d", resp.StatusCode)
	}
	// httputil's error messages vary; just confirm the call failed at TLS.
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "tls") &&
		!strings.Contains(err.Error(), "handshake") && !strings.Contains(err.Error(), "EOF") &&
		!strings.Contains(err.Error(), "alert") && !strings.Contains(err.Error(), "connection") {
		t.Logf("warning: TLS reject error wording unexpected: %v", err)
	}
}

// TestMTLS_FullHandshake_ValidCertProxies verifies the happy path: client
// presents a CA-signed cert, the TLS layer accepts it, and the proxy
// forwards the request to the upstream.
func TestMTLS_FullHandshake_ValidCertProxies(t *testing.T) {
	const domain = "tunnels.example.com"
	ca := generateCA(t)
	clientCert := issueClientCert(t, ca, "alice@example.com")

	upstreamHit := false
	d := newFakeDialer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from upstream"))
	}))
	d.register("app", &proxy.Resolved{
		ServiceID:  "svc-mtls",
		AccessMode: proxy.AccessModeMTLS,
		LocalHost:  "127.0.0.1:3000",
		MTLSCAPEM:  ca.pemBytes,
	})

	ac := proxy.NewAccessChecker(&fakeValidator{}, domain)
	p := proxy.New(d, ac, domain, testLog())

	ts := startMTLSTestServer(t, p, domain)
	defer ts.Close()

	rootPool := x509.NewCertPool()
	rootPool.AddCert(ts.Certificate())
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:            rootPool,
				ServerName:         "app." + domain,
				InsecureSkipVerify: true, //nolint:gosec // test cert CN ≠ SNI on purpose
				Certificates:       []tls.Certificate{clientCert},
			},
		},
	}
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "app." + domain

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d (body=%q)", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hello from upstream") {
		t.Errorf("body mismatch: %q", body)
	}
	if !upstreamHit {
		t.Error("upstream handler not invoked")
	}
}

// TestMTLS_FullHandshake_NonMTLSVhostHasNoClientAuth verifies that vhosts
// without an mtls config don't require a client cert. This guards the
// per-vhost split — adding mtls to one service must NOT break the others.
func TestMTLS_FullHandshake_NonMTLSVhostHasNoClientAuth(t *testing.T) {
	const domain = "tunnels.example.com"
	ca := generateCA(t)

	d := newFakeDialer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	// Two services: one mtls, one open.
	d.register("app", &proxy.Resolved{
		ServiceID:  "svc-mtls",
		AccessMode: proxy.AccessModeMTLS,
		LocalHost:  "127.0.0.1:3000",
		MTLSCAPEM:  ca.pemBytes,
	})
	d.register("public", &proxy.Resolved{
		ServiceID:  "svc-open",
		AccessMode: proxy.AccessModeOpen,
		LocalHost:  "127.0.0.1:3000",
	})

	ac := proxy.NewAccessChecker(&fakeValidator{}, domain)
	p := proxy.New(d, ac, domain, testLog())

	ts := startMTLSTestServer(t, p, domain)
	defer ts.Close()

	rootPool := x509.NewCertPool()
	rootPool.AddCert(ts.Certificate())
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:            rootPool,
				ServerName:         "public." + domain,
				InsecureSkipVerify: true, //nolint:gosec // test cert CN ≠ SNI
			},
		},
	}
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "public." + domain
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("public service handshake failed unexpectedly: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 on non-mtls vhost, got %d", resp.StatusCode)
	}
}
