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
)

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
}

// AuthResponse is the server's reply to an AuthRequest.
type AuthResponse struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// TunnelRegister asks the server to allocate a public port for a tunnel.
type TunnelRegister struct {
	Name       string `json:"name"`        // human-friendly label
	Type       string `json:"type"`        // "tcp" | "http"
	RemotePort int    `json:"remote_port"` // 0 = auto-assign
	LocalAddr  string `json:"local_addr"`  // "127.0.0.1:3000"
}

// TunnelRegisterResponse is the server's reply to a TunnelRegister message.
type TunnelRegisterResponse struct {
	OK         bool   `json:"ok"`
	TunnelID   string `json:"tunnel_id,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"` // resolved port; 0 for http tunnels
	URL        string `json:"url,omitempty"`         // e.g. "https://burrow.example.com/svc/k7p2qx/" (http only)
	Hostname   string `json:"hostname,omitempty"`    // no longer set by the server; kept so older peers still decode
	Error      string `json:"error,omitempty"`
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
}

// StreamHeader is the first frame the client writes on a new data stream,
// pairing it (by StreamID) to a pending visitor connection on the server.
type StreamHeader struct {
	// StreamID is the server-generated id from the new_connection notify.
	StreamID string `json:"stream_id"`
	// TunnelID is the tunnel this data stream serves.
	TunnelID string `json:"tunnel_id"`
}
