package api

// tcp_hostname_handlers.go — the TLS passthrough host name of a tcp service
// (tcp complete, E1). A client that names it in its ClientHello on the
// relay's passthrough listener reaches the service, so a new name widens
// who can reach it: the write asks for a recent confirmation and is no
// management MCP tool (x-burrow-mcp: false). The read is, and says every
// name the service has there and whether they reach it.

import (
	"context"
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
// hostname_only says the relay opens no public port for the service while a
// name reaches it (it is reached by host name only).
type tcpHostnameResp struct {
	TCPHostname  string   `json:"tcp_hostname"`
	Hostnames    []string `json:"hostnames"`
	Passthrough  bool     `json:"passthrough"`
	Reachable    bool     `json:"reachable"`
	HostnameOnly bool     `json:"hostname_only"`
	// ShadowedBy says who owns a name of the service that the http side
	// serves instead (H12): "custom_domain" when a custom domain covers it,
	// "relay" when the relay serves it itself on its HTTPS port (only while
	// the 443 mux runs). Absent when no name is shadowed. A shadowed name
	// reaches no tcp service on the mux; the first owner found is reported.
	// Only while the 443 mux runs: the dedicated listener does not consult
	// custom domains.
	ShadowedBy string `json:"shadowed_by,omitempty"`
	// ReachableOn says where a client naming the service reaches it now:
	// "https" (the 443 mux, with at least one name not shadowed and the
	// lookup healthy) and the dedicated listener's address. Reachable is
	// true when it is not empty.
	ReachableOn []string `json:"reachable_on"`
}

// tcpHostnameReq sets the host name ("" clears it), whether the service is
// reached by host name only, or both; at least one.
type tcpHostnameReq struct {
	TCPHostname  *string `json:"tcp_hostname"`
	HostnameOnly *bool   `json:"hostname_only"`
}

// tcpHostnames are the names the passthrough listener knows the service by,
// by the relay's rule (server.passthroughNames): <slug>.<tcp domain>, and
// the custom name unless it is the tcp domain or under it (a name stored
// there before the domain was set never routes: those names are the
// slugs').
func (d Deps) tcpHostnames(h store.TCPHostname) []string {
	out := []string{}
	dom := strings.ToLower(strings.TrimSuffix(d.TCPDomain, "."))
	if dom != "" && h.Slug != "" && !strings.Contains(h.Slug, ".") {
		out = append(out, h.Slug+"."+dom)
	}
	if n := h.Hostname; n != "" && (dom == "" || (n != dom && !strings.HasSuffix(n, "."+dom))) {
		out = append(out, n)
	}
	return out
}

func (d Deps) tcpHostnameView(ctx context.Context, h store.TCPHostname) tcpHostnameResp {
	out := tcpHostnameResp{TCPHostname: h.Hostname, Hostnames: d.tcpHostnames(h), Passthrough: d.TLSPassthrough, HostnameOnly: h.HostnameOnly, ReachableOn: []string{}}
	open := h.AccessMode == "" || h.AccessMode == "open" || h.AccessMode == "ip"
	// The http side wins a name only on the 443 mux; the dedicated listener
	// does not consult custom domains or the relay's names.
	free := 0
	for _, n := range out.Hostnames {
		by, unknown := d.shadowedBy(ctx, n)
		if by != "" && out.ShadowedBy == "" {
			out.ShadowedBy = by
		}
		if by == "" && !unknown {
			free++
		}
	}
	if open && len(out.Hostnames) > 0 {
		if d.TLSPassthroughMux && free > 0 {
			out.ReachableOn = append(out.ReachableOn, "https")
		}
		if d.TLSPassthroughListen != "" {
			out.ReachableOn = append(out.ReachableOn, d.TLSPassthroughListen)
		} else if d.TLSPassthrough && !d.TLSPassthroughMux {
			out.ReachableOn = append(out.ReachableOn, "passthrough_listener")
		}
	}
	out.Reachable = len(out.ReachableOn) > 0
	return out
}

// domainCoverer is what a CustomDomains store that can say a custom domain
// covers a name provides (*db.DB does).
type domainCoverer interface {
	CustomDomainCovers(ctx context.Context, name string) (bool, error)
}

// shadowedBy says which owner the http side wins a tcp name with on the
// 443 mux, in the mux's order (H11 2-3): "relay", then "custom_domain"; ""
// for none. Without the mux nothing shadows. unknown is true when the
// lookup failed: the mux terminates then, so the name does not count as
// reachable there, and no owner is claimed.
func (d Deps) shadowedBy(ctx context.Context, name string) (by string, unknown bool) {
	if !d.TLSPassthroughMux {
		return "", false
	}
	if d.RelayNames.Owns(name) {
		return "relay", false
	}
	if c, ok := d.CustomDomains.(domainCoverer); ok {
		covers, err := c.CustomDomainCovers(ctx, name)
		if err != nil {
			return "", true
		}
		if covers {
			return "custom_domain", false
		}
	}
	return "", false
}

// underName reports whether name is dom or lies under it.
func underName(name, dom string) bool {
	dom = strings.ToLower(strings.TrimSuffix(dom, "."))
	return dom != "" && (name == dom || strings.HasSuffix(name, "."+dom))
}

// hostnameOnlyRefusal is why a service with h cannot be reached by host name
// only on this relay, "" when it can: the relay needs the passthrough
// listener, and the service a name on it.
func (d Deps) hostnameOnlyRefusal(h store.TCPHostname) string {
	if !d.TLSPassthrough {
		return "hostname_only needs the relay's TLS passthrough listener (BURROW_TLS_PASSTHROUGH_LISTEN); without it the service has no way in but its port"
	}
	if len(d.tcpHostnames(h)) == 0 {
		return "hostname_only needs a host name the service is reached by: set tcp_hostname, or ask the operator for BURROW_TCP_DOMAIN (the slug name)"
	}
	return ""
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
	writeJSON(w, http.StatusOK, d.tcpHostnameView(r.Context(), h))
}

// PutServiceTCPHostname handles PUT /api/v1/services/{serviceID}/tcp-hostname:
// tcp_hostname, a host name such as rdp.example.com, or "" for none, and
// hostname_only, true to open no public port for the service (reached by
// host name only), false for the port again; at least one. The name is
// stored lower case and is unique among services; a name of the relay's own
// domains or of a custom domain is refused. Setting one (not clearing it)
// needs services:configure:any: an owner with :own alone gets the slug name
// under the tcp domain only. hostname_only may be true only while the relay
// has the passthrough listener and the service a name on it (400 otherwise),
// and the only name of such a service is not cleared. The relay applies both
// at once. The store audits each write (service.tcp_hostname.update,
// service.tcp_hostname_only.update).
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
	if in.TCPHostname == nil && in.HostnameOnly == nil {
		writeErr(w, http.StatusBadRequest, "tcp_hostname or hostname_only is required")
		return
	}
	name := ""
	if in.TCPHostname != nil && *in.TCPHostname != "" {
		var ok bool
		if name, ok = store.NormalizeTCPHostname(*in.TCPHostname); !ok {
			writeErr(w, http.StatusBadRequest, "tcp_hostname must be a host name such as rdp.example.com")
			return
		}
		if dom := d.reservedTCPHostname(name); dom != "" {
			writeErr(w, http.StatusBadRequest, "tcp_hostname must not be "+dom+" or a name under it: the relay gives those names out itself")
			return
		}
		for _, acme := range d.ACMENames {
			if underName(name, acme) {
				writeErr(w, http.StatusConflict, "tcp_hostname "+name+" is, or lies under, the ACME name "+acme+": the relay obtains certificates for it")
				return
			}
		}
		if d.RelayNames.Owns(name) {
			writeErr(w, http.StatusConflict, "tcp_hostname "+name+" is a name the relay serves itself on its HTTPS port (its certificate or ACME names)")
			return
		}
	}
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	ctx, uid := r.Context(), userID(r.Context())
	// What the service will be after the write, checked before anything is
	// written: by host name only needs a name that reaches it. Only a write
	// that turns the port off or clears a name can break that.
	if (in.HostnameOnly != nil && *in.HostnameOnly) || (in.TCPHostname != nil && *in.TCPHostname == "") {
		cur, err := d.Services.ServiceTCPHostname(ctx, uid, role, serviceID)
		if err != nil {
			if !mapServiceErr(w, err, "service not found") {
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
			return
		}
		next := cur
		if in.TCPHostname != nil {
			next.Hostname = name
		}
		if in.HostnameOnly != nil {
			next.HostnameOnly = *in.HostnameOnly
		}
		if why := d.hostnameOnlyRefusal(next); next.HostnameOnly && why != "" {
			if in.HostnameOnly == nil {
				why = "the service is reached by host name only (hostname_only) and this is its only name: turn hostname_only off first, or give it another name"
			}
			writeErr(w, http.StatusBadRequest, why)
			return
		}
	}
	var after store.TCPHostname
	if in.TCPHostname != nil {
		_, after, err = d.Services.SetServiceTCPHostname(ctx, uid, role, serviceID, *in.TCPHostname)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrInvalidTCPHostname):
				writeErr(w, http.StatusBadRequest, "tcp_hostname must be a host name such as rdp.example.com")
			case errors.Is(err, store.ErrTCPHostnameTaken):
				writeErr(w, http.StatusConflict, "tcp_hostname is used by another service")
			case errors.Is(err, store.ErrTCPHostnameReserved):
				writeErr(w, http.StatusConflict, "tcp_hostname is a custom domain of a service, or under one: an http service owns that name")
			case errors.Is(err, store.ErrTCPHostnameNeedsAny):
				writeErr(w, http.StatusForbidden, "a tcp_hostname of its own needs services:configure:any; the service is reached by its slug name under the tcp domain")
			case !mapServiceErr(w, err, "service not found"):
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
			return
		}
	}
	if in.HostnameOnly != nil {
		_, after, err = d.Services.SetServiceTCPHostnameOnly(ctx, uid, role, serviceID, *in.HostnameOnly)
		if err != nil {
			// A name written just before is told to the relay all the same.
			if in.TCPHostname != nil {
				d.refreshTCP(ctx, serviceID)
			}
			if !mapServiceErr(w, err, "service not found") {
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
			return
		}
	}
	d.refreshTCP(ctx, serviceID)
	writeJSON(w, http.StatusOK, d.tcpHostnameView(ctx, after))
}
