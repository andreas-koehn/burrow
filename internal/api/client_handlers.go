package api

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// ConnectInfo is the response shape for GET /api/v1/clients/connect-info.
// It tells the "Connect a client" wizard what `--server` argument to put
// into the generated `burrow connect …` command. Distinct from the HTTP
// dashboard host:port — the control plane listens on a different port
// (e.g. :7000 in the bundled compose stack; :8080 serves the dashboard).
type ConnectInfo struct {
	Server string `json:"server"`
}

// GetConnectInfo returns the relay control-plane endpoint clients should
// dial. Session-authed (any signed-in user can render the wizard).
//
// The value is Deps.controlEndpoint's, which the public discovery endpoint
// returns too.
func (d Deps) GetConnectInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ConnectInfo{Server: d.controlEndpoint(r)})
}

// ListClients returns live clients grouped with aggregate traffic (tunnels:read:any).
// GET /api/v1/clients
func (d Deps) ListClients(w http.ResponseWriter, r *http.Request) {
	var out []ClientView
	if d.Clients != nil {
		out = d.Clients.ListClients()
	}
	if out == nil {
		out = []ClientView{}
	}
	writeJSON(w, http.StatusOK, out)
}

// GetClient returns one client + its services (tunnels:read:any).
// GET /api/v1/clients/{sessionID}
func (d Deps) GetClient(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "sessionID")
	if d.Clients == nil {
		writeErr(w, http.StatusNotFound, "client not found")
		return
	}
	cd, ok := d.Clients.GetClient(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "client not found")
		return
	}
	if cd.Services == nil {
		cd.Services = []ClientServiceView{}
	}
	writeJSON(w, http.StatusOK, cd)
}

// ListKnownClients returns every client token with its status: online with
// its live sessions, offline with the last session the relay remembers, or
// never connected (tunnels:read:any, like ListClients).
// GET /api/v1/clients/known
func (d Deps) ListKnownClients(w http.ResponseWriter, r *http.Request) {
	out := []KnownClient{}
	if d.Clients != nil {
		list, err := d.Clients.ListKnownClients(r.Context())
		if err != nil {
			d.Log.Error("list known clients failed", "err", err)
			writeErr(w, http.StatusInternalServerError, "list failed")
			return
		}
		out = append(out, list...)
	}
	for i := range out {
		if out[i].Served == nil {
			out[i].Served = []string{}
		}
		if out[i].Sessions == nil {
			out[i].Sessions = []ClientView{}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// KnownClients joins the client tokens with the live sessions by token id:
//
//   - a token with a live session is online: its platform is that of the
//     newest session, it serves what its sessions serve, and last_seen is the
//     stored one;
//   - a token that was seen and has no live session is offline, with what
//     the relay stored of its last session;
//   - a token that was never seen is never connected and shows no platform;
//   - a live session without a token (it was revoked meanwhile, or the relay
//     does not know its id) is online with an empty token id. Sessions that
//     name the same revoked token are one row.
//
// Online rows come first (by name), then the offline ones (last seen first),
// then those that never connected (by name).
func KnownClients(tokens []db.ClientToken, live []LiveClient) []KnownClient {
	byToken := map[string][]LiveClient{}
	var orphans [][]LiveClient
	for _, lc := range live {
		if lc.TokenID == "" {
			orphans = append(orphans, []LiveClient{lc})
			continue
		}
		byToken[lc.TokenID] = append(byToken[lc.TokenID], lc)
	}
	out := make([]KnownClient, 0, len(tokens)+len(orphans))
	for _, ct := range tokens {
		kc := KnownClient{
			TokenID: ct.ID, TokenName: ct.Name, UserID: ct.UserID, Status: ClientNeverConnected,
			Served: []string{}, Sessions: []ClientView{},
		}
		sessions := byToken[ct.ID]
		delete(byToken, ct.ID)
		switch {
		case len(sessions) > 0:
			kc.LastSeen = ct.LastSeen
			kc.setOnline(sessions)
		case ct.LastSeen != nil:
			kc.Status, kc.LastSeen = ClientOffline, ct.LastSeen
			kc.OS, kc.Arch, kc.ClientVersion, kc.RemoteAddr = ct.LastOS, ct.LastArch, ct.LastClientVersion, ct.LastRemoteAddr
			kc.Served = append(kc.Served, ct.LastServed...)
		}
		out = append(out, kc)
	}
	// What is left names a token that is gone.
	for _, sessions := range byToken {
		orphans = append(orphans, sessions)
	}
	for _, sessions := range orphans {
		var kc KnownClient
		kc.setOnline(sessions)
		kc.TokenName, kc.UserID = kc.Sessions[0].TokenName, kc.Sessions[0].UserID
		out = append(out, kc)
	}

	rank := map[string]int{ClientOnline: 0, ClientOffline: 1, ClientNeverConnected: 2}
	newest := func(kc KnownClient) string {
		if len(kc.Sessions) == 0 {
			return ""
		}
		return kc.Sessions[0].SessionID
	}
	slices.SortStableFunc(out, func(a, b KnownClient) int {
		if c := cmp.Compare(rank[a.Status], rank[b.Status]); c != 0 {
			return c
		}
		if a.Status == ClientOffline {
			if c := b.LastSeen.Compare(*a.LastSeen); c != 0 {
				return c
			}
		}
		return cmp.Or(
			cmp.Compare(a.TokenName, b.TokenName),
			cmp.Compare(a.TokenID, b.TokenID),
			cmp.Compare(newest(a), newest(b)),
		)
	})
	return out
}

// setOnline fills the row from the live sessions of one token (at least one).
func (kc *KnownClient) setOnline(sessions []LiveClient) {
	sessions = slices.Clone(sessions)
	slices.SortFunc(sessions, func(a, b LiveClient) int {
		return cmp.Or(b.ConnectedAt.Compare(a.ConnectedAt), cmp.Compare(a.SessionID, b.SessionID))
	})
	first := sessions[0]
	kc.Status = ClientOnline
	kc.OS, kc.Arch, kc.ClientVersion, kc.RemoteAddr = first.OS, first.Arch, first.ClientVersion, first.RemoteAddr
	if kc.LastSeen == nil && !first.ConnectedAt.IsZero() {
		// Nothing is stored for it (yet): it was seen when it connected.
		at := first.ConnectedAt.UTC()
		kc.LastSeen = &at
	}
	kc.Served, kc.Sessions = []string{}, make([]ClientView, 0, len(sessions))
	for _, s := range sessions {
		kc.Sessions = append(kc.Sessions, s.ClientView)
		for _, name := range s.Served {
			if name != "" {
				kc.Served = append(kc.Served, name)
			}
		}
	}
	slices.Sort(kc.Served)
	kc.Served = slices.Compact(kc.Served)
}

type accessModeReq struct {
	AccessMode string `json:"access_mode"`
}

// SetAccessMode sets a service's per-service access mode. PUT
// /api/v1/tunnels/{id}/access-mode. Scoped to the caller (own tunnels);
// 'open' is runtime-effective, 'api_key'/'burrow_login' are accepted and
// persisted but inert in v0.2.0.
//
// v0.3 back-compat: when d.LiveTunnels is wired and the tunnel ID resolves to
// a live entry, the handler delegates to d.Services.SetServiceAccessMode on
// the service row (the v0.3 canonical path). If d.LiveTunnels is nil or the
// tunnel has no live entry, it falls back to the legacy
// d.AccessModes.SetTunnelAccessMode path (keeps v0.2 tests green).
func (d Deps) SetAccessMode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var in accessModeReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.AccessMode == "" {
		writeErr(w, http.StatusBadRequest, "access_mode is required")
		return
	}
	if in.AccessMode == "mtls" && !d.HostRouting {
		writeErr(w, http.StatusConflict, "mTLS needs host routing, which is disabled on this relay")
		return
	}

	// v0.3 delegation path: resolve tunnelID → serviceID via live registry.
	if d.LiveTunnels != nil {
		if loc, ok := d.LiveTunnels.LookupByTunnelID(id); ok {
			callerRole, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal error")
				return
			}
			uid := userID(r.Context())
			if err := d.Services.SetServiceAccessMode(r.Context(), uid, callerRole, loc.ServiceID, in.AccessMode, "", nil); err != nil {
				if !mapServiceErr(w, err, "service not found") {
					writeErr(w, http.StatusInternalServerError, "set access mode failed")
				}
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// Legacy v0.2 path: write the tunnels.access_mode column directly.
	err := d.AccessModes.SetTunnelAccessMode(r.Context(), id, userID(r.Context()), in.AccessMode)
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "tunnel not found")
		return
	}
	if err != nil {
		// store.ErrInvalidAccessMode surfaces as a 400 (its message contains
		// the allowed set); any other error is a 500.
		if strings.Contains(err.Error(), "access_mode must be") || errors.Is(err, store.ErrInvalidAccessMode) {
			writeErr(w, http.StatusBadRequest, "access_mode must be 'open', 'api_key', or 'burrow_login'")
			return
		}
		writeErr(w, http.StatusInternalServerError, "set access mode failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
