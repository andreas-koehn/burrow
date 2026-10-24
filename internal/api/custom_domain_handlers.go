// custom_domain_handlers.go — per-service custom domain CRUD JSON API
// (spec D.2 / D.3 / D.4 / v0.5.0 Task 7).
//
// Carry-overs:
//
//   - cmd/server wiring (tls.Config.GetCertificate callback) is deferred to
//     Task 17. The customdomain.Store + CertCallback are implemented and tested
//     in internal/proxy/customdomain/; only the cmd/server injection is missing.
//   - Cert-expiry webhook rate-limiting: the cert.expiring event IS emitted via
//     the webhook dispatcher; per-domain once-per-day rate-limiting is deferred
//     to Task 9/12 integration (the current dispatcher has no built-in
//     rate-limit primitive keyed by (event, subject)). The emission code path
//     is stubbed with a TODO comment below.
//   - Daily cert-expiry gauge tick is registered in this file using the
//     sync.Once lazy-ticker pattern; once Task 9 (compaction tick) is wired,
//     the operator may migrate to that tick. Documented in BACKLOG.md carry-over.

package api

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy/customdomain"
)

// ─── Store interface ─────────────────────────────────────────────────────────

// CustomDomainStore is the narrow DB surface the custom domain handlers consume.
// *db.DB satisfies it via the methods in internal/db/custom_domains.go.
type CustomDomainStore interface {
	InsertCustomDomain(ctx context.Context, d db.ServiceCustomDomain) (db.ServiceCustomDomain, error)
	UpdateCustomDomain(ctx context.Context, d db.ServiceCustomDomain) error
	GetCustomDomain(ctx context.Context, serviceID, id string) (db.ServiceCustomDomain, error)
	ListCustomDomains(ctx context.Context, serviceID string) ([]db.ServiceCustomDomain, error)
	DeleteCustomDomain(ctx context.Context, serviceID, id string) error
	ListAllCustomDomains(ctx context.Context) ([]db.ServiceCustomDomain, error)
}

// CustomDomainCacheInvalidator evicts a per-domain cert from the SNI cache
// after INSERT / UPDATE / DELETE so the next TLS handshake re-fetches.
// *customdomain.Store satisfies it. Nil is safe (cache simply isn't
// invalidated, harmless until the store restarts).
type CustomDomainCacheInvalidator interface {
	InvalidateHostname(hostname string)
}

// CustomDomainCerts obtains the certificates of the custom domains with
// cert_source auto while the relay terminates TLS with ACME (H10).
// *acme.CustomDomains satisfies it. Manage and Unmanage never wait on the
// CA; Manage refuses a name the relay does not own (H12) by itself too.
type CustomDomainCerts interface {
	Manage(serviceID, hostname string) error
	Unmanage(hostname string)
	Ready(hostname string) bool
}

// The cert_status values of a custom domain (H10), derived, not stored.
const (
	// CertStatusReady: the certificate is there (uploaded, or obtained).
	CertStatusReady = "ready"
	// CertStatusPending: the relay is still obtaining it (auto under
	// ACME), or cannot serve one (an auto row in file-certificate mode).
	CertStatusPending = "pending"
	// CertStatusNotNeeded: auto behind a proxy that terminates TLS.
	CertStatusNotNeeded = "not_needed"
)

// ─── Wire shapes ─────────────────────────────────────────────────────────────

// domainResp is the JSON wire shape for a single custom domain (no key_pem).
// A separate struct is used deliberately so key_pem can never accidentally
// appear in a serialised response.
//
// Status / status_updated_at (v0.5.2 Task 10) reflect the state-machine
// column populated by the daily customdomain.StatusTick. The closed enum is:
// "pending" | "active" | "cert_expiring" | "cert_expired".
type domainResp struct {
	ID              string    `json:"id"`
	ServiceID       string    `json:"service_id"`
	Hostname        string    `json:"hostname"`
	CertSHA256      string    `json:"cert_sha256"`
	NotBefore       time.Time `json:"not_before"`
	NotAfter        time.Time `json:"not_after"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Status          string    `json:"status"`
	StatusUpdatedAt time.Time `json:"status_updated_at"`
	// CertSource is uploaded or auto; CertStatus is ready, pending or
	// not_needed (H10).
	CertSource string `json:"cert_source"`
	CertStatus string `json:"cert_status"`
}

// domainResp is toDomainResp with the cert_status of this relay.
func (d Deps) domainResp(row db.ServiceCustomDomain) domainResp {
	out := toDomainResp(row)
	out.CertStatus = d.certStatus(row)
	return out
}

// certStatus derives the cert_status of a row: an uploaded certificate is
// ready; an auto one is ready once certmagic has it, pending until then,
// pending too where nobody obtains it (file certificate, no ACME), and
// not_needed behind a proxy that terminates TLS (a plain listener).
func (d Deps) certStatus(row db.ServiceCustomDomain) string {
	if row.CertSource != db.CertSourceAuto {
		return CertStatusReady
	}
	switch {
	case d.CustomDomainCerts != nil:
		if d.CustomDomainCerts.Ready(row.Hostname) {
			return CertStatusReady
		}
		return CertStatusPending
	case d.CustomDomainFileTLS:
		return CertStatusPending
	default:
		return CertStatusNotNeeded
	}
}

// toDomainResp projects db.ServiceCustomDomain → domainResp (no key_pem).
// The persisted Status / StatusUpdatedAt columns are surfaced as-is; if
// Status is empty, customdomain.ComputeStatus is used as a fallback.
//
// In practice the fallback is unreachable for DB-loaded rows: migration 0019
// backfills every existing row to 'active' and the column carries a
// DEFAULT 'active' on all subsequent INSERTs. The fallback is a cheap safety
// net for ServiceCustomDomain values constructed without a DB load (e.g.
// test fixtures, hypothetical handler bugs) so the wire never carries an
// empty enum value.
func toDomainResp(d db.ServiceCustomDomain) domainResp {
	status := d.Status
	if status == "" {
		status = customdomain.ComputeStatus(d.NotAfter, time.Now())
	}
	source := d.CertSource
	if source == "" {
		source = db.CertSourceUploaded
	}
	// The stored status tracks an uploaded certificate's expiry; an auto
	// one is renewed by certmagic (the daily tick skips it), so a row
	// switched from an expired upload must not keep saying so.
	if source == db.CertSourceAuto {
		status = customdomain.StatusActive
	}
	return domainResp{
		CertSource:      source,
		ID:              d.ID,
		ServiceID:       d.ServiceID,
		Hostname:        d.Hostname,
		CertSHA256:      d.CertSHA256,
		NotBefore:       d.NotBefore,
		NotAfter:        d.NotAfter,
		CreatedAt:       d.CreatedAt,
		UpdatedAt:       d.UpdatedAt,
		Status:          status,
		StatusUpdatedAt: d.StatusUpdatedAt,
	}
}

// postDomainReq is the POST /services/{id}/domains request body.
// cert_source is uploaded (the default, so an older dashboard keeps
// working) or auto; cert_pem and key_pem are for uploaded only (H10).
type postDomainReq struct {
	Hostname   string `json:"hostname"`
	CertSource string `json:"cert_source"`
	CertPEM    string `json:"cert_pem"`
	KeyPEM     string `json:"key_pem"`
}

// putDomainReq is the PUT /services/{id}/domains/{did} request body, the
// same as POST's.
type putDomainReq struct {
	Hostname   string `json:"hostname"`
	CertSource string `json:"cert_source"`
	CertPEM    string `json:"cert_pem"`
	KeyPEM     string `json:"key_pem"`
}

// domainCert is what a custom-domain write stores of its certificate.
type domainCert struct {
	source              string
	certPEM, keyPEM     string
	sha256              string
	notBefore, notAfter time.Time
}

// domainCert validates the certificate part of a custom-domain write for
// the (normalised, unrefused) hostname: uploaded needs a certificate and
// key that pass validateCertAndKey; auto takes none, and is refused for a
// wildcard (HTTP-01 and TLS-ALPN-01 cannot prove one) and where the relay
// serves a file certificate without ACME, since nobody could serve the
// name. ok false: the 400 is written.
func (d Deps) domainCert(w http.ResponseWriter, hostname, source, certPEM, keyPEM string) (domainCert, bool) {
	if source == "" {
		source = db.CertSourceUploaded
	}
	switch source {
	case db.CertSourceAuto:
		switch {
		case strings.HasPrefix(hostname, "*."):
			writeErr(w, http.StatusBadRequest, "a wildcard custom domain needs cert_source uploaded")
		case d.CustomDomainCerts == nil && d.CustomDomainFileTLS:
			writeErr(w, http.StatusBadRequest, "cert_source auto needs ACME (BURROW_ACME_DOMAIN) or a proxy in front that terminates TLS; this relay serves a file certificate, so upload one")
		case certPEM != "" || keyPEM != "":
			writeErr(w, http.StatusBadRequest, "cert_pem and key_pem are for cert_source uploaded; leave them out with auto")
		default:
			return domainCert{source: source}, true
		}
		return domainCert{}, false
	case db.CertSourceUploaded:
	default:
		writeErr(w, http.StatusBadRequest, "cert_source must be uploaded or auto")
		return domainCert{}, false
	}
	if certPEM == "" || keyPEM == "" {
		writeErr(w, http.StatusBadRequest, "cert_pem and key_pem are required")
		return domainCert{}, false
	}
	result, status, errBody := validateCertAndKey(certPEM, keyPEM, hostname, d.CertValidationRoots)
	if errBody != nil {
		writeJSON(w, status, errBody)
		return domainCert{}, false
	}
	if result.warnNOSAN {
		w.Header().Set("Burrow-Warning", "cert-no-san")
	}
	return domainCert{
		source: source, certPEM: certPEM, keyPEM: keyPEM, sha256: result.certSHA256,
		notBefore: result.notBefore, notAfter: result.notAfter,
	}, true
}

// certValidationResult holds parsed cert fields after successful validation.
type certValidationResult struct {
	cert       *x509.Certificate
	tlsCert    tls.Certificate
	certSHA256 string
	notBefore  time.Time
	notAfter   time.Time
	// warnNOSAN is true when the cert has no SANs and hostname was matched via CN.
	warnNOSAN bool
}

// validateCertAndKey validates a cert_pem + key_pem pair per spec D.3.
// Returns (nil, 400 body) on any failure; returns the parsed result on success.
//
// Production uses VerifyOptions{} (system roots). Test code generates a
// CA-signed chain and can pass a custom opts parameter for the system-roots
// override.
func validateCertAndKey(certPEM, keyPEM, hostname string, roots *x509.CertPool) (*certValidationResult, int, map[string]string) {
	hostname = strings.ToLower(hostname)

	// 1. Parse cert_pem.
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, http.StatusBadRequest, map[string]string{
			"error":  "cert_pem must contain at least one CERTIFICATE block",
			"reason": "chain_invalid",
		}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, http.StatusBadRequest, map[string]string{
			"error":  fmt.Sprintf("parse certificate: %v", err),
			"reason": "chain_invalid",
		}
	}

	// 2. Check expiry.
	if cert.NotAfter.Before(time.Now()) {
		return nil, http.StatusBadRequest, map[string]string{
			"error":  "certificate has already expired",
			"reason": "expired",
		}
	}

	// 3. SAN / CN match.
	warnNOSAN := false
	if len(cert.DNSNames) > 0 || len(cert.IPAddresses) > 0 {
		// Has SANs; verify hostname is covered.
		matched := false
		for _, san := range cert.DNSNames {
			if matchesSAN(san, hostname) {
				matched = true
				break
			}
		}
		if !matched {
			// Try IP as well.
			ip := net.ParseIP(hostname)
			if ip != nil {
				for _, ipSAN := range cert.IPAddresses {
					if ipSAN.Equal(ip) {
						matched = true
						break
					}
				}
			}
		}
		if !matched {
			return nil, http.StatusBadRequest, map[string]string{
				"error":  fmt.Sprintf("hostname %q is not covered by the certificate SAN", hostname),
				"reason": "san_mismatch",
			}
		}
	} else {
		// No SANs: fall back to CN (legacy). Match; emit warning header.
		cn := strings.ToLower(cert.Subject.CommonName)
		if !matchesSAN(cn, hostname) {
			return nil, http.StatusBadRequest, map[string]string{
				"error":  fmt.Sprintf("hostname %q is not covered by the certificate SAN or CN", hostname),
				"reason": "san_mismatch",
			}
		}
		warnNOSAN = true
	}

	// 4. Parse key_pem.
	keyBlock, _ := pem.Decode([]byte(keyPEM))
	if keyBlock == nil {
		return nil, http.StatusBadRequest, map[string]string{
			"error":  "key_pem must contain a valid private key block",
			"reason": "key_mismatch",
		}
	}
	privKey, parseErr := parsePrivateKey(keyBlock)
	if parseErr != nil {
		return nil, http.StatusBadRequest, map[string]string{
			"error":  fmt.Sprintf("parse private key: %v", parseErr),
			"reason": "key_mismatch",
		}
	}

	// 5. Key–cert public key match.
	if !publicKeysMatch(cert.PublicKey, privKey) {
		return nil, http.StatusBadRequest, map[string]string{
			"error":  "private key does not match certificate public key",
			"reason": "key_mismatch",
		}
	}

	// 6. Chain verification (system roots in production; caller-supplied pool in tests).
	opts := x509.VerifyOptions{}
	if roots != nil {
		opts.Roots = roots
	}
	if _, verifyErr := cert.Verify(opts); verifyErr != nil {
		return nil, http.StatusBadRequest, map[string]string{
			"error":  fmt.Sprintf("certificate chain validation failed: %v", verifyErr),
			"reason": "chain_invalid",
		}
	}

	// Compute cert_sha256 from the DER bytes.
	sum := sha256.Sum256(block.Bytes)
	certSHA256 := hex.EncodeToString(sum[:])

	// Build a tls.Certificate for the cache.
	tlsCert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, http.StatusBadRequest, map[string]string{
			"error":  fmt.Sprintf("build TLS certificate: %v", err),
			"reason": "chain_invalid",
		}
	}

	return &certValidationResult{
		cert:       cert,
		tlsCert:    tlsCert,
		certSHA256: certSHA256,
		notBefore:  cert.NotBefore,
		notAfter:   cert.NotAfter,
		warnNOSAN:  warnNOSAN,
	}, 0, nil
}

// matchesSAN reports whether the SAN san (which may be a wildcard like
// "*.example.com") covers hostname. Both arguments should be lower-cased.
func matchesSAN(san, hostname string) bool {
	if san == hostname {
		return true
	}
	if strings.HasPrefix(san, "*.") {
		// Wildcard: matches exactly one non-dot label.
		suffix := san[1:] // ".example.com"
		if strings.HasSuffix(hostname, suffix) {
			label := strings.TrimSuffix(hostname, suffix)
			return label != "" && !strings.Contains(label, ".")
		}
	}
	return false
}

// parsePrivateKey tries PKCS#8 first, then PKCS#1 (RSA), then EC.
func parsePrivateKey(block *pem.Block) (any, error) {
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("unsupported private key format (tried PKCS8, PKCS1, EC)")
}

// publicKeysMatch compares the public key in the certificate to the public key
// derived from the private key. Supports RSA, ECDSA, Ed25519 via crypto.Signer.
func publicKeysMatch(certPub, privKey any) bool {
	// All standard Go private key types (ecdsa.PrivateKey, rsa.PrivateKey,
	// ed25519.PrivateKey) implement crypto.Signer whose Public() method returns
	// the corresponding crypto.PublicKey. Note: a custom interface with
	// "Public() any" does NOT satisfy this — the return type must match exactly.
	s, ok := privKey.(crypto.Signer)
	if !ok {
		return false
	}
	pub := s.Public()

	// Use the Equal method available on RSA/ECDSA/Ed25519 since Go 1.15.
	type equaler interface {
		Equal(x crypto.PublicKey) bool
	}
	if e, ok := certPub.(equaler); ok {
		return e.Equal(pub)
	}
	// Fallback: compare DER-encoded byte representations.
	cb, _ := x509.MarshalPKIXPublicKey(certPub)
	pb, _ := x509.MarshalPKIXPublicKey(pub)
	return len(cb) > 0 && len(cb) == len(pb) && string(cb) == string(pb)
}

// ─── Permission gate ─────────────────────────────────────────────────────────

// ensureCustomDomainAccess scopes the per-service custom domain routes by
// owner. The gate of the route let the caller pass with
// services:configure:own or :any; by effectivePerms (a session: its role; a
// token: declared and granted):
//
//   - PermServicesConfigureAny → allow.
//   - PermServicesConfigureOwn → allow only when the caller owns the service.
//   - else                     → 403.
//
// Returns (serviceOwnerID, true) to continue; ("", false) means the handler
// already wrote the response.
func (d Deps) ensureCustomDomainAccess(w http.ResponseWriter, r *http.Request, serviceID string) (string, bool) {
	if serviceID == "" {
		writeErr(w, http.StatusBadRequest, "service id is required")
		return "", false
	}
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return "", false
	}
	uid := userID(r.Context())

	hasAny := effectivePerms(r.Context(), role, authz.PermServicesConfigureAny)
	hasOwn := effectivePerms(r.Context(), role, authz.PermServicesConfigureOwn)
	if !hasAny && !hasOwn {
		writeErr(w, http.StatusForbidden, "forbidden")
		return "", false
	}

	// We need the service row to surface 404 cleanly and to check ownership.
	var svcLookup ServiceOwnerLookup
	if d.IPGeoServices != nil {
		svcLookup = d.IPGeoServices
	} else if d.CredentialServices != nil {
		svcLookup = d.CredentialServices
	}
	if svcLookup == nil {
		if !hasAny {
			writeErr(w, http.StatusInternalServerError, "service lookup unavailable")
			return "", false
		}
		return "", true // :any callers can proceed without the ownership lookup
	}
	svc, err := svcLookup.GetServiceByID(r.Context(), serviceID)
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "service not found")
		return "", false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "service lookup failed")
		return "", false
	}
	if !hasAny && svc.UserID != uid {
		writeErr(w, http.StatusForbidden, "forbidden")
		return "", false
	}
	// A direct provider has no tunnel a custom domain could lead to.
	if svc.Type == "direct" {
		writeDirectServiceErr(w)
		return "", false
	}
	return svc.UserID, true
}

// ─── Handlers ────────────────────────────────────────────────────────────────

// ListCustomDomains handles GET /api/v1/services/{serviceID}/domains.
// Returns 200 [{Domain}] — no key_pem.
func (d Deps) ListCustomDomains(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if _, ok := d.ensureCustomDomainAccess(w, r, serviceID); !ok {
		return
	}
	if d.CustomDomains == nil {
		writeJSON(w, http.StatusOK, []domainResp{})
		return
	}
	rows, err := d.CustomDomains.ListCustomDomains(r.Context(), serviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list domains failed")
		return
	}
	out := make([]domainResp, len(rows))
	for i, row := range rows {
		out[i] = d.domainResp(row)
	}
	writeJSON(w, http.StatusOK, out)
}

// PostCustomDomain handles POST /api/v1/services/{serviceID}/domains.
// Validates cert + key, inserts, returns 201 Domain.
func (d Deps) PostCustomDomain(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if _, ok := d.ensureCustomDomainAccess(w, r, serviceID); !ok {
		return
	}
	if d.CustomDomains == nil {
		writeErr(w, http.StatusInternalServerError, "custom domain store unavailable")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024) // cert PEMs can be large
	var in postDomainReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(in.Hostname) == "" {
		writeErr(w, http.StatusBadRequest, "hostname is required")
		return
	}
	// Normalised before any check, so a trailing dot or a wildcard cannot
	// slip past the ownership refusals (H12).
	var nameOK bool
	if in.Hostname, nameOK = db.NormalizeDomainName(in.Hostname); !nameOK {
		writeErr(w, http.StatusBadRequest, "hostname must be a DNS name such as app.example.com (letters, digits and '-', an international name in punycode, at most one leading \"*.\")")
		return
	}
	if why := d.customDomainRefusal(in.Hostname); why != "" {
		writeErr(w, http.StatusConflict, why)
		return
	}
	cert, ok := d.domainCert(w, in.Hostname, in.CertSource, in.CertPEM, in.KeyPEM)
	if !ok {
		return
	}

	row := db.ServiceCustomDomain{
		ServiceID:  serviceID,
		Hostname:   in.Hostname,
		CertSource: cert.source,
		CertPEM:    cert.certPEM,
		KeyPEM:     cert.keyPEM,
		CertSHA256: cert.sha256,
		NotBefore:  cert.notBefore,
		NotAfter:   cert.notAfter,
	}
	inserted, err := d.CustomDomains.InsertCustomDomain(r.Context(), row)
	if err != nil {
		if errors.Is(err, db.ErrHostIsTCPName) {
			writeErr(w, http.StatusConflict, "hostname is a tcp host name")
			return
		}
		if errors.Is(err, db.ErrInvalidHostname) {
			writeErr(w, http.StatusBadRequest, "hostname must be a DNS name such as app.example.com")
			return
		}
		if errors.Is(err, db.ErrDuplicateHostname) {
			writeErr(w, http.StatusConflict, "hostname already bound")
			return
		}
		writeErr(w, http.StatusInternalServerError, "insert domain failed")
		return
	}

	// Invalidate SNI cache.
	if d.CustomDomainCache != nil {
		d.CustomDomainCache.InvalidateHostname(in.Hostname)
	}
	// An auto name: the relay starts obtaining its certificate (H10). A
	// failure there is logged and audited by the ACME side; the domain
	// stays bound with cert_status pending.
	if cert.source == db.CertSourceAuto && d.CustomDomainCerts != nil {
		_ = d.CustomDomainCerts.Manage(serviceID, in.Hostname)
	}

	// Audit (best-effort).
	if d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceCustomDomainAdd,
			SubjectID: serviceID, SubjectLabel: in.Hostname,
			Result:   "ok",
			SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(map[string]string{
				"hostname":    in.Hostname,
				"cert_source": cert.source,
				"cert_sha256": cert.sha256,
			}),
		})
	}

	d.uploadedCertSignals(r.Context(), serviceID, in.Hostname, cert)

	writeJSON(w, http.StatusCreated, d.domainResp(inserted))
}

// GetCustomDomain handles GET /api/v1/services/{serviceID}/domains/{did}.
// Returns 200 Domain (no key_pem).
func (d Deps) GetCustomDomain(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	did := chi.URLParam(r, "did")
	if _, ok := d.ensureCustomDomainAccess(w, r, serviceID); !ok {
		return
	}
	if d.CustomDomains == nil {
		writeErr(w, http.StatusNotFound, "domain not found")
		return
	}
	row, err := d.CustomDomains.GetCustomDomain(r.Context(), serviceID, did)
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "domain not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get domain failed")
		return
	}
	writeJSON(w, http.StatusOK, d.domainResp(row))
}

// PutCustomDomain handles PUT /api/v1/services/{serviceID}/domains/{did}.
// Validates the new cert + key and replaces the row. Returns 204.
func (d Deps) PutCustomDomain(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	did := chi.URLParam(r, "did")
	if _, ok := d.ensureCustomDomainAccess(w, r, serviceID); !ok {
		return
	}
	if d.CustomDomains == nil {
		writeErr(w, http.StatusInternalServerError, "custom domain store unavailable")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var in putDomainReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(in.Hostname) == "" {
		writeErr(w, http.StatusBadRequest, "hostname is required")
		return
	}
	// Normalised before any check, so a trailing dot or a wildcard cannot
	// slip past the ownership refusals (H12).
	var nameOK bool
	if in.Hostname, nameOK = db.NormalizeDomainName(in.Hostname); !nameOK {
		writeErr(w, http.StatusBadRequest, "hostname must be a DNS name such as app.example.com (letters, digits and '-', an international name in punycode, at most one leading \"*.\")")
		return
	}
	if why := d.customDomainRefusal(in.Hostname); why != "" {
		writeErr(w, http.StatusConflict, why)
		return
	}
	cert, ok := d.domainCert(w, in.Hostname, in.CertSource, in.CertPEM, in.KeyPEM)
	if !ok {
		return
	}

	// Fetch old hostname (for cache invalidation and the ACME order).
	old, oldErr := d.CustomDomains.GetCustomDomain(r.Context(), serviceID, did)

	row := db.ServiceCustomDomain{
		ID:         did,
		ServiceID:  serviceID,
		Hostname:   in.Hostname,
		CertSource: cert.source,
		CertPEM:    cert.certPEM,
		KeyPEM:     cert.keyPEM,
		CertSHA256: cert.sha256,
		NotBefore:  cert.notBefore,
		NotAfter:   cert.notAfter,
	}
	if err := d.CustomDomains.UpdateCustomDomain(r.Context(), row); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "domain not found")
			return
		}
		if errors.Is(err, db.ErrHostIsTCPName) {
			writeErr(w, http.StatusConflict, "hostname is a tcp host name")
			return
		}
		if errors.Is(err, db.ErrInvalidHostname) {
			writeErr(w, http.StatusBadRequest, "hostname must be a DNS name such as app.example.com")
			return
		}
		if errors.Is(err, db.ErrDuplicateHostname) {
			writeErr(w, http.StatusConflict, "hostname already bound")
			return
		}
		writeErr(w, http.StatusInternalServerError, "update domain failed")
		return
	}

	// Invalidate SNI cache (old + new hostname).
	if d.CustomDomainCache != nil {
		if oldErr == nil {
			d.CustomDomainCache.InvalidateHostname(old.Hostname)
		}
		d.CustomDomainCache.InvalidateHostname(in.Hostname)
	}
	// The ACME order follows the row (H10): the old auto name stops when
	// it is renamed or switched to uploaded, the new auto name starts.
	if d.CustomDomainCerts != nil {
		if oldErr == nil && old.CertSource == db.CertSourceAuto &&
			(cert.source != db.CertSourceAuto || old.Hostname != in.Hostname) {
			d.CustomDomainCerts.Unmanage(old.Hostname)
		}
		if cert.source == db.CertSourceAuto {
			_ = d.CustomDomainCerts.Manage(serviceID, in.Hostname)
		}
	}

	// Audit (best-effort).
	if d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceCustomDomainUpdate,
			SubjectID: serviceID, SubjectLabel: in.Hostname,
			Result:   "ok",
			SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(map[string]string{
				"hostname":    in.Hostname,
				"cert_source": cert.source,
				"cert_sha256": cert.sha256,
			}),
		})
	}

	d.uploadedCertSignals(r.Context(), serviceID, in.Hostname, cert)

	w.WriteHeader(http.StatusNoContent)
}

// DeleteCustomDomain handles DELETE /api/v1/services/{serviceID}/domains/{did}.
// Returns 204.
func (d Deps) DeleteCustomDomain(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	did := chi.URLParam(r, "did")
	if _, ok := d.ensureCustomDomainAccess(w, r, serviceID); !ok {
		return
	}
	if d.CustomDomains == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Fetch hostname before delete for audit + cache invalidation.
	row, fetchErr := d.CustomDomains.GetCustomDomain(r.Context(), serviceID, did)

	if err := d.CustomDomains.DeleteCustomDomain(r.Context(), serviceID, did); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete domain failed")
		return
	}

	// Invalidate SNI cache.
	if fetchErr == nil && d.CustomDomainCache != nil {
		d.CustomDomainCache.InvalidateHostname(row.Hostname)
	}
	// An auto name: its certificate is no longer obtained or renewed.
	if fetchErr == nil && row.CertSource == db.CertSourceAuto && d.CustomDomainCerts != nil {
		d.CustomDomainCerts.Unmanage(row.Hostname)
	}

	// Audit (best-effort).
	if fetchErr == nil && d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceCustomDomainDelete,
			SubjectID: serviceID, SubjectLabel: row.Hostname,
			Result:   "ok",
			SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(map[string]string{
				"hostname":    row.Hostname,
				"cert_source": toDomainResp(row).CertSource,
				"cert_sha256": row.CertSHA256,
			}),
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

// uploadedCertSignals sets the expiry gauge of an uploaded certificate and
// emits cert.expiring when it lands within 14 days. An auto certificate
// is renewed by certmagic and has neither.
func (d Deps) uploadedCertSignals(ctx context.Context, serviceID, hostname string, cert domainCert) {
	if cert.source != db.CertSourceUploaded {
		return
	}
	if d.Metrics != nil {
		days := float64(time.Until(cert.notAfter)) / float64(24*time.Hour)
		d.Metrics.SetCertExpiryDays(hostname, days)
	}
	// Rate-limiting is a TODO (Task 9/12 carry-over — the dispatcher has no
	// built-in per-domain once-per-day limiter).
	if cert.notAfter.Before(time.Now().Add(14*24*time.Hour)) && d.WebhookDispatcher != nil {
		d.WebhookDispatcher.Publish(ctx, "cert.expiring", map[string]any{
			"hostname":   hostname,
			"service_id": serviceID,
			"not_after":  cert.notAfter.UTC().Format(time.RFC3339),
		})
	}
}

// CustomDomainRefusal is customDomainRefusal for the relay's startup: the
// auto names loaded into certmagic pass the same H12 checks as a write
// (their rows may predate a change of the configuration).
func (d Deps) CustomDomainRefusal(hostname string) string {
	return d.customDomainRefusal(hostname)
}

// customDomainRefusal says why hostname (lower case, "*.<parent>" for a
// wildcard) cannot be a custom domain, "" when it can (H12). The message
// is the 409's. Refused: a name that is, lies under or covers BURROW_TCP_DOMAIN
// (the slug names of the tcp services are the passthrough's), and a name the
// relay serves itself (the dashboard, the services host, the ACME names, the
// names of its certificate) or one under them. A tcp service's own
// tcp_hostname is the store's to say (db.ErrHostIsTCPName), in the same
// transaction as the write.
func (d Deps) customDomainRefusal(hostname string) string {
	base := strings.TrimPrefix(hostname, "*.")
	if tcp := strings.ToLower(strings.TrimSuffix(d.TCPDomain, ".")); tcp != "" && (underName(base, tcp) || underName(tcp, base)) {
		return "hostname is a tcp host name: " + base + " is, lies under, or covers BURROW_TCP_DOMAIN " + tcp + ", whose names are the tcp services'"
	}
	own := append([]string{d.AuthDomain, d.ServicesDomain}, d.ACMENames...)
	for _, o := range own {
		if underName(base, o) {
			return "hostname " + base + " is a name the relay serves itself, or lies under one (" + strings.ToLower(o) + ")"
		}
	}
	probes := []string{base}
	if hostname != base {
		probes = append(probes, "x."+base) // what a wildcard over it covers
	}
	for _, p := range probes {
		for n := p; ; {
			if d.RelayNames.Owns(n) {
				return "hostname " + base + " is a name the relay serves itself, or lies under one (" + n + ")"
			}
			_, up, ok := strings.Cut(n, ".")
			if !ok || !strings.Contains(up, ".") {
				break
			}
			n = up
		}
	}
	return ""
}
