package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// serviceResp is the JSON shape of one service in the list response (Part E).
type serviceResp struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Slug         string `json:"slug"`
	URL          string `json:"url"`
	AccessMode   string `json:"access_mode"`
	APIKeyHeader string `json:"api_key_header"`
	Connected    bool   `json:"connected"`
	RemotePort   int    `json:"remote_port"`
	LocalAddr    string `json:"local_addr"`
}

// serviceDetailResp extends serviceResp with the single-service aggregate fields.
type serviceDetailResp struct {
	serviceResp
	APIKeyCount  int      `json:"api_key_count"`
	AccessPolicy []string `json:"access_policy"`
}

// apiKeyResp is the safe (no hash/plaintext) representation of a service API key.
type apiKeyResp struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	LastUsed  *time.Time `json:"last_used"`
	CreatedAt time.Time  `json:"created_at"`
}

// createAPIKeyResp is returned by POST /api-keys — plaintext shown once.
type createAPIKeyResp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

// accessPolicyResp is the JSON shape for GET /access-policy.
type accessPolicyResp struct {
	Roles []string `json:"roles"`
}

// composeServiceURL returns "https://<authDomain>/svc/<slug>/" when both parts
// are set, and "" otherwise (tcp service, no slug yet, or no auth domain).
func composeServiceURL(slug, authDomain string) string {
	if slug == "" || authDomain == "" {
		return ""
	}
	return "https://" + authDomain + "/svc/" + slug + "/"
}

// composeLive queries the LiveTunnelLookup for live state and returns the
// snapshot (or a zero-value snapshot when d.LiveTunnels is nil or miss).
func (d Deps) composeLive(serviceID string) LiveTunnelSnapshot {
	if d.LiveTunnels == nil {
		return LiveTunnelSnapshot{}
	}
	snap, _ := d.LiveTunnels.LookupByServiceID(serviceID)
	return snap
}

// callerRole loads the role for the authenticated user from the context.
// Returns "" on lookup failure; handlers should treat "" as an unprivileged role.
func (d Deps) callerRole(r *http.Request) (string, error) {
	uid := userID(r.Context())
	u, err := d.Users.GetUserByID(r.Context(), uid)
	if err != nil {
		return "", err
	}
	return u.Role, nil
}

// mapServiceErr maps a store/db error to the appropriate HTTP status + JSON body.
// Returns false when the error was not handled (caller should write 500).
func mapServiceErr(w http.ResponseWriter, err error, notFoundMsg string) bool {
	switch {
	case errors.Is(err, store.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden")
		return true
	case errors.Is(err, db.ErrNotFound):
		writeErr(w, http.StatusNotFound, notFoundMsg)
		return true
	case errors.Is(err, store.ErrInvalidAccessMode):
		writeErr(w, http.StatusBadRequest, "access_mode must be 'open', 'api_key', 'burrow_login', or 'mtls'")
		return true
	case errors.Is(err, store.ErrServiceNotHTTP):
		writeErr(w, http.StatusConflict, "api_key, burrow_login, and mtls require an http service")
		return true
	case errors.Is(err, store.ErrMTLSCARequired):
		writeErr(w, http.StatusBadRequest, "mtls access mode requires mtls_ca_pem")
		return true
	case errors.Is(err, store.ErrInvalidMTLSCAPEM):
		writeErr(w, http.StatusBadRequest, "invalid CA PEM")
		return true
	case errors.Is(err, store.ErrInvalidSlug):
		writeErr(w, http.StatusBadRequest, auth.SlugRule)
		return true
	case errors.Is(err, store.ErrDirectService):
		writeErr(w, http.StatusConflict, "this service backs a direct AI provider; its slug and access mode cannot be changed")
		return true
	case errors.Is(err, store.ErrSlugTaken):
		writeErr(w, http.StatusConflict, "slug already in use")
		return true
	case errors.Is(err, store.ErrNameRequired):
		writeErr(w, http.StatusBadRequest, "name is required")
		return true
	case errors.Is(err, store.ErrUnknownRole):
		writeErr(w, http.StatusBadRequest, "unknown role")
		return true
	}
	return false
}

// ListServices handles GET /api/v1/services.
func (d Deps) ListServices(w http.ResponseWriter, r *http.Request) {
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	svcs, err := d.Services.ListServices(r.Context(), uid, role)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]serviceResp, 0, len(svcs))
	for _, sv := range svcs {
		// The backing row of a direct AI provider is not a service anyone
		// tunnels; it is managed under AI providers.
		if sv.Type == "direct" {
			continue
		}
		snap := d.composeLive(sv.ID)
		out = append(out, serviceResp{
			ID:           sv.ID,
			Name:         sv.Name,
			Type:         sv.Type,
			Slug:         sv.Subdomain,
			URL:          composeServiceURL(sv.Subdomain, d.AuthDomain),
			AccessMode:   sv.AccessMode,
			APIKeyHeader: sv.APIKeyHeader,
			Connected:    snap.Connected,
			RemotePort:   snap.RemotePort,
			LocalAddr:    snap.LocalAddr,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// GetService handles GET /api/v1/services/{serviceID}.
func (d Deps) GetService(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	det, err := d.Services.GetService(r.Context(), uid, role, serviceID)
	if err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	snap := d.composeLive(det.ID)
	policy := det.AccessPolicy
	if policy == nil {
		policy = []string{}
	}
	resp := serviceDetailResp{
		serviceResp: serviceResp{
			ID:           det.ID,
			Name:         det.Name,
			Type:         det.Type,
			Slug:         det.Subdomain,
			URL:          composeServiceURL(det.Subdomain, d.AuthDomain),
			AccessMode:   det.AccessMode,
			APIKeyHeader: det.APIKeyHeader,
			Connected:    snap.Connected,
			RemotePort:   snap.RemotePort,
			LocalAddr:    snap.LocalAddr,
		},
		APIKeyCount:  det.APIKeyCount,
		AccessPolicy: policy,
	}
	writeJSON(w, http.StatusOK, resp)
}

type setAccessModeReq struct {
	AccessMode   string `json:"access_mode"`
	APIKeyHeader string `json:"api_key_header"`
	// MTLSCAPEM is the operator-supplied PEM-encoded trust anchor used to
	// verify visitor client certs in mtls mode. Required when AccessMode
	// is "mtls"; ignored for any other mode. Burrow does NOT sign client
	// certs in v0.4.0.
	MTLSCAPEM string `json:"mtls_ca_pem"`
}

// SetServiceAccessMode handles PUT /api/v1/services/{serviceID}/access-mode.
//
// Accepts {access_mode: "open"|"api_key"|"burrow_login"|"mtls"}. For mtls
// the request body MUST carry a non-empty mtls_ca_pem with at least one
// CERTIFICATE block; the store layer validates the PEM and returns 400 on
// bad input.
func (d Deps) SetServiceAccessMode(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	// 16 KiB accommodates a multi-cert PEM bundle while still bounding
	// memory growth (a CA + a few intermediates fit easily).
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var in setAccessModeReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.AccessMode == "" {
		writeErr(w, http.StatusBadRequest, "access_mode is required")
		return
	}
	// Spec Q4 RESOLVED: reject burrow_login when no auth_domain is configured.
	if in.AccessMode == "burrow_login" && d.AuthDomain == "" {
		writeErr(w, http.StatusConflict, "burrow_login requires a configured auth_domain")
		return
	}
	if in.AccessMode == "mtls" && !d.HostRouting {
		writeErr(w, http.StatusConflict, "mTLS needs host routing, which is disabled on this relay")
		return
	}
	// api_key_header is only honored for api_key mode (spec Part C). When set,
	// validate against RFC 7230 token rules so we never persist an
	// uninterpretable header name (e.g. "Authorization: Bearer" with a colon).
	header := ""
	if in.AccessMode == "api_key" {
		header = in.APIKeyHeader
		if header != "" && !isValidHTTPHeaderName(header) {
			writeErr(w, http.StatusBadRequest, "api_key_header must be a valid HTTP header name (RFC 7230 token)")
			return
		}
	}
	// mtls_ca_pem is only honored for mtls mode.
	var caPEM []byte
	if in.AccessMode == "mtls" {
		caPEM = []byte(in.MTLSCAPEM)
	}
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	if err := d.Services.SetServiceAccessMode(r.Context(), uid, role, serviceID, in.AccessMode, header, caPEM); err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListAPIKeys handles GET /api/v1/services/{serviceID}/api-keys.
func (d Deps) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	keys, err := d.Services.ListAPIKeys(r.Context(), uid, role, serviceID)
	if err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	out := make([]apiKeyResp, len(keys))
	for i, k := range keys {
		out[i] = apiKeyResp{
			ID:        k.ID,
			Name:      k.Name,
			LastUsed:  k.LastUsed,
			CreatedAt: k.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type createAPIKeyReq struct {
	Name string `json:"name"`
}

// CreateAPIKey handles POST /api/v1/services/{serviceID}/api-keys.
func (d Deps) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in createAPIKeyReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	id, plaintext, err := d.Services.CreateAPIKey(r.Context(), uid, role, serviceID, in.Name)
	if err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, createAPIKeyResp{
		ID:   id,
		Name: in.Name,
		Key:  plaintext,
	})
}

// DeleteAPIKey handles DELETE /api/v1/services/{serviceID}/api-keys/{id}.
func (d Deps) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	keyID := chi.URLParam(r, "id")
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	if err := d.Services.DeleteAPIKey(r.Context(), uid, role, serviceID, keyID); err != nil {
		if !mapServiceErr(w, err, "api key not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetAccessPolicy handles GET /api/v1/services/{serviceID}/access-policy.
func (d Deps) GetAccessPolicy(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	roles, err := d.Services.GetAccessPolicy(r.Context(), uid, role, serviceID)
	if err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if roles == nil {
		roles = []string{}
	}
	writeJSON(w, http.StatusOK, accessPolicyResp{Roles: roles})
}

type setAccessPolicyReq struct {
	Roles []string `json:"roles"`
}

// ─── v0.5.2 P3.6 / Task 9: admin POST /api/v1/services ──────────────────────

// postServiceReq is the wire shape of POST /api/v1/services. service_id is
// REQUIRED and validated against serviceIDRe; title and access_mode are
// optional (access_mode defaults to "public"). slug is optional too: when
// omitted, one is generated the first time the client connects.
type postServiceReq struct {
	ServiceID  string `json:"service_id"`
	Title      string `json:"title,omitempty"`
	AccessMode string `json:"access_mode,omitempty"`
	Slug       string `json:"slug,omitempty"`
}

// serviceIDRe is the validation regex for the pre-provisioning service_id.
// Lowercase alphanum + underscore/hyphen, 3-64 chars. Tighter than the legacy
// client-supplied name field (which the GetOrCreateService upsert path keeps
// permissive). Mirrors the docs/openapi.yaml constraint exactly.
var serviceIDRe = regexp.MustCompile(`^[a-z0-9_-]{3,64}$`)

// postServiceAccessModes is the closed set of access_mode values accepted by
// POST /services. Mirrors the openapi.yaml enum. "open"/"api_key"/
// "burrow_login"/"mtls" are the runtime enum on the services row, but the
// admin POST surfaces the more intuitive "public" alias (mapped to "open"
// in the stored row) so dashboards / e2e flows can use the shape from the
// spec without translating.
// NOTE: "mtls" is intentionally excluded — it requires mtls_ca_pem material
// the POST body does not carry; admins set mtls via PUT /access-mode after
// pre-provisioning the row + uploading the CA bundle.
var postServiceAccessModes = map[string]string{
	"public":       "open",
	"open":         "open",
	"api_key":      "api_key",
	"burrow_login": "burrow_login",
}

// PostService handles POST /api/v1/services — admin pre-provisioning of a
// service row (v0.5.2 P3.6 / Task 9). Permission: admin (gated by
// RequireAdmin in router.go; the handler does no second check).
//
// Body: { service_id (req), title?, access_mode?, slug? }.
//   - service_id matches ^[a-z0-9_-]{3,64}$.
//   - title is stored on the services.name column (max 120 chars).
//   - access_mode defaults to "public" (mapped to the stored enum value "open").
//   - slug matches auth.ValidSlug; omitted means generated at first connect.
//
// On UNIQUE violation returns 409. On success returns 201 with
// { id, slug, created_at }. The audit event is `service.create` with payload
// `{by_admin: true}` so downstream consumers can distinguish admin
// pre-provisioning from the implicit client-side GetOrCreateService path.
func (d Deps) PostService(w http.ResponseWriter, r *http.Request) {
	// 8 KiB accommodates any reasonable title; far below the JSON timeout
	// budget. Mirrors createAPIKey's MaxBytesReader posture.
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var in postServiceReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate service_id.
	if !serviceIDRe.MatchString(in.ServiceID) {
		writeErr(w, http.StatusBadRequest, "service_id must match ^[a-z0-9_-]{3,64}$")
		return
	}

	// Default + validate access_mode.
	if in.AccessMode == "" {
		in.AccessMode = "public"
	}
	storedMode, ok := postServiceAccessModes[in.AccessMode]
	if !ok {
		writeErr(w, http.StatusBadRequest, "access_mode must be one of: public, open, api_key, burrow_login")
		return
	}
	if storedMode == "burrow_login" && d.AuthDomain == "" {
		// Mirrors the SetServiceAccessMode rejection in the same handler
		// file — burrow_login requires a configured auth_domain.
		writeErr(w, http.StatusConflict, "burrow_login requires a configured auth_domain")
		return
	}

	// Validate title length.
	if len(in.Title) > 120 {
		writeErr(w, http.StatusBadRequest, "title must be at most 120 chars")
		return
	}
	if in.Slug != "" && !auth.ValidSlug(in.Slug) {
		writeErr(w, http.StatusBadRequest, auth.SlugRule)
		return
	}

	// Store availability check.
	if d.Services == nil {
		writeErr(w, http.StatusInternalServerError, "service store unavailable")
		return
	}

	// Compose the row. user_id is the authenticated admin so the service is
	// owned by the operator that pre-provisioned it (consistent with how the
	// client-side GetOrCreateService path scopes ownership to the caller).
	now := time.Now().UTC()
	row := db.Service{
		ID:           in.ServiceID,
		UserID:       userID(r.Context()),
		Name:         in.Title,
		Type:         "http",  // pre-provisioned rows are http by default; tcp services are bound by the client at connect time
		Subdomain:    in.Slug, // empty means generated when the client first connects
		AccessMode:   storedMode,
		APIKeyHeader: "Authorization",
		CreatedAt:    now,
	}
	if err := d.Services.CreateService(r.Context(), row); err != nil {
		if errors.Is(err, db.ErrDuplicateService) {
			writeErr(w, http.StatusConflict, "service id or slug already in use")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Audit (best-effort). The action is ActionServiceCreate ("service.create");
	// the payload carries `by_admin: true` so consumers can distinguish admin
	// pre-provisioning from the implicit client-side GetOrCreateService path.
	if d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceCreate,
			SubjectID: row.ID, SubjectLabel: row.Name,
			Result:   "ok",
			SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(map[string]any{
				"by_admin":    true,
				"access_mode": storedMode,
			}),
		})
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         row.ID,
		"slug":       in.Slug,
		"created_at": now.Format(time.RFC3339),
	})
}

// SetAccessPolicy handles PUT /api/v1/services/{serviceID}/access-policy.
func (d Deps) SetAccessPolicy(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var in setAccessPolicyReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if in.Roles == nil {
		in.Roles = []string{}
	}
	// Spec Part D: pre-validate each role before calling the store so the error
	// body can include the offending role name: {"error":"unknown role \"<r>\""}
	for _, roleName := range in.Roles {
		if _, ok := authz.Get(roleName); !ok {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown role %q", roleName))
			return
		}
	}
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	if err := d.Services.SetAccessPolicy(r.Context(), uid, role, serviceID, in.Roles); err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isValidHTTPHeaderName reports whether s is a non-empty RFC 7230 token
// (the grammar for HTTP header field names). The valid set is:
//
//	tchar = "!" / "#" / "$" / "%" / "&" / "'" / "*" / "+" / "-" / "." / "^"
//	      / "_" / "`" / "|" / "~" / DIGIT / ALPHA
//
// In particular: NO colon, NO whitespace, NO control characters.
func isValidHTTPHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '!' || c == '#' || c == '$' || c == '%' || c == '&' || c == '\'':
		case c == '*' || c == '+' || c == '-' || c == '.' || c == '^' || c == '_':
		case c == '`' || c == '|' || c == '~':
		default:
			return false
		}
	}
	return true
}

type setSlugReq struct {
	Slug string `json:"slug"`
}

// PutServiceSlug handles PUT /api/v1/services/{serviceID}/slug. The old URL
// stops working immediately; there is no redirect.
func (d Deps) PutServiceSlug(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var in setSlugReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	old, err := d.Services.SetServiceSlug(r.Context(), userID(r.Context()), role, serviceID, in.Slug)
	if err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if d.AuditAppender != nil && old != in.Slug {
		lc := audit.LogContextFrom(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceSlugChange,
			SubjectID: serviceID,
			Result:    "ok",
			SourceIP:  lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(map[string]any{"old": old, "new": in.Slug}),
		})
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"slug": in.Slug,
		"url":  composeServiceURL(in.Slug, d.AuthDomain),
	})
}

// GetSlugSuggestion handles GET /api/v1/services/slug-suggestion.
func (d Deps) GetSlugSuggestion(w http.ResponseWriter, r *http.Request) {
	slug, err := d.Services.SuggestSlug(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"slug": slug})
}
