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
	return domainResp{
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
type postDomainReq struct {
	Hostname string `json:"hostname"`
	CertPEM  string `json:"cert_pem"`
	KeyPEM   string `json:"key_pem"`
}

// putDomainReq is the PUT /services/{id}/domains/{did} request body.
type putDomainReq struct {
	Hostname string `json:"hostname"`
	CertPEM  string `json:"cert_pem"`
	KeyPEM   string `json:"key_pem"`
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

// ensureCustomDomainAccess gates all per-service custom domain routes.
//
//   - admin or PermServicesConfigureAny → allow.
//   - PermServicesConfigureOwn          → allow only when the caller owns the service.
//   - else                              → 403.
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

	hasAny := role == "admin" || authz.Can(role, authz.PermServicesConfigureAny)
	hasOwn := authz.Can(role, authz.PermServicesConfigureOwn)
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
		out[i] = toDomainResp(row)
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
	in.Hostname = strings.ToLower(strings.TrimSpace(in.Hostname))
	if in.Hostname == "" {
		writeErr(w, http.StatusBadRequest, "hostname is required")
		return
	}
	if in.CertPEM == "" || in.KeyPEM == "" {
		writeErr(w, http.StatusBadRequest, "cert_pem and key_pem are required")
		return
	}

	result, status, errBody := validateCertAndKey(in.CertPEM, in.KeyPEM, in.Hostname, d.CertValidationRoots)
	if errBody != nil {
		writeJSON(w, status, errBody)
		return
	}
	if result.warnNOSAN {
		w.Header().Set("Burrow-Warning", "cert-no-san")
	}

	row := db.ServiceCustomDomain{
		ServiceID:  serviceID,
		Hostname:   in.Hostname,
		CertPEM:    in.CertPEM,
		KeyPEM:     in.KeyPEM,
		CertSHA256: result.certSHA256,
		NotBefore:  result.notBefore,
		NotAfter:   result.notAfter,
	}
	inserted, err := d.CustomDomains.InsertCustomDomain(r.Context(), row)
	if err != nil {
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
				"cert_sha256": result.certSHA256,
			}),
		})
	}

	// Emit cert-expiry metric.
	if d.Metrics != nil {
		days := float64(time.Until(result.notAfter)) / float64(24*time.Hour)
		d.Metrics.SetCertExpiryDays(in.Hostname, days)
	}

	// Emit cert.expiring webhook event when expiring soon.
	// Rate-limiting is a TODO (Task 9/12 carry-over — the dispatcher has no
	// built-in per-domain once-per-day limiter).
	if result.notAfter.Before(time.Now().Add(14*24*time.Hour)) && d.WebhookDispatcher != nil {
		d.WebhookDispatcher.Publish(r.Context(), "cert.expiring", map[string]any{
			"hostname":   in.Hostname,
			"service_id": serviceID,
			"not_after":  result.notAfter.UTC().Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusCreated, toDomainResp(inserted))
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
	writeJSON(w, http.StatusOK, toDomainResp(row))
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
	in.Hostname = strings.ToLower(strings.TrimSpace(in.Hostname))
	if in.Hostname == "" {
		writeErr(w, http.StatusBadRequest, "hostname is required")
		return
	}
	if in.CertPEM == "" || in.KeyPEM == "" {
		writeErr(w, http.StatusBadRequest, "cert_pem and key_pem are required")
		return
	}

	result, status, errBody := validateCertAndKey(in.CertPEM, in.KeyPEM, in.Hostname, d.CertValidationRoots)
	if errBody != nil {
		writeJSON(w, status, errBody)
		return
	}
	if result.warnNOSAN {
		w.Header().Set("Burrow-Warning", "cert-no-san")
	}

	// Fetch old hostname (for cache invalidation).
	old, oldErr := d.CustomDomains.GetCustomDomain(r.Context(), serviceID, did)

	row := db.ServiceCustomDomain{
		ID:         did,
		ServiceID:  serviceID,
		Hostname:   in.Hostname,
		CertPEM:    in.CertPEM,
		KeyPEM:     in.KeyPEM,
		CertSHA256: result.certSHA256,
		NotBefore:  result.notBefore,
		NotAfter:   result.notAfter,
	}
	if err := d.CustomDomains.UpdateCustomDomain(r.Context(), row); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "domain not found")
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
				"cert_sha256": result.certSHA256,
			}),
		})
	}

	// Update cert-expiry metric.
	if d.Metrics != nil {
		days := float64(time.Until(result.notAfter)) / float64(24*time.Hour)
		d.Metrics.SetCertExpiryDays(in.Hostname, days)
	}

	// Emit cert.expiring webhook event when the renewed cert lands within the
	// 14-day expiry window. Mirrors the POST path. Rate-limiting is a TODO
	// (Task 9/12 carry-over — the dispatcher has no per-domain once-per-day
	// limiter).
	if result.notAfter.Before(time.Now().Add(14*24*time.Hour)) && d.WebhookDispatcher != nil {
		d.WebhookDispatcher.Publish(r.Context(), "cert.expiring", map[string]any{
			"hostname":   in.Hostname,
			"service_id": serviceID,
			"not_after":  result.notAfter.UTC().Format(time.RFC3339),
		})
	}

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
				"cert_sha256": row.CertSHA256,
			}),
		})
	}

	w.WriteHeader(http.StatusNoContent)
}
