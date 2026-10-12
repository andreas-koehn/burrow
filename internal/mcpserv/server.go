// Package mcpserv is Burrow's management MCP server: the relay's own
// management surface as a closed set of tools for MCP clients.
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
// Permissions: every tool declares an authz.Permission string that maps to
// the SAME permission the equivalent REST endpoint enforces (e.g.
// tunnels_list → tunnels:read:any). Cookie/admin shortcuts are NOT applied;
// the bearer token's permission set is the source of truth, intersected
// with the minter user's CURRENT role (matches Task 18 effectivePerms). A
// token sees and can call exactly the tools that rule allows.
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
// One audit row (mcp.tool.call) is written per tools/call on a tool of the
// registry: ok, error, or denied.
package mcpserv

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
	bearer   BearerStore
	users    UserLookup
	log      *slog.Logger
	tools    map[string]Tool
	ordered  []Tool           // declaration order
	toolList []ToolDescriptor // the same order, for Tools()
	audit    AuditAppender    // optional; nil = audit hook disabled

	rateLimit  int
	rateWindow time.Duration
	handler    http.Handler
}

// Option adjusts a Server at construction.
type Option func(*Server)

// WithRateLimit replaces the default rate limit: at most requests per
// automation token per window.
func WithRateLimit(requests int, window time.Duration) Option {
	return func(s *Server) { s.rateLimit, s.rateWindow = requests, window }
}

// SetAuditAppender installs an audit appender. The MCP server emits one
// mcp.tool.call audit row per tools/call dispatch — both denied (the
// bearer's perms did not include the tool's required permission) and ok.
// Nil disables the audit hook entirely.
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
// the answer is never blocked on the audit append. The row names the tool
// and the outcome and nothing else: no arguments, no result.
func (s *Server) emitToolAudit(ctx context.Context, tok *authedToken, toolName, result string) {
	if s == nil || s.audit == nil || tok == nil {
		return
	}
	_ = s.audit.Append(ctx, audit.Event{
		ActorID:      tok.userID,
		Action:       audit.ActionMCPToolCall,
		SubjectLabel: toolName,
		Result:       result,
	})
}

// New returns a Server with the closed 12-tool inventory installed. Pass nil
// for bearer/users in tests that only exercise the registry; every request
// is then answered 500, never served.
//
// The toolStore parameter is the production wiring seam — cmd/server
// constructs a single adapter over *store.Store and passes it here. Tests
// provide a fake.
func New(bearer BearerStore, users UserLookup, toolStore ToolStore, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.Default()
	}
	tools := buildTools(toolStore)
	ordered := make([]Tool, 0, len(orderedTools))
	list := make([]ToolDescriptor, 0, len(orderedTools))
	for _, name := range orderedTools {
		t := tools[name]
		ordered = append(ordered, t)
		list = append(list, ToolDescriptor{
			Name:             t.Name,
			Description:      t.Description,
			ParametersSchema: t.ParamsSchema,
			Permission:       string(t.Permission),
		})
	}
	s := &Server{
		bearer:     bearer,
		users:      users,
		log:        log,
		tools:      tools,
		ordered:    ordered,
		toolList:   list,
		rateLimit:  DefaultRateLimit,
		rateWindow: DefaultRateWindow,
	}
	for _, opt := range opts {
		opt(s)
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
	return s
}

// ToolDescriptor is the read-only view of a Tool exposed via the JSON API's
// GET /api/v1/mcp/tools endpoint. The api package consumes this slice via
// Server.Tools() — there is no need to expose the live Call func.
type ToolDescriptor struct {
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	ParametersSchema json.RawMessage `json:"parameters_schema"`
	Permission       string          `json:"permission"`
}

// Tools returns the closed tool inventory in stable declaration order.
// Used by the dashboard API to render the operator's "available tools"
// surface and by callers that wish to enumerate without issuing a tools/list
// RPC. The returned slice is shared — callers MUST NOT mutate it.
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
// request context. Bearer auth runs BEFORE any JSON-RPC parsing, so a
// malformed body without a valid token is a 401, not a parse error.
func (s *Server) requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, err := s.authenticate(r)
		if err != nil {
			writeAuthErr(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	})
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
	role    string
	perms   []string
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
		tokenID: info.ID,
		userID:  u.ID,
		role:    u.Role,
		perms:   info.Permissions,
	}, nil
}

// Sentinel auth errors — internal to the package, never wire-visible.
var (
	errAuthMissing  = errors.New("missing bearer token")
	errAuthInvalid  = errors.New("invalid bearer token")
	errAuthInternal = errors.New("internal error")
)

// ErrForbidden is the sentinel a Tool.Call returns when the underlying
// store rejects the request on permission grounds. The call is audited as
// "denied" and answered with an isError result.
var ErrForbidden = errors.New("forbidden")

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
	ctxKeyCallerID ctxKey = iota
	ctxKeyCallerRole
	ctxKeyToken
	ctxKeyMCPServer
)

func withCaller(ctx context.Context, uid, role string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyCallerID, uid)
	ctx = context.WithValue(ctx, ctxKeyCallerRole, role)
	return ctx
}

// withToken stores the authenticated token of an HTTP request.
func withToken(ctx context.Context, tok *authedToken) context.Context {
	return context.WithValue(ctx, ctxKeyToken, tok)
}

// tokenFrom returns the token withToken stored, or nil.
func tokenFrom(ctx context.Context) *authedToken {
	tok, _ := ctx.Value(ctxKeyToken).(*authedToken)
	return tok
}

// CallerID returns the bearer-authed user id for the current MCP request,
// or "" when invoked outside an MCP tool call. Exported so tool wrappers
// in this package (tools.go) can resolve "owned by" semantics without
// reaching back into the Server.
func CallerID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyCallerID).(string)
	return v
}

// CallerRole returns the user's CURRENT role for the bearer-authed request.
func CallerRole(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyCallerRole).(string)
	return v
}

// ----- sha256Hex (kept local) ----------------------------------------------

// Imported via api.sha256Hex would create a dependency; the algorithm is
// trivial and the helper is hot-path private. The bytes hashed are the
// "bua_…" plaintext exactly as the user supplies them — matches the
// AutomationTokenView.Hash that the store wrote at mint time.
// Implementation lives in token_hash.go.
