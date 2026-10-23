package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/caddyserver/certmagic"

	"github.com/ankoehn/burrow/internal/devcert"
)

func TestControlCertSHA256_FileCertIsTheLeafOnTheWire(t *testing.T) {
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "dev-server.pem"), filepath.Join(dir, "dev-server-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	want := hex.EncodeToString(sum[:])

	s, err := New(Options{Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey: filepath.Join(dir, "dev-server-key.pem"), Auth: fpAuth()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ControlCertSHA256("relay.example.com")
	if err != nil || got != want {
		t.Fatalf("fingerprint %q (%v), want %q", got, err, want)
	}
}

func TestControlCertSHA256_FollowsGetCertificate(t *testing.T) {
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "dev-server.pem"), filepath.Join(dir, "dev-server-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	current := &cert
	var asked string
	s, err := New(Options{Listen: "127.0.0.1:0", Auth: fpAuth(), GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		asked = h.ServerName
		return current, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ControlCertSHA256("relay.example.com")
	if err != nil || asked != "relay.example.com" {
		t.Fatalf("first %q (%v), asked for %q", first, err, asked)
	}
	// A renewed certificate is a new fingerprint: nothing is cached.
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	renewed, err := tls.LoadX509KeyPair(filepath.Join(dir, "dev-server.pem"), filepath.Join(dir, "dev-server-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	current = &renewed
	second, err := s.ControlCertSHA256("relay.example.com")
	if err != nil || second == first {
		t.Fatalf("after renewal %q (%v), want a new fingerprint (was %q)", second, err, first)
	}
}

func fpAuth() AuthFunc {
	return func(context.Context, string) (string, error) { return "", errors.New("unused") }
}

// A real certmagic config behind GetCertificate, as with built-in ACME. The
// discovery Host header is the caller's, so a name certmagic does not manage
// must neither panic (it reads hello.Conn) nor trigger anything: it is asked
// for a managed name instead, or fails with an error.
func TestControlCertSHA256_CertmagicForeignHostDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "dev-server.pem"), filepath.Join(dir, "dev-server-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	magic := certmagic.New(certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return &certmagic.Config{}, nil },
	}), certmagic.Config{Storage: &certmagic.FileStorage{Path: t.TempDir()}})
	if _, err := magic.CacheUnmanagedTLSCertificate(context.Background(), cert, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Listen: "127.0.0.1:0", Auth: fpAuth(), GetCertificate: magic.GetCertificate,
		CertNames: []string{"localhost"}})
	if err != nil {
		t.Fatal(err)
	}
	want := Fingerprint0(cert)
	for _, host := range []string{"localhost", "LOCALHOST", "attacker.example", "", "1.2.3.4"} {
		got, err := s.ControlCertSHA256(host)
		if err != nil || got != want {
			t.Errorf("host %q: %q (%v), want the managed certificate %q", host, got, err, want)
		}
	}
	// Without CertNames the foreign name still cannot panic: an error at worst.
	s2, _ := New(Options{Listen: "127.0.0.1:0", Auth: fpAuth(), GetCertificate: magic.GetCertificate})
	if _, err := s2.ControlCertSHA256("attacker.example"); err == nil {
		t.Log("foreign name answered without CertNames")
	}
}

func Fingerprint0(c tls.Certificate) string {
	sum := sha256.Sum256(c.Certificate[0])
	return hex.EncodeToString(sum[:])
}
