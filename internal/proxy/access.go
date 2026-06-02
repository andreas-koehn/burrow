package proxy

import (
	"context"
	"crypto/x509"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/ankoehn/burrow/internal/db"
)

// AccessMode is the string enum carried in Resolved.AccessMode. The four
// supported values are exported so other packages (the store layer, the API
// handlers, tests) can refer to the canonical names without copying strings.
type AccessMode = string

const (
	// AccessModeOpen — no per-request authentication; every request reaches
	// the upstream.
	AccessModeOpen AccessMode = "open"
	// AccessModeAPIKey — visitor must present a valid service API key in
	// the configured header (default Authorization: Bearer ...).
	AccessModeAPIKey AccessMode = "api_key"
	// AccessModeBurrowLogin — visitor is bounced to the Burrow gate to log
	// in; gate sets burrow_session cookie + role.
	AccessModeBurrowLogin AccessMode = "burrow_login"
	// AccessModeMTLS — visitor must present a TLS client certificate signed
	// by the per-service mTLS CA. Verification is performed during the TLS
	// handshake (proxy.GetConfigForClient sets ClientCAs +
	// RequireAndVerifyClientCert); the access checker re-confirms the cert
	// is present and may perform a defense-in-depth chain re-verify.
	AccessModeMTLS AccessMode = "mtls"
)

// APIKeyValidator is the narrow interface the accessChecker uses to validate
// API keys. *store.Store satisfies it implicitly (same method signature).
// Tests use a local fake instead of pulling in the full store.
type APIKeyValidator interface {
	ValidateAPIKey(ctx context.Context, serviceID, presented string) (bool, error)
}

// SessionValidator is the narrow interface the accessChecker uses to validate
// burrow_login visitor sessions on the proxy hot path. *store.Store satisfies
// it implicitly. When nil (e.g. unit-test constructions), the checker falls
// back to the v0.3.0-original behaviour of always 302-redirecting burrow_login
// requests to the gate — which on its own creates a gate↔service redirect
// loop for visitors with a valid session, so production wiring MUST pass a
// non-nil SessionValidator (cmd/server uses NewAccessCheckerWithSessionsAndLogger).
type SessionValidator interface {
	ValidateSession(ctx context.Context, sessionID string) (userID string, err error)
	GetUserByID(ctx context.Context, id string) (db.User, error)
	RoleAllowed(ctx context.Context, serviceID, role string) (bool, error)
}

// accessChecker implements AccessChecker for all three access modes:
// "open", "api_key", and "burrow_login".
//
// Design note — burrow_login redirect:
// The checker is constructed with authDomain so it can build the full gate
// URL (https://<authDomain>/__burrow/login?next=<escaped original URL>)
// and return status=302 + Location header. The Proxy's ServeHTTP already
// propagates hdr headers and writes the status code; no proxy.go change is
// required for redirect support.
type accessChecker struct {
	v  APIKeyValidator
	sv SessionValidator // optional; when non-nil, burrow_login passes
	// through visitors with a valid session cookie + allowed role (the
	// proxy-side validation the spec calls for; without this, the
	// gate→service→gate loop makes burrow_login non-functional).
	authDomain string
	log        *slog.Logger
}

// NewAccessChecker returns an AccessChecker that enforces the access policy
// described by Resolved.AccessMode on every request.
//
//   - v:          API key validator (e.g. *store.Store). Must not be nil for
//     api_key mode; ignored for open and burrow_login modes.
//   - authDomain: the Burrow auth/ingress domain (e.g. "tunnels.example.com").
//     Required for burrow_login redirects; may be empty only if no service
//     uses burrow_login mode (the store layer rejects that at write time).
func NewAccessChecker(v APIKeyValidator, authDomain string) AccessChecker {
	return &accessChecker{
		v:          v,
		authDomain: authDomain,
		log:        slog.Default(),
	}
}

// NewAccessCheckerWithLogger is like NewAccessChecker but uses a caller-supplied
// logger. Used internally when the Proxy wires itself.
func NewAccessCheckerWithLogger(v APIKeyValidator, authDomain string, log *slog.Logger) AccessChecker {
	return &accessChecker{
		v:          v,
		authDomain: authDomain,
		log:        log,
	}
}

// NewAccessCheckerWithSessionsAndLogger is the production constructor: it adds
// a SessionValidator so burrow_login requests can be passed through directly
// when the visitor already has a valid session cookie + allowed role. Without
// this, every burrow_login request 302s to the gate and the gate then 302s
// back, creating an infinite redirect loop. cmd/server uses this constructor.
func NewAccessCheckerWithSessionsAndLogger(v APIKeyValidator, sv SessionValidator, authDomain string, log *slog.Logger) AccessChecker {
	return &accessChecker{
		v:          v,
		sv:         sv,
		authDomain: authDomain,
		log:        log,
	}
}

// Allow enforces the access mode policy for a single request.
//
// Return values follow the AccessChecker contract:
//   - ok=true:  let the request proceed to the upstream.
//   - ok=false: write (status, body, hdr) and return; do not proxy.
func (ac *accessChecker) Allow(ctx context.Context, res *Resolved, r *http.Request) (ok bool, status int, body string, hdr http.Header) {
	switch res.AccessMode {
	case AccessModeOpen:
		return true, 0, "", nil

	case AccessModeAPIKey:
		return ac.checkAPIKey(ctx, res, r)

	case AccessModeBurrowLogin:
		return ac.checkBurrowLogin(ctx, res, r)

	case AccessModeMTLS:
		return ac.checkMTLS(ctx, res, r)

	default:
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return false, http.StatusInternalServerError, `{"error":"unknown access mode"}`, h
	}
}

// checkMTLS enforces mtls mode.
//
// The TLS handshake is the primary authentication boundary — proxy.go's
// GetConfigForClient already sets ClientAuth = RequireAndVerifyClientCert
// with the per-service CA, so a request reaching this checker should
// already have r.TLS.PeerCertificates populated. We re-check here as
// defense-in-depth: if the TLS layer somehow let the request through
// without a client cert (e.g. unit-test construction, h2c-spoofing, future
// non-TLS path), we refuse with 401.
//
// When the service carries a CA PEM, we ALSO re-verify the leaf cert
// against the CA pool here. This is paranoid: tls.RequireAndVerifyClientCert
// already verifies the chain at handshake time. But the re-verify catches
// the case where a test or future caller hands us a Resolved with mtls but
// invokes Allow with an r.TLS populated by a different (lax) config — and
// it makes the invariant explicit at the access-checker boundary, not just
// at TLS-handshake time.
func (ac *accessChecker) checkMTLS(_ context.Context, res *Resolved, r *http.Request) (bool, int, string, http.Header) {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")

	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return false, http.StatusUnauthorized, `{"error":"client cert required"}`, h
	}

	// Defense-in-depth: when the service's CA PEM is set, re-verify the
	// leaf against the CA pool. tls.RequireAndVerifyClientCert already did
	// this during the handshake; the re-check here makes the invariant
	// explicit at the access-checker boundary and protects against future
	// callers that wire mtls without GetConfigForClient.
	if len(res.MTLSCAPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(res.MTLSCAPEM) {
			ac.log.Warn("mtls: ca pem appended no certs", "service_id", res.ServiceID)
			return false, http.StatusInternalServerError, `{"error":"invalid CA configuration"}`, h
		}
		leaf := r.TLS.PeerCertificates[0]
		intermediates := x509.NewCertPool()
		for _, c := range r.TLS.PeerCertificates[1:] {
			intermediates.AddCert(c)
		}
		opts := x509.VerifyOptions{
			Roots:         pool,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		if _, err := leaf.Verify(opts); err != nil {
			ac.log.Warn("mtls: leaf cert failed verify",
				"service_id", res.ServiceID,
				"subject", leaf.Subject.CommonName,
				"err", err)
			return false, http.StatusUnauthorized, `{"error":"client cert required"}`, h
		}
	}

	return true, 0, "", nil
}

// checkAPIKey enforces api_key mode. It reads the API key from the configured
// header (defaulting to Authorization), strips a "Bearer " prefix when using
// the default header, then delegates validation to the APIKeyValidator.
func (ac *accessChecker) checkAPIKey(ctx context.Context, res *Resolved, r *http.Request) (bool, int, string, http.Header) {
	headerName := res.APIKeyHeader
	if headerName == "" {
		headerName = "Authorization"
	}

	raw := r.Header.Get(headerName)
	var presented string
	if headerName == "Authorization" {
		presented = parseBearer(raw)
	} else {
		presented = strings.TrimSpace(raw)
	}

	if presented == "" {
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		h.Set("WWW-Authenticate", "Bearer")
		return false, http.StatusUnauthorized, `{"error":"missing api key"}`, h
	}

	valid, err := ac.v.ValidateAPIKey(ctx, res.ServiceID, presented)
	if err != nil {
		// Log at warn; do NOT expose internal error details to the visitor.
		ac.log.Warn("api key validation error", "service_id", res.ServiceID, "err", err)
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return false, http.StatusUnauthorized, `{"error":"invalid api key"}`, h
	}
	if !valid {
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return false, http.StatusUnauthorized, `{"error":"invalid api key"}`, h
	}
	return true, 0, "", nil
}

// checkBurrowLogin enforces burrow_login mode.
//
// When a SessionValidator is wired (production path), the checker first
// inspects the visitor's burrow_session cookie:
//   - Cookie valid, user not suspended, role ∈ service access policy → allow
//     through (return ok=true).
//   - Otherwise (no cookie / invalid / suspended / role not allowed) → 302
//     redirect to the gate; the gate then either re-renders the login form or
//     (for the role-denied case) the friendly access-denied HTML page.
//
// Without a SessionValidator (legacy two-arg constructors used by older unit
// tests), every request 302s to the gate — which would create a gate↔service
// redirect loop in production, but is acceptable for tests that never carry
// a session cookie.
//
// If authDomain is empty (should never happen — the store layer rejects
// burrow_login without a configured auth_domain at write time), we fail
// closed with 500.
func (ac *accessChecker) checkBurrowLogin(ctx context.Context, res *Resolved, r *http.Request) (bool, int, string, http.Header) {
	if ac.authDomain == "" {
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return false, http.StatusInternalServerError, `{"error":"burrow_login requires auth_domain"}`, h
	}

	if ac.sv != nil {
		if c, err := r.Cookie("burrow_session"); err == nil && c.Value != "" {
			uid, err := ac.sv.ValidateSession(ctx, c.Value)
			if err == nil {
				user, err := ac.sv.GetUserByID(ctx, uid)
				if err == nil && user.Status != "suspended" {
					allowed, err := ac.sv.RoleAllowed(ctx, res.ServiceID, user.Role)
					if err == nil && allowed {
						return true, 0, "", nil
					}
				}
			}
		}
	}

	originalURL := "https://" + r.Host + r.RequestURI
	gateURL := "https://" + ac.authDomain + "/__burrow/login?next=" + url.QueryEscape(originalURL)

	h := make(http.Header)
	h.Set("Location", gateURL)
	return false, http.StatusFound, "", h
}

// parseBearer extracts the token from an Authorization header value that
// follows the "Bearer <token>" scheme. Returns the trimmed token, or an empty
// string when the header is absent, empty, or does not carry a Bearer token.
//
// Matching is case-sensitive on the "Bearer " prefix (single space) per common
// practice and the spec's intent to be OpenAI-client-friendly.
func parseBearer(s string) string {
	const prefix = "Bearer "
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, prefix) {
		return strings.TrimSpace(s[len(prefix):])
	}
	return ""
}
