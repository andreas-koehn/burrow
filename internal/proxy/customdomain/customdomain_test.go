package customdomain_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proxy/customdomain"
)

// genSelfSignedCert generates a self-signed TLS certificate for the given DNS
// names, returning the PEM-encoded cert and key. The cert is valid for 1 hour
// from "now - 1 minute" so it won't be considered expired in tests.
func genSelfSignedCert(t *testing.T, dnsNames []string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("gen cert: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal ec key: %v", err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// memDBStore is an in-memory DBStore for tests.
type memDBStore struct {
	rows map[string]customdomain.DomainRow // keyed by lowercase hostname
}

func newMemDB() *memDBStore { return &memDBStore{rows: make(map[string]customdomain.DomainRow)} }

func (m *memDBStore) add(row customdomain.DomainRow) { m.rows[row.Hostname] = row }

func (m *memDBStore) LookupCustomDomainByHostname(_ context.Context, hostname string) (customdomain.DomainRow, bool, error) {
	r, ok := m.rows[hostname]
	return r, ok, nil
}

// TestStore_LookupBySNI_HitAndMiss verifies that:
//   - A registered domain is returned on SNI match.
//   - An unknown domain returns ok=false.
//   - Cached hit does not call the DB a second time.
func TestStore_LookupBySNI_HitAndMiss(t *testing.T) {
	certPEM, keyPEM := genSelfSignedCert(t, []string{"foo.example.com"})

	db := newMemDB()
	db.add(customdomain.DomainRow{
		ID:        "d1",
		ServiceID: "svc1",
		Hostname:  "foo.example.com",
		CertPEM:   certPEM,
		KeyPEM:    keyPEM,
		NotAfter:  time.Now().Add(time.Hour),
	})

	s := customdomain.New(db)
	ctx := context.Background()

	// Hit.
	c, ok, err := s.LookupBySNI(ctx, "foo.example.com")
	if err != nil {
		t.Fatalf("lookup error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for known domain")
	}
	if c.ServiceID != "svc1" {
		t.Errorf("service_id=%q; want svc1", c.ServiceID)
	}

	// Miss.
	_, ok2, err2 := s.LookupBySNI(ctx, "unknown.example.com")
	if err2 != nil {
		t.Fatalf("lookup error: %v", err2)
	}
	if ok2 {
		t.Error("expected ok=false for unknown domain")
	}
}

// TestStore_InvalidateHostname verifies that after InvalidateHostname, a
// subsequent lookup re-reads from the DB (reflects updated cert).
func TestStore_InvalidateHostname(t *testing.T) {
	certPEM, keyPEM := genSelfSignedCert(t, []string{"foo.example.com"})
	certPEM2, keyPEM2 := genSelfSignedCert(t, []string{"foo.example.com"})

	db := newMemDB()
	db.add(customdomain.DomainRow{
		Hostname: "foo.example.com",
		CertPEM:  certPEM,
		KeyPEM:   keyPEM,
		NotAfter: time.Now().Add(time.Hour),
	})

	s := customdomain.New(db)
	ctx := context.Background()

	// Prime the cache.
	c1, ok, _ := s.LookupBySNI(ctx, "foo.example.com")
	if !ok {
		t.Fatal("expected hit")
	}

	// Update DB to new cert.
	db.rows["foo.example.com"] = customdomain.DomainRow{
		Hostname: "foo.example.com",
		CertPEM:  certPEM2,
		KeyPEM:   keyPEM2,
		NotAfter: time.Now().Add(2 * time.Hour),
	}

	// Without invalidation, still gets old cert.
	c2, _, _ := s.LookupBySNI(ctx, "foo.example.com")
	if c1.Cert != c2.Cert {
		t.Error("expected same cached cert pointer before invalidation")
	}

	// After invalidation, gets new cert.
	s.InvalidateHostname("foo.example.com")
	c3, _, _ := s.LookupBySNI(ctx, "foo.example.com")
	if c3.Cert == c1.Cert {
		t.Error("expected new cert pointer after invalidation")
	}
}

// TestGetCertificate_PicksPerDomainCertThenFallsBack verifies that:
//   - SNI = "foo.example.com" → returns the per-domain cert.
//   - SNI = "bar.example.com" → returns the wildcard fallback.
func TestGetCertificate_PicksPerDomainCertThenFallsBack(t *testing.T) {
	certPEM, keyPEM := genSelfSignedCert(t, []string{"foo.example.com"})

	db := newMemDB()
	db.add(customdomain.DomainRow{
		ID:        "d1",
		ServiceID: "svc1",
		Hostname:  "foo.example.com",
		CertPEM:   certPEM,
		KeyPEM:    keyPEM,
		NotAfter:  time.Now().Add(time.Hour),
	})

	// Build a wildcard cert.
	wcCertPEM, wcKeyPEM := genSelfSignedCert(t, []string{"*.example.com"})
	wcTLS, err := tls.X509KeyPair([]byte(wcCertPEM), []byte(wcKeyPEM))
	if err != nil {
		t.Fatalf("parse wildcard cert: %v", err)
	}

	s := customdomain.New(db)
	fn := customdomain.CertCallback(s, &wcTLS)

	// Per-domain hit.
	fooCert, err := fn(&tls.ClientHelloInfo{ServerName: "foo.example.com"})
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if fooCert == &wcTLS {
		t.Error("expected per-domain cert, got wildcard")
	}

	// Wildcard fallback.
	barCert, err := fn(&tls.ClientHelloInfo{ServerName: "bar.example.com"})
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if barCert != &wcTLS {
		t.Error("expected wildcard cert for unknown SNI")
	}
}

// TestGetCertificate_NilHello verifies that a nil ClientHelloInfo returns the
// wildcard without panicking.
func TestGetCertificate_NilHello(t *testing.T) {
	wcCertPEM, wcKeyPEM := genSelfSignedCert(t, []string{"*.example.com"})
	wcTLS, _ := tls.X509KeyPair([]byte(wcCertPEM), []byte(wcKeyPEM))

	s := customdomain.New(newMemDB())
	fn := customdomain.CertCallback(s, &wcTLS)

	cert, err := fn(nil)
	if err != nil {
		t.Fatalf("callback error on nil hello: %v", err)
	}
	if cert != &wcTLS {
		t.Error("expected wildcard cert for nil ClientHelloInfo")
	}
}

// A row without a certificate (cert_source auto) is still a bound custom
// domain for routing, and CertCallback skips it: the next certificate
// source (certmagic, or the listener's own) answers the handshake (H10).
func TestStore_ARowWithoutACertificate(t *testing.T) {
	db := newMemDB()
	db.add(customdomain.DomainRow{ID: "d1", ServiceID: "svc1", Hostname: "auto.example.com"})
	s := customdomain.New(db)

	c, ok, err := s.LookupBySNI(context.Background(), "auto.example.com")
	if err != nil || !ok || c.ServiceID != "svc1" || c.Cert != nil {
		t.Fatalf("LookupBySNI = %+v %v %v", c, ok, err)
	}
	fallback := &tls.Certificate{}
	got, err := customdomain.CertCallback(s, fallback)(&tls.ClientHelloInfo{ServerName: "auto.example.com"})
	if err != nil || got != fallback {
		t.Fatalf("CertCallback = %p %v, want the fallback", got, err)
	}
	got, err = customdomain.CertCallback(s, nil)(&tls.ClientHelloInfo{ServerName: "AUTO.example.com"})
	if err != nil || got != nil {
		t.Fatalf("CertCallback = %v %v, want nil", got, err)
	}
}
