// Package mcpserv implements Burrow's `burrowd mcp` server (spec Part P).
//
// The MCP server exposes Burrow's management surface as a closed set of
// JSON-RPC 2.0 tools over HTTP. v0.4.0 supports the "tools/list" and
// "tools/call" methods only; "tools/streamable" SSE is deferred to v0.5 (Q2).
//
// Auth: every request MUST carry "Authorization: Bearer bua_<token>" where
// the token is an automation bearer minted via Task 18. Sessions / cookies
// are NOT honored here — the MCP listener is intended for non-browser
// (LLM-client) automation and binds to a dedicated port (default :7800,
// OFF unless configured).
//
// Permissions: every tool declares an authz.Permission string that maps to
// the SAME permission the equivalent REST endpoint enforces (e.g.
// tunnels_list → tunnels:read:any). Cookie/admin shortcuts are NOT applied;
// the bearer token's permission set is the source of truth, intersected
// with the minter user's CURRENT role (matches Task 18 effectivePerms).
//
// Wire format (request):
//
//	POST / HTTP/1.1
//	Content-Type: application/json
//	Authorization: Bearer bua_xxx
//	{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}
//
// Wire format (response):
//
//	200 OK
//	Content-Type: application/json
//	{"jsonrpc":"2.0","id":1,"result":{"tools":[...]}}
//
// Errors follow JSON-RPC 2.0:
//
//	-32700 parse error           (malformed JSON)
//	-32600 invalid request       (missing jsonrpc/method, etc.)
//	-32601 method not found
//	-32602 invalid params        (unknown tool name in tools/call)
//	-32603 internal error        (also used for "forbidden")
//
// Authentication failures (missing/invalid bearer) return HTTP 401 with a
// JSON body — they happen BEFORE JSON-RPC parsing so they cannot embed an
// rpc id.
package mcpserv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
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

// Server is the MCP JSON-RPC 2.0 HTTP handler. The zero value is NOT
// usable — call New().
type Server struct {
	bearer   BearerStore
	users    UserLookup
	log      *slog.Logger
	tools    map[string]Tool
	ordered  []Tool           // declaration order, for the per-token MCP server
	toolList []ToolDescriptor // stable order, for tools/list
	audit    AuditAppender    // optional; nil = audit hook disabled
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
// the JSON-RPC response is never blocked on the audit append.
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
// for bearer/users in tests that only exercise the registry; the JSON-RPC
// handler will still 401 every authed request, which is the correct shape.
//
// The toolStore parameter is the production wiring seam — cmd/server (Task
// 25) constructs a single *store.Store and passes it here. Tests provide a
// fake.
func New(bearer BearerStore, users UserLookup, toolStore ToolStore, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	tools := buildTools(toolStore)
	// buildTools returns a map; orderedTools gives the list its order.
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
	return &Server{
		bearer:   bearer,
		users:    users,
		log:      log,
		tools:    tools,
		ordered:  ordered,
		toolList: list,
	}
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

// ----- JSON-RPC plumbing ---------------------------------------------------

// JSON-RPC 2.0 error codes (per the spec).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// rpcRequest is the wire shape of an incoming JSON-RPC 2.0 request.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// rpcResponse is the wire shape of a JSON-RPC 2.0 response. Exactly one of
// Result / Error is populated on success / failure respectively.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// callRequest is the params shape of a tools/call request.
type callRequest struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// listResult is the result shape of a tools/list response.
type listResult struct {
	Tools []ToolDescriptor `json:"tools"`
}

// ----- HTTP entry point ----------------------------------------------------

// ServeHTTP implements http.Handler. The MCP server only answers a single
// route (POST /); every other method/path returns 405 / 404. Bearer auth
// runs BEFORE JSON-RPC parsing so malformed body without a valid token
// still surfaces as 401 (not a JSON-RPC parse error).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	// Auth FIRST: HTTP-level failure cannot embed a JSON-RPC id.
	tok, err := s.authenticate(r)
	if err != nil {
		writeAuthErr(w, err)
		return
	}

	// Bound body to 1 MiB — JSON-RPC requests are small even with
	// tool arguments; anything larger is almost certainly an attack.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeRPCError(w, nil, codeParseError, "request body read failed", nil)
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		writeRPCError(w, nil, codeInvalidRequest, "empty request body", nil)
		return
	}

	var req rpcRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		writeRPCError(w, nil, codeParseError, "parse error", nil)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPCError(w, req.ID, codeInvalidRequest, "invalid request", nil)
		return
	}

	switch req.Method {
	case "tools/list":
		writeRPCResult(w, req.ID, listResult{Tools: s.toolList})
	case "tools/call":
		s.handleToolCall(w, r.Context(), req, tok)
	default:
		writeRPCError(w, req.ID, codeMethodNotFound, "method not found", nil)
	}
}

// handleToolCall validates the requested tool, checks the permission gate,
// and invokes the Call func. Errors map to JSON-RPC -32603 with a clear
// message — "forbidden" for permission failures, "internal error" for
// implementation faults.
func (s *Server) handleToolCall(w http.ResponseWriter, ctx context.Context, req rpcRequest, tok *authedToken) {
	var p callRequest
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPCError(w, req.ID, codeInvalidParams, "invalid params", nil)
			return
		}
	}
	if p.Name == "" {
		writeRPCError(w, req.ID, codeInvalidParams, "name is required", nil)
		return
	}
	tool, ok := s.tools[p.Name]
	if !ok {
		writeRPCError(w, req.ID, codeInvalidParams, "unknown tool: "+p.Name, nil)
		return
	}
	if !tok.can(tool.Permission) {
		s.emitToolAudit(ctx, tok, tool.Name, "denied")
		writeRPCError(w, req.ID, codeInternalError, "forbidden", nil)
		return
	}
	// Inject caller identity into the context so tools can resolve
	// "owned by" semantics without re-fetching the bearer row.
	callCtx := withCaller(ctx, tok.userID, tok.role)
	result, err := tool.Call(callCtx, p.Arguments)
	if err != nil {
		// Surface the forbidden sentinel as "forbidden" (-32603) — store-
		// layer permission failures hit this path when a token granted
		// only :own scope tries to read :any data.
		if errors.Is(err, ErrForbidden) {
			s.emitToolAudit(ctx, tok, tool.Name, "denied")
			writeRPCError(w, req.ID, codeInternalError, "forbidden", nil)
			return
		}
		s.emitToolAudit(ctx, tok, tool.Name, "error")
		s.log.Warn("mcp tool call failed", "tool", tool.Name, "err", err)
		writeRPCError(w, req.ID, codeInternalError, err.Error(), nil)
		return
	}
	s.emitToolAudit(ctx, tok, tool.Name, "ok")
	writeRPCResult(w, req.ID, json.RawMessage(result))
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
	_ = s.bearer.TouchBearer(r.Context(), info.ID)
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
// store rejects the request on permission grounds. The handler maps it to
// the JSON-RPC -32603 "forbidden" response.
var ErrForbidden = errors.New("forbidden")

// writeAuthErr renders a bearer-auth failure as HTTP 401 with a JSON body.
// 500 is used only for internal lookup faults; the standard 401 shape
// matches the rest of the API (writeErr in api/middleware.go).
func writeAuthErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errAuthInternal):
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
	default:
		// Missing OR invalid bearer both render identically — never tell
		// an unauthenticated caller whether the token "would" have been
		// valid.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid bearer token"}`))
	}
}

// writeRPCResult writes a successful JSON-RPC response. The result is the
// already-marshaled JSON for the tool's payload (or any other Marshal-able
// value when produced by tools/list internally).
func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	var raw json.RawMessage
	if rm, ok := result.(json.RawMessage); ok {
		raw = rm
	} else {
		buf, err := json.Marshal(result)
		if err != nil {
			writeRPCError(w, id, codeInternalError, "marshal failed", nil)
			return
		}
		raw = buf
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: raw}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeRPCError writes a JSON-RPC 2.0 error response. id may be nil for
// failures that occur before the request id was parsed (parse errors).
func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string, data json.RawMessage) {
	resp := rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg, Data: data},
	}
	w.Header().Set("Content-Type", "application/json")
	// JSON-RPC errors are still HTTP 200 — the JSON-RPC error object IS
	// the response. Only the HTTP-level auth check returns 401/405.
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// ----- ctx plumbing --------------------------------------------------------

type ctxKey int

const (
	ctxKeyCallerID ctxKey = iota
	ctxKeyCallerRole
	ctxKeyToken
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

func withCaller(ctx context.Context, uid, role string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyCallerID, uid)
	ctx = context.WithValue(ctx, ctxKeyCallerRole, role)
	return ctx
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
// Implementation lives in token_hash.go to keep this file focused on the
// JSON-RPC plumbing.
