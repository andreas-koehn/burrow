package api

// http_settings_handlers.go — the settings of an http service that shape
// how the relay speaks to the local service (HTTP/2, gRPC and SNI on 443,
// H13): the upstream protocol, how a TLS upstream is verified, and the cap
// on a gRPC call. The write is no recent-auth route (nothing here widens who
// can reach the service) but it stays off the management MCP server: it sets
// a trust anchor and can switch verification off. GET /ingress/status
// reports the env-only ingress switches.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/store"
)

// ClientUpstreamReader is the relay as the API reads the upstream protocol
// the client that serves an http service named at registration
// (*server.Server). It is found on Deps.Connections.
type ClientUpstreamReader interface {
	ClientUpstreamProtocol(serviceID string) string
}

// IngressInfo is the env-only ingress configuration GET /ingress/status
// reports; cmd/server fills Deps.Ingress.
type IngressInfo struct {
	H2C               bool   // BURROW_HTTP_H2C
	HTTPSPassthrough  bool   // BURROW_TLS_PASSTHROUGH_ON_HTTPS
	PassthroughListen string // BURROW_TLS_PASSTHROUGH_LISTEN, "" for none
	ACME              bool   // built-in ACME (BURROW_ACME_DOMAIN)
}

// ingressStatusResp is the wire shape of GET /api/v1/ingress/status.
type ingressStatusResp struct {
	H2C               bool   `json:"h2c"`
	HTTPSPassthrough  bool   `json:"https_passthrough"`
	PassthroughListen string `json:"passthrough_listen"`
	ACME              bool   `json:"acme"`
}

// httpSettingsResp is the http settings of a service. The CA is never read
// back: upstream_tls_ca_sha256 is the SHA-256 (hex) of its PEM, "" for none.
// client_upstream_protocol is the protocol the client that serves the
// service named at registration ("" for none) and effective_upstream_protocol
// the one the relay uses now: the client's, else upstream_protocol.
type httpSettingsResp struct {
	UpstreamProtocol          string `json:"upstream_protocol"`
	UpstreamTLSVerify         bool   `json:"upstream_tls_verify"`
	UpstreamTLSServerName     string `json:"upstream_tls_server_name"`
	UpstreamTLSCASHA256       string `json:"upstream_tls_ca_sha256"`
	GRPCMaxDurationS          int    `json:"grpc_max_duration_s"`
	ClientUpstreamProtocol    string `json:"client_upstream_protocol"`
	EffectiveUpstreamProtocol string `json:"effective_upstream_protocol"`
}

// httpSettingsReq changes the settings it names and keeps the others. ""
// clears upstream_tls_server_name and upstream_tls_ca_pem.
type httpSettingsReq struct {
	UpstreamProtocol      *string `json:"upstream_protocol"`
	UpstreamTLSVerify     *bool   `json:"upstream_tls_verify"`
	UpstreamTLSServerName *string `json:"upstream_tls_server_name"`
	UpstreamTLSCAPEM      *string `json:"upstream_tls_ca_pem"`
	GRPCMaxDurationS      *int    `json:"grpc_max_duration_s"`
}

func (d Deps) httpSettingsView(serviceID string, s store.HTTPSettings) httpSettingsResp {
	out := httpSettingsResp{
		UpstreamProtocol: s.UpstreamProtocol, UpstreamTLSVerify: s.UpstreamTLSVerify,
		UpstreamTLSServerName: s.UpstreamTLSServerName, UpstreamTLSCASHA256: s.UpstreamTLSCASHA256,
		GRPCMaxDurationS: s.GRPCMaxDurationS,
	}
	if l, ok := d.Connections.(ClientUpstreamReader); ok {
		out.ClientUpstreamProtocol = l.ClientUpstreamProtocol(serviceID)
	}
	out.EffectiveUpstreamProtocol = proto.EffectiveUpstream(out.ClientUpstreamProtocol, s.UpstreamProtocol)
	return out
}

// mapHTTPSettingsErr writes the answer for an error of the http settings
// store; false if err is none of those.
func mapHTTPSettingsErr(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrInvalidUpstreamProtocol):
		writeErr(w, http.StatusBadRequest, "upstream_protocol must be http1, https, h2c or h2")
	case errors.Is(err, store.ErrInvalidGRPCMaxDuration):
		writeErr(w, http.StatusBadRequest, "grpc_max_duration_s must be 0 (no cap) or 1 to 86400")
	case errors.Is(err, store.ErrInvalidUpstreamCA):
		writeErr(w, http.StatusBadRequest, "upstream_tls_ca_pem must be PEM with a CERTIFICATE block, at most 64 KiB")
	case errors.Is(err, store.ErrInvalidUpstreamTLSServerName):
		writeErr(w, http.StatusBadRequest, "upstream_tls_server_name must be a DNS name (an IP address is none)")
	case errors.Is(err, store.ErrServiceNotHTTPSettings):
		writeErr(w, http.StatusConflict, "this setting applies to http services only")
	default:
		return false
	}
	return true
}

// GetServiceHTTPSettings handles GET /api/v1/services/{serviceID}/http-settings.
func (d Deps) GetServiceHTTPSettings(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s, err := d.Services.ServiceHTTPSettings(r.Context(), userID(r.Context()), role, serviceID)
	if err != nil {
		if !mapHTTPSettingsErr(w, err) && !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, d.httpSettingsView(serviceID, s))
}

// PutServiceHTTPSettings handles PUT /api/v1/services/{serviceID}/http-settings.
// The proxy reads the settings per request, so a change applies to the next
// one. The store audits the write (service.http_settings.update).
func (d Deps) PutServiceHTTPSettings(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	r.Body = http.MaxBytesReader(w, r.Body, 96<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in httpSettingsReq
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if in.UpstreamProtocol == nil && in.UpstreamTLSVerify == nil && in.UpstreamTLSServerName == nil &&
		in.UpstreamTLSCAPEM == nil && in.GRPCMaxDurationS == nil {
		writeErr(w, http.StatusBadRequest, "at least one of upstream_protocol, upstream_tls_verify, upstream_tls_server_name, upstream_tls_ca_pem, grpc_max_duration_s is required")
		return
	}
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	_, after, err := d.Services.SetServiceHTTPSettings(r.Context(), userID(r.Context()), role, serviceID, store.HTTPSettingsUpdate{
		UpstreamProtocol: in.UpstreamProtocol, UpstreamTLSVerify: in.UpstreamTLSVerify,
		UpstreamTLSServerName: in.UpstreamTLSServerName, UpstreamTLSCAPEM: in.UpstreamTLSCAPEM,
		GRPCMaxDurationS: in.GRPCMaxDurationS,
	})
	if err != nil {
		if !mapHTTPSettingsErr(w, err) && !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, d.httpSettingsView(serviceID, after))
}

// GetIngressStatus handles GET /api/v1/ingress/status: the env-only ingress
// switches, for a caller with services:configure:own or :any.
func (d Deps) GetIngressStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, ingressStatusResp{
		H2C: d.Ingress.H2C, HTTPSPassthrough: d.Ingress.HTTPSPassthrough,
		PassthroughListen: d.Ingress.PassthroughListen, ACME: d.Ingress.ACME,
	})
}
