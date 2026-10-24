package relaynames

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The relay's own names: exact names (case and a trailing dot aside), a
// wildcard over exactly one label, every name under a domain given as
// under, localhost and addresses. A nil set owns those last two only.
func TestOwns(t *testing.T) {
	s := New([]string{"Relay.Example.com."}, "dash.example.com", "*.Wild.example.com", "", "apps.example.com.")
	for name, want := range map[string]bool{
		"dash.example.com":         true,
		"DASH.example.com.":        true,
		"x.dash.example.com":       false,
		"example.com":              false,
		"a.wild.example.com":       true,
		"wild.example.com":         false,
		"a.b.wild.example.com":     false,
		"relay.example.com":        true,
		"deep.x.relay.example.com": true,
		"xrelay.example.com":       false,
		"apps.example.com":         true,
		"x.apps.example.com":       false,
		"localhost":                true,
		"127.0.0.1":                true,
		"::1":                      true,
		"rdp.example.com":          false,
		"":                         false,
	} {
		if got := s.Owns(name); got != want {
			t.Errorf("Owns(%q) = %v, want %v", name, got, want)
		}
	}
	var none *Set
	if none.Owns("dash.example.com") || !none.Owns("localhost") {
		t.Fatal("nil set")
	}
}

// CertDNSNames reads the DNS names of the first certificate of a PEM file
// (the leaf); a file that holds none is an error.
func TestCertDNSNames(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der := func(names ...string) []byte {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"},
			NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), DNSNames: names}
		b, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "chain.pem")
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der("dash.example.com", "*.wild.example.com")}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der("ca.example.com")})...)
	if err := os.WriteFile(p, chain, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := CertDNSNames(p)
	if err != nil || !slices.Equal(got, []string{"dash.example.com", "*.wild.example.com"}) {
		t.Fatalf("%v %v", got, err)
	}
	bad := filepath.Join(dir, "bad.pem")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)
	if _, err := CertDNSNames(bad); err == nil {
		t.Fatal("no certificate accepted")
	}
	if _, err := CertDNSNames(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("a missing file accepted")
	}
}
