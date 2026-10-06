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
)

// CapRequestSummaries is the capability (AuthRequest.Capabilities) of a client
// that reads request_summary messages.
const CapRequestSummaries = "request_summaries"

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
}

// TunnelRegister asks the server to allocate a public port for a tunnel.
type TunnelRegister struct {
	Name       string `json:"name"`        // human-friendly label
	Type       string `json:"type"`        // "tcp" | "http"
	RemotePort int    `json:"remote_port"` // 0 = auto-assign
	LocalAddr  string `json:"local_addr"`  // "127.0.0.1:3000"
	// Slug and Access (a relay access mode: open, api_key, burrow_login) are
	// wishes for an http service that does not exist yet. The relay applies
	// them only when it creates the service. Optional.
	Slug   string `json:"slug,omitempty"`
	Access string `json:"access,omitempty"`
}

// TunnelRegisterResponse is the server's reply to a TunnelRegister message.
type TunnelRegisterResponse struct {
	OK         bool   `json:"ok"`
	TunnelID   string `json:"tunnel_id,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"` // resolved port; 0 for http tunnels
	URL        string `json:"url,omitempty"`         // e.g. "https://burrow.example.com/svc/k7p2qx/" (http only)
	Hostname   string `json:"hostname,omitempty"`    // no longer set by the server; kept so older peers still decode
	Error      string `json:"error,omitempty"`
	// The fields below are optional; an older relay sends none of them.
	//
	// AccessMode is the access mode of the http service as it is now.
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
	// or it is a tcp service, which has neither.
	Ignored []string `json:"ignored,omitempty"`
	// Code names the reason of a refusal (see the Code constants).
	Code string `json:"code,omitempty"`
}

// NewConnection notifies the client that a visitor has connected to a tunnel port.
type NewConnection struct {
	TunnelID string `json:"tunnel_id"`
	StreamID string `json:"stream_id"` // uuid; client opens a yamux stream with this id in its first frame
	SourceIP string `json:"source_ip"`
}

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
)

// StreamHeader is the first frame the client writes on a new data stream,
// pairing it (by StreamID) to a pending visitor connection on the server.
type StreamHeader struct {
	// StreamID is the server-generated id from the new_connection notify.
	StreamID string `json:"stream_id"`
	// TunnelID is the tunnel this data stream serves.
	TunnelID string `json:"tunnel_id"`
}
