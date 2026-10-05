package store

// Package store — services.go
// Permission-gated service/key/policy methods and hot-path lookup helpers.
//
// # ServiceView vs ServiceDetail design decision
//
// ServiceView carries the durable fields the store can read from the DB:
// id, name, type, subdomain, access_mode, api_key_header, created_at.
// It intentionally omits live/runtime fields (hostname, connected,
// remote_port, local_addr) that come from the in-process tunnel registry or
// the auth_domain setting — both are owned by the API/wiring layer (Task 10),
// not the store. Task 10 composes the full response by embedding a ServiceView
// and populating the live fields itself.
//
// ServiceDetail extends ServiceView with the two aggregate fields the API needs
// for the single-service GET: api_key_count and access_policy. These require an
// extra db round-trip so they live only on the detail view, not the list view.

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

var (
	// ErrInvalidSlug is returned when a slug does not match auth.ValidSlug.
	ErrInvalidSlug = errors.New("store: invalid slug")
	// ErrSlugTaken is returned when another service already uses the slug.
	ErrSlugTaken = errors.New("store: slug already in use")
	// ErrDirectService is returned when a setting that only makes sense for a
	// tunnelled service is changed on the backing row of a direct AI provider.
	ErrDirectService = errors.New("store: service backs a direct AI provider")
)

// ServiceView is the durable-fields representation of a service returned by
// ListServices. Live/runtime fields (hostname, connected, remote_port,
// local_addr) are absent here; Task 10 (api handlers) composes them from the
// tunnel registry and auth_domain setting.
type ServiceView struct {
	ID           string
	UserID       string
	Name         string
	Type         string // "http" or "tcp"
	Subdomain    string // "" for tcp or unset http
	AccessMode   string // "open" | "api_key" | "burrow_login"
	APIKeyHeader string // effective header name (default "Authorization")
	CreatedAt    time.Time
}

// ServiceDetail extends ServiceView with the aggregate fields returned by the
// single-service GET endpoint. Task 10 may further overlay live runtime fields.
type ServiceDetail struct {
	ServiceView
	APIKeyCount  int
	AccessPolicy []string // roles; empty slice = deny-all
}

// canConfigure loads the service and checks whether callerID/callerRole may
// configure it. It returns the loaded service so callers avoid a second load.
//
//   - PermServicesConfigureAny → allow unconditionally (admin)
//   - PermServicesConfigureOwn → allow only when service.UserID == callerID
//   - otherwise → ErrForbidden
//
// db.ErrNotFound is propagated unchanged (maps to 404, not 403).
func (s *Store) canConfigure(ctx context.Context, callerID, callerRole, serviceID string) (db.Service, error) {
	svc, err := s.q.GetServiceByID(ctx, serviceID)
	if err != nil {
		return db.Service{}, err // includes db.ErrNotFound
	}
	if authz.Can(callerRole, authz.PermServicesConfigureAny) {
		return svc, nil
	}
	if authz.Can(callerRole, authz.PermServicesConfigureOwn) && svc.UserID == callerID {
		return svc, nil
	}
	return db.Service{}, ErrForbidden
}

// ListServices returns all services visible to the caller.
// Admins (PermTunnelsReadAny re-used as the read-any signal, matching v0.2
// pattern — admin has all :any perms) see every service; regular users see
// only their own. The list is returned as ServiceViews (live fields deferred
// to Task 10).
func (s *Store) ListServices(ctx context.Context, callerID, callerRole string) ([]ServiceView, error) {
	var rows []db.Service
	var err error
	if authz.Can(callerRole, authz.PermServicesConfigureAny) {
		rows, err = s.q.ListAllServices(ctx)
	} else {
		rows, err = s.q.ListServicesByUser(ctx, callerID)
	}
	if err != nil {
		return nil, err
	}
	out := make([]ServiceView, len(rows))
	for i, r := range rows {
		out[i] = serviceToView(r)
	}
	return out, nil
}

// GetService returns the full ServiceDetail for a single service.
// The caller must have configure:own (owner) or configure:any (admin).
// api_key_count and access_policy are populated; live runtime fields are
// left at their zero values for Task 10 to populate.
func (s *Store) GetService(ctx context.Context, callerID, callerRole, serviceID string) (ServiceDetail, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return ServiceDetail{}, err
	}
	keys, err := s.q.ListServiceAPIKeys(ctx, svc.ID)
	if err != nil {
		return ServiceDetail{}, err
	}
	policy, err := s.q.GetAccessPolicy(ctx, svc.ID)
	if err != nil {
		return ServiceDetail{}, err
	}
	return ServiceDetail{
		ServiceView:  serviceToView(svc),
		APIKeyCount:  len(keys),
		AccessPolicy: policy,
	}, nil
}

// SetServiceAccessMode validates and applies a new access mode to the given
// service. Gate: configure:own (owner) or configure:any (admin).
// Validation order: gate → mode enum → http-only constraint → mtls_ca_pem.
//   - ErrForbidden if not authorized
//   - db.ErrNotFound if the service does not exist
//   - ErrInvalidAccessMode if mode ∉ {open,api_key,burrow_login,mtls}
//   - ErrServiceNotHTTP if mode ≠ "open" and service.Type ≠ "http"
//   - ErrMTLSCARequired if mode == "mtls" and caPEM is empty
//   - ErrInvalidMTLSCAPEM if mode == "mtls" and caPEM has no valid block
//
// An empty header defaults to "Authorization" for api_key mode (stored in DB).
// For non-api_key modes the header is written as "Authorization" as well,
// keeping the DB column consistent.
//
// caPEM is the operator-supplied trust anchor for mtls mode. When mode is
// "mtls" the store validates it contains at least one CERTIFICATE block and
// persists it via SetServiceMTLSCAPEM; for any other mode caPEM is ignored.
// Switching AWAY from mtls does NOT clear the existing CA blob — that lets
// operators flip back to mtls later without re-pasting the CA. To explicitly
// clear, set access_mode to mtls then immediately switch back, or call the
// dedicated DB method.
func (s *Store) SetServiceAccessMode(ctx context.Context, callerID, callerRole, serviceID, mode, header string, caPEM []byte) error {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return err
	}
	// A direct provider's backing row stays in api_key mode: /ai/ refuses
	// any other mode, and there is no tunnel the other modes could protect.
	if svc.Type == "direct" {
		return ErrDirectService
	}
	switch mode {
	case "open", "api_key", "burrow_login", "mtls":
	default:
		return ErrInvalidAccessMode
	}
	if mode != "open" && svc.Type != "http" {
		return ErrServiceNotHTTP
	}
	if mode == "mtls" {
		if len(caPEM) == 0 {
			return ErrMTLSCARequired
		}
		if !validateCAPEM(caPEM) {
			return ErrInvalidMTLSCAPEM
		}
	}
	if header == "" {
		header = "Authorization"
	}
	oldMode := svc.AccessMode
	if err := s.q.SetServiceAccessMode(ctx, serviceID, mode, header); err != nil {
		return err
	}
	if mode == "mtls" {
		if err := s.q.SetServiceMTLSCAPEM(ctx, serviceID, string(caPEM)); err != nil {
			return err
		}
	}
	s.emitAudit(ctx, audit.ActionServiceAccessModeUpdate, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]string{"from": oldMode, "to": mode})
	})
	if mode == "mtls" {
		s.emitAudit(ctx, audit.ActionMtlsCAUpdate, func(e *audit.Event) {
			e.SubjectID = serviceID
			e.SubjectLabel = svc.Name
		})
	}
	return nil
}

// SetServiceSlug changes the URL segment of an http service. Same permission
// rule as SetServiceAccessMode (owner or a role that may configure any
// service). Returns the previous slug so the caller can audit the change.
func (s *Store) SetServiceSlug(ctx context.Context, callerID, callerRole, serviceID, slug string) (string, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return "", err
	}
	if svc.Type == "direct" {
		return "", ErrDirectService
	}
	if svc.Type != "http" {
		return "", ErrServiceNotHTTP
	}
	if !auth.ValidSlug(slug) {
		return "", ErrInvalidSlug
	}
	if svc.Subdomain == slug {
		return slug, nil
	}
	if err := s.q.SetServiceSubdomain(ctx, serviceID, slug); err != nil {
		// isUniqueViolation matches SQLite; Postgres reports
		// "violates unique constraint".
		if isUniqueViolation(err) || containsStr(err.Error(), "unique constraint") {
			return "", ErrSlugTaken
		}
		return "", err
	}
	return svc.Subdomain, nil
}

// SuggestSlug returns a generated slug that no service uses right now. The
// result is a suggestion only: uniqueness is enforced when it is saved.
func (s *Store) SuggestSlug(ctx context.Context) (string, error) {
	for i := 0; i < 8; i++ {
		slug, err := auth.GenerateSlug()
		if err != nil {
			return "", err
		}
		if _, err := s.q.GetServiceBySubdomain(ctx, slug); errors.Is(err, db.ErrNotFound) {
			return slug, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("store: no free slug after 8 attempts")
}

// validateCAPEM reports whether pem contains at least one parseable
// CERTIFICATE block.
func validateCAPEM(in []byte) bool {
	rest := in
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			return false
		}
		if block.Type == "CERTIFICATE" {
			if _, err := x509.ParseCertificate(block.Bytes); err == nil {
				return true
			}
		}
		if len(next) == 0 {
			return false
		}
		rest = next
	}
}

// ListAPIKeys returns all API keys for the given service.
// Gate: configure:own (owner) or configure:any (admin).
// Key hashes are included in the returned rows (the caller is trusted).
func (s *Store) ListAPIKeys(ctx context.Context, callerID, callerRole, serviceID string) ([]db.ServiceAPIKey, error) {
	if _, err := s.canConfigure(ctx, callerID, callerRole, serviceID); err != nil {
		return nil, err
	}
	return s.q.ListServiceAPIKeys(ctx, serviceID)
}

// CreateAPIKey generates and stores a new service API key.
// Gate: configure:own (owner) or configure:any (admin).
// Returns: (id, plaintext, error).
//   - ErrNameRequired if name is empty
//   - ErrForbidden if not authorized
//   - db.ErrNotFound if the service does not exist
//
// Only the sha256-hex hash is persisted; plaintext is returned once and never
// stored. Plaintext has the "buk_" prefix.
func (s *Store) CreateAPIKey(ctx context.Context, callerID, callerRole, serviceID, name string) (id, plaintext string, err error) {
	if name == "" {
		return "", "", ErrNameRequired
	}
	if _, err := s.canConfigure(ctx, callerID, callerRole, serviceID); err != nil {
		return "", "", err
	}
	pt, hash, err := auth.GenerateAPIKey()
	if err != nil {
		return "", "", err
	}
	id = uuid.NewString()
	if err := s.q.CreateServiceAPIKey(ctx, db.ServiceAPIKey{
		ID:        id,
		ServiceID: serviceID,
		Name:      name,
		KeyHash:   hash,
	}); err != nil {
		return "", "", err
	}
	keyID := id
	s.emitAudit(ctx, audit.ActionServiceAPIKeyCreate, func(e *audit.Event) {
		e.SubjectID = keyID
		e.SubjectLabel = name
		e.Payload = audit.MustJSON(map[string]string{"service_id": serviceID})
	})
	return id, pt, nil
}

// DeleteAPIKey removes a service API key.
// Gate: configure:own (owner) or configure:any (admin).
// Propagates db.ErrNotFound if the key does not exist.
func (s *Store) DeleteAPIKey(ctx context.Context, callerID, callerRole, serviceID, keyID string) error {
	if _, err := s.canConfigure(ctx, callerID, callerRole, serviceID); err != nil {
		return err
	}
	// Capture key name for the audit row before delete.
	var keyName string
	if keys, err := s.q.ListServiceAPIKeys(ctx, serviceID); err == nil {
		for _, k := range keys {
			if k.ID == keyID {
				keyName = k.Name
				break
			}
		}
	}
	if err := s.q.DeleteServiceAPIKey(ctx, keyID, serviceID); err != nil {
		return err
	}
	s.emitAudit(ctx, audit.ActionServiceAPIKeyRevoke, func(e *audit.Event) {
		e.SubjectID = keyID
		e.SubjectLabel = keyName
		e.Payload = audit.MustJSON(map[string]string{"service_id": serviceID})
	})
	return nil
}

// GetAccessPolicy returns the access policy roles for the given service.
// Gate: configure:own (owner) or configure:any (admin).
// Returns a non-nil empty slice when no roles are set (deny-all).
func (s *Store) GetAccessPolicy(ctx context.Context, callerID, callerRole, serviceID string) ([]string, error) {
	if _, err := s.canConfigure(ctx, callerID, callerRole, serviceID); err != nil {
		return nil, err
	}
	return s.q.GetAccessPolicy(ctx, serviceID)
}

// SetAccessPolicy replaces the full access policy for the given service.
// Gate: configure:own (owner) or configure:any (admin).
// Each role is validated against the built-in authz role set; unknown roles
// return ErrUnknownRole (maps to HTTP 400). An empty roles slice means
// deny-all.
func (s *Store) SetAccessPolicy(ctx context.Context, callerID, callerRole, serviceID string, roles []string) error {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return err
	}
	for _, r := range roles {
		if _, ok := authz.Get(r); !ok {
			return ErrUnknownRole
		}
	}
	if err := s.q.SetAccessPolicy(ctx, serviceID, roles); err != nil {
		return err
	}
	s.emitAudit(ctx, audit.ActionServiceAccessPolicyUpdate, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]any{"roles": roles})
	})
	return nil
}

// ValidateAPIKey checks the presented plaintext key against the service's keys
// and returns the matched key's id. ok is false (with a nil error) when no key
// matches. This is a hot-path helper used by the proxy middleware; it has NO
// permission gate (the caller has already proven service identity via
// routing).
//
// On a hit: best-effort touch last_used (touch error ignored).
// On other errors than db.ErrNotFound: propagates.
func (s *Store) ValidateAPIKey(ctx context.Context, serviceID, presented string) (string, bool, error) {
	hash := auth.HashToken(presented)
	key, err := s.q.GetServiceAPIKeyByHash(ctx, serviceID, hash)
	if err != nil {
		if err == db.ErrNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	// Best-effort: update last_used without failing validation on error.
	_ = s.q.TouchServiceAPIKey(ctx, key.ID)
	return key.ID, true, nil
}

// ServiceForSubdomain returns the service registered for the given subdomain.
// This is a hot-path helper used by the proxy router; it has NO permission gate.
// Delegates directly to db.GetServiceBySubdomain; propagates db.ErrNotFound.
func (s *Store) ServiceForSubdomain(ctx context.Context, sub string) (db.Service, error) {
	return s.q.GetServiceBySubdomain(ctx, sub)
}

// ServiceByID returns the service row for id. Hot-path helper used by the
// proxy router; it has NO permission gate. Propagates db.ErrNotFound.
func (s *Store) ServiceByID(ctx context.Context, id string) (db.Service, error) {
	return s.q.GetServiceByID(ctx, id)
}

// GetServiceIPGeo returns the ip-geo policy config for the given service.
// This is a hot-path helper used by the proxy router; it has NO permission gate.
// Returns a zero-value config (Enabled=false, empty slices) when no row exists.
func (s *Store) GetServiceIPGeo(ctx context.Context, serviceID string) (db.ServiceIPGeoConfig, error) {
	return s.q.GetServiceIPGeo(ctx, serviceID)
}

// RoleAllowed reports whether the given role is in the service's access policy.
// This is a hot-path helper used by the proxy auth middleware; it has NO
// permission gate.
// An empty policy (deny-all) returns false.
func (s *Store) RoleAllowed(ctx context.Context, serviceID, role string) (bool, error) {
	policy, err := s.q.GetAccessPolicy(ctx, serviceID)
	if err != nil {
		return false, err
	}
	for _, r := range policy {
		if r == role {
			return true, nil
		}
	}
	return false, nil
}

// serviceToView maps a db.Service row to a ServiceView (durable fields only).
func serviceToView(s db.Service) ServiceView {
	return ServiceView{
		ID:           s.ID,
		UserID:       s.UserID,
		Name:         s.Name,
		Type:         s.Type,
		Subdomain:    s.Subdomain,
		AccessMode:   s.AccessMode,
		APIKeyHeader: s.APIKeyHeader,
		CreatedAt:    s.CreatedAt,
	}
}

// CreateService is the admin-only pre-provisioning surface exposed via
// POST /api/v1/services (v0.5.2 P3.6). It delegates straight to the DB layer
// — permission checking happens in the API handler (RequireAdmin) so this
// method intentionally has no per-caller authz; tests that want to bypass
// the API entirely call it on *db.DB directly. Returns db.ErrDuplicateService
// on UNIQUE-constraint violations, mapped to HTTP 409 by the handler.
func (s *Store) CreateService(ctx context.Context, svc db.Service) error {
	return s.q.CreateService(ctx, svc)
}

// ServiceAccessMode returns the durable access mode of a service. It is the
// source of truth the dashboard's Tunnels and Client views must show; the
// per-session tunnels row only carries the legacy v0.2 value.
func (s *Store) ServiceAccessMode(ctx context.Context, serviceID string) (string, error) {
	svc, err := s.q.GetServiceByID(ctx, serviceID)
	if err != nil {
		return "", err
	}
	return svc.AccessMode, nil
}
