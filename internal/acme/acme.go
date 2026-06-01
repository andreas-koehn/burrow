// Package acme wraps caddyserver/certmagic to give burrowd built-in,
// auto-renewing Let's Encrypt certificates without any external proxy.
package acme

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/mholt/acmez/v3"
)

// Config is the narrow set of inputs the Manager needs.
type Config struct {
	Domains []string // hostnames to obtain certs for (first is the primary)
	Email   string   // ACME account email
	CA      string   // ACME directory URL
	Storage string   // filesystem path for cert/account persistence
	Log     *slog.Logger
}

// Manager owns a certmagic.Config and serves certificates for the configured
// domains across every listener.
type Manager struct {
	magic *certmagic.Config
	acme  *certmagic.ACMEIssuer
	log   *slog.Logger
}

// New builds a Manager and synchronously obtains/loads certificates for all
// configured domains. It blocks until certs are ready (ManageSync) so every
// listener has a cert before it starts serving. Returns an error the caller
// should treat as fatal (misconfigured domain / unreachable :80).
func New(ctx context.Context, c Config) (*Manager, error) {
	if len(c.Domains) == 0 {
		return nil, fmt.Errorf("acme: at least one domain is required")
	}
	log := c.Log
	if log == nil {
		log = slog.Default()
	}

	// Configure the process-global CertMagic defaults (burrowd uses a single
	// ACME configuration), then build a Config from them. This is CertMagic's
	// documented simple-usage pattern and wires an ACMEIssuer into Issuers[0].
	certmagic.Default.Storage = &certmagic.FileStorage{Path: c.Storage}
	certmagic.DefaultACME.Agreed = true
	certmagic.DefaultACME.Email = c.Email
	if c.CA != "" {
		certmagic.DefaultACME.CA = c.CA
	}

	magic := certmagic.NewDefault()

	if err := magic.ManageSync(ctx, c.Domains); err != nil {
		return nil, fmt.Errorf("acme: obtain certificates for %s: %w", strings.Join(c.Domains, ","), err)
	}

	var issuer *certmagic.ACMEIssuer
	if len(magic.Issuers) > 0 {
		issuer, _ = magic.Issuers[0].(*certmagic.ACMEIssuer)
	}
	if issuer == nil {
		return nil, fmt.Errorf("acme: no ACME issuer configured (cannot serve HTTP-01 challenges)")
	}

	log.Info("acme: certificates ready", "domains", c.Domains, "ca", certmagic.DefaultACME.CA)
	return &Manager{magic: magic, acme: issuer, log: log}, nil
}

// GetCertificate is a tls.Config.GetCertificate function backed by CertMagic.
// Use it directly on listeners (e.g. the control channel) where the ACME
// TLS-ALPN protocol must NOT be advertised.
func (m *Manager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return m.magic.GetCertificate(hello)
}

// TLSConfig returns a *tls.Config wired with GetCertificate and the TLS-ALPN
// challenge protocol, with h2/http1.1 prepended. Use for the HTTPS listeners
// (dashboard, proxy ingress) that should also solve TLS-ALPN-01 challenges.
func (m *Manager) TLSConfig() *tls.Config {
	cfg := m.magic.TLSConfig()
	cfg.MinVersion = tls.VersionTLS12
	cfg.NextProtos = append([]string{"h2", "http/1.1"}, cfg.NextProtos...)
	return cfg
}

// HTTPChallengeHandler wraps an http.Handler so the HTTP-01 challenge is solved
// on :80. Mount on the :80 listener ahead of an HTTPS redirect handler.
func (m *Manager) HTTPChallengeHandler(next http.Handler) http.Handler {
	return m.acme.HTTPChallengeHandler(next)
}

// ACMETLSProto is the ALPN protocol name for TLS-ALPN-01, exported so callers
// composing their own tls.Config (e.g. the proxy listener) can append it.
const ACMETLSProto = acmez.ACMETLS1Protocol
