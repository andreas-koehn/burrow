// internal/proto/messages.go
package proto

import "encoding/json"

// ProtocolVersion is the current control-protocol version number.
const ProtocolVersion = 1

// MessageType identifies the kind of control message in an Envelope.
type MessageType string

// Control-protocol message type constants.
const (
	MsgAuthRequest        MessageType = "auth_request"
	MsgAuthResponse       MessageType = "auth_response"
	MsgTunnelRegister     MessageType = "tunnel_register"
	MsgTunnelRegisterResp MessageType = "tunnel_register_response"
	MsgTunnelUnregister   MessageType = "tunnel_unregister"
	MsgNewConnection      MessageType = "new_connection"
	MsgPing               MessageType = "ping"
	MsgPong               MessageType = "pong"
	MsgError              MessageType = "error"
	MsgStreamOpen         MessageType = "stream_open"
	// MsgRequestSummary travels from the relay to a client that announced
	// CapRequestSummaries, and to no other.
	MsgRequestSummary MessageType = "request_summary"
	// MsgForwardOpen is the first frame of a stream a client opens to reach
	// a tcp service through the relay (a relay that announces CapForward);
	// the relay answers MsgForwardOpenResp on the same stream and, when it
	// is OK, pipes the stream's bytes to the service from then on. An older
	// relay closes such a stream without a word.
	MsgForwardOpen     MessageType = "forward_open"
	MsgForwardOpenResp MessageType = "forward_open_response"
)

// CapRequestSummaries is the capability (AuthRequest.Capabilities) of a client
// that reads request_summary messages.
const CapRequestSummaries = "request_summaries"

// CapProxyProtocolV2 is the capability (AuthRequest.Capabilities) of a client
// that writes a PROXY protocol v2 header to its target when new_connection
// asks for one. A relay does not pair a visitor of a service that asks for
// the header with a client that lacks it: an older client would hand the
// target the visitor's bytes without the header.
const CapProxyProtocolV2 = "proxy_protocol_v2"

// Capabilities a relay announces in AuthResponse.Capabilities. A client that
// needs one checks for it before it relies on it: an older relay announces
// none.
const (
	// CapTCPAccess: the relay applies TunnelRegister.Access to a tcp tunnel
	// (the modes open, ip and private) and reports the mode it applied in
	// TunnelRegisterResponse.AccessMode.
	CapTCPAccess = "tcp_access"
	// CapForward: the relay accepts forward streams to private tcp services.
	CapForward = "forward"
	// CapUpstreamProtocol: the relay applies TunnelRegister.UpstreamProtocol
	// to an http tunnel and speaks that protocol to the local service. A
	// client that wants anything but UpstreamHTTP1 from a relay without it
	// fails the registration: an older relay speaks HTTP/1.1 whatever the
	// client asked.
	CapUpstreamProtocol = "upstream_protocol"
)

// The protocols the relay speaks to the local service of an http tunnel
// (TunnelRegister.UpstreamProtocol, the service's upstream_protocol). The
// relay does the TLS and HTTP/2 framing over the tunnel stream; the client
// pipes bytes, whatever the protocol.
const (
	// UpstreamHTTP1 is HTTP/1.1 in plain text: the default.
	UpstreamHTTP1 = "http1"
	// UpstreamHTTPS is HTTP/1.1 over TLS (ALPN http/1.1).
	UpstreamHTTPS = "https"
	// UpstreamH2C is HTTP/2 in plain text, by prior knowledge.
	UpstreamH2C = "h2c"
	// UpstreamH2 is HTTP/2 over TLS (ALPN h2 only).
	UpstreamH2 = "h2"
)

// ValidUpstreamProtocol reports whether s names one of the Upstream…
// protocols.
func ValidUpstreamProtocol(s string) bool {
	switch s {
	case UpstreamHTTP1, UpstreamHTTPS, UpstreamH2C, UpstreamH2:
		return true
	}
	return false
}

// Envelope wraps every control message with its type and an optional correlation ID.
type Envelope struct {
	Type    MessageType     `json:"type"`
	ID      string          `json:"id,omitempty"` // correlation id for requests
	Payload json.RawMessage `json:"payload"`
}

// AuthRequest is sent by the client immediately after opening a control connection.
type AuthRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	Token           string `json:"token"`
	ClientVersion   string `json:"client_version"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	// hostname (optional, since v0.3 extension)
	Hostname string `json:"hostname,omitempty"`
	// Capabilities names the optional messages this client understands. A
	// relay that does not know a name ignores it. Optional.
	Capabilities []string `json:"capabilities,omitempty"`
}

// AuthResponse is the server's reply to an AuthRequest.
type AuthResponse struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
	// RelayVersion is sent on success. Optional: an older relay sends none.
	RelayVersion string `json:"relay_version,omitempty"`
	// Code names the reason of a refusal (see the Code constants). Optional:
	// an older relay sends Error alone.
	Code string `json:"code,omitempty"`
	// Capabilities names what this relay does beyond the base protocol (the
	// Cap… constants for a relay), on success. Optional: an older relay
	// sends none, and a client must not assume any.
	Capabilities []string `json:"capabilities,omitempty"`
}

// TunnelRegister asks the server to allocate a public port for a tunnel.
type TunnelRegister struct {
	Name       string `json:"name"`        // human-friendly label
	Type       string `json:"type"`        // "tcp" | "http"
	RemotePort int    `json:"remote_port"` // 0 = auto-assign
	LocalAddr  string `json:"local_addr"`  // "127.0.0.1:3000"
	// Slug and Access are wishes for a service that does not exist yet. The
	// relay applies them only when it creates the service. Access is a relay
	// access mode: open, api_key or burrow_login for http; open, ip or
	// private for tcp (a relay that announces CapTCPAccess; an older one
	// ignores it for tcp and reports it in Ignored). Optional.
	Slug   string `json:"slug,omitempty"`
	Access string `json:"access,omitempty"`
	// AccessDefault is the mode a tcp service gets when this registration
	// creates it and Access is empty: a new client sends "private" when the
	// user asked for no mode. Unlike Access it is no wish: a service that
	// exists keeps its stored mode and nothing is refused or ignored. A relay
	// without it creates the service open. Optional.
	AccessDefault string `json:"access_default,omitempty"`
	// ProxyProtocol (tcp only) is ProxyProtocolV2 when the client writes a
	// PROXY protocol v2 header to its local target for every connection
	// because its own configuration says so (burrow.yaml proxy_protocol, the
	// --proxy-protocol flag), whatever the service's setting. The relay only
	// shows it; it changes nothing on the relay. Optional.
	ProxyProtocol string `json:"proxy_protocol,omitempty"`
	// UpstreamProtocol (http only) is the protocol the relay speaks to the
	// local service, one of the Upstream… constants, when the client's own
	// configuration names one (burrow.yaml upstream_protocol, the
	// --upstream-protocol flag, burrow grpc). It applies to this tunnel in
	// place of the service's stored setting and is not stored. A relay that
	// announces CapUpstreamProtocol refuses an unknown value (CodeBadRequest);
	// an older relay ignores the field. Optional: "" leaves the service's
	// setting.
	UpstreamProtocol string `json:"upstream_protocol,omitempty"`
}

// TunnelRegisterResponse is the server's reply to a TunnelRegister message.
type TunnelRegisterResponse struct {
	OK         bool   `json:"ok"`
	TunnelID   string `json:"tunnel_id,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"` // resolved port; 0 for http tunnels and tcp tunnels in mode private
	URL        string `json:"url,omitempty"`         // e.g. "https://burrow.example.com/svc/k7p2qx/" (http only)
	Hostname   string `json:"hostname,omitempty"`    // no longer set by the server; kept so older peers still decode
	Error      string `json:"error,omitempty"`
	// The fields below are optional; an older relay sends none of them.
	//
	// AccessMode is the access mode of the service as it is now: of an http
	// service, and of a tcp service on a relay that announces CapTCPAccess.
	// A tcp tunnel in mode private has no public port: RemotePort is 0.
	AccessMode string `json:"access_mode,omitempty"`
	// Created says that this registration created the service.
	Created bool `json:"created,omitempty"`
	// DashboardURL is the page of the service in the dashboard.
	DashboardURL string `json:"dashboard_url,omitempty"`
	// GatewayOnly says that the service answers through the AI gateway only;
	// URL is then empty.
	GatewayOnly bool `json:"gateway_only,omitempty"`
	// Ignored lists, sorted, which of "access" and "slug" the client asked
	// for and the relay did not apply: the service existed with other values,
	// or it is a tcp service, which has no slug (and, on an older relay, no
	// access mode).
	Ignored []string `json:"ignored,omitempty"`
	// Code names the reason of a refusal (see the Code constants).
	Code string `json:"code,omitempty"`
	// Slug is the slug of a tcp service: the global name a forward reaches
	// it by (ForwardOpen.Target). It is no address; a tcp service has no
	// URL.
	Slug string `json:"slug,omitempty"`
}

// NewConnection notifies the client that a visitor has connected to a tunnel port.
type NewConnection struct {
	TunnelID string `json:"tunnel_id"`
	StreamID string `json:"stream_id"` // uuid; client opens a yamux stream with this id in its first frame
	SourceIP string `json:"source_ip"`
	// DestAddr is the address (host:port) the visitor connected to, as the
	// relay saw it: the destination of a PROXY protocol header. Optional;
	// an older relay sends none.
	DestAddr string `json:"dest_addr,omitempty"`
	// ProxyProtocol is ProxyProtocolV2 when the service asks for a PROXY
	// protocol v2 header: the client writes one, with SourceIP (without a
	// "forward:" prefix) as the source and DestAddr as the destination, to
	// its local target before any byte of the connection. Optional; "" is
	// none.
	ProxyProtocol string `json:"proxy_protocol,omitempty"`
}

// ProxyProtocolV2 is the PROXY protocol version a tcp service may ask for
// (NewConnection.ProxyProtocol, TunnelRegister.ProxyProtocol).
const ProxyProtocolV2 = "v2"

// TunnelUnregister asks the server to drop a previously registered tunnel.
type TunnelUnregister struct {
	TunnelID string `json:"tunnel_id"`
}

// Ping is an application-level heartbeat (control stream).
type Ping struct {
	Nonce string `json:"nonce"`
}

// Pong answers a Ping with the same nonce.
type Pong struct {
	Nonce string `json:"nonce"`
}

// Error is a generic protocol error message.
type Error struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// RequestSummary says that one request to an http service has been answered.
// It names the request and nothing of its content: no header, no query string,
// no body. Method and Path come from the visitor; both ends pass them through
// SummaryMethod and SummaryPath.
type RequestSummary struct {
	TunnelID   string `json:"tunnel_id"`
	Time       string `json:"time"` // when the request arrived; RFC 3339, UTC
	Method     string `json:"method"`
	Path       string `json:"path"` // the path the app saw, without the query; at most MaxSummaryPath bytes
	Status     int    `json:"status"`
	DurationMs int64  `json:"duration_ms"` // 0 when the relay does not measure it
	// GRPCStatus is the grpc-status of a gRPC call (0 is OK); absent for any
	// other request and for a call that ended without one.
	GRPCStatus *int `json:"grpc_status,omitempty"`
}

// Codes of control-plane refusals. They travel next to the text, which stays
// as it was for peers that match on it.
const (
	CodeInvalidToken      = "invalid_token"
	CodeClientTooOld      = "client_too_old"
	CodeSlugInvalid       = "slug_invalid"
	CodeSlugTaken         = "slug_taken"
	CodeAccessInvalid     = "access_invalid"
	CodeUnknownTunnelType = "unknown_tunnel_type"
	CodeInternal          = "internal"
	// CodeForbidden: the token's owner may not do what was asked.
	CodeForbidden = "forbidden"
	// CodePortUnavailable: the public port of a tcp tunnel cannot be had.
	CodePortUnavailable = "port_unavailable"
	// CodeBadRequest: a message that could not be read or was not expected.
	CodeBadRequest = "bad_request"
	// CodeNameInUse: the token's owner has a service of that name with
	// another type (an http service where a tcp tunnel was asked, or the
	// other way round).
	CodeNameInUse = "name_in_use"
	// CodeServiceBusy: another client serves the service now. It may be the
	// last session of the same client that the relay has not seen end yet,
	// so a client tries again.
	CodeServiceBusy = "service_busy"
	// CodeRateLimited: too many attempts from this address or session; the
	// client waits and tries again.
	CodeRateLimited = "rate_limited"
	// CodeScopeDenied: the token's scope does not allow the action: a
	// forward-only token registering a tunnel, or a publish-only token opening
	// a forward. It ends that action only, not the session.
	CodeScopeDenied = "scope_denied"
	// CodeNotFound: a forward to a service that does not exist or that the
	// caller may not reach; the two are told apart nowhere.
	CodeNotFound = "not_found"
	// CodeOffline: the service of a forward exists, but no client serves it
	// now (or its client did not answer in time).
	CodeOffline = "offline"
)

// ForwardOpen is the first frame of a forward stream. Target names a tcp
// service: its slug, the name of one of the caller's own services, or its
// id.
type ForwardOpen struct {
	Target string `json:"target"`
}

// ForwardOpenResponse answers a ForwardOpen. On OK the stream carries the
// service's bytes from the next byte on; otherwise the relay closes it after
// this frame. Code is one of CodeNotFound, CodeOffline, CodeRateLimited,
// CodeBadRequest and CodeInternal. ServiceID and Name name the service that
// was reached (OK only).
type ForwardOpenResponse struct {
	OK        bool   `json:"ok"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
	ServiceID string `json:"service_id,omitempty"`
	Name      string `json:"name,omitempty"`
}

// StreamHeader is the first frame the client writes on a new data stream,
// pairing it (by StreamID) to a pending visitor connection on the server.
type StreamHeader struct {
	// StreamID is the server-generated id from the new_connection notify.
	StreamID string `json:"stream_id"`
	// TunnelID is the tunnel this data stream serves.
	TunnelID string `json:"tunnel_id"`
}

// EffectiveUpstream is the upstream protocol that applies to an http tunnel:
// the one its client named at registration, else the service's stored
// setting, else UpstreamHTTP1. The relay and the API both use it, so that what
// the dashboard shows is what the proxy does.
func EffectiveUpstream(client, stored string) string {
	if client != "" {
		return client
	}
	if stored != "" {
		return stored
	}
	return UpstreamHTTP1
}
