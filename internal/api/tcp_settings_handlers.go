package api

// tcp_settings_handlers.go — the settings of a tcp service that shape its
// connections (tcp complete): the PROXY protocol header its client writes
// to the target (E2) and the idle timeout (E4). Neither widens who may
// connect, so the write is no recent-auth route and stays a management MCP
// tool. A setting that widens access (a TLS passthrough host name) does not
// belong on this route.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/store"
)

// ClientProxyReader is the relay as the API reads the PROXY protocol the
// client that serves a tcp service writes by its own configuration
// (*server.Server). It is found on Deps.Connections.
type ClientProxyReader interface {
	ClientProxyProtocol(serviceID string) string
}

// tcpSettingsResp is the tcp settings of a service. client_proxy_protocol is
// "v2" while the client that serves it writes a PROXY v2 header by its own
// configuration (burrow.yaml proxy_protocol, --proxy-protocol), whatever
// proxy_protocol says; "" when it does not or no client serves it.
type tcpSettingsResp struct {
	ProxyProtocol       string `json:"proxy_protocol"`
	IdleTimeoutS        int    `json:"idle_timeout_s"`
	ClientProxyProtocol string `json:"client_proxy_protocol"`
}

// tcpSettingsReq changes the settings it names and keeps the others.
type tcpSettingsReq struct {
	ProxyProtocol *string `json:"proxy_protocol"`
	IdleTimeoutS  *int    `json:"idle_timeout_s"`
}

func (d Deps) tcpSettingsView(serviceID string, s store.TCPSettings) tcpSettingsResp {
	out := tcpSettingsResp{ProxyProtocol: s.ProxyProtocol, IdleTimeoutS: s.IdleTimeoutS}
	if l, ok := d.Connections.(ClientProxyReader); ok {
		out.ClientProxyProtocol = l.ClientProxyProtocol(serviceID)
	}
	return out
}

// GetServiceTCPSettings handles GET /api/v1/services/{serviceID}/tcp-settings.
func (d Deps) GetServiceTCPSettings(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s, err := d.Services.ServiceTCPSettings(r.Context(), userID(r.Context()), role, serviceID)
	if err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, d.tcpSettingsView(serviceID, s))
}

// PutServiceTCPSettings handles PUT /api/v1/services/{serviceID}/tcp-settings:
// proxy_protocol ("" or "v2") and idle_timeout_s (0, or 60 to 86400), either
// or both. The relay applies them without a new registration: the PROXY
// protocol to the next connection, the idle timeout to the open ones too.
// The store audits the write (service.tcp_settings.update).
func (d Deps) PutServiceTCPSettings(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in tcpSettingsReq
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if in.ProxyProtocol == nil && in.IdleTimeoutS == nil {
		writeErr(w, http.StatusBadRequest, "proxy_protocol or idle_timeout_s is required")
		return
	}
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	_, after, err := d.Services.SetServiceTCPSettings(r.Context(), userID(r.Context()), role, serviceID,
		store.TCPSettingsUpdate{ProxyProtocol: in.ProxyProtocol, IdleTimeoutS: in.IdleTimeoutS})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidProxyProtocol):
			writeErr(w, http.StatusBadRequest, `proxy_protocol must be "" (none) or "v2"`)
		case errors.Is(err, store.ErrInvalidIdleTimeout):
			writeErr(w, http.StatusBadRequest, "idle_timeout_s must be 0 (never) or 60 to 86400")
		case !mapServiceErr(w, err, "service not found"):
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.refreshTCP(r.Context(), serviceID)
	writeJSON(w, http.StatusOK, d.tcpSettingsView(serviceID, after))
}
