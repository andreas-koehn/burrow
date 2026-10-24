package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"slices"
	"time"

	"github.com/ankoehn/burrow/internal/acme"
	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// autoDomainStagger is the pause between two auto custom domains handed to
// certmagic at startup (H10): a stored certificate loads without the CA, a
// missing one is ordered, and a relay with many names does not send them
// in one burst. certmagic adds its own rate limiter and retries a failed
// order with exponential backoff, so a broken name does not hammer the CA.
const autoDomainStagger = 2 * time.Second

// customDomainLister lists every custom domain (*db.DB).
type customDomainLister interface {
	ListAllCustomDomains(ctx context.Context) ([]db.ServiceCustomDomain, error)
}

// autoCustomDomains returns the custom domains with cert_source auto.
func autoCustomDomains(ctx context.Context, l customDomainLister) ([]acme.CustomName, error) {
	rows, err := l.ListAllCustomDomains(ctx)
	if err != nil {
		return nil, err
	}
	var out []acme.CustomName
	for _, r := range rows {
		if r.CertSource == db.CertSourceAuto {
			out = append(out, acme.CustomName{ServiceID: r.ServiceID, Hostname: r.Hostname})
		}
	}
	return out, nil
}

// auditCertFailed audits a failure of the ACME side for a custom domain
// (service.custom_domain.cert_failed, one row per host name per hour).
func auditCertFailed(app api.AuditAppender) func(serviceID, hostname, reason string) {
	return func(serviceID, hostname, reason string) {
		if app == nil {
			return
		}
		_ = app.Append(context.Background(), audit.Event{
			Action:    audit.ActionServiceCustomDomainCertFailed,
			SubjectID: serviceID, SubjectLabel: hostname,
			Result:         "error",
			AggregationKey: hostname,
			Payload:        audit.MustJSON(map[string]string{"hostname": hostname, "error": reason}),
		})
	}
}

// certIssuer is *acme.Manager as startCustomDomainCerts uses it.
type certIssuer interface {
	acme.Issuer
	OnCertEvent(fn func(event, name string, err error))
}

// startCustomDomainCerts builds the ACME side of the auto custom domains
// under ACME (H10): certmagic's events reach it, every order passes the
// H12 refusals of the API (allow), failures are logged and audited, and
// the auto names that exist are loaded in the background. It never waits
// on the CA; a failed listing is logged and the relay starts all the same.
func startCustomDomainCerts(ctx context.Context, m certIssuer, l customDomainLister, allow func(string) string, app api.AuditAppender, log *slog.Logger) *acme.CustomDomains {
	cd := acme.NewCustomDomains(ctx, m, acme.CustomOptions{
		Allow: allow, OnFailure: auditCertFailed(app), Stagger: autoDomainStagger, Log: log,
	})
	m.OnCertEvent(cd.CertEvent)
	names, err := autoCustomDomains(ctx, l)
	if err != nil {
		log.Warn("acme: listing the auto custom domains failed; their certificates are ordered when they are next written", "err", err)
		return cd
	}
	cd.Load(names)
	return cd
}

// warnAutoWithoutACME tells the operator of auto custom domains no
// certificate is served for: the dashboard serves a file certificate and
// ACME is off.
func warnAutoWithoutACME(ctx context.Context, l customDomainLister, log *slog.Logger) {
	names, err := autoCustomDomains(ctx, l)
	if err != nil {
		return
	}
	for _, n := range names {
		log.Warn("custom domain has cert_source auto, but the relay serves a file certificate without ACME: "+
			"no certificate is served for it; upload one or set BURROW_ACME_DOMAIN", "hostname", n.Hostname, "service_id", n.ServiceID)
	}
}

// customDomainGetCertificate puts the uploaded certificates of custom
// domains (custom, customdomain.CertCallback) in front of next, the
// listener's own source (certmagic, or nil for the listener's file
// certificate). A TLS-ALPN-01 challenge and a name the relay owns go to
// next without a lookup; a custom domain without a certificate (auto) falls
// through to next, which holds certmagic's certificate for it.
func customDomainGetCertificate(owns func(string) bool, custom, next func(*tls.ClientHelloInfo) (*tls.Certificate, error)) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello != nil && hello.ServerName != "" && !owns(hello.ServerName) &&
			!slices.Contains(hello.SupportedProtos, acme.ACMETLSProto) {
			if c, err := custom(hello); err == nil && c != nil {
				return c, nil
			}
		}
		if next == nil {
			return nil, nil
		}
		return next(hello)
	}
}

// dispatchCustomDomains reports whether the dashboard listener may hand a
// request to the proxy by its Host (H10). It needs a name of the relay's
// own to keep: the dashboard's host (auth domain, or the first ACME name)
// or the names of its file certificate. Without any (a plain listener and
// no BURROW_AUTH_DOMAIN), a custom domain could be the dashboard's own
// host, which the relay cannot tell, so custom domains are then served on
// the host-routed ingress only.
func dispatchCustomDomains(authDomain string, fileTLS, acmeOn bool) bool {
	return authDomain != "" || fileTLS || acmeOn
}
