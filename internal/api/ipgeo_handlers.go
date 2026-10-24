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

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/server"
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

// ipGeoView is ipGeoResp with whether its country rules are enforced: false
// when it has country rules and no GeoIP database is loaded.
type ipGeoView struct {
	Enabled              bool     `json:"enabled"`
	AllowCIDRs           []string `json:"allow_cidrs"`
	BlockCIDRs           []string `json:"block_cidrs"`
	AllowCountries       []string `json:"allow_countries"`
	BlockCountries       []string `json:"block_countries"`
	CountryRulesEnforced bool     `json:"country_rules_enforced"`
}

// geoEnabled reports whether a GeoIP database is loaded.
func (d Deps) geoEnabled() bool {
	return d.GeoLookup != nil && d.GeoLookup.Enabled()
}

// ipGeoReq is the PUT body shape. All four lists are accepted; the country
// lists need a GeoIP database (BURROW_GEOIP_DB) to be saved.
type ipGeoReq struct {
	Enabled        bool     `json:"enabled"`
	AllowCIDRs     []string `json:"allow_cidrs"`
	BlockCIDRs     []string `json:"block_cidrs"`
	AllowCountries []string `json:"allow_countries"`
	BlockCountries []string `json:"block_countries"`
}

// geoStatusResp is the wire shape of GET /api/v1/geo/status. enabled is true
// while a database is loaded. Configured says a path is set (BURROW_GEOIP_DB),
// Error why it could not be loaded, and RulesNotEnforced how many services
// have country rules that nothing enforces because no database is loaded.
type geoStatusResp struct {
	Enabled          bool   `json:"enabled"`
	Configured       bool   `json:"configured"`
	DBPath           string `json:"db_path"`
	DBAgeSeconds     *int64 `json:"db_age_seconds"`
	Error            string `json:"error,omitempty"`
	RulesNotEnforced int    `json:"rules_not_enforced"`
}

// ensureIPGeoServiceAccess scopes a per-service IP/Geo handler by owner. The
// gate of the route let the caller pass with ipgeo:manage:own or :any; by
// effectivePerms (a session: its role; a token: declared and granted):
//
//   - PermIPGeoManageAny → allow + surface 404 cleanly.
//   - PermIPGeoManageOwn → allow only when the caller owns the service.
//   - else               → 403 forbidden.
//
// Returns true to continue; false to indicate the handler already wrote the
// response (caller MUST return).
func (d Deps) ensureIPGeoServiceAccess(w http.ResponseWriter, r *http.Request, serviceID string) bool {
	_, ok := d.ipGeoService(w, r, serviceID)
	return ok
}

// ipGeoService is ensureIPGeoServiceAccess that also returns the service it
// read, so that a handler does not read it again.
func (d Deps) ipGeoService(w http.ResponseWriter, r *http.Request, serviceID string) (db.Service, bool) {
	fail := func(status int, msg string) (db.Service, bool) {
		writeErr(w, status, msg)
		return db.Service{}, false
	}
	if serviceID == "" {
		return fail(http.StatusBadRequest, "service id is required")
	}
	role, err := d.callerRole(r)
	if err != nil {
		return fail(http.StatusInternalServerError, "internal error")
	}
	uid := userID(r.Context())

	hasAny := effectivePerms(r.Context(), role, authz.PermIPGeoManageAny)
	hasOwn := effectivePerms(r.Context(), role, authz.PermIPGeoManageOwn)
	if !hasAny && !hasOwn {
		return fail(http.StatusForbidden, "forbidden")
	}
	if d.IPGeoServices == nil {
		return fail(http.StatusInternalServerError, "service lookup unavailable")
	}
	svc, err := d.IPGeoServices.GetServiceByID(r.Context(), serviceID)
	if errors.Is(err, db.ErrNotFound) {
		return fail(http.StatusNotFound, "service not found")
	}
	if err != nil {
		return fail(http.StatusInternalServerError, "service lookup failed")
	}
	if !hasAny && svc.UserID != uid {
		return fail(http.StatusForbidden, "forbidden")
	}
	return svc, true
}

// The widest allow entry of a tcp service: a /8 of IPv4, a /32 of IPv6. A
// wider one is all but open mode, which is chosen as a mode, not as a list.
// Block entries may be as wide as they like: they narrow access.
const (
	tcpAllowMinBits4 = 8
	tcpAllowMinBits6 = 32
)

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
	writeJSON(w, http.StatusOK, ipGeoView{
		Enabled:              cfg.Enabled,
		AllowCIDRs:           nonNilStrings(cfg.AllowCIDRs),
		BlockCIDRs:           nonNilStrings(cfg.BlockCIDRs),
		AllowCountries:       nonNilStrings(cfg.AllowCountries),
		BlockCountries:       nonNilStrings(cfg.BlockCountries),
		CountryRulesEnforced: d.geoEnabled() || (len(cfg.AllowCountries) == 0 && len(cfg.BlockCountries) == 0),
	})
}

// PutServiceIPGeo handles PUT /api/v1/services/{serviceID}/ip-geo.
//
// Validation order: gating → JSON decode → CIDR validity → country codes →
// write. Empty/nil arrays are normalised to [] before persistence so a
// subsequent GET returns the same shape.
func (d Deps) PutServiceIPGeo(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	svc, ok := d.ipGeoService(w, r, serviceID)
	if !ok {
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
	// Country rules cannot be enforced without a database, and a rule that is
	// not enforced is worse than none: a deny that lets everyone in. Clearing
	// the country lists is always possible.
	if len(in.AllowCountries)+len(in.BlockCountries) > 0 && !d.geoEnabled() {
		writeErr(w, http.StatusBadRequest, "country rules need a GeoIP database: no GeoLite2-Country .mmdb is loaded; set BURROW_GEOIP_DB to its path and restart, or save the rules without countries")
		return
	}
	if d.IPGeo == nil {
		writeErr(w, http.StatusInternalServerError, "ip-geo store unavailable")
		return
	}
	// A tcp service in mode ip compiles the lists on the relay, which must
	// read every entry (a block entry left out would let in what it blocks).
	if svc.Type == "tcp" {
		for _, c := range append(append([]string{}, in.AllowCIDRs...), in.BlockCIDRs...) {
			if _, err := server.ParseTCPPrefix(c); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		for _, c := range in.AllowCIDRs {
			p, _ := server.ParseTCPPrefix(c)
			if (p.Addr().Is4() && p.Bits() < tcpAllowMinBits4) || (p.Addr().Is6() && p.Bits() < tcpAllowMinBits6) {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("allow entry %q is too wide for a tcp service: at most a /8 of IPv4 or a /32 of IPv6; use the access mode open for a port open to all", c))
				return
			}
		}
	}
	before, err := d.IPGeo.GetServiceIPGeo(r.Context(), serviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load ip-geo failed")
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
	d.auditIPGeo(r, svc, before, ipGeoResp(in))
	// The lists are the permanent part of a tcp service's ip policy.
	d.refreshTCP(r.Context(), serviceID)
	w.WriteHeader(http.StatusNoContent)
}

// GetGeoStatus handles GET /api/v1/geo/status: the global geo-lookup status,
// for a caller with ipgeo:manage:own or :any. Country rules saved before the
// database went missing are not enforced; rules_not_enforced counts the
// services they belong to.
func (d Deps) GetGeoStatus(w http.ResponseWriter, r *http.Request) {
	g := d.GeoLookup
	if g == nil {
		g = proxy.NoopGeoLookup()
	}
	resp := geoStatusResp{
		Enabled:    g.Enabled(),
		Configured: g.DBPath() != "",
		DBPath:     g.DBPath(),
	}
	if g.Enabled() {
		age := g.DBAgeSeconds()
		resp.DBAgeSeconds = &age
	}
	if e, ok := g.(interface{ LastError() string }); ok {
		resp.Error = e.LastError()
	}
	if !g.Enabled() {
		if c, ok := d.IPGeo.(interface {
			CountCountryRules(ctx context.Context) (int, error)
		}); ok {
			n, err := c.CountCountryRules(r.Context())
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "count country rules failed")
				return
			}
			resp.RulesNotEnforced = n
		}
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

// auditIPGeo writes ipgeo.update with the rules before and after the change.
func (d Deps) auditIPGeo(r *http.Request, svc db.Service, before db.ServiceIPGeoConfig, after ipGeoResp) {
	if d.AuditAppender == nil {
		return
	}
	from := ipGeoResp{
		Enabled: before.Enabled, AllowCIDRs: nonNilStrings(before.AllowCIDRs), BlockCIDRs: nonNilStrings(before.BlockCIDRs),
		AllowCountries: nonNilStrings(before.AllowCountries), BlockCountries: nonNilStrings(before.BlockCountries),
	}
	lc := audit.LogContextFrom(r.Context())
	if err := d.AuditAppender.Append(r.Context(), audit.Event{
		ActorID: lc.ActorID, ActorEmail: lc.ActorEmail, Action: audit.ActionIPGeoUpdate,
		SubjectID: svc.ID, SubjectLabel: svc.Name, Result: "ok",
		SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
		Payload: audit.MustJSON(map[string]any{"from": from, "to": after}),
	}); err != nil {
		d.warn("audit write failed", "action", audit.ActionIPGeoUpdate, "err", err)
	}
}
