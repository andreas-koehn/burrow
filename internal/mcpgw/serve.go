package mcpgw

// serve.go — the client side of the MCP gateway: /mcp/<slug> behind gateway
// keys and OAuth grants (D3, D4, D7, D8, D12, D13, D16, D22, D23). A grant
// carries an allow-list and a read-only switch like a key's; below, "key"
// stands for either.
//
// Each request gets its own mcp.Server holding only the tools the key may
// call: approved, not hidden, served under an unambiguous name (ServedSet),
// allowed by the key's allow-list (store.ToolAllowedNative on the served
// name) and, for a read-only key, classified read-only by the operator. A
// call on any other tool gets the SDK's "unknown tool", the same answer as a
// tool that does not exist.
//
// A tools/call then runs the pipeline of Call:
//
//	limits      the source's concurrency and rpm, the key's rate limits
//	redaction   of the arguments (scope tool_arguments)
//	guardrails  the request patterns on the redacted argument JSON
//	upstream    CallTool through the pool, bounded by the source's timeout
//	result      capped at MaxResultBytes, redacted (scope tool_result)
//	guardrails  the answer patterns on the result's text
//	record      usage row, inspector entry, audit row (sampled), observer
//
// Redaction, guardrails and the inspector follow the AI settings of the
// source's backing service (Policy), as model traffic does; the judge and
// the budgets do not apply to tool calls (D12).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/aimeter"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/inspector"
	"github.com/ankoehn/burrow/internal/oauth"
	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/quota"
	"github.com/ankoehn/burrow/internal/redact"
	"github.com/ankoehn/burrow/internal/store"
	"github.com/ankoehn/burrow/internal/version"
)

// Bounds of the serving side.
const (
	// MaxRequestBytes bounds a request body, as on /mcp/burrow.
	MaxRequestBytes = 1 << 20
	// MaxResultBytes caps a tool's result (D13): a larger one is an error.
	MaxResultBytes = 1 << 20
	// DefaultCallTimeout and MaxCallTimeout bound one call (D13).
	DefaultCallTimeout = 60 * time.Second
	MaxCallTimeout     = 300 * time.Second
	// concurrencyRetryAfter is what a caller is told to wait when every
	// place of a source is taken.
	concurrencyRetryAfter = time.Second
)

// protocolVersions are the revisions served, the same set as /mcp/burrow (D4).
var protocolVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

// ServeStore is what serving reads. *store.Store satisfies it.
type ServeStore interface {
	MCPSourceBySlug(ctx context.Context, slug string) (db.MCPSource, error)
	ListMCPTools(ctx context.Context, slug string) ([]db.MCPTool, error)
	// ListMCPSources returns every source ordered by slug (the aggregated /mcp).
	ListMCPSources(ctx context.Context) ([]db.MCPSource, error)
}

// KeyValidator validates a presented gateway key, owner checks included
// (store.Store.ValidateGatewayKey). It must not cache.
type KeyValidator interface {
	ValidateGatewayKey(ctx context.Context, presented string) (store.GatewayKey, bool, error)
}

// RateCharger charges the minute-window rate limits (quota.Engine.ChargeRate).
type RateCharger interface {
	ChargeRate(ctx context.Context, who quota.Subjects, dim string, units int) quota.Decision
}

// ToolCaller calls a tool upstream by its native name (Pool.CallTool).
type ToolCaller interface {
	CallTool(ctx context.Context, src db.MCPSource, params *mcp.CallToolParams) (*mcp.CallToolResult, error)
}

// AuditAppender writes audit rows (*audit.Logger).
type AuditAppender interface {
	Append(ctx context.Context, e audit.Event) error
}

// Policy is what applies to the calls of one source: the AI settings of its
// backing service, as for model traffic.
type Policy struct {
	// Redact runs the redaction rules over arguments and results;
	// RedactForLogsOnly keeps the original for the upstream and the caller
	// and the redacted text for the inspector.
	Redact            bool
	RedactForLogsOnly bool
	// Guardrails are the effective guardrail settings; nil = none.
	Guardrails *guardrails.Settings
	// Inspect captures calls in the backing service's inspector ring of
	// InspectMax entries (0 = the default size).
	Inspect    bool
	InspectMax int
}

// GatewayOptions configures a Gateway.
type GatewayOptions struct {
	Store    ServeStore
	Keys     KeyValidator
	Upstream ToolCaller
	// Limits charges a key's rate limits; nil = none.
	Limits RateCharger
	// Redact and Guardrails are the engines; nil = that step is off.
	Redact     *redact.Engine
	Guardrails *guardrails.Engine
	// Policy returns the policy of a source's backing service; nil = no
	// redaction, guardrails or inspector.
	Policy    func(ctx context.Context, serviceID string) Policy
	Meter     aimeter.Sink
	Inspector *inspector.Manager
	Audit     AuditAppender
	Observer  observe.ToolCallObserver
	Log       *slog.Logger
	// MaxResultBytes overrides MaxResultBytes (tests).
	MaxResultBytes int
	// OAuth checks OAuth access tokens (*oauth.Server); nil = gateway keys
	// only.
	OAuth OAuthTokens
}

// OAuthTokens checks OAuth access tokens and words the 401 challenge
// (*oauth.Server).
type OAuthTokens interface {
	VerifyAccessToken(r *http.Request, plain, path string) (oauth.Verified, error)
	Challenge(r *http.Request, path string, invalid bool) string
}

// Gateway serves tool sources to MCP clients. Safe for concurrent use.
type Gateway struct {
	opts   GatewayOptions
	log    *slog.Logger
	sdk    http.Handler
	origin *http.CrossOriginProtection
	now    func() time.Time

	mu       sync.Mutex
	limits   map[string]*sourceLimits // by slug
	sampled  map[string]time.Time     // ok-call audit sample: key|source -> minute
	reported map[string]string        // name conflicts already logged, by slug
}

// NewGateway returns a gateway.
func NewGateway(opts GatewayOptions) *Gateway {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.MaxResultBytes <= 0 {
		opts.MaxResultBytes = MaxResultBytes
	}
	g := &Gateway{
		opts: opts, log: log, origin: http.NewCrossOriginProtection(), now: time.Now,
		limits: map[string]*sourceLimits{}, sampled: map[string]time.Time{}, reported: map[string]string{},
	}
	// The same handler options as /mcp/burrow (D4). The server of a request
	// is built before the SDK runs and handed over through the context.
	g.sdk = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		srv, _ := r.Context().Value(ctxKeyServer{}).(*mcp.Server)
		return srv
	}, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: MaxRequestBytes,
		// A relay behind a reverse proxy on the same machine receives
		// loopback requests with a public Host; nothing here answers without
		// a gateway key (see mcpserv).
		DisableLocalhostProtection: true,
	})
	return g
}

type ctxKeyServer struct{}

// Caller is who makes a call: a gateway key, or an OAuth grant (D23), by
// its allow-list. Exactly one of GatewayKeyID and GrantID is set.
type Caller struct {
	GatewayKeyID string
	// GrantID is the OAuth grant whose access token was presented.
	GrantID string
	UserID  string
	// AllowedTools holds "<source>__<tool>" and "<source>__*"; empty = none.
	AllowedTools []string
	// ReadOnly: only tools the operator classed read-only.
	ReadOnly bool
}

// May reports whether c may see and call t of source slug (D7, D8). The
// allow-list is matched against the name the tool is served under.
func (c Caller) May(slug string, t Tool) bool {
	if c.ReadOnly && !t.ReadOnly {
		return false
	}
	return store.ToolAllowedNative(c.AllowedTools, slug, t.Name)
}

// RequestInfo is what the audit row and the inspector entry say about the
// HTTP request a call came in.
type RequestInfo struct {
	RequestID string
	RemoteIP  string
	UserAgent string
	Path      string
}

func requestInfo(r *http.Request) RequestInfo {
	ip := r.RemoteAddr
	if h, _, err := net.SplitHostPort(ip); err == nil {
		ip = h
	}
	id := middleware.GetReqID(r.Context())
	if id == "" {
		id = uuid.NewString()
	}
	return RequestInfo{RequestID: clean(id, 128), RemoteIP: clean(ip, 64), UserAgent: clean(r.UserAgent(), 256), Path: r.URL.Path}
}

// clean cuts s to n bytes between characters and makes it valid UTF-8.
func clean(s string, n int) string {
	if len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return strings.ToValidUTF8(s, "")
}

// presentedKey reads the key as the AI gateway does: a bearer token, else
// x-api-key.
func presentedKey(r *http.Request) string {
	if tok, ok := bearerToken(r); ok {
		return tok
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// bearerToken is the token of an Authorization header with the Bearer
// scheme (any case, any spacing).
func bearerToken(r *http.Request) (string, bool) {
	const scheme = "Bearer "
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); len(auth) > len(scheme) && strings.EqualFold(auth[:len(scheme)], scheme) {
		return strings.TrimSpace(auth[len(scheme):]), true
	}
	return "", false
}

// errUnauthorized: no valid credential; errNoCredential: none at all (the
// challenge then carries no error code).
var (
	errUnauthorized = errors.New("a valid gateway key is required")
	errNoCredential = fmt.Errorf("no credential: %w", errUnauthorized)
)

// subject names the caller in limits and audit samples.
func (c Caller) subject() string {
	if c.GrantID != "" {
		return "oauth:" + c.GrantID
	}
	return c.GatewayKeyID
}

// Authenticate returns the caller of r at the endpoint path ("/mcp" or
// "/mcp/<slug>"): a gateway key whose owner may still configure the gateway
// (ValidateGatewayKey), or an OAuth access token issued for exactly this
// endpoint whose grant and user the authorization server checks now
// (VerifyAccessToken, D22, D23, D28a). An OAuth token is read from the
// Authorization header only. It returns errUnauthorized for a missing,
// malformed, unknown, revoked or expired credential, a token of another
// endpoint, and an owner who is suspended, gone or demoted, all alike;
// other errors are the store's.
func (g *Gateway) Authenticate(r *http.Request, path string) (Caller, error) {
	if g.opts.OAuth != nil {
		if plain, ok := bearerToken(r); ok && strings.HasPrefix(plain, oauth.AccessTokenPrefix) {
			v, err := g.opts.OAuth.VerifyAccessToken(r, plain, path)
			if errors.Is(err, oauth.ErrInvalidToken) {
				return Caller{}, errUnauthorized
			}
			if err != nil {
				return Caller{}, err
			}
			return Caller{GrantID: v.GrantID, UserID: v.User.ID, AllowedTools: v.AllowedTools, ReadOnly: v.ToolsReadOnly}, nil
		}
	}
	presented := presentedKey(r)
	if presented == "" {
		return Caller{}, errNoCredential
	}
	if g.opts.Keys == nil || !strings.HasPrefix(presented, "bgw_") {
		return Caller{}, errUnauthorized
	}
	k, ok, err := g.opts.Keys.ValidateGatewayKey(r.Context(), presented)
	if err != nil {
		return Caller{}, err
	}
	if !ok {
		return Caller{}, errUnauthorized
	}
	return Caller{GatewayKeyID: k.ID, UserID: k.UserID, AllowedTools: k.AllowedTools, ReadOnly: k.ToolsReadOnly}, nil
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// perimeter is the start of every request at the endpoint path: a browser
// on another origin is refused (403), then the credential is checked (401
// with the challenge of the authorization server: the address of the
// endpoint's protected resource metadata and the default scope, with
// error="invalid_token" when a credential was presented; a bare "Bearer"
// without an authorization server). It answers and returns false when the
// request goes no further.
func (g *Gateway) perimeter(w http.ResponseWriter, r *http.Request, path string) (Caller, bool) {
	if err := g.origin.Check(r); err != nil {
		writeJSONError(w, http.StatusForbidden, "cross-origin request refused")
		return Caller{}, false
	}
	caller, err := g.Authenticate(r, path)
	if errors.Is(err, errUnauthorized) {
		challenge := "Bearer"
		if g.opts.OAuth != nil {
			challenge = g.opts.OAuth.Challenge(r, path, !errors.Is(err, errNoCredential))
		}
		w.Header().Set("WWW-Authenticate", challenge)
		writeJSONError(w, http.StatusUnauthorized, errUnauthorized.Error())
		return Caller{}, false
	}
	if err != nil {
		g.log.Error("mcp gateway: credential validation failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return Caller{}, false
	}
	return caller, true
}

// ServeSource serves /mcp/<slug>. In order: a browser on another origin
// (403), the gateway key (401 with a Bearer challenge), the source (404 for
// an unknown or disabled one), the body (413 over 1 MiB, 400 for a JSON-RPC
// batch), then the SDK.
func (g *Gateway) ServeSource(w http.ResponseWriter, r *http.Request, slug string) {
	caller, ok := g.perimeter(w, r, "/mcp/"+slug)
	if !ok {
		return
	}
	src, err := g.source(r.Context(), slug)
	if errors.Is(err, store.ErrMCPSourceNotFound) {
		writeJSONError(w, http.StatusNotFound, "tool source not found")
		return
	}
	if err != nil {
		g.log.Error("mcp gateway: source not read", "source", slug, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	tools, all, err := g.ToolsFor(r.Context(), src, caller)
	if err != nil {
		g.log.Error("mcp gateway: tools not read", "source", src.Slug, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	info := requestInfo(r)
	g.serveSDK(w, r, func() *mcp.Server {
		srv := NewServer()
		for _, t := range tools {
			g.AddTool(srv, t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return g.Call(ctx, CallRequest{Caller: caller, Source: src, Tool: t, Arguments: req.Params.Arguments, Info: info})
			})
		}
		srv.AddReceivingMiddleware(g.deniedCalls(caller, src, all, info))
		return srv
	})
}

// source returns an enabled source; a disabled one is not found.
func (g *Gateway) source(ctx context.Context, slug string) (db.MCPSource, error) {
	if !store.ValidToolSourceSlug(slug) {
		return db.MCPSource{}, store.ErrMCPSourceNotFound
	}
	src, err := g.opts.Store.MCPSourceBySlug(ctx, slug)
	if err != nil {
		return db.MCPSource{}, err
	}
	if !src.Enabled {
		return db.MCPSource{}, store.ErrMCPSourceNotFound
	}
	return src, nil
}

// ToolsFor returns the tools of src that c may see, and every tool src
// serves (for telling a denied call from an unknown one). Name conflicts are
// logged once per change.
func (g *Gateway) ToolsFor(ctx context.Context, src db.MCPSource, c Caller) (mine, all []Tool, err error) {
	rows, err := g.opts.Store.ListMCPTools(ctx, src.Slug)
	if err != nil {
		return nil, nil, err
	}
	all, conflicts := ServedSet(rows)
	g.reportConflicts(src.Slug, conflicts)
	for _, t := range all {
		if c.May(src.Slug, t) {
			mine = append(mine, t)
		}
	}
	return mine, all, nil
}

func (g *Gateway) reportConflicts(slug string, conflicts map[string]string) {
	sig := ""
	if len(conflicts) > 0 {
		b, _ := json.Marshal(conflicts) // map keys are sorted
		sig = string(b)
	}
	g.mu.Lock()
	changed := g.reported[slug] != sig
	if sig == "" {
		delete(g.reported, slug)
	} else {
		g.reported[slug] = sig
	}
	g.mu.Unlock()
	if changed && sig != "" {
		g.log.Warn("mcp gateway: tools share a served name and are not served", "source", slug, "tools", sig)
	}
}

// NewServer returns an empty per-request MCP server: tools only, revisions
// as on /mcp/burrow, lists never cached by a client across keys.
func NewServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "burrow-tools", Title: "Burrow tool gateway", Version: version.Version}, &mcp.ServerOptions{
		Capabilities:              &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		SupportedProtocolVersions: protocolVersions,
		SetCacheable: func(_ context.Context, _ mcp.Request, c *mcp.Cacheable) {
			c.TTLMs = 0
			c.CacheScope = "private"
		},
	})
	srv.AddReceivingMiddleware(toolsOnly)
	return srv
}

// AddTool adds t under its served name with handler h. A definition the SDK
// refuses (an input schema that is not an object, say: upstream text) is left
// out and logged, never a panic.
func (g *Gateway) AddTool(srv *mcp.Server, t Tool, h mcp.ToolHandler) {
	defer func() {
		if p := recover(); p != nil {
			g.log.Warn("mcp gateway: tool definition refused, tool not served", "source", t.Source, "tool", t.Native)
		}
	}()
	srv.AddTool(t.sdkTool(), h)
}

// sdkTool is the descriptor a client sees. readOnlyHint is the operator's
// classification (D8), not the upstream's.
func (t Tool) sdkTool() *mcp.Tool {
	ann := &mcp.ToolAnnotations{}
	if len(t.Annotations) > 0 {
		_ = json.Unmarshal(t.Annotations, ann)
	}
	ann.ReadOnlyHint = t.ReadOnly
	out := &mcp.Tool{Name: t.Name, Title: t.Title, Description: t.Description, Annotations: ann}
	if len(t.InputSchema) > 0 {
		out.InputSchema = t.InputSchema
	}
	if len(t.OutputSchema) > 0 {
		out.OutputSchema = t.OutputSchema
	}
	return out
}

// refusedPrefixes are the method families beyond tools (D3).
var refusedPrefixes = []string{"resources/", "prompts/", "completion/", "logging/", "tasks/", "sampling/", "elicitation/", "roots/"}

// refused reports whether method is outside tools (D3).
func refused(method string) bool {
	for _, p := range refusedPrefixes {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}

func refusedMessage(method string) string {
	return "the Burrow tool gateway serves tools only; " + clean(method, 64) + " is not supported"
}

// refuseMethod answers a JSON-RPC request for a method beyond tools with
// -32601 naming the limit (HTTP 200, as any JSON-RPC error) and reports
// whether it did. Anything else, notifications included, is left to the SDK.
func refuseMethod(w http.ResponseWriter, body []byte) bool {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(body, &msg) != nil || len(msg.ID) == 0 || bytes.Equal(msg.ID, []byte("null")) || !refused(msg.Method) {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": msg.ID,
		"error": map[string]any{"code": jsonrpc.CodeMethodNotFound, "message": refusedMessage(msg.Method)},
	})
	return true
}

// toolsOnly answers a method outside tools with -32601 naming the limit.
func toolsOnly(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if refused(method) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: refusedMessage(method)}
		}
		return next(ctx, method, req)
	}
}

// deniedCalls records a tools/call on a tool the source serves but the
// caller may not use. It does not answer: the tool is not on this request's
// server, so the SDK's "unknown tool" follows, as for a tool that does not
// exist.
func (g *Gateway) deniedCalls(c Caller, src db.MCPSource, all []Tool, info RequestInfo) mcp.Middleware {
	byName := make(map[string]Tool, len(all))
	for _, t := range all {
		byName[t.Name] = t
	}
	return g.denied(c, info, func(name string) (db.MCPSource, Tool, bool) {
		t, ok := byName[name]
		return src, t, ok
	})
}

// denied is the middleware behind deniedCalls: served looks a requested name
// up among everything the endpoint serves to anybody; a name found there that
// the caller may not use is recorded as denied.
func (g *Gateway) denied(c Caller, info RequestInfo, served func(name string) (db.MCPSource, Tool, bool)) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (res mcp.Result, err error) {
			defer func() {
				if p := recover(); p != nil {
					g.log.Error("mcp gateway: request panicked", "method", method, "panic_type", fmt.Sprintf("%T", p), "stack", string(debug.Stack()))
					res, err = nil, errors.New("internal error")
				}
			}()
			if method == "tools/call" {
				if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p != nil {
					if src, t, found := served(p.Name); found && !c.May(src.Slug, t) {
						g.observe(c, src, p.Name, outcomeDenied, 0, 0)
						g.appendAudit(ctx, c, src, info, outcomeDenied, map[string]any{"tool": p.Name, "reason": "not allowed for this key"})
					}
				}
			}
			return next(ctx, method, req)
		}
	}
}

// serveSDK refuses an oversized body and a JSON-RPC batch before the SDK
// sees them, then serves the request with the server build returns.
func (g *Gateway) serveSDK(w http.ResponseWriter, r *http.Request, build func() *mcp.Server) {
	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad request")
			return
		}
		if len(body) > MaxRequestBytes {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		// A batch would let one POST run any number of calls past the limits
		// that count requests.
		if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '[' {
			writeJSONError(w, http.StatusBadRequest, "batch requests are not supported")
			return
		}
		// A method beyond tools is refused here, before the SDK: on
		// 2026-07-28 the SDK turns some away itself, without saying why.
		if refuseMethod(w, body) {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	ctx := context.WithValue(r.Context(), ctxKeyServer{}, build())
	g.sdk.ServeHTTP(w, r.WithContext(ctx))
}

// ----- the call pipeline -----------------------------------------------------

// Outcomes of a call, as the observer and the audit row name them.
const (
	outcomeOK     = "ok"
	outcomeDenied = "denied"
	outcomeError  = "error"
)

// CallRequest is one tools/call to run through the pipeline.
type CallRequest struct {
	Caller    Caller
	Source    db.MCPSource
	Tool      Tool
	Arguments json.RawMessage
	Info      RequestInfo
}

// callRecord collects what the pipeline did, for the records at the end.
type callRecord struct {
	outcome   string
	status    int    // the gateway's status of the call, HTTP-style
	reason    string // why it failed or was refused (audit and inspector, never the caller)
	args      []byte // the arguments as recorded (redacted)
	result    []byte // the result as recorded (redacted); nil when there is none
	bytesIn   int64
	bytesOut  int64
	redacted  []redact.RuleHit
	guardrail *inspector.GuardrailMark
	policy    Policy
	limited   bool // refused by a limit: audited at most once per key, source and minute
}

// limitError is the JSON-RPC error of a limit hit (HTTP 200 per MCP), with
// retryAfterMs in its data.
func limitError(msg, limit string, retry time.Duration) error {
	ms := retry.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	data, _ := json.Marshal(map[string]any{"retryAfterMs": ms, "limit": limit})
	return &jsonrpc.Error{Code: CodeLimitExceeded, Message: msg, Data: data}
}

// CodeLimitExceeded is the JSON-RPC error code of a limit hit.
const CodeLimitExceeded = -32029

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// Messages a caller gets. They never carry the reason of a refusal.
const (
	msgGuardrail     = "refused by a guardrail"
	msgRedactionArgs = "the arguments were refused by a redaction rule"
	msgRedactionRes  = "the result was withheld by a redaction rule"
	msgBadArgs       = "arguments must be a JSON object"
	msgInternal      = "internal error"
)

// Call runs one tools/call of req.Tool through the pipeline (D12). A tool's
// own failure, an unreachable upstream, a refusal and a result over the cap
// are tool results with isError; a limit hit is a JSON-RPC error with
// retryAfterMs. The call is metered, captured, audited and observed whatever
// its outcome. It recovers a panic: the SDK runs handlers on a goroutine of
// its own.
func (g *Gateway) Call(ctx context.Context, req CallRequest) (res *mcp.CallToolResult, rerr error) {
	started := g.now()
	rec := &callRecord{outcome: outcomeError, status: http.StatusInternalServerError}
	defer func() {
		if p := recover(); p != nil {
			g.log.Error("mcp gateway: call panicked", "source", req.Source.Slug, "tool", req.Tool.Name, "panic_type", fmt.Sprintf("%T", p), "stack", string(debug.Stack()))
			rec.outcome, rec.status, rec.reason = outcomeError, http.StatusInternalServerError, "internal error"
			res, rerr = errorResult(msgInternal), nil
		}
		g.finish(ctx, req, rec, g.now().Sub(started))
	}()

	args := bytes.TrimSpace(req.Arguments)
	if len(args) == 0 || bytes.Equal(args, []byte("null")) {
		args = []byte("{}")
	}
	rec.bytesIn = int64(len(args))
	var obj map[string]json.RawMessage
	if args[0] != '{' || json.Unmarshal(args, &obj) != nil {
		rec.outcome, rec.status, rec.reason = outcomeError, http.StatusBadRequest, "arguments are not an object"
		return errorResult(msgBadArgs), nil
	}

	// Limits: the source's places and rate, then the key's rate limits.
	release, err := g.admit(ctx, req)
	if err != nil {
		rec.outcome, rec.status, rec.reason, rec.limited = outcomeDenied, http.StatusTooManyRequests, err.Error(), true
		return nil, err
	}
	defer release()

	if g.opts.Policy != nil {
		rec.policy = g.opts.Policy(ctx, req.Source.ServiceID)
	}
	pol := rec.policy

	// Redaction of the arguments.
	upstreamArgs, recorded := args, args
	if pol.Redact && g.opts.Redact != nil {
		out, dropped, hits, err := g.opts.Redact.Apply(args, redact.ScopeToolArguments)
		rec.redacted = append(rec.redacted, hits...)
		switch {
		case err != nil:
			rec.reason = "redaction failed"
			return errorResult(msgInternal), nil
		case dropped != nil:
			rec.outcome, rec.status, rec.reason = outcomeDenied, http.StatusBadRequest, "redaction rule "+dropped.Name
			return errorResult(msgRedactionArgs), nil
		case !json.Valid(out):
			rec.reason = "the redacted arguments are not valid JSON"
			return errorResult(msgRedactionArgs), nil
		}
		recorded = out
		if !pol.RedactForLogsOnly {
			upstreamArgs = out
		}
	}
	rec.args = recorded

	// Guardrails: the request patterns on the redacted argument JSON.
	if gs := pol.Guardrails; gs != nil && gs.Enabled && g.opts.Guardrails != nil {
		if hit, pattern := g.opts.Guardrails.InspectRequest(recorded, gs.CustomPatterns); hit {
			action := gs.Action
			if action == "" {
				action = guardrails.ActionRefuse403
			}
			rec.guardrail = &inspector.GuardrailMark{Pattern: pattern, Direction: guardrails.DirectionRequest, Action: action}
			if action != guardrails.ActionLogOnly {
				rec.outcome, rec.status, rec.reason = outcomeDenied, http.StatusForbidden, "guardrail "+pattern
				return errorResult(msgGuardrail), nil
			}
		}
	}

	// The upstream call, bounded by the source's timeout.
	cctx, cancel := context.WithTimeout(ctx, callTimeout(req.Source))
	defer cancel()
	up, err := g.opts.Upstream.CallTool(cctx, req.Source, &mcp.CallToolParams{Name: req.Tool.Native, Arguments: json.RawMessage(upstreamArgs)})
	if err != nil {
		rec.outcome, rec.status = outcomeError, http.StatusBadGateway
		rec.reason = Describe(err)
		msg := clientMessage(err)
		if errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			rec.status, rec.reason, msg = http.StatusGatewayTimeout, "the tool source did not answer in time", "the tool source did not answer in time"
		}
		return errorResult(msg), nil
	}

	// The result: only content, structuredContent and isError go on.
	raw, err := json.Marshal(&mcp.CallToolResult{Content: up.Content, StructuredContent: up.StructuredContent, IsError: up.IsError})
	if err != nil {
		rec.status, rec.reason = http.StatusBadGateway, "the result could not be encoded"
		return errorResult("the tool source sent a result that could not be read"), nil
	}
	rec.bytesOut = int64(len(raw))
	if len(raw) > g.opts.MaxResultBytes {
		rec.status, rec.reason = http.StatusBadGateway, "result over "+strconv.Itoa(g.opts.MaxResultBytes)+" bytes"
		return errorResult(fmt.Sprintf("the tool's result is larger than %d bytes", g.opts.MaxResultBytes)), nil
	}
	deliver, recordedRes := raw, raw
	if pol.Redact && g.opts.Redact != nil {
		out, dropped, hits, err := g.opts.Redact.Apply(raw, redact.ScopeToolResult)
		rec.redacted = append(rec.redacted, hits...)
		switch {
		case err != nil:
			rec.reason = "redaction failed"
			return errorResult(msgInternal), nil
		case dropped != nil:
			rec.outcome, rec.status, rec.reason = outcomeDenied, http.StatusForbidden, "redaction rule "+dropped.Name
			return errorResult(msgRedactionRes), nil
		}
		recordedRes = out
		if !pol.RedactForLogsOnly {
			deliver = out
		}
	}
	out, err := decodeResult(deliver)
	if err != nil {
		rec.status, rec.reason = http.StatusBadGateway, "the redacted result is not valid"
		return errorResult(msgRedactionRes), nil
	}
	rec.result = recordedRes

	// Guardrails: the answer patterns on the result's text.
	if gs := pol.Guardrails; gs != nil && gs.Answers.Enabled && g.opts.Guardrails != nil {
		if hit, pattern := g.opts.Guardrails.InspectAnswerText(answerText(out), gs.CustomPatterns); hit {
			action := gs.Answers.Action
			if action != guardrails.AnswerActionBlock {
				action = guardrails.AnswerActionFlag
			}
			rec.guardrail = &inspector.GuardrailMark{Pattern: pattern, Direction: guardrails.DirectionAnswer, Action: action}
			if action == guardrails.AnswerActionBlock {
				rec.outcome, rec.status, rec.reason = outcomeDenied, http.StatusForbidden, "guardrail "+pattern
				return errorResult(msgGuardrail), nil
			}
		}
	}

	rec.status, rec.outcome = http.StatusOK, outcomeOK
	if out.IsError {
		// The tool itself failed: the call reached it, the outcome is an error.
		rec.outcome, rec.reason = outcomeError, "the tool reported an error"
	}
	return out, nil
}

// decodeResult reads a result back after redaction; structuredContent keeps
// its JSON text.
func decodeResult(raw []byte) (*mcp.CallToolResult, error) {
	var out mcp.CallToolResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	var sc struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		return nil, err
	}
	out.StructuredContent = nil
	if len(sc.StructuredContent) > 0 && !bytes.Equal(sc.StructuredContent, []byte("null")) {
		out.StructuredContent = sc.StructuredContent
	}
	if out.Content == nil {
		out.Content = []mcp.Content{}
	}
	return &out, nil
}

// answerText is the text the answer patterns read (D16): the result's text
// contents, one per line, and its structuredContent as JSON, which a client
// may show as well.
func answerText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
			b.WriteByte('\n')
		}
	}
	if raw, ok := r.StructuredContent.(json.RawMessage); ok {
		b.Write(raw)
	}
	return b.String()
}

// clientMessage is what a caller is told about an upstream failure: the
// pool's own description, without text the upstream wrote.
func clientMessage(err error) string {
	var rpc *jsonrpcError
	if asJSONRPC(err, &rpc) {
		return "the tool source answered with an error"
	}
	return Describe(err)
}

// callTimeout is the source's timeout, 60 s by default, at most 300 s (D13).
func callTimeout(src db.MCPSource) time.Duration {
	d := time.Duration(src.TimeoutS) * time.Second
	switch {
	case d <= 0:
		return DefaultCallTimeout
	case d > MaxCallTimeout:
		return MaxCallTimeout
	}
	return d
}

// ----- limits ----------------------------------------------------------------

// sourceLimits are the places and the rate of one source. They outlive a
// change of the source's limits: the calls already running keep counting
// against the new max, and the bucket keeps its tokens (clamped to the new
// rpm), so a change neither resets nor doubles what a source admits.
type sourceLimits struct {
	mu       sync.Mutex
	inflight int // calls holding a place
	rpm      int // the bucket's capacity, as last configured
	tokens   float64
	at       time.Time // last refill
	used     time.Time // last admit, for pruning
}

// limitsIdle is how long the limits of a source nobody calls are kept.
const limitsIdle = 10 * time.Minute

// takePlace finds the limits of src and acquires from them in one step under
// the gateway lock. Forget and the pruning run under that lock and drop only
// limits with no call running, so the limits acquired from are still the ones
// in the map: a Forget between the lookup and the acquire would otherwise let
// the next call build fresh limits while this one held a place on the old.
// The lock order is gateway, then limits (as in pruneLocked).
func (g *Gateway) takePlace(src db.MCPSource, now time.Time) (l *sourceLimits, limit string, wait time.Duration, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	l = g.limits[src.Slug]
	if l == nil {
		if len(g.limits) >= 64 {
			g.pruneLocked(now)
		}
		l = &sourceLimits{rpm: -1, at: now}
		g.limits[src.Slug] = l
	}
	limit, wait, ok = l.acquire(src, now)
	return l, limit, wait, ok
}

// pruneLocked drops the limits of sources nobody has called for limitsIdle
// and nothing runs on (deleted sources among them).
func (g *Gateway) pruneLocked(now time.Time) {
	for slug, l := range g.limits {
		l.mu.Lock()
		idle := l.inflight == 0 && now.Sub(l.used) > limitsIdle
		l.mu.Unlock()
		if idle {
			delete(g.limits, slug)
		}
	}
}

// Forget drops the limits of a source that was changed or deleted, when no
// call of it is running; running calls keep them until they end (the next
// change or the idle pruning drops them then).
func (g *Gateway) Forget(slug string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.limits[slug]; l != nil {
		l.mu.Lock()
		idle := l.inflight == 0
		l.mu.Unlock()
		if idle {
			delete(g.limits, slug)
		}
	}
}

// acquire takes a place (max 0 = no concurrency limit, as documented for the
// source's max_concurrent) and one request from the per-minute bucket
// (capacity rpm, refilled at rpm per minute; rpm 0 = no limit). It returns
// which limit refused and how long to wait, or ok.
func (l *sourceLimits) acquire(src db.MCPSource, now time.Time) (limit string, wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.used = now
	if src.MaxConcurrent > 0 && l.inflight >= src.MaxConcurrent {
		return "source_concurrency", concurrencyRetryAfter, false
	}
	if src.RPM > 0 {
		perSec := float64(src.RPM) / 60
		switch {
		case l.rpm < 0:
			l.tokens = float64(src.RPM) // first use: a full bucket
		case l.rpm != src.RPM:
			l.tokens = min(l.tokens, float64(src.RPM))
		}
		l.rpm = src.RPM
		l.tokens = min(float64(src.RPM), l.tokens+now.Sub(l.at).Seconds()*perSec)
		l.at = now
		if l.tokens < 1 {
			return "source_rpm", time.Duration((1 - l.tokens) / perSec * float64(time.Second)), false
		}
		l.tokens--
	} else {
		l.rpm, l.at = 0, now
	}
	l.inflight++
	return "", 0, true
}

func (l *sourceLimits) release() {
	l.mu.Lock()
	l.inflight--
	l.mu.Unlock()
}

// admit applies the limits of a call (D13). The key's rate limits come
// first, so a key over its own limit does not use up the source's rpm for
// the other keys; then a place of the source (not waited for) and the
// source's rpm. It returns the function that gives the place back, or the
// limit error.
func (g *Gateway) admit(ctx context.Context, req CallRequest) (func(), error) {
	// A key's call is charged to the key's limits; a grant's to none of a
	// key's. Both count against the backing service's and global limits.
	who := quota.Subjects{ServiceID: req.Source.ServiceID}
	switch {
	case req.Caller.GatewayKeyID != "":
		who.APIKeyID, who.GatewayKeyID = gatewayKeySubject+req.Caller.GatewayKeyID, req.Caller.GatewayKeyID
	case req.Caller.GrantID != "":
		who.APIKeyID = oauthGrantSubject + req.Caller.GrantID
	}
	if g.opts.Limits != nil && who.APIKeyID != "" {
		d := g.opts.Limits.ChargeRate(ctx, who, quota.DimensionRPM, 1)
		if !d.Allow {
			return nil, limitError("rate limit reached", "rate_limit", time.Duration(max(d.RetryAfter, 1))*time.Second)
		}
	}
	now := g.now()
	l, limit, wait, ok := g.takePlace(req.Source, now)
	switch {
	case ok:
		return l.release, nil
	case limit == "source_concurrency":
		return nil, limitError(fmt.Sprintf("the tool source allows %d calls at a time", req.Source.MaxConcurrent), limit, wait)
	default:
		return nil, limitError(fmt.Sprintf("the tool source allows %d calls per minute", req.Source.RPM), limit, wait)
	}
}

// gatewayKeySubject prefixes a gateway key id in quota.Subjects.APIKeyID, as
// the AI chain does (aigw.GatewayKeySubjectPrefix).
const gatewayKeySubject = "gw:"

// oauthGrantSubject prefixes an OAuth grant id there: no api_key limit names
// it, so only the service's and global limits apply to a grant's calls.
const oauthGrantSubject = "oauth:"

// ----- records ---------------------------------------------------------------

// finish writes what a call leaves behind: the usage row, the inspector
// entry, the audit row and the observer event.
func (g *Gateway) finish(ctx context.Context, req CallRequest, rec *callRecord, took time.Duration) {
	// The records must not be lost to a caller that went away.
	ctx = context.WithoutCancel(ctx)
	src := req.Source
	if g.opts.Meter != nil {
		_ = g.opts.Meter.Record(ctx, aimeter.Sample{
			ServiceID: src.ServiceID, Kind: aimeter.KindMCP,
			BytesIn: rec.bytesIn, BytesOut: rec.bytesOut, UpstreamStatus: rec.status,
			GatewayKeyID: req.Caller.GatewayKeyID, OAuthGrantID: req.Caller.GrantID, ProviderSlug: src.Slug,
			RequestedModel: req.Tool.Name, TargetModel: req.Tool.Native,
			RequestID: req.Info.RequestID, LatencyMs: took.Milliseconds(),
		})
	}
	if rec.policy.Inspect && g.opts.Inspector != nil {
		g.capture(req, rec, took)
	}
	payload := map[string]any{"tool": req.Tool.Name, "status": rec.status, "latency_ms": took.Milliseconds()}
	if req.Caller.GrantID != "" {
		payload["oauth_grant_id"] = req.Caller.GrantID
	} else {
		payload["gateway_key_id"] = req.Caller.GatewayKeyID
	}
	if req.Tool.Native != req.Tool.Name {
		payload["native_tool"] = req.Tool.Native
	}
	if rec.reason != "" {
		payload["reason"] = rec.reason
	}
	if rec.guardrail != nil {
		payload["guardrail"] = map[string]string{"pattern": rec.guardrail.Pattern, "direction": rec.guardrail.Direction, "action": rec.guardrail.Action}
	}
	if !rec.limited || g.sample("limit", req.Caller.subject(), src.Slug) {
		g.appendAudit(ctx, req.Caller, src, req.Info, rec.outcome, payload)
	}
	g.observe(req.Caller, src, req.Tool.Name, rec.outcome, rec.status, took)
}

// capture adds the call to the inspector ring of the source's backing
// service. Arguments and result are recorded as redacted.
func (g *Gateway) capture(req CallRequest, rec *callRecord, took time.Duration) {
	max := rec.policy.InspectMax
	if max <= 0 {
		max = inspector.DefaultMaxRequests
	}
	reqBody, reqOmitted, reqTrunc := inspector.TruncateRequest(rec.args)
	respBody, respOmitted, respTrunc := inspector.TruncateResponse(rec.result)
	hits := make([]inspector.RedactionHit, 0, len(rec.redacted))
	for _, h := range rec.redacted {
		hits = append(hits, inspector.RedactionHit{Rule: h.Rule.Name, Count: h.Count})
	}
	var params json.RawMessage
	if len(rec.args) > 0 && len(rec.args) <= inspector.MaxReqBodyBytes {
		params = append(json.RawMessage(nil), rec.args...)
	}
	g.opts.Inspector.GetOrCreate(req.Source.ServiceID, max).Capture(inspector.Entry{
		ID: inspector.NewID(), ServiceID: req.Source.ServiceID, TS: g.now().UTC(),
		Method: http.MethodPost, Path: req.Info.Path, Status: rec.status, DurationMs: took.Milliseconds(),
		BytesIn: rec.bytesIn, BytesOut: rec.bytesOut,
		ReqHeaders: map[string]string{"Content-Type": "application/json"}, ReqBody: reqBody,
		RespHeaders: map[string]string{"Content-Type": "application/json"}, RespBody: respBody,
		Truncated: reqTrunc || respTrunc, BytesOmitted: reqOmitted + respOmitted,
		Redactions: hits, Dropped: []string{}, RemoteIP: req.Info.RemoteIP, TraceID: req.Info.RequestID,
		MCP:      &inspector.MCPInfo{Method: "tools/call", Tool: req.Tool.Name, Params: params},
		Provider: req.Source.Slug, Model: req.Tool.Name, GatewayKeyID: req.Caller.GatewayKeyID, OAuthGrantID: req.Caller.GrantID,
		Guardrail: rec.guardrail,
	})
}

// appendAudit writes an mcp.gateway.call row (D30): every error and
// refusal, and at most one ok call per key, source and minute. (Limit
// refusals are sampled the same way by finish.)
func (g *Gateway) appendAudit(ctx context.Context, c Caller, src db.MCPSource, info RequestInfo, outcome string, payload map[string]any) {
	if g.opts.Audit == nil {
		return
	}
	if outcome == outcomeOK && !g.sample("ok", c.subject(), src.Slug) {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			g.log.Error("mcp gateway: audit write panicked", "panic_type", fmt.Sprintf("%T", p))
		}
	}()
	if err := g.opts.Audit.Append(ctx, audit.Event{
		ActorID: c.UserID, Action: audit.ActionMCPGatewayCall,
		SubjectID: src.Slug, SubjectLabel: src.Slug, Result: outcome,
		SourceIP: info.RemoteIP, UserAgent: info.UserAgent, RequestID: info.RequestID,
		Payload: audit.MustJSON(payload),
	}); err != nil {
		g.log.Warn("mcp gateway: audit write failed", "err", err)
	}
}

// sample reports whether a call of kind ("ok" or "limit") by key on source
// is audited: the first one of each minute.
func (g *Gateway) sample(kind, key, slug string) bool {
	now := g.now()
	minute := now.Truncate(time.Minute)
	k := kind + "|" + key + "|" + slug
	g.mu.Lock()
	defer g.mu.Unlock()
	if last, ok := g.sampled[k]; ok && last.Equal(minute) {
		return false
	}
	if len(g.sampled) >= 4096 {
		for kk, at := range g.sampled {
			if !at.Equal(minute) {
				delete(g.sampled, kk)
			}
		}
	}
	g.sampled[k] = minute
	return true
}

// observe hands the call to the observer (metrics: source and outcome).
// src.Slug comes from the stored source row, so the label is a valid slug.
func (g *Gateway) observe(c Caller, src db.MCPSource, tool, outcome string, status int, took time.Duration) {
	if g.opts.Observer == nil {
		return
	}
	defer func() { _ = recover() }()
	g.opts.Observer.ObserveToolCall(observe.ToolCall{
		Server: "gateway", Source: src.Slug, Tool: tool, GatewayKeyID: c.GatewayKeyID, OAuthGrantID: c.GrantID, UserID: c.UserID,
		Outcome: outcome, Status: status, Duration: took, LatencyMs: took.Milliseconds(),
	})
}
