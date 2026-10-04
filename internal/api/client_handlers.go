package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

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
// When Deps.ControlListen is empty (legacy boot, tests) the handler
// falls back to the request Host header so the wizard still copy-pastes
// something usable on a single-host dev deploy.
//
// When ControlListen is port-only (e.g. ":7000" — relay bound to every
// interface), the host part is empty. Splice in the dashboard request's
// hostname so the wizard renders a copyable host:port instead of a
// malformed ":7000" string.
func (d Deps) GetConnectInfo(w http.ResponseWriter, r *http.Request) {
	server := d.ControlListen
	if server == "" {
		server = r.Host
	} else if strings.HasPrefix(server, ":") {
		// Strip an existing :port off r.Host (Chrome sends "host:8080") and
		// glue it to ControlListen's port.
		host := r.Host
		if i := strings.LastIndex(host, ":"); i > 0 {
			host = host[:i]
		}
		if host != "" {
			server = host + server
		}
	}
	writeJSON(w, http.StatusOK, ConnectInfo{Server: server})
}

// ListClients returns live clients grouped with aggregate traffic (admin only).
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

// GetClient returns one client + its services (admin only).
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
			callerRole, err := d.callerRole(r)
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
