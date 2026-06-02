package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// ─── Test helpers ─────────────────────────────────────────────────────────────

// genCA generates a self-signed CA cert for test chains.
func genCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, *x509.CertPool, []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genCA: gen key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "burrow-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("genCA: create cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert, priv, pool, pemBytes
}

// genCert generates a leaf cert signed by ca, for the given SANs.
// Returns PEM-encoded cert and key.
func genCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, sans []string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genCert: gen key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: sans[0]},
		DNSNames:     sans,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &priv.PublicKey, caKey)
	if err != nil {
		t.Fatalf("genCert: create cert: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("genCert: marshal key: %v", err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// genExpiredCert generates a leaf cert that is already expired.
func genExpiredCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, sans []string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genExpiredCert: gen key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: sans[0]},
		DNSNames:     sans,
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-time.Hour), // already expired
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &priv.PublicKey, caKey)
	if err != nil {
		t.Fatalf("genExpiredCert: create cert: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// ─── Validator tests ─────────────────────────────────────────────────────────

// TestPutCustomDomainRejectsSANMismatch verifies that a cert whose SAN does not
// include the requested hostname is rejected with 400 and reason=san_mismatch.
func TestPutCustomDomainRejectsSANMismatch(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)

	// Cert SAN = ["bar.example.com"], hostname = "foo.example.com"
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"bar.example.com"})

	result, status, errBody := validateCertAndKey(certPEM, keyPEM, "foo.example.com", pool)
	if result != nil {
		t.Error("expected nil result on SAN mismatch")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status %d; want 400", status)
	}
	if errBody["reason"] != "san_mismatch" {
		t.Errorf("reason=%q; want san_mismatch (body=%v)", errBody["reason"], errBody)
	}
}

// TestCertValidatorRejectsChainInvalid verifies that a self-signed cert with no
// trusted root is rejected with reason=chain_invalid.
func TestCertValidatorRejectsChainInvalid(t *testing.T) {
	// Build a standalone self-signed cert with matching SAN.
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(99),
		Subject:      pkix.Name{CommonName: "foo.example.com"},
		DNSNames:     []string{"foo.example.com"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	// Pass a pool that does NOT contain the self-signed cert as a trust anchor
	// (empty pool → chain_invalid).
	emptyPool := x509.NewCertPool()
	result, status, errBody := validateCertAndKey(certPEM, keyPEM, "foo.example.com", emptyPool)
	if result != nil {
		t.Error("expected nil result on chain_invalid")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status %d; want 400", status)
	}
	if errBody["reason"] != "chain_invalid" {
		t.Errorf("reason=%q; want chain_invalid (body=%v)", errBody["reason"], errBody)
	}
}

// TestCertValidatorRejectsKeyMismatch verifies that a cert + mismatched key
// pair is rejected with reason=key_mismatch.
func TestCertValidatorRejectsKeyMismatch(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)
	certPEM, _ := genCert(t, ca, caKey, []string{"foo.example.com"})

	// Generate a different key (not matching the cert).
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keyDER, _ := x509.MarshalECPrivateKey(otherKey)
	wrongKeyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	result, status, errBody := validateCertAndKey(certPEM, wrongKeyPEM, "foo.example.com", pool)
	if result != nil {
		t.Error("expected nil result on key_mismatch")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status %d; want 400", status)
	}
	if errBody["reason"] != "key_mismatch" {
		t.Errorf("reason=%q; want key_mismatch (body=%v)", errBody["reason"], errBody)
	}
}

// TestCertValidatorRejectsExpired verifies that an already-expired cert is
// rejected with reason=expired.
func TestCertValidatorRejectsExpired(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)
	certPEM, keyPEM := genExpiredCert(t, ca, caKey, []string{"foo.example.com"})

	result, status, errBody := validateCertAndKey(certPEM, keyPEM, "foo.example.com", pool)
	if result != nil {
		t.Error("expected nil result on expired cert")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status %d; want 400", status)
	}
	if errBody["reason"] != "expired" {
		t.Errorf("reason=%q; want expired (body=%v)", errBody["reason"], errBody)
	}
}

// TestCertValidatorAcceptsValidChain verifies that a valid CA-signed cert is
// accepted.
func TestCertValidatorAcceptsValidChain(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"foo.example.com"})

	result, status, errBody := validateCertAndKey(certPEM, keyPEM, "foo.example.com", pool)
	if errBody != nil {
		t.Fatalf("expected no error, got status=%d body=%v", status, errBody)
	}
	if result == nil {
		t.Fatal("expected non-nil result on valid cert")
	}
	if result.certSHA256 == "" {
		t.Error("certSHA256 is empty")
	}
	if result.notAfter.IsZero() {
		t.Error("notAfter is zero")
	}
}

// TestCertValidatorWildcardSAN verifies that a wildcard cert covers a subdomain.
func TestCertValidatorWildcardSAN(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"*.example.com"})

	result, _, errBody := validateCertAndKey(certPEM, keyPEM, "foo.example.com", pool)
	if errBody != nil {
		t.Fatalf("expected no error for wildcard SAN, got %v", errBody)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

// ─── Webhook stub ────────────────────────────────────────────────────────────

// stubDispatcher records Publish calls for assertion in tests.
type stubDispatcher struct {
	mu     sync.Mutex
	events []string
}

func (s *stubDispatcher) Publish(_ context.Context, event string, _ any) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (s *stubDispatcher) DeliverNow(_ context.Context, _, _ string, _ any) (int, int, error) {
	return 0, 0, nil
}

func (s *stubDispatcher) published() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.events))
	copy(out, s.events)
	return out
}

// ─── In-memory stub for handler tests ─────────────────────────────────────────

type stubCustomDomainStore struct {
	rows        map[string]db.ServiceCustomDomain // keyed by id
	insertErr   error
	updateErr   error
	getErr      error
	listErr     error
	deleteErr   error
	lastInsert  db.ServiceCustomDomain
	lastUpdate  db.ServiceCustomDomain
	lastDeleted string
}

func newStubDomainStore() *stubCustomDomainStore {
	return &stubCustomDomainStore{rows: make(map[string]db.ServiceCustomDomain)}
}

func (s *stubCustomDomainStore) InsertCustomDomain(_ context.Context, d db.ServiceCustomDomain) (db.ServiceCustomDomain, error) {
	if s.insertErr != nil {
		return db.ServiceCustomDomain{}, s.insertErr
	}
	d.ID = "did-1"
	d.CreatedAt = time.Now()
	d.UpdatedAt = time.Now()
	s.rows[d.ID] = d
	s.lastInsert = d
	return d, nil
}
func (s *stubCustomDomainStore) UpdateCustomDomain(_ context.Context, d db.ServiceCustomDomain) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.rows[d.ID] = d
	s.lastUpdate = d
	return nil
}
func (s *stubCustomDomainStore) GetCustomDomain(_ context.Context, _, id string) (db.ServiceCustomDomain, error) {
	if s.getErr != nil {
		return db.ServiceCustomDomain{}, s.getErr
	}
	row, ok := s.rows[id]
	if !ok {
		return db.ServiceCustomDomain{}, db.ErrNotFound
	}
	return row, nil
}
func (s *stubCustomDomainStore) ListCustomDomains(_ context.Context, _ string) ([]db.ServiceCustomDomain, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []db.ServiceCustomDomain
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}
func (s *stubCustomDomainStore) DeleteCustomDomain(_ context.Context, _, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.lastDeleted = id
	delete(s.rows, id)
	return nil
}
func (s *stubCustomDomainStore) ListAllCustomDomains(_ context.Context) ([]db.ServiceCustomDomain, error) {
	return nil, nil
}

// ─── Handler tests ────────────────────────────────────────────────────────────

// newDomainDeps returns a Deps pre-wired for custom domain handler tests.
// Pass a non-nil roots to trust a custom CA (for tests that need the handler
// path to succeed with a test-generated certificate chain).
func newDomainDeps(ds *stubCustomDomainStore) Deps {
	return newDomainDepsWithRoots(ds, nil)
}

func newDomainDepsWithRoots(ds *stubCustomDomainStore, roots *x509.CertPool) Deps {
	return Deps{
		Users:               &fakeUserStore{role: "admin"},
		Log:                 discardLog(),
		IPGeoServices:       &stubSvcLookup{ownerID: "u-self"},
		CustomDomains:       ds,
		CertValidationRoots: roots,
	}
}

// TestListCustomDomains_Empty verifies that an empty list returns 200 [].
func TestListCustomDomains_Empty(t *testing.T) {
	d := newDomainDeps(newStubDomainStore())
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.get(t, "/api/v1/services/svc1/domains")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d; want 200", resp.StatusCode)
	}
	var out []any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty array, got %v", out)
	}
}

// TestPostCustomDomain_SANMismatch verifies that posting a cert with SAN mismatch
// returns 400 with reason=san_mismatch via the HTTP API.
func TestPostCustomDomain_SANMismatch(t *testing.T) {
	ca, caKey, _, _ := genCA(t)
	// cert SAN = bar.example.com, hostname = foo.example.com
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"bar.example.com"})

	d := newDomainDeps(newStubDomainStore())
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	body := map[string]any{
		"hostname": "foo.example.com",
		"cert_pem": certPEM,
		"key_pem":  keyPEM,
	}
	resp := ac.post(t, "/api/v1/services/svc1/domains", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d; want 400", resp.StatusCode)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["reason"] != "san_mismatch" {
		t.Errorf("reason=%q; want san_mismatch (body=%v)", out["reason"], out)
	}
}

// TestPostCustomDomain_DuplicateHostname verifies that posting a cert for a
// hostname that is already registered returns 409 Conflict. The test injects
// the test CA into Deps.CertValidationRoots so the chain validation step
// trusts our test-generated cert, letting the request reach the store which
// is pre-loaded with ErrDuplicateHostname.
func TestPostCustomDomain_DuplicateHostname(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"foo.example.com"})

	ds := newStubDomainStore()
	ds.insertErr = db.ErrDuplicateHostname

	d := newDomainDepsWithRoots(ds, pool)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	body := map[string]any{
		"hostname": "foo.example.com",
		"cert_pem": certPEM,
		"key_pem":  keyPEM,
	}
	resp := ac.post(t, "/api/v1/services/svc1/domains", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d; want 409 (body=%s)", resp.StatusCode, readBody(t, resp))
	}
}

// TestPutCustomDomain_EmitsCertExpiringWebhook verifies that PUT emits a
// cert.expiring event when the renewed cert is within the 14-day expiry window.
// This covers the spec gap fixed in the review: POST emitted it but PUT did not.
func TestPutCustomDomain_EmitsCertExpiringWebhook(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)

	// Generate a cert that expires in 3 days (< 14 days → must trigger webhook).
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(77),
		Subject:      pkix.Name{CommonName: "foo.example.com"},
		DNSNames:     []string{"foo.example.com"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(3 * 24 * time.Hour), // expiring soon
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, ca, &priv.PublicKey, caKey)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	ds := newStubDomainStore()
	// Pre-seed so GET (old hostname fetch) + UPDATE both succeed.
	ds.rows["did-1"] = db.ServiceCustomDomain{
		ID: "did-1", ServiceID: "svc1", Hostname: "foo.example.com",
		NotAfter: time.Now().Add(30 * 24 * time.Hour),
	}

	disp := &stubDispatcher{}
	d := newDomainDepsWithRoots(ds, pool)
	d.WebhookDispatcher = disp

	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	body := map[string]any{
		"hostname": "foo.example.com",
		"cert_pem": certPEM,
		"key_pem":  keyPEM,
	}
	resp := ac.put(t, "/api/v1/services/svc1/domains/did-1", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d; want 204 (body=%s)", resp.StatusCode, readBody(t, resp))
	}

	events := disp.published()
	found := false
	for _, e := range events {
		if e == "cert.expiring" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("cert.expiring webhook not emitted on PUT with expiring cert; events=%v", events)
	}
}

// TestPutCustomDomain_NoWebhookWhenNotExpiring verifies that PUT does NOT emit
// cert.expiring when the renewed cert has plenty of time left.
func TestPutCustomDomain_NoWebhookWhenNotExpiring(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)
	// genCert gives NotAfter = now+24h which IS within 14 days. Use a cert valid for 30 days instead.
	var certPEM, keyPEM string
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(88),
		Subject:      pkix.Name{CommonName: "bar.example.com"},
		DNSNames:     []string{"bar.example.com"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour), // 30 days — NOT expiring soon
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, ca, &priv.PublicKey, caKey)
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	ds := newStubDomainStore()
	ds.rows["did-2"] = db.ServiceCustomDomain{
		ID: "did-2", ServiceID: "svc1", Hostname: "bar.example.com",
		NotAfter: time.Now().Add(30 * 24 * time.Hour),
	}

	disp := &stubDispatcher{}
	d := newDomainDepsWithRoots(ds, pool)
	d.WebhookDispatcher = disp

	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	body := map[string]any{
		"hostname": "bar.example.com",
		"cert_pem": certPEM,
		"key_pem":  keyPEM,
	}
	resp := ac.put(t, "/api/v1/services/svc1/domains/did-2", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d; want 204 (body=%s)", resp.StatusCode, readBody(t, resp))
	}

	events := disp.published()
	for _, e := range events {
		if e == "cert.expiring" {
			t.Errorf("cert.expiring should NOT be emitted for a 30-day cert; events=%v", events)
		}
	}
}

// TestGetCustomDomain_NotFound verifies that getting an unknown domain returns 404.
func TestGetCustomDomain_NotFound(t *testing.T) {
	d := newDomainDeps(newStubDomainStore())
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.get(t, "/api/v1/services/svc1/domains/nonexistent")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d; want 404", resp.StatusCode)
	}
}

// TestDeleteCustomDomain_204 verifies that deleting a domain returns 204.
func TestDeleteCustomDomain_204(t *testing.T) {
	ds := newStubDomainStore()
	ds.rows["did-1"] = db.ServiceCustomDomain{
		ID: "did-1", ServiceID: "svc1", Hostname: "foo.example.com",
		NotAfter: time.Now().Add(time.Hour),
	}
	d := newDomainDeps(ds)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.delete(t, "/api/v1/services/svc1/domains/did-1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d; want 204", resp.StatusCode)
	}
	if _, ok := ds.rows["did-1"]; ok {
		t.Error("expected domain to be deleted from store")
	}
}

// TestDomainRespNoKeyPEM verifies that the domainResp struct does not contain key_pem.
// This is a compile-time + reflection guard.
func TestDomainRespNoKeyPEM(t *testing.T) {
	ds := newStubDomainStore()
	ds.rows["did-1"] = db.ServiceCustomDomain{
		ID: "did-1", ServiceID: "svc1", Hostname: "foo.example.com",
		CertSHA256: "abc123",
		NotBefore:  time.Now().Add(-time.Hour),
		NotAfter:   time.Now().Add(time.Hour),
		CertPEM:    "should-not-appear",
		KeyPEM:     "must-not-appear",
	}
	d := newDomainDeps(ds)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.get(t, "/api/v1/services/svc1/domains/did-1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	// Decode to raw map and check for absence of key_pem.
	var raw map[string]any
	body := readBodyJSON(t, resp)
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["key_pem"]; ok {
		t.Error("key_pem MUST NOT appear in the GET domain response")
	}
	if _, ok := raw["cert_pem"]; ok {
		t.Error("cert_pem MUST NOT appear in the GET domain response")
	}
}

// TestComputeStatus moved to internal/proxy/customdomain (v0.5.2 Task 10
// follow-up): the local `computeStatus` helper was removed in favour of
// `customdomain.ComputeStatus`, which is covered there with a stronger,
// frozen-clock test matrix (60d / 14d edge / 1d / 0d exact / past).

// TestMatchesSAN verifies wildcard and exact SAN matching.
func TestMatchesSAN(t *testing.T) {
	cases := []struct {
		san      string
		hostname string
		want     bool
	}{
		{"foo.example.com", "foo.example.com", true},
		{"*.example.com", "foo.example.com", true},
		{"*.example.com", "bar.example.com", true},
		{"*.example.com", "example.com", false},     // no label before .
		{"*.example.com", "a.b.example.com", false}, // two labels
		{"bar.example.com", "foo.example.com", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s", tc.san, tc.hostname), func(t *testing.T) {
			got := matchesSAN(tc.san, tc.hostname)
			if got != tc.want {
				t.Errorf("matchesSAN(%q, %q) = %v; want %v", tc.san, tc.hostname, got, tc.want)
			}
		})
	}
}

// TestPublicKeysMatch verifies the public key equality check.
func TestPublicKeysMatch(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if !publicKeysMatch(&priv.PublicKey, priv) {
		t.Error("matching key should return true")
	}
	if publicKeysMatch(&priv.PublicKey, other) {
		t.Error("mismatched key should return false")
	}
}

// readBodyJSON reads the full body as a string (for JSON assertions).
func readBodyJSON(t *testing.T, r *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return sb.String()
}

// genBareCNCert generates a leaf cert with NO SANs and CommonName=hostname,
// signed by the given CA. Used to test the bare-CN fallback path (spec D.3).
func genBareCNCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, hostname string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genBareCNCert: gen key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: hostname},
		// Intentionally empty DNSNames and IPAddresses → no SANs.
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &priv.PublicKey, caKey)
	if err != nil {
		t.Fatalf("genBareCNCert: create cert: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("genBareCNCert: marshal key: %v", err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// TestPostCustomDomain_BareCNWarns verifies spec D.3 step 2: when the cert has
// no SANs, the hostname is matched via CommonName, the request is accepted
// (201), and the response includes Burrow-Warning: cert-no-san.
func TestPostCustomDomain_BareCNWarns(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)
	certPEM, keyPEM := genBareCNCert(t, ca, caKey, "foo.example.com")

	ds := newStubDomainStore()
	d := newDomainDepsWithRoots(ds, pool)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	body := map[string]any{
		"hostname": "foo.example.com",
		"cert_pem": certPEM,
		"key_pem":  keyPEM,
	}
	resp := ac.post(t, "/api/v1/services/svc1/domains", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d; want 201 (body=%s)", resp.StatusCode, readBody(t, resp))
	}
	warn := resp.Header.Get("Burrow-Warning")
	if warn != "cert-no-san" {
		t.Errorf("Burrow-Warning=%q; want cert-no-san", warn)
	}
}

// TestProxyRoutesByCustomDomainHost is exercised in internal/proxy/proxy_test.go
// (TestProxyCustomDomainRouting). This is a compile-time marker only.
func TestProxyRoutesByCustomDomainHost(t *testing.T) {
	t.Log("custom domain vhost routing is covered by TestProxyCustomDomainRouting in internal/proxy/proxy_test.go")
}

// ─── TLS key-pair quick sanity ────────────────────────────────────────────────

// TestTLSKeyPairParseable verifies that genCert+genCA produce a key pair that
// tls.X509KeyPair can parse.
func TestTLSKeyPairParseable(t *testing.T) {
	ca, caKey, _, _ := genCA(t)
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"foo.example.com"})
	_, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		t.Fatalf("tls.X509KeyPair: %v", err)
	}
}
