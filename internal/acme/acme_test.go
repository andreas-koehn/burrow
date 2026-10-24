package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"math/big"
	"testing"
	"time"
)

func TestNew_RequiresDomain(t *testing.T) {
	if _, err := New(context.Background(), Config{Email: "a@b.c", CA: "x", Storage: t.TempDir()}); err == nil {
		t.Fatal("expected error with no domains")
	}
}

func TestACMETLSProto_NonEmpty(t *testing.T) {
	if ACMETLSProto == "" {
		t.Fatal("ACMETLSProto must be the acme-tls/1 protocol string")
	}
}

// selfSigned is a certificate for name valid from an hour ago for a day.
func selfSigned(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// With a CA that cannot be reached, ManageAsync returns at once and the
// failure arrives as a cert_failed event: a broken ACME never holds up the
// relay, and the caller can log and audit it.
func TestManager_ManageAsyncWithAFailingCA(t *testing.T) {
	m, err := newManager(Config{Email: "a@b.c", CA: "http://127.0.0.1:1/directory", Storage: t.TempDir()}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	failed := make(chan string, 8)
	m.OnCertEvent(func(event, name string, err error) {
		if event == "cert_failed" && err != nil {
			failed <- name
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	if err := m.ManageAsync(ctx, []string{"auto.example.com"}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("ManageAsync took %v", d)
	}
	select {
	case name := <-failed:
		if name != "auto.example.com" {
			t.Fatalf("failed %q", name)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no cert_failed event")
	}
	if m.HasCertificate("auto.example.com") {
		t.Fatal("a certificate without a CA")
	}
}

func TestManager_HasCertificateAndUnmanage(t *testing.T) {
	m, err := newManager(Config{Email: "a@b.c", CA: "http://127.0.0.1:1/directory", Storage: t.TempDir()}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.magic.CacheUnmanagedTLSCertificate(context.Background(), selfSigned(t, "up.example.com"), nil); err != nil {
		t.Fatal(err)
	}
	if !m.HasCertificate("up.example.com") || m.HasCertificate("other.example.com") {
		t.Fatal("HasCertificate")
	}
	// Unmanage removes managed certificates only: an unmanaged one stays.
	m.Unmanage([]string{"up.example.com"})
	if !m.HasCertificate("up.example.com") {
		t.Fatal("Unmanage removed an unmanaged certificate")
	}
}
