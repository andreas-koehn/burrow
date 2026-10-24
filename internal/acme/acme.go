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
	"sync/atomic"

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
	cache *certmagic.Cache
	acme  *certmagic.ACMEIssuer
	log   *slog.Logger
	// onEvent is the hook OnCertEvent sets (certmagic events of every
	// managed name, renewals included).
	onEvent atomic.Pointer[func(event, name string, err error)]
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

	m, err := newManager(c, log)
	if err != nil {
		return nil, err
	}
	if err := m.magic.ManageSync(ctx, c.Domains); err != nil {
		return nil, fmt.Errorf("acme: obtain certificates for %s: %w", strings.Join(c.Domains, ","), err)
	}
	log.Info("acme: certificates ready", "domains", c.Domains, "ca", certmagic.DefaultACME.CA)
	return m, nil
}

// newManager builds the certmagic Config and its cache, and obtains
// nothing.
func newManager(c Config, log *slog.Logger) (*Manager, error) {
	// Configure the process-global CertMagic defaults (burrowd uses a single
	// ACME configuration). The Config gets a cache of its own, so that a
	// custom domain can be unmanaged (Cache.RemoveManaged), and an event
	// hook: renewals use the same Config (GetConfigForCert).
	certmagic.Default.Storage = &certmagic.FileStorage{Path: c.Storage}
	certmagic.DefaultACME.Agreed = true
	certmagic.DefaultACME.Email = c.Email
	if c.CA != "" {
		certmagic.DefaultACME.CA = c.CA
	}

	m := &Manager{log: log}
	m.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return m.magic, nil },
		Logger:           certmagic.Default.Logger,
	})
	m.magic = certmagic.New(m.cache, certmagic.Config{OnEvent: m.event})

	var issuer *certmagic.ACMEIssuer
	if len(m.magic.Issuers) > 0 {
		issuer, _ = m.magic.Issuers[0].(*certmagic.ACMEIssuer)
	}
	if issuer == nil {
		return nil, fmt.Errorf("acme: no ACME issuer configured (cannot serve HTTP-01 challenges)")
	}
	m.acme = issuer
	return m, nil
}

// event passes certmagic's events on to the OnCertEvent hook. It never
// cancels an event.
func (m *Manager) event(_ context.Context, event string, data map[string]any) error {
	fn := m.onEvent.Load()
	if fn == nil {
		return nil
	}
	name, _ := data["identifier"].(string)
	err, _ := data["error"].(error)
	(*fn)(event, name, err)
	return nil
}

// OnCertEvent sets the hook told of certmagic's events (cert_obtaining,
// cert_obtained, cert_failed, …) with the name and, for a failure, the
// error. It must return quickly.
func (m *Manager) OnCertEvent(fn func(event, name string, err error)) {
	m.onEvent.Store(&fn)
}

// ManageAsync starts obtaining and renewing certificates for names without
// waiting for the CA; failures are retried by certmagic with exponential
// backoff and reported as cert_failed events. Cancelling ctx stops the
// retries.
func (m *Manager) ManageAsync(ctx context.Context, names []string) error {
	return m.magic.ManageAsync(ctx, names)
}

// Unmanage stops the renewal of the names' certificates and drops them
// from the cache. The stored files stay.
func (m *Manager) Unmanage(names []string) {
	subjects := make([]certmagic.SubjectIssuer, 0, len(names))
	for _, n := range names {
		subjects = append(subjects, certmagic.SubjectIssuer{Subject: n})
	}
	m.cache.RemoveManaged(subjects)
}

// HasCertificate reports whether a certificate for name that has not
// expired is loaded.
func (m *Manager) HasCertificate(name string) bool {
	for _, c := range m.cache.AllMatchingCertificates(name) {
		if !c.Expired() {
			return true
		}
	}
	return false
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
