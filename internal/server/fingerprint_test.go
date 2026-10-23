package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

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
