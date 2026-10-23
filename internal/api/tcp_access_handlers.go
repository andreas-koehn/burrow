package api

// tcp_access_handlers.go — the access of tcp services (native tcp, P02):
// just-in-time grants of mode ip, the cap on connected visitors, and the
// hand-over of every such change to the relay, whose live tunnel applies it.
//
// Grants widen who may reach a port, so the routes that add one are not
// tools of the management MCP server and ask for a recent confirmation (as
// the access mode does).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// IPGrantStore is the surface of the just-in-time grants. *db.DB satisfies
// it.
type IPGrantStore interface {
	CreateIPGrant(ctx context.Context, g db.IPGrant) error
	ListIPGrants(ctx context.Context, serviceID string, now time.Time) ([]db.IPGrant, error)
	DeleteIPGrant(ctx context.Context, serviceID, id string) (db.IPGrant, error)
	PruneIPGrants(ctx context.Context, serviceID string, now time.Time) (int64, error)
}

// TCPServiceRefresher is the relay as the API tells it of a change to a tcp
// service. *server.Server satisfies it.
type TCPServiceRefresher interface {
	RefreshTCPService(ctx context.Context, serviceID string) error
}

// Bounds of a grant.
const (
	// MaxIPGrantTTLHours is the longest a grant lasts (7 days).
	MaxIPGrantTTLHours = 7 * 24
	// MaxActiveIPGrants is how many grants one service may have at once.
	MaxActiveIPGrants = 32
	// ipGrantMaxNote bounds the note of a grant.
	ipGrantMaxNote = 200
	// A grant is for one place: at most a /16 of IPv4 or a /48 of IPv6.
	// Wider ranges belong in the permanent list.
	ipGrantMinBits4 = 16
	ipGrantMinBits6 = 48
)

// tcpRefreshTimeout bounds what the relay reads when it is told of a change.
const tcpRefreshTimeout = 5 * time.Second

// refreshTCP tells the relay that the access of the service changed. A
// failure is logged: the relay then closes the tunnel (it fails closed), and
// the client registers again.
func (d Deps) refreshTCP(ctx context.Context, serviceID string) {
	if d.TCPServices == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tcpRefreshTimeout)
	defer cancel()
	if err := d.TCPServices.RefreshTCPService(ctx, serviceID); err != nil {
		d.warn("tcp service refresh failed; its tunnel was closed", "service_id", serviceID, "err", err)
	}
}

// afterAccessModeChange applies a new access mode of a tcp service to its
// live tunnel: the session that serves it is closed, so that its client
// registers again under the new mode (D14), and the relay is told, which
// also covers a registration under way. before is the service as it was.
func (d Deps) afterAccessModeChange(ctx context.Context, before store.ServiceDetail, mode string) {
	if before.Type != "tcp" || before.AccessMode == mode {
		return
	}
	if d.ServiceSessions != nil {
		d.ServiceSessions.CloseByService(before.ID)
	}
	d.refreshTCP(ctx, before.ID)
}

// ipGrantResp is a grant on the wire. Family is "ipv4" or "ipv6".
type ipGrantResp struct {
	ID        string    `json:"id"`
	CIDR      string    `json:"cidr"`
	Family    string    `json:"family"`
	ExpiresAt time.Time `json:"expires_at"`
	Note      string    `json:"note"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// ipGrantCreatedResp is the answer to a new grant: the grant, whether its
// address is the caller's, and a warning when that address cannot be the
// caller's public one.
type ipGrantCreatedResp struct {
	ipGrantResp
	FromCaller bool   `json:"from_caller"`
	Warning    string `json:"warning,omitempty"`
}

func grantResp(g db.IPGrant) ipGrantResp {
	fam := "ipv4"
	if p, err := netip.ParsePrefix(g.CIDR); err == nil && p.Addr().Is6() {
		fam = "ipv6"
	}
	return ipGrantResp{ID: g.ID, CIDR: g.CIDR, Family: fam, ExpiresAt: g.ExpiresAt, Note: g.Note, CreatedBy: g.CreatedBy, CreatedAt: g.CreatedAt}
}

type ipGrantReq struct {
	CIDR     string `json:"cidr"`
	TTLHours int    `json:"ttl_hours"`
	Note     string `json:"note"`
}

// tcpServiceForGrants gates a grant route as the ip-geo routes are gated
// (ipgeo:manage, the owner for :own) and returns the service, which must be
// a tcp service. false: the answer is written.
func (d Deps) tcpServiceForGrants(w http.ResponseWriter, r *http.Request) (db.Service, bool) {
	serviceID := chi.URLParam(r, "serviceID")
	if !d.ensureIPGeoServiceAccess(w, r, serviceID) {
		return db.Service{}, false
	}
	svc, err := d.IPGeoServices.GetServiceByID(r.Context(), serviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "service lookup failed")
		return db.Service{}, false
	}
	if svc.Type != "tcp" {
		writeErr(w, http.StatusConflict, "ip grants apply to tcp services")
		return db.Service{}, false
	}
	if d.IPGrants == nil {
		writeErr(w, http.StatusInternalServerError, "ip grant store unavailable")
		return db.Service{}, false
	}
	return svc, true
}

// ListIPGrants handles GET /api/v1/services/{serviceID}/ip-grants: the
// grants that have not expired, the first to expire first.
func (d Deps) ListIPGrants(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.tcpServiceForGrants(w, r)
	if !ok {
		return
	}
	grants, err := d.IPGrants.ListIPGrants(r.Context(), svc.ID, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load ip grants failed")
		return
	}
	out := make([]ipGrantResp, len(grants))
	for i, g := range grants {
		out[i] = grantResp(g)
	}
	writeJSON(w, http.StatusOK, out)
}

// PostIPGrant handles POST /api/v1/services/{serviceID}/ip-grants: a grant
// for cidr, or for the caller's own address when cidr is empty, for
// ttl_hours (1 to 168). The caller's address is the one the dashboard sees
// (after the trusted proxies): it may be IPv6 where the remote desktop client
// uses IPv4, or a proxy's; the answer says which family it is, and warns
// when it is not a public address.
func (d Deps) PostIPGrant(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.tcpServiceForGrants(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in ipGrantReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.TTLHours < 1 || in.TTLHours > MaxIPGrantTTLHours {
		writeErr(w, http.StatusBadRequest, "ttl_hours must be 1 to 168")
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	if len(in.Note) > ipGrantMaxNote {
		writeErr(w, http.StatusBadRequest, "note must be at most 200 bytes")
		return
	}
	fromCaller := strings.TrimSpace(in.CIDR) == ""
	var prefix netip.Prefix
	if fromCaller {
		a, err := netip.ParseAddr(remoteIP(r))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "the relay cannot read your address; give a cidr")
			return
		}
		a = a.Unmap().WithZone("")
		prefix = netip.PrefixFrom(a, a.BitLen())
	} else {
		p, err := server.ParseTCPPrefix(strings.TrimSpace(in.CIDR))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "cidr must be an address or a CIDR")
			return
		}
		prefix = p
	}
	if (prefix.Addr().Is4() && prefix.Bits() < ipGrantMinBits4) || (prefix.Addr().Is6() && prefix.Bits() < ipGrantMinBits6) {
		writeErr(w, http.StatusBadRequest, "a grant is for one place: at most a /16 of IPv4 or a /48 of IPv6; put wider ranges in the permanent list")
		return
	}
	now := time.Now()
	ctx := r.Context()
	if _, err := d.IPGrants.PruneIPGrants(ctx, svc.ID, now); err != nil {
		writeErr(w, http.StatusInternalServerError, "save ip grant failed")
		return
	}
	active, err := d.IPGrants.ListIPGrants(ctx, svc.ID, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save ip grant failed")
		return
	}
	if len(active) >= MaxActiveIPGrants {
		writeErr(w, http.StatusConflict, "the service has 32 grants; revoke one first")
		return
	}
	g := db.IPGrant{
		ID: uuid.NewString(), ServiceID: svc.ID, CIDR: prefix.String(),
		ExpiresAt: now.Add(time.Duration(in.TTLHours) * time.Hour).UTC().Truncate(time.Second),
		Note:      in.Note, CreatedBy: userID(ctx), CreatedAt: now.UTC(),
	}
	if err := d.IPGrants.CreateIPGrant(ctx, g); err != nil {
		writeErr(w, http.StatusInternalServerError, "save ip grant failed")
		return
	}
	d.auditGrant(r, audit.ActionServiceIPGrantAdd, svc, g, map[string]any{
		"grant_id": g.ID, "cidr": g.CIDR, "expires_at": g.ExpiresAt, "ttl_hours": in.TTLHours, "from_caller": fromCaller,
	})
	d.refreshTCP(ctx, svc.ID)
	resp := ipGrantCreatedResp{ipGrantResp: grantResp(g), FromCaller: fromCaller}
	if a := prefix.Addr(); a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || cgnat.Contains(a) {
		resp.Warning = "this is not a public address: the relay may see a proxy, a Docker gateway or a carrier NAT here rather than the address your remote desktop client connects from"
	}
	writeJSON(w, http.StatusCreated, resp)
}

// cgnat is the shared address space of carrier-grade NAT (RFC 6598).
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// DeleteIPGrant handles DELETE /api/v1/services/{serviceID}/ip-grants/{grantID}.
// New visitors from the range are refused at once; connected ones stay.
func (d Deps) DeleteIPGrant(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.tcpServiceForGrants(w, r)
	if !ok {
		return
	}
	g, err := d.IPGrants.DeleteIPGrant(r.Context(), svc.ID, chi.URLParam(r, "grantID"))
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "grant not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "revoke ip grant failed")
		return
	}
	d.auditGrant(r, audit.ActionServiceIPGrantRevoke, svc, g, map[string]any{
		"grant_id": g.ID, "cidr": g.CIDR, "expires_at": g.ExpiresAt,
	})
	d.refreshTCP(r.Context(), svc.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) auditGrant(r *http.Request, action string, svc db.Service, _ db.IPGrant, payload map[string]any) {
	if d.AuditAppender == nil {
		return
	}
	lc := audit.LogContextFrom(r.Context())
	if err := d.AuditAppender.Append(r.Context(), audit.Event{
		ActorID: lc.ActorID, ActorEmail: lc.ActorEmail, Action: action,
		SubjectID: svc.ID, SubjectLabel: svc.Name, Result: "ok",
		SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
		Payload: audit.MustJSON(payload),
	}); err != nil {
		d.warn("audit write failed", "action", action, "err", err)
	}
}

type maxConnectionsReq struct {
	MaxConnections *int `json:"max_connections"`
}

// PutServiceMaxConnections handles PUT /api/v1/services/{serviceID}/max-connections:
// the cap on connected visitors of a tcp service, 1 to 256, or 0 for the
// relay's default (32). It applies to the next visitor; connected ones stay.
func (d Deps) PutServiceMaxConnections(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var in maxConnectionsReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.MaxConnections == nil {
		writeErr(w, http.StatusBadRequest, "max_connections is required")
		return
	}
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := d.Services.SetServiceMaxConnections(r.Context(), userID(r.Context()), role, serviceID, *in.MaxConnections); err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.refreshTCP(r.Context(), serviceID)
	w.WriteHeader(http.StatusNoContent)
}
