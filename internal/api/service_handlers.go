package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

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
	// GatewayOnly: reachable through the AI gateway only; url is then "".
	GatewayOnly bool   `json:"gateway_only"`
	Connected   bool   `json:"connected"`
	RemotePort  int    `json:"remote_port"`
	LocalAddr   string `json:"local_addr"`
	// Status says in one word what connected and last_seen say together:
	// see servedState.
	Status string `json:"status"`
	// LastSeen is when a client last served the service (null when none ever
	// did) and LastClient the name of the client token of that client.
	LastSeen   *time.Time `json:"last_seen"`
	LastClient string     `json:"last_client"`
	// ClientSessionID is the session that serves the service now, "" when
	// none does.
	ClientSessionID string `json:"client_session_id"`
	// ProviderSlug is the slug of the tunnel provider the service backs, ""
	// when it backs none.
	ProviderSlug string `json:"provider_slug"`
	// MaxConnections is a tcp service's cap on connected visitors; 0 is the
	// relay's default (32). Always 0 for other types.
	MaxConnections int `json:"max_connections"`
}

// What a service or a tunnel provider reports of the client behind it: the
// three states of a KnownClient.
const (
	servedOnline         = "online"
	servedOffline        = "offline"
	servedNeverConnected = "never_connected"
)

// servedState is the status of anything served through a client: "online"
// when a live tunnel serves it; else "offline" when a client served it
// before; else "never_connected".
func servedState(live bool, lastSeen *time.Time) string {
	switch {
	case live:
		return servedOnline
	case lastSeen != nil:
		return servedOffline
	}
	return servedNeverConnected
}

// serviceView joins a stored service with its live tunnel. providerSlug is
// the slug of the tunnel provider it backs (see providerSlugByService).
func (d Deps) serviceView(sv store.ServiceView, providerSlug string) serviceResp {
	snap := d.composeLive(sv.ID)
	v := serviceResp{
		ID:           sv.ID,
		Name:         sv.Name,
		Type:         sv.Type,
		Slug:         sv.Subdomain,
		URL:          d.serviceURL(sv.Type, sv.Subdomain, sv.GatewayOnly),
		GatewayOnly:  sv.GatewayOnly,
		AccessMode:   sv.AccessMode,
		APIKeyHeader: sv.APIKeyHeader,
		Connected:    snap.Connected,
		RemotePort:   snap.RemotePort,
		LocalAddr:    snap.LocalAddr,
		Status:       servedState(snap.Connected, sv.LastSeen),
		LastSeen:     sv.LastSeen,
		LastClient:   sv.LastClient,
		ProviderSlug: providerSlug,

		MaxConnections: sv.MaxConnections,
	}
	if snap.Connected {
		v.ClientSessionID = snap.SessionID
	}
	return v
}

// providerSlugByService maps a service id to the slug of the tunnel provider
// that service backs, with one providers query for the whole response. Empty
// when the provider store is not wired or the query fails (logged): the
// services still render.
func (d Deps) providerSlugByService(ctx context.Context) map[string]string {
	out := map[string]string{}
	if d.AIProviders == nil {
		return out
	}
	providers, err := d.AIProviders.ListProviders(ctx)
	if err != nil {
		d.warn("service view: provider list failed", "err", err)
		return out
	}
	for _, p := range providers {
		if p.Kind != "direct" {
			out[p.ServiceID] = p.Slug
		}
	}
	return out
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

// serviceURL is the direct address of a service: "" for a gateway-only one,
// which has no /svc/ address that works, and for a tcp service: its slug is
// the name a forward reaches it by, never an address over HTTP.
func (d Deps) serviceURL(typ, slug string, gatewayOnly bool) string {
	if gatewayOnly || typ == "tcp" {
		return ""
	}
	return composeServiceURL(slug, d.servicesHost())
}

// servicesHost is the host /svc/ is served on: the services domain when one
// is set, the auth domain otherwise, and "" when there is no auth domain (the
// /svc/ routes are then not mounted at all).
func (d Deps) servicesHost() string {
	if d.AuthDomain == "" {
		return ""
	}
	if d.ServicesDomain != "" {
		return d.ServicesDomain
	}
	return d.AuthDomain
}

// composeServiceURL returns "https://<host>/svc/<slug>/" when both parts are
// set, and "" otherwise (tcp service, no slug yet, or no host).
func composeServiceURL(slug, host string) string {
	if slug == "" || host == "" {
		return ""
	}
	return "https://" + host + "/svc/" + slug + "/"
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
		writeErr(w, http.StatusBadRequest, "access_mode must be 'open', 'api_key', 'burrow_login', or 'mtls' (http), or 'open', 'ip', or 'private' (tcp)")
		return true
	case errors.Is(err, store.ErrTCPAccessMode):
		writeErr(w, http.StatusConflict, "a tcp service takes the access modes open, ip and private")
		return true
	case errors.Is(err, store.ErrHTTPAccessMode):
		writeErr(w, http.StatusConflict, "ip and private are access modes of a tcp service")
		return true
	case errors.Is(err, store.ErrServiceNotTCP):
		writeErr(w, http.StatusConflict, "this setting applies to tcp services only")
		return true
	case errors.Is(err, store.ErrInvalidMaxConnections):
		writeErr(w, http.StatusBadRequest, "max_connections must be 0 (the default, 32) or 1 to 256")
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
		writeErr(w, http.StatusConflict, "this service backs a direct AI provider; its slug, access mode and gateway-only setting cannot be changed")
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
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
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
	providerSlugs := d.providerSlugByService(r.Context())
	out := make([]serviceResp, 0, len(svcs))
	for _, sv := range svcs {
		// The backing row of a direct AI provider is not a service anyone
		// tunnels; it is managed under AI providers.
		if sv.Type == "direct" {
			continue
		}
		out = append(out, d.serviceView(sv, providerSlugs[sv.ID]))
	}
	writeJSON(w, http.StatusOK, out)
}

// GetService handles GET /api/v1/services/{serviceID}.
func (d Deps) GetService(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
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
	policy := det.AccessPolicy
	if policy == nil {
		policy = []string{}
	}
	resp := serviceDetailResp{
		serviceResp:  d.serviceView(det.ServiceView, d.providerSlugByService(r.Context())[det.ID]),
		APIKeyCount:  det.APIKeyCount,
		AccessPolicy: policy,
	}
	writeJSON(w, http.StatusOK, resp)
}

// errServiceConnected is the 409 of a delete while a client serves the service.
const errServiceConnected = "the service is connected; stop the client or remove the tunnel from its config, then delete"

// DeleteService handles DELETE /api/v1/services/{serviceID}. The owner, or a
// role that may configure any service, deletes; anyone else gets the 404 of a
// service that is not there.
//
// A service that a client serves now is refused with 409: a client creates the
// service again, in mode open and without the dashboard's settings, when it
// registers the tunnel name next, so deleting a connected service only
// replaces it with an open one. Likewise 409 when an AI provider uses the
// service: delete the provider (and the models that use it) first.
//
// Afterwards the certificates of its custom domains leave the SNI cache, the
// in-memory state under its id goes, and a session that connected in the
// meantime is closed.
func (d Deps) DeleteService(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	// Reads the service with the permission check of the delete: whoever may
	// not delete it is not told anything about it.
	det, err := d.Services.GetService(r.Context(), uid, role, serviceID)
	if errors.Is(err, store.ErrForbidden) || errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "service not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if d.composeLive(serviceID).Connected {
		writeErr(w, http.StatusConflict, errServiceConnected)
		return
	}
	// The hostnames are read first: the rows go with the service.
	var domains []db.ServiceCustomDomain
	if d.CustomDomains != nil {
		rows, err := d.CustomDomains.ListCustomDomains(r.Context(), serviceID)
		if err != nil {
			d.warn("service delete: custom domains not listed", "service_id", serviceID, "err", err)
		}
		domains = rows
	}
	name, err := d.Services.DeleteService(r.Context(), uid, role, serviceID)
	switch {
	case errors.Is(err, store.ErrForbidden), errors.Is(err, db.ErrNotFound):
		writeErr(w, http.StatusNotFound, "service not found")
		return
	case errors.Is(err, store.ErrServiceHasProvider):
		writeErr(w, http.StatusConflict, "an AI provider still uses this service ("+
			errDetail(err, store.ErrServiceHasProvider)+"); delete the provider, and the models that use it, first")
		return
	case errors.Is(err, store.ErrServiceHasMCPSource):
		writeErr(w, http.StatusConflict, "a tool source still uses this service ("+
			errDetail(err, store.ErrServiceHasMCPSource)+"); delete the tool source first")
		return
	case errors.Is(err, store.ErrProviderInUse):
		writeErr(w, http.StatusConflict, "models still use this service's provider ("+
			errDetail(err, store.ErrProviderInUse)+"); remove them first")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	d.endCustomDomains(domains)
	// A client that connected after the check above.
	if d.ServiceSessions != nil {
		if n := d.ServiceSessions.CloseByService(serviceID); n > 0 {
			d.Log.Info("service sessions closed", "service_id", serviceID, "sessions", n)
		}
	}
	if d.OnServiceDeleted != nil {
		d.OnServiceDeleted(serviceID)
	}
	if d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceDelete,
			SubjectID: serviceID, SubjectLabel: name,
			Result:   "ok",
			SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(map[string]string{"slug": det.Subdomain, "owner_id": det.UserID}),
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// errDetail is the text an error adds after its sentinel ("sentinel: detail").
func errDetail(err, sentinel error) string {
	_, detail, _ := strings.Cut(err.Error(), sentinel.Error()+": ")
	return detail
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
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	// What the service is now: a tcp service whose mode changes has its
	// live tunnel closed. A failed read leaves it to the store to answer.
	before, _ := d.Services.GetService(r.Context(), uid, role, serviceID)
	if err := d.Services.SetServiceAccessMode(r.Context(), uid, role, serviceID, in.AccessMode, header, caPEM); err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.afterAccessModeChange(r.Context(), serviceID, before, in.AccessMode)
	w.WriteHeader(http.StatusNoContent)
}

// ListAPIKeys handles GET /api/v1/services/{serviceID}/api-keys.
func (d Deps) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
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
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
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
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
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
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
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
//
// With type "tcp" the row is a tcp service instead (postTCPService): name is
// required, service_id, title and slug are not used, access_mode is one of
// open, ip, private (default private), tcp_port pins the public port and
// proxy_protocol and idle_timeout_s are its first settings.
type postServiceReq struct {
	ServiceID  string `json:"service_id"`
	Title      string `json:"title,omitempty"`
	AccessMode string `json:"access_mode,omitempty"`
	Slug       string `json:"slug,omitempty"`
	// Type is "http" (the default) or "tcp"; the fields below are tcp's.
	Type          string `json:"type,omitempty"`
	Name          string `json:"name,omitempty"`
	TCPPort       *int   `json:"tcp_port,omitempty"`
	ProxyProtocol string `json:"proxy_protocol,omitempty"`
	IdleTimeoutS  *int   `json:"idle_timeout_s,omitempty"`
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
// service row (v0.5.2 P3.6 / Task 9). Permission: services:configure:any
// (routeAccess in access.go; the handler does no second check).
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

	switch in.Type {
	case "", "http":
		if in.Name != "" || in.TCPPort != nil || in.ProxyProtocol != "" || in.IdleTimeoutS != nil {
			writeErr(w, http.StatusBadRequest, "name, tcp_port, proxy_protocol and idle_timeout_s belong to type tcp")
			return
		}
	case "tcp":
		d.postTCPService(w, r, in)
		return
	default:
		writeErr(w, http.StatusBadRequest, `type must be "http" or "tcp"`)
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

// tcpNameRe is what a tcp service may be called: the name `burrow tcp --name`
// registers, which a forward and the yaml refer to.
var tcpNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// postTCPService creates a tcp service before any client connects. It is the
// row a registration of that name later finds: same owner (the caller), a
// generated slug, the mode and, when asked for, a pinned port. The relay
// holds the port for the service from the start, so no other service takes it.
// Not an MCP tool, like the rest of the route: open and ip widen access.
func (d Deps) postTCPService(w http.ResponseWriter, r *http.Request, in postServiceReq) {
	if in.ServiceID != "" || in.Title != "" || in.Slug != "" {
		writeErr(w, http.StatusBadRequest, "a tcp service takes name, not service_id, title or slug")
		return
	}
	if !tcpNameRe.MatchString(in.Name) {
		writeErr(w, http.StatusBadRequest, "name must be 1 to 64 characters: letters, digits, dot, underscore, hyphen")
		return
	}
	mode := in.AccessMode
	if mode == "" {
		mode = "private"
	}
	if mode != "open" && mode != "ip" && mode != "private" {
		writeErr(w, http.StatusBadRequest, "access_mode of a tcp service must be one of: open, ip, private")
		return
	}
	// open and ip put a port on the internet: like the access-mode change,
	// they need the password confirmed (private does not).
	if mode != "private" && !d.reauthSatisfied(r) {
		writeErr(w, http.StatusForbidden, reauthRequired)
		return
	}
	row := db.Service{
		ID: uuid.NewString(), UserID: userID(r.Context()), Name: in.Name, Type: "tcp", AccessMode: mode,
		APIKeyHeader: "Authorization", ProxyProtocol: in.ProxyProtocol,
	}
	if in.TCPPort != nil {
		p := *in.TCPPort
		if d.TCPPortMax > 0 && (p < d.TCPPortMin || p > d.TCPPortMax) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("tcp_port must be %d to %d", d.TCPPortMin, d.TCPPortMax))
			return
		}
		if p < 1 || p > 65535 {
			writeErr(w, http.StatusBadRequest, "tcp_port must be a port number")
			return
		}
		row.TCPPort, row.TCPPortPinned = p, true
	}
	if in.ProxyProtocol != "" && in.ProxyProtocol != "v2" {
		writeErr(w, http.StatusBadRequest, `proxy_protocol must be "" (none) or "v2"`)
		return
	}
	if in.IdleTimeoutS != nil {
		n := *in.IdleTimeoutS
		if n != 0 && (n < store.MinTCPIdleTimeoutS || n > store.MaxTCPIdleTimeoutS) {
			writeErr(w, http.StatusBadRequest, "idle_timeout_s must be 0 (never) or 60 to 86400")
			return
		}
		row.IdleTimeoutS = n
	}
	if d.Services == nil {
		writeErr(w, http.StatusInternalServerError, "service store unavailable")
		return
	}
	slug, err := auth.GenerateSlug()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	row.Subdomain = slug
	row.CreatedAt = time.Now().UTC()
	if err := d.Services.CreateService(r.Context(), row); err != nil {
		switch {
		case errors.Is(err, db.ErrTCPPortReserved):
			writeErr(w, http.StatusConflict, fmt.Sprintf("tcp_port %d is held by another service", row.TCPPort))
		case errors.Is(err, db.ErrDuplicateService):
			writeErr(w, http.StatusConflict, "a service named "+strconv.Quote(in.Name)+" exists already (tcp and http services share names per owner), or the generated slug was taken: choose another name")
		default:
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		payload := map[string]any{"by_admin": true, "type": "tcp", "access_mode": mode}
		if row.TCPPort != 0 {
			payload["tcp_port"] = row.TCPPort
		}
		if row.ProxyProtocol != "" {
			payload["proxy_protocol"] = row.ProxyProtocol
		}
		if in.IdleTimeoutS != nil {
			payload["idle_timeout_s"] = row.IdleTimeoutS
		}
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceCreate,
			SubjectID: row.ID, SubjectLabel: row.Name,
			Result:   "ok",
			SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(payload),
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         row.ID,
		"slug":       slug,
		"created_at": row.CreatedAt.Format(time.RFC3339),
	})
}

// roleKnown reports whether name is a role: a built-in one or a stored custom
// role.
func (d Deps) roleKnown(ctx context.Context, name string) (bool, error) {
	if _, ok := authz.Get(name); ok {
		return true, nil
	}
	if d.Roles == nil {
		return false, nil
	}
	if _, err := d.Roles.GetRole(ctx, name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
		known, err := d.roleKnown(r.Context(), roleName)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !known {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown role %q", roleName))
			return
		}
	}
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
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
	// The policy is the forward policy of a tcp service: a role taken out
	// of it ends that role's live forwards.
	d.recheckServiceForwards(r.Context(), serviceID)
	w.WriteHeader(http.StatusNoContent)
}

// isValidHTTPHeaderName reports whether s is a non-empty RFC 7230 token (the
// grammar for HTTP header field names): NO colon, NO whitespace, NO control
// characters. The rule lives in the store package, where the MCP tool that
// sets an access mode reaches it too.
func isValidHTTPHeaderName(s string) bool { return store.ValidHTTPHeaderName(s) }

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
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	old, gatewayOnly, err := d.Services.SetServiceSlug(r.Context(), userID(r.Context()), role, serviceID, in.Slug)
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
		"url":  d.serviceURL("http", in.Slug, gatewayOnly), // only an http service has a slug to set
	})
}

type setGatewayOnlyReq struct {
	GatewayOnly *bool `json:"gateway_only"`
}

// PutServiceGatewayOnly handles PUT /api/v1/services/{serviceID}/gateway-only.
// A gateway-only service answers 404 at /svc/<slug>/ and on custom domains and
// stays reachable through the AI gateway. Same permission as the other service
// settings (owner, or a role that may configure any service).
func (d Deps) PutServiceGatewayOnly(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var in setGatewayOnlyReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.GatewayOnly == nil {
		writeErr(w, http.StatusBadRequest, "gateway_only (boolean) is required")
		return
	}
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	on := *in.GatewayOnly
	if err := d.Services.SetServiceGatewayOnly(r.Context(), userID(r.Context()), role, serviceID, on); err != nil {
		if errors.Is(err, store.ErrServiceNotHTTP) {
			writeErr(w, http.StatusConflict, "gateway-only requires an http service")
			return
		}
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceGatewayOnlyUpdate,
			SubjectID: serviceID,
			Result:    "ok",
			SourceIP:  lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(map[string]any{"gateway_only": on}),
		})
	}
	writeJSON(w, http.StatusOK, map[string]bool{"gateway_only": on})
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
