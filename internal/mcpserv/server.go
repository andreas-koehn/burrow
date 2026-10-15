// Package mcpserv is Burrow's management MCP server: the relay's own JSON
// API, offered to MCP clients as tools.
//
// Protocol: the official Go SDK serves the Streamable HTTP transport,
// stateless, for the MCP revisions 2026-07-28, 2025-11-25, 2025-06-18 and
// 2025-03-26 (sdk.go). The package is an http.Handler; the API router mounts
// it at /mcp/burrow on the main address.
//
// Auth: every request MUST carry "Authorization: Bearer bua_<token>" where
// the token is an automation bearer. Sessions / cookies are NOT honored
// here — the endpoint is for non-browser (LLM-client) automation, and a
// request a browser sends from another origin is refused before the token
// is looked at.
//
// Tools: the operations of the OpenAPI document (registry.go) are the tools.
// A short list of them (curated.go) is offered as tools of their own. A call
// is replayed as an HTTP request through the relay's own router with the
// caller's Authorization header (dispatch.go), so the permission gate, the
// validation, the handler and the audit rows are those of the REST call. A
// token is shown the tools whose x-burrow-permission it satisfies (mayUse),
// the same rule the API's gate applies: the token declares the permission
// AND the role of its user grants it now.
//
// What a request passes, in order:
//
//	checkOrigin    403  a browser on another origin
//	requireBearer  401  missing, unknown or expired token, suspended user
//	               500  the token store failed
//	rate limit     429  more than the token's share per minute
//	the SDK        413  body over 1 MiB (checked by mcpHandler)
//	               400  a JSON-RPC batch (mcpHandler)
//	               405  anything but POST
//	               200  JSON-RPC, errors included
//
// One audit row (mcp.tool.call) is written per tools/call on a known tool:
// ok, dry_run, error, or denied. It names the token's user, the client
// address, the user agent and the request id, like the rows of the JSON API
// (auditContext); the API writes its own rows for what the call changes.
package mcpserv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// EndpointPath is where the API router mounts the server on the main address.
const EndpointPath = "/mcp/burrow"

// The default rate limit: requests per automation token per window. A client
// on a 2025 revision spends three requests before its first tool call.
const (
	DefaultRateLimit  = 300
	DefaultRateWindow = time.Minute
)

// AuditAppender is the narrow append surface the MCP server uses to record
// one mcp.tool.call row per tools/call dispatch. *audit.Logger satisfies it
// directly; the field is optional (nil → audit hook disabled).
type AuditAppender interface {
	Append(ctx context.Context, e audit.Event) error
}

// ----- Bearer auth surface -------------------------------------------------

// TokenInfo is the narrow view of an automation bearer row the MCP server
// needs from BearerStore. The same shape as api.AutomationTokenInfo but
// kept here so mcpserv doesn't depend on the api package.
type TokenInfo struct {
	ID          string
	UserID      string
	Permissions []string
	ExpiresAt   *time.Time
}

// BearerStore is the lookup + touch surface the bearer middleware consumes.
// The production adapter wraps *store.Store; tests provide an in-memory fake.
type BearerStore interface {
	// LookupBearer returns the row matching the sha256-hex hash. It MUST
	// return db.ErrNotFound when no token has that hash so the middleware
	// can map missing/wrong tokens to a uniform 401.
	LookupBearer(ctx context.Context, hash string) (TokenInfo, error)
	// TouchBearer best-effort updates last_used; the middleware never
	// fails the request on a touch error.
	TouchBearer(ctx context.Context, id string) error
}

// UserLookup is the narrow user-fetch surface used to resolve the bearer
// token's user → current role (so role demotion narrows reach in real-time).
type UserLookup interface {
	GetUserByID(ctx context.Context, id string) (db.User, error)
}

// ----- Server --------------------------------------------------------------

// Server is the management MCP server, an http.Handler. The zero value is
// NOT usable — call New().
type Server struct {
	bearer BearerStore
	users  UserLookup
	log    *slog.Logger
	audit  AuditAppender // optional; nil = audit hook disabled

	// reg is the registry of the OpenAPI document New was given.
	reg *Registry
	// curated are the operations of curatedTools, in that order;
	// curatedByName finds one by its tool name.
	curated       []Operation
	curatedByName map[string]Operation
	// curatedDesc holds the descriptions curatedTools gives, by operation id.
	curatedDesc map[string]string
	// toolSchemas holds the marshalled input schema of each curated
	// operation, by operation id. Read only after New.
	toolSchemas map[string]json.RawMessage
	toolList    []ToolDescriptor // the curated tools, for Tools()
	// validators caches the resolved argument schema of an operation, by id
	// (*jsonschema.Resolved).
	validators sync.Map
	// api is the relay's router (SetAPIHandler); nil until it is set.
	api atomic.Pointer[http.Handler]

	rateLimit int
	// toolTimeout overrides the package constant of that name; tests only.
	toolTimeout time.Duration
	rateWindow  time.Duration
	handler     http.Handler
}

// Option adjusts a Server at construction.
type Option func(*Server)

// WithRateLimit replaces the default rate limit: at most requests per
// automation token per window.
func WithRateLimit(requests int, window time.Duration) Option {
	return func(s *Server) { s.rateLimit, s.rateWindow = requests, window }
}

// SetAuditAppender installs an audit appender. The MCP server emits one
// mcp.tool.call audit row per tools/call on a known tool: ok, dry_run,
// error, or denied (the token may not use the tool, or the API answered 401
// or 403). Nil disables the audit hook entirely.
//
// Safe to call once at startup; not safe to mutate after the server has
// been handed to goroutines.
func (s *Server) SetAuditAppender(a AuditAppender) {
	if s == nil {
		return
	}
	s.audit = a
}

// emitToolAudit appends one mcp.tool.call row recording the (tool, result)
// pair under the bearer's user as actor. Nil-safe — best-effort on writes;
// the answer is never blocked on the audit append. The row names the tool,
// the outcome, who called and from where (the request's audit context), and
// nothing else: no arguments, no result.
func (s *Server) emitToolAudit(ctx context.Context, tok *authedToken, toolName, result string) {
	if s == nil || s.audit == nil || tok == nil {
		return
	}
	lc := audit.LogContextFrom(ctx)
	_ = s.audit.Append(ctx, audit.Event{
		ActorID:      tok.userID,
		ActorEmail:   tok.email,
		Action:       audit.ActionMCPToolCall,
		SubjectLabel: toolName,
		Result:       result,
		SourceIP:     lc.SourceIP,
		UserAgent:    lc.UserAgent,
		RequestID:    lc.RequestID,
	})
}

// New returns a Server whose tools are built from spec, the bytes of the
// relay's OpenAPI document. It fails when the document does not make a
// registry (NewRegistry) or when a curated tool names an operation the
// registry does not have. Pass nil for bearer/users in tests that serve
// mcpHandler directly; every request through ServeHTTP is then answered 500.
//
// A call is answered "the relay is starting" until SetAPIHandler hands the
// server the router to replay it through.
func New(bearer BearerStore, users UserLookup, spec []byte, log *slog.Logger, opts ...Option) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	reg, err := NewRegistry(spec)
	if err != nil {
		return nil, err
	}
	s := &Server{
		bearer:        bearer,
		users:         users,
		log:           log,
		reg:           reg,
		curatedByName: map[string]Operation{},
		curatedDesc:   map[string]string{},
		toolSchemas:   map[string]json.RawMessage{},
		rateLimit:     DefaultRateLimit,
		rateWindow:    DefaultRateWindow,
	}
	for _, opt := range opts {
		opt(s)
	}
	for _, ct := range curatedTools {
		op, ok := reg.Lookup(ct.ID)
		if !ok {
			return nil, fmt.Errorf("curated tool %s: no such operation in the spec", ct.ID)
		}
		schema, err := json.Marshal(op.InputSchema())
		if err != nil {
			return nil, fmt.Errorf("curated tool %s: input schema: %w", ct.ID, err)
		}
		s.curated = append(s.curated, op)
		s.curatedByName[ToolName(op.ID)] = op
		s.toolSchemas[op.ID] = schema
		if ct.Description != "" {
			s.curatedDesc[op.ID] = ct.Description
		}
		perms := make([]string, 0, len(op.Access.AnyOf))
		for _, p := range op.Access.AnyOf {
			perms = append(perms, string(p))
		}
		first := ""
		if len(perms) > 0 {
			first = perms[0]
		}
		s.toolList = append(s.toolList, ToolDescriptor{
			Name:             ToolName(op.ID),
			Description:      s.description(op),
			ParametersSchema: schema,
			Permission:       first,
			Permissions:      perms,
			Kind:             op.Kind(),
			Generic:          false,
		})
	}
	limit := httprate.Limit(s.rateLimit, s.rateWindow,
		httprate.WithKeyFuncs(func(r *http.Request) (string, error) {
			return tokenFrom(r.Context()).tokenID, nil
		}),
		httprate.WithLimitHandler(func(w http.ResponseWriter, _ *http.Request) {
			writeHTTPError(w, http.StatusTooManyRequests, "rate limit exceeded")
		}),
	)
	s.handler = checkOrigin(s.requireBearer(limit(s.touchBearer(s.mcpHandler()))))
	return s, nil
}

// SetAPIHandler hands the server the relay's router: every tool call is
// replayed through it. Calls before it is set are answered "the relay is
// starting". Safe to call while requests are served.
func (s *Server) SetAPIHandler(h http.Handler) { s.api.Store(&h) }

// OperationCount is the number of operations the generic tools reach. There
// are none yet.
func (s *Server) OperationCount() int { return 0 }

// maxDescriptionBytes bounds the description of a tool built from an
// operation's summary and description.
const maxDescriptionBytes = 1024

// description is what a tool built from op says about itself: the text
// curatedTools gives, or the operation's summary and the first paragraph of
// its description, cut to maxDescriptionBytes.
func (s *Server) description(op Operation) string {
	if d := s.curatedDesc[op.ID]; d != "" {
		return d
	}
	d := op.Summary
	if para, _, _ := strings.Cut(strings.TrimSpace(op.Description), "\n\n"); para != "" {
		if d != "" {
			d += "\n\n"
		}
		d += strings.TrimSpace(para)
	}
	if len(d) > maxDescriptionBytes {
		cut := maxDescriptionBytes
		for cut > 0 && !utf8.RuneStart(d[cut]) {
			cut--
		}
		d = d[:cut]
	}
	return d
}

// ToolDescriptor is the read-only view of a tool exposed via the JSON API's
// GET /api/v1/mcp/tools endpoint.
type ToolDescriptor struct {
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	ParametersSchema json.RawMessage `json:"parameters_schema"`
	// Permission is the first of Permissions, "" for an operation every
	// token may call.
	Permission string `json:"permission"`
	// Permissions are those of which the token needs one (x-burrow-permission
	// any_of).
	Permissions []string `json:"permissions"`
	Kind        string   `json:"kind"` // read | write | delete
	// Generic is true for the tools that reach any operation.
	Generic bool `json:"generic"`
}

// Tools returns the curated tools in the order of curatedTools. Used by the
// dashboard API to render the operator's "available tools" surface. The
// returned slice is shared — callers MUST NOT mutate it.
func (s *Server) Tools() []ToolDescriptor { return s.toolList }

// ----- HTTP entry point ----------------------------------------------------

// ServeHTTP implements http.Handler: origin check, bearer auth, the rate
// limit of the token, then the SDK.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// checkOrigin refuses what a browser sends from another origin, before the
// token is looked at. The check is the standard library's: a request with
// neither Origin nor Sec-Fetch-Site passes (that is every CLI client), a
// same-origin request passes, a cross-origin or cross-site one is refused.
// GET, HEAD and OPTIONS pass too and change nothing here: the endpoint
// answers POST only, and a preflight gets no CORS header back, so the
// browser never sends the request it asked about.
func checkOrigin(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := cop.Check(r); err != nil {
			writeHTTPError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireBearer authenticates the request and hands the token on in the
// request context, together with the audit context of the request. Bearer
// auth runs BEFORE any JSON-RPC parsing, so a malformed body without a valid
// token is a 401, not a parse error.
func (s *Server) requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, err := s.authenticate(r)
		if err != nil {
			writeAuthErr(w, err)
			return
		}
		ctx := audit.WithLogContext(withToken(r.Context(), tok), auditContext(r, tok))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// auditContext is who acts and from where, for the mcp.tool.call rows of
// this request (emitToolAudit reads it with audit.LogContextFrom). It holds
// what the JSON API's withAuditContext holds, built the same way; the API
// request a call becomes carries the same address, user agent and request id
// (dispatch.go), so the API's own rows for the change name the same.
//
// The address is r.RemoteAddr: the API router's TrustedProxyMiddleware has
// put the client's address there when a trusted proxy forwarded the request.
// No forwarding header is read here. The request id is the one of the
// router's request-id middleware, empty when the handler is served without
// it. The audit model has no field for the token, so the row does not name
// it.
//
// Address, user agent and request id are text the caller supplies (the
// request id is taken from an X-Request-Id header when one is sent, the
// address from a forwarding header behind a trusted proxy): each goes
// through auditText.
func auditContext(r *http.Request, tok *authedToken) audit.LogContext {
	ip := r.RemoteAddr
	if h, _, err := net.SplitHostPort(ip); err == nil {
		ip = h
	}
	return audit.LogContext{
		ActorID:    tok.userID,
		ActorEmail: tok.email,
		SourceIP:   auditText(ip),
		UserAgent:  auditText(r.UserAgent()),
		RequestID:  auditText(middleware.GetReqID(r.Context())),
	}
}

// maxAuditTextBytes bounds each piece of text a caller can put into an audit
// row. A row is written for every tools/call, reads and refused calls
// included, so without a bound one token could add a megabyte per call.
const maxAuditTextBytes = 256

// auditText cuts s to maxAuditTextBytes, between two characters, and makes
// it valid UTF-8: a header value may hold any bytes, and a database that
// stores text as UTF-8 refuses a row that is not, which would let a caller
// keep its calls out of the audit log.
func auditText(s string) string {
	if len(s) > maxAuditTextBytes {
		cut := maxAuditTextBytes
		for i := 1; i < utf8.UTFMax && !utf8.RuneStart(s[cut]); i++ {
			cut--
		}
		s = s[:cut]
	}
	if utf8.ValidString(s) {
		return s
	}
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	for len(s) > maxAuditTextBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// touchBearer records last_used. It sits behind the rate limit, so a request
// that is refused with 429 costs no write.
func (s *Server) touchBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := tokenFrom(r.Context()); tok != nil {
			_ = s.bearer.TouchBearer(r.Context(), tok.tokenID)
		}
		next.ServeHTTP(w, r)
	})
}

// ----- Auth ----------------------------------------------------------------

// authedToken is the resolved (token, user) pair for a bearer-authed
// request. .can() implements the AND of (token declares perm) AND (user's
// current role grants perm) — the same shape as api.effectivePerms.
type authedToken struct {
	tokenID string
	userID  string
	email   string // for the audit trail
	role    string
	perms   []string
	// bearer is the request's whole Authorization value; a tool call sends
	// it on to the API.
	bearer string
	// remoteAddr and userAgent are the request's, for the API request of a
	// tool call.
	remoteAddr string
	userAgent  string
}

// mayUse reports whether the token may call op: the same decision the
// API's gate takes for the route, from the operation's x-burrow-permission.
// The list of a request and the check of a call both ask here.
func (t *authedToken) mayUse(op Operation) bool {
	switch op.Access.Kind {
	case "public", "self":
		return true
	case "needs":
		if op.Access.Every {
			for _, p := range authz.AllPermissions() {
				if !t.can(p) {
					return false
				}
			}
		}
		for _, p := range op.Access.AnyOf {
			if t.can(p) {
				return true
			}
		}
		return false
	default: // "dashboard" never reaches the registry; anything else is refused
		return false
	}
}

// can returns true when the requested permission is in BOTH the bearer's
// declared permission set AND the user's current role's grant set. Admin
// passes trivially via authz.Can.
func (t *authedToken) can(p authz.Permission) bool {
	have := false
	for _, bp := range t.perms {
		if bp == string(p) {
			have = true
			break
		}
	}
	if !have {
		return false
	}
	return authz.Can(t.role, p)
}

// authenticate resolves the Authorization header to an authedToken or
// returns an error suitable for writeAuthErr. The function returns nil,
// errAuthMissing if the header is absent so callers can distinguish
// "unauthenticated" from "bad credentials" if needed in the future.
func (s *Server) authenticate(r *http.Request) (*authedToken, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return nil, errAuthMissing
	}
	plain := strings.TrimPrefix(auth, "Bearer ")
	if !strings.HasPrefix(plain, "bua_") {
		return nil, errAuthInvalid
	}
	if s.bearer == nil || s.users == nil {
		return nil, errAuthInternal
	}
	info, err := s.bearer.LookupBearer(r.Context(), sha256Hex(plain))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, errAuthInvalid
		}
		return nil, errAuthInternal
	}
	if info.ExpiresAt != nil && info.ExpiresAt.Before(time.Now().UTC()) {
		return nil, errAuthInvalid
	}
	u, err := s.users.GetUserByID(r.Context(), info.UserID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, errAuthInvalid
		}
		return nil, errAuthInternal
	}
	if u.Status == "suspended" {
		return nil, errAuthInvalid
	}
	return &authedToken{
		tokenID:    info.ID,
		userID:     u.ID,
		email:      u.Email,
		role:       u.Role,
		perms:      info.Permissions,
		bearer:     auth,
		remoteAddr: r.RemoteAddr,
		userAgent:  r.UserAgent(),
	}, nil
}

// Sentinel auth errors — internal to the package, never wire-visible.
var (
	errAuthMissing  = errors.New("missing bearer token")
	errAuthInvalid  = errors.New("invalid bearer token")
	errAuthInternal = errors.New("internal error")
)

// writeAuthErr renders a bearer-auth failure as HTTP 401 with a JSON body
// and the challenge an MCP client expects. 500 is used only for internal
// lookup faults.
func writeAuthErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errAuthInternal) {
		writeHTTPError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Missing OR invalid bearer both render identically — never tell
	// an unauthenticated caller whether the token "would" have been
	// valid.
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeHTTPError(w, http.StatusUnauthorized, "invalid bearer token")
}

// writeHTTPError writes {"error": msg} with the given status: the shape of
// the rest of the API (writeErr in api/middleware.go) for everything that
// is refused before the MCP layer.
func writeHTTPError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ----- ctx plumbing --------------------------------------------------------

type ctxKey int

const (
	ctxKeyToken ctxKey = iota
	ctxKeyMCPServer
)

// withToken stores the authenticated token of an HTTP request.
func withToken(ctx context.Context, tok *authedToken) context.Context {
	return context.WithValue(ctx, ctxKeyToken, tok)
}

// tokenFrom returns the token withToken stored, or nil.
func tokenFrom(ctx context.Context) *authedToken {
	tok, _ := ctx.Value(ctxKeyToken).(*authedToken)
	return tok
}

// ----- sha256Hex (kept local) ----------------------------------------------

// Imported via api.sha256Hex would create a dependency; the algorithm is
// trivial and the helper is hot-path private. The bytes hashed are the
// "bua_…" plaintext exactly as the user supplies them — matches the
// AutomationTokenView.Hash that the store wrote at mint time.
// Implementation lives in token_hash.go.
