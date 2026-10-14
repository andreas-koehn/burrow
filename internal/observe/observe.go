// Package observe holds the event values of Burrow's observation points.
//
// There are four places where something a relay operator wants to know about
// is finished: a control session opened or closed (internal/server), a
// proxied request or a tcp connection ended (internal/proxy, internal/server),
// a model request ended (internal/aigw), and an MCP tool call ended. Each of
// them hands exactly one value of a type below to an observer. What is done
// with it — metrics, webhook events, audit entries, later traces — is the
// observer's business (cmd/server), so a new consumer needs no new call site.
//
// The package has no dependencies and no behaviour. The interface a package
// calls is declared in that package, next to the call.
//
// Rules for every field:
//   - nothing of a request or response body, no header value, no secret, no
//     key or token (an id of a key's row is not the key);
//   - a field that a visitor or a client controls (Method, TunnelName,
//     SourceIP) says so; an observer never uses it as a metric label without
//     bounding it first.
//
// An observer is called on the goroutine of what it observes. It must return
// at once: work that can wait (a database write, a delivery) is handed to
// something else.
package observe

import "time"

// The kinds of a Session event.
const (
	SessionOpened = "session_opened" // a client logged in on the control channel
	SessionClosed = "session_closed" // its session ended, for whatever reason
	TunnelOpened  = "tunnel_opened"  // the session registered a tunnel
	TunnelClosed  = "tunnel_closed"  // a tunnel was unregistered, or its session ended
	TunnelRefused = "tunnel_refused" // a registration was refused
)

// Session is one event of a client's control session. The session fields are
// set for every kind, the tunnel fields for the Tunnel… kinds.
type Session struct {
	Kind      string
	SessionID string
	UserID    string // the owner of the client token
	TokenID   string // the id of the client token's row; "" when the authenticator names none
	// ConnectedAt is when the session logged in. Duration is how long it
	// lasted (SessionClosed only).
	ConnectedAt time.Time
	Duration    time.Duration
	// Outcome is how the session ended (SessionClosed): an Outcome… value. ""
	// means the server knows of no error and is counted as OutcomeClean.
	Outcome string

	TunnelID   string
	TunnelType string // "http" or "tcp"
	// TunnelName is the client's name for the tunnel: a client's words, never
	// a metric label.
	TunnelName string
	ServiceID  string // the durable service of an http tunnel; "" for tcp
	AccessMode string // of the service, when the registration reported it
	// ServiceCreated: this registration created the service (TunnelOpened).
	ServiceCreated bool
	// RefusalCode is the proto.Code… value the client was answered with
	// (TunnelRefused).
	RefusalCode string
}

// The kinds of Traffic; they are the kinds of the connection log.
const (
	TrafficHTTP = "http_proxy"
	TrafficTCP  = "tcp_proxy"
)

// The outcomes of Traffic; they are the statuses of the connection log.
const (
	OutcomeClean    = "closed_clean"
	OutcomeError    = "closed_error"
	OutcomeIdle     = "closed_idle"
	OutcomeRejected = "rejected"
)

// Why a request was rejected before it reached a service.
const (
	RejectedIPGeo  = "ip_geo" // the service's IP or country policy
	RejectedAccess = "access" // the service's access mode
)

// Traffic is one finished unit of data-plane traffic: a request the proxy
// answered for an http service, or a visitor's connection through a tcp
// tunnel.
type Traffic struct {
	Kind      string // TrafficHTTP or TrafficTCP
	ServiceID string // "" for a tcp tunnel, which has no service
	TunnelID  string
	UserID    string // the owner of the client that served it
	SessionID string

	// Method is the request method as the visitor sent it (TrafficHTTP).
	Method string
	// Status is the status the visitor received; 0 for tcp. A connection that
	// was taken over (an upgrade) is a 101.
	Status  int
	Outcome string
	// RejectReason is set when Outcome is OutcomeRejected.
	RejectReason string
	// KeyService: the service has an API-key header, which is how the proxy
	// tells a model service from a plain one (see emitAIUpstreamErrorAudit).
	KeyService bool
	// ClientGone: the visitor hung up before the upstream answered. The
	// outcome may be an error, but the upstream did not fail.
	ClientGone bool
	// SourceIP is the visitor's address as the connection log stores it. It
	// is for the IP field of the access.denied webhook payload only: never a
	// metric label, never logged.
	SourceIP string

	// BytesIn is what the visitor sent to the service (the request body for
	// http), BytesOut what it received.
	BytesIn, BytesOut int64
	Started           time.Time
	Duration          time.Duration
}

// The outcomes of a ModelRequest.
const (
	ModelAnswered         = "answered"          // an upstream was asked
	ModelCacheHit         = "cache_hit"         // answered from the exact cache
	ModelSemanticHit      = "semantic_hit"      // answered from the semantic cache
	ModelGuardrailRefused = "guardrail_refused" // a guardrail refused it
	ModelRedactionDropped = "redaction_dropped" // a redaction rule dropped it
	ModelTooLarge         = "too_large"         // the body was over the limit
)

// The values of ModelRequest.Cache.
const (
	CacheNotAsked = ""
	CacheHit      = "hit"
	CacheMiss     = "miss"
)

// ModelRequest is one request the AI chain finished, whoever answered it.
type ModelRequest struct {
	ServiceID    string // the service the chain ran under
	APIKeyID     string // the id of the service key's row; "" without one
	GatewayKeyID string // the id of the gateway key's row; "" without one
	ProviderSlug string // who answered (or was first in line), "" outside the gateway
	// RequestedModel is the caller's name for the model: a caller's words,
	// never a metric label (set by the chain; read by tests and later
	// consumers). TargetModel is the provider's model that answered.
	RequestedModel, TargetModel string
	Kind                        string // "openai", "anthropic", "mcp" or "unknown"; "" = not determined (refused before the request was read)

	Outcome string
	Status  int // what the caller received
	// Metered: a usage row was written. False for a refusal and for a call
	// that is no inference (counting tokens).
	Metered             bool
	TokensIn, TokensOut int
	// ReportedCostUSD is the cost a trusted upstream stated, nil when the
	// price table applies.
	ReportedCostUSD *float64
	Streamed        bool
	Latency         time.Duration

	Cache string // CacheNotAsked, CacheHit or CacheMiss, of the exact cache
	// The semantic cache, when the service has it on and it is compiled in.
	SemanticAsked    bool
	SemanticHit      bool
	SemanticPolicy   string // the service's fallback policy, on a hit
	SemanticErr      string // "" or "timeout" or "index"
	SemanticPromoted bool
	// PromotedKey is the exact-cache key of the entry that was promoted.
	PromotedKey string

	// Redactions names the redaction rules that changed the request and how
	// often (set by the chain, W05; the observer announces each rule once, so
	// Count is for later consumers); rule names, never what they matched.
	Redactions []Redaction
	// GuardrailPattern is the id of the pattern that refused the request,
	// GuardrailAction the action taken (ModelGuardrailRefused). Both are set by
	// the chain (W05); the observer uses the pattern, the action is for later
	// consumers.
	GuardrailPattern, GuardrailAction string

	// Attempts is what the gateway sent to upstreams, in order; empty outside
	// a dialect endpoint.
	Attempts []Attempt
}

// Redaction is one rule that fired and its count.
type Redaction struct {
	Rule  string
	Count int
}

// Attempt is one upstream the gateway sent a request to.
type Attempt struct {
	ProviderSlug string
	ServiceID    string // the provider's backing service
	// Status is the upstream's status, 0 when it gave none.
	Status int
	// Failed: the attempt counts against the provider (a 5xx, no response,
	// a timeout), as the breaker sees it.
	Failed bool
	// Sent: the request left for the upstream. Set by the gateway (W06); no
	// observer reads it yet, it is for the attempt log and later consumers.
	Sent bool
}

// ToolCall is one finished MCP tool call. Nothing produces it yet: it is the
// event type the MCP server hands to its observer when it grows one, declared
// here so that all four observation points speak the same vocabulary.
//
//	type ToolCallObserver interface{ ObserveToolCall(observe.ToolCall) }
type ToolCall struct {
	Server   string // which MCP server: "management", or the slug of a gateway target
	Tool     string // the tool's name as the caller or an upstream gave it: never a label
	TokenID  string // the id of the automation token's row
	UserID   string
	Outcome  string // "ok", "denied" or "error"
	Duration time.Duration
}
