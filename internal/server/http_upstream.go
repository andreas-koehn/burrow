package server

import "github.com/ankoehn/burrow/internal/proto"

// The upstream protocol of an http tunnel (HTTP/2 and gRPC, H4). The relay's
// proxy speaks it to the local service over the tunnel stream; the client
// only pipes bytes. A client may name one at registration, which then applies
// to its tunnel in place of the service's stored setting; the relay does not
// store it.

// errUpstreamProtocol is the refusal of a registration that names an unknown
// upstream protocol.
func errUpstreamProtocol(p string) string {
	return "unknown upstream_protocol \"" + p + "\": use http1, https, h2c or h2"
}

// ClientUpstreamProtocol is the upstream protocol the client of this http
// tunnel named at registration (proto.Upstream…), or "" when it named none
// and the service's setting applies.
func (t *Tunnel) ClientUpstreamProtocol() string {
	if t == nil || !t.IsHTTP {
		return ""
	}
	return t.clientUpstream
}

// ClientUpstreamProtocol is the upstream protocol the client that serves the
// http service serviceID named at registration, or "" when it named none or
// no client serves the service.
func (s *Server) ClientUpstreamProtocol(serviceID string) string {
	tn, ok := s.LookupHTTPTunnelByServiceID(serviceID)
	if !ok {
		return ""
	}
	return tn.ClientUpstreamProtocol()
}

// EffectiveUpstreamProtocol is the upstream protocol that applies to a tunnel
// of an http service whose stored setting is stored: the client's, when it
// named one, else stored, else proto.UpstreamHTTP1.
func EffectiveUpstreamProtocol(tn *Tunnel, stored string) string {
	return proto.EffectiveUpstream(tn.ClientUpstreamProtocol(), stored)
}
