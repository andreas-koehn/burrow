package api

// tcp_hostname_handlers.go — the TLS passthrough host name of a tcp service
// (tcp complete, E1). A client that names it in its ClientHello on the
// relay's passthrough listener reaches the service, so a new name widens
// who can reach it: the write asks for a recent confirmation and is no
// management MCP tool (x-burrow-mcp: false). The read is, and says every
// name the service has there and whether they reach it.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/store"
)

// tcpHostnameResp is the TLS passthrough host name of a service.
// hostnames are all the names the passthrough listener knows it by:
// <slug>.<BURROW_TCP_DOMAIN> when a tcp domain is set, and tcp_hostname.
// passthrough says whether the listener runs; reachable whether a client
// that names one of them reaches the service now (the listener runs and the
// service is in mode open or ip: a private service is never reached so).
type tcpHostnameResp struct {
	TCPHostname string   `json:"tcp_hostname"`
	Hostnames   []string `json:"hostnames"`
	Passthrough bool     `json:"passthrough"`
	Reachable   bool     `json:"reachable"`
}

// tcpHostnameReq sets the host name; "" clears it.
type tcpHostnameReq struct {
	TCPHostname *string `json:"tcp_hostname"`
}

func (d Deps) tcpHostnameView(h store.TCPHostname) tcpHostnameResp {
	out := tcpHostnameResp{TCPHostname: h.Hostname, Hostnames: []string{}, Passthrough: d.TLSPassthrough}
	if d.TCPDomain != "" && h.Slug != "" {
		out.Hostnames = append(out.Hostnames, h.Slug+"."+d.TCPDomain)
	}
	if h.Hostname != "" {
		out.Hostnames = append(out.Hostnames, h.Hostname)
	}
	open := h.AccessMode == "" || h.AccessMode == "open" || h.AccessMode == "ip"
	out.Reachable = d.TLSPassthrough && open && len(out.Hostnames) > 0
	return out
}

// reservedTCPHostname names the relay's own domain a host name is, or is
// under, "" for none: the tcp domain (whose names are the slugs'), the
// dashboard's domain and the services host, and the names under each. A passthrough name there would
// shadow a name the relay gives out itself. A custom domain of a service is
// refused by the store (store.ErrTCPHostnameReserved).
func (d Deps) reservedTCPHostname(name string) string {
	for _, dom := range []string{d.TCPDomain, d.AuthDomain} {
		dom = strings.ToLower(strings.TrimSuffix(dom, "."))
		if dom != "" && (name == dom || strings.HasSuffix(name, "."+dom)) {
			return dom
		}
	}
	if sd := strings.ToLower(d.ServicesDomain); sd != "" && (name == sd || strings.HasSuffix(name, "."+sd)) {
		return sd
	}
	return ""
}

// GetServiceTCPHostname handles GET /api/v1/services/{serviceID}/tcp-hostname.
func (d Deps) GetServiceTCPHostname(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h, err := d.Services.ServiceTCPHostname(r.Context(), userID(r.Context()), role, serviceID)
	if err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, d.tcpHostnameView(h))
}

// PutServiceTCPHostname handles PUT /api/v1/services/{serviceID}/tcp-hostname:
// tcp_hostname, a host name such as rdp.example.com, or "" for none. It is
// stored lower case and is unique among services; a name of the relay's own
// domains or of a custom domain is refused. Setting one (not clearing it)
// needs services:configure:any: an owner with :own alone gets the slug name
// under the tcp domain only. The relay applies it at once. The store audits the
// write (service.tcp_hostname.update).
func (d Deps) PutServiceTCPHostname(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in tcpHostnameReq
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if in.TCPHostname == nil {
		writeErr(w, http.StatusBadRequest, "tcp_hostname is required")
		return
	}
	if *in.TCPHostname != "" {
		name, ok := store.NormalizeTCPHostname(*in.TCPHostname)
		if !ok {
			writeErr(w, http.StatusBadRequest, "tcp_hostname must be a host name such as rdp.example.com")
			return
		}
		if dom := d.reservedTCPHostname(name); dom != "" {
			writeErr(w, http.StatusBadRequest, "tcp_hostname must not be "+dom+" or a name under it: the relay gives those names out itself")
			return
		}
	}
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	_, after, err := d.Services.SetServiceTCPHostname(r.Context(), userID(r.Context()), role, serviceID, *in.TCPHostname)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidTCPHostname):
			writeErr(w, http.StatusBadRequest, "tcp_hostname must be a host name such as rdp.example.com")
		case errors.Is(err, store.ErrTCPHostnameTaken):
			writeErr(w, http.StatusConflict, "tcp_hostname is used by another service")
		case errors.Is(err, store.ErrTCPHostnameReserved):
			writeErr(w, http.StatusConflict, "tcp_hostname is a custom domain of a service, or under one")
		case errors.Is(err, store.ErrTCPHostnameNeedsAny):
			writeErr(w, http.StatusForbidden, "a tcp_hostname of its own needs services:configure:any; the service is reached by its slug name under the tcp domain")
		case !mapServiceErr(w, err, "service not found"):
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.refreshTCP(r.Context(), serviceID)
	writeJSON(w, http.StatusOK, d.tcpHostnameView(after))
}
