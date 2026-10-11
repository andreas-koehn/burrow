package api

// ipgeo_handlers.go — per-service IP/Geo CRUD + the global /geo/status surface
// (Spec Part J). These are the JSON endpoints the dashboard uses to manage the
// CIDR allow/block lists + country lists per service. mTLS lives on the
// existing PUT /access-mode endpoint (extended in service_handlers.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
)

// IPGeoStore is the narrow CRUD surface the ip-geo handlers consume. *db.DB
// satisfies it via GetServiceIPGeo + SetServiceIPGeo.
type IPGeoStore interface {
	GetServiceIPGeo(ctx context.Context, serviceID string) (db.ServiceIPGeoConfig, error)
	SetServiceIPGeo(ctx context.Context, cfg db.ServiceIPGeoConfig) error
}

// ServiceOwnerLookup is the narrow ownership surface. *db.DB satisfies it via
// GetServiceByID (we only need the user_id). This is shared with other v0.4.0
// handlers that gate :own permissions.
type ServiceOwnerLookup interface {
	GetServiceByID(ctx context.Context, id string) (db.Service, error)
}

// ipGeoResp is the wire shape of GET /api/v1/services/{id}/ip-geo. Empty
// arrays are emitted as [] (never null) so the UI can iterate unconditionally.
type ipGeoResp struct {
	Enabled        bool     `json:"enabled"`
	AllowCIDRs     []string `json:"allow_cidrs"`
	BlockCIDRs     []string `json:"block_cidrs"`
	AllowCountries []string `json:"allow_countries"`
	BlockCountries []string `json:"block_countries"`
}

// ipGeoReq is the PUT body shape. All four lists are accepted; the country
// lists are stored for forward-compat with the geo build tag (Task 17) but
// the default build's middleware ignores them.
type ipGeoReq struct {
	Enabled        bool     `json:"enabled"`
	AllowCIDRs     []string `json:"allow_cidrs"`
	BlockCIDRs     []string `json:"block_cidrs"`
	AllowCountries []string `json:"allow_countries"`
	BlockCountries []string `json:"block_countries"`
}

// geoStatusResp is the wire shape of GET /api/v1/geo/status. enabled=false in
// the default build (no geo tag); Task 17 will surface enabled=true + the
// MMDB path + age once it lights up the lookup behind the build tag.
type geoStatusResp struct {
	Enabled      bool   `json:"enabled"`
	DBPath       string `json:"db_path"`
	DBAgeSeconds *int64 `json:"db_age_seconds"`
}

// ensureIPGeoServiceAccess gates a per-service IP/Geo handler.
//
//   - admin or PermIPGeoManageAny → allow + optionally surface 404 cleanly.
//   - PermIPGeoManageOwn          → allow only when the caller owns the service.
//   - else                        → 403 forbidden.
//
// Returns true to continue; false to indicate the handler already wrote the
// response (caller MUST return).
func (d Deps) ensureIPGeoServiceAccess(w http.ResponseWriter, r *http.Request, serviceID string) bool {
	if serviceID == "" {
		writeErr(w, http.StatusBadRequest, "service id is required")
		return false
	}
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return false
	}
	uid := userID(r.Context())

	hasAny := effectivePerms(r.Context(), role, authz.PermIPGeoManageAny)
	hasOwn := effectivePerms(r.Context(), role, authz.PermIPGeoManageOwn)
	if !hasAny && !hasOwn {
		writeErr(w, http.StatusForbidden, "forbidden")
		return false
	}
	if d.IPGeoServices == nil {
		writeErr(w, http.StatusInternalServerError, "service lookup unavailable")
		return false
	}
	svc, err := d.IPGeoServices.GetServiceByID(r.Context(), serviceID)
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "service not found")
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "service lookup failed")
		return false
	}
	if !hasAny && svc.UserID != uid {
		writeErr(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

// GetServiceIPGeo handles GET /api/v1/services/{serviceID}/ip-geo.
func (d Deps) GetServiceIPGeo(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if !d.ensureIPGeoServiceAccess(w, r, serviceID) {
		return
	}
	if d.IPGeo == nil {
		writeErr(w, http.StatusInternalServerError, "ip-geo store unavailable")
		return
	}
	cfg, err := d.IPGeo.GetServiceIPGeo(r.Context(), serviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load ip-geo failed")
		return
	}
	writeJSON(w, http.StatusOK, ipGeoResp{
		Enabled:        cfg.Enabled,
		AllowCIDRs:     nonNilStrings(cfg.AllowCIDRs),
		BlockCIDRs:     nonNilStrings(cfg.BlockCIDRs),
		AllowCountries: nonNilStrings(cfg.AllowCountries),
		BlockCountries: nonNilStrings(cfg.BlockCountries),
	})
}

// PutServiceIPGeo handles PUT /api/v1/services/{serviceID}/ip-geo.
//
// Validation order: gating → JSON decode → CIDR validity → country codes →
// write. Empty/nil arrays are normalised to [] before persistence so a
// subsequent GET returns the same shape.
func (d Deps) PutServiceIPGeo(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if !d.ensureIPGeoServiceAccess(w, r, serviceID) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var in ipGeoReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.AllowCIDRs == nil {
		in.AllowCIDRs = []string{}
	}
	if in.BlockCIDRs == nil {
		in.BlockCIDRs = []string{}
	}
	if in.AllowCountries == nil {
		in.AllowCountries = []string{}
	}
	if in.BlockCountries == nil {
		in.BlockCountries = []string{}
	}
	// CIDR validation (compiles via proxy.CompileIPGeoPolicy so the API
	// rejects every CIDR/host syntax error the engine would otherwise hit
	// at request time).
	if _, err := proxy.CompileIPGeoPolicy(in.AllowCIDRs, in.BlockCIDRs, in.AllowCountries, in.BlockCountries); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Country code validation: ISO-3166-1 alpha-2.
	for _, c := range in.AllowCountries {
		if err := proxy.ValidateCountryCode(c); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid allow_countries entry %q: %s", c, err.Error()))
			return
		}
	}
	for _, c := range in.BlockCountries {
		if err := proxy.ValidateCountryCode(c); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid block_countries entry %q: %s", c, err.Error()))
			return
		}
	}
	if d.IPGeo == nil {
		writeErr(w, http.StatusInternalServerError, "ip-geo store unavailable")
		return
	}
	if err := d.IPGeo.SetServiceIPGeo(r.Context(), db.ServiceIPGeoConfig{
		ServiceID:      serviceID,
		Enabled:        in.Enabled,
		AllowCIDRs:     in.AllowCIDRs,
		BlockCIDRs:     in.BlockCIDRs,
		AllowCountries: in.AllowCountries,
		BlockCountries: in.BlockCountries,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "save ip-geo failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetGeoStatus handles GET /api/v1/geo/status. Session-authed; any user may
// read the global geo-lookup status (mirrors the cache/redaction settings
// reads).
//
// In the default build (no geo tag) enabled=false and db_path="". Task 17
// surfaces real values when the MMDBGeoLookup is wired.
func (d Deps) GetGeoStatus(w http.ResponseWriter, r *http.Request) {
	g := d.GeoLookup
	if g == nil {
		g = proxy.NoopGeoLookup()
	}
	resp := geoStatusResp{
		Enabled: g.Enabled(),
		DBPath:  g.DBPath(),
	}
	if g.Enabled() {
		age := g.DBAgeSeconds()
		resp.DBAgeSeconds = &age
	}
	writeJSON(w, http.StatusOK, resp)
}

// nonNilStrings returns a non-nil slice (possibly empty) for JSON encoding.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
