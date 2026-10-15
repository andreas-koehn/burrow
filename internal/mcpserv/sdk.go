// sdk.go — the MCP side of the package: the tools served through the
// official Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// The SDK's Streamable HTTP handler runs stateless, which is the only mode in
// which it serves revision 2026-07-28; the 2025 revisions keep working on the
// same endpoint. Each request gets its own mcp.Server holding only the tools
// its token may call. That makes tools/list honest by construction, and a
// tools/call on anything else gets the SDK's own "unknown tool" error, the
// same one a name outside the registry gets.

package mcpserv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/version"
)

// MaxBodyBytes bounds a request body. MCP requests are small even with tool
// arguments; the SDK answers anything larger with 413.
const MaxBodyBytes = 1 << 20

// protocolVersions are the MCP revisions offered: 2026-07-28 and the 2025
// revisions, not 2024-11-05 (ROADMAP-next.md, B11). A legacy initialize for a
// revision outside the list is answered with 2025-11-25.
var protocolVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

// mcpHandler returns the SDK's Streamable HTTP handler for the registry. It
// expects the authenticated token in the request context (withToken) and
// answers 400 without one.
func (s *Server) mcpHandler() http.Handler {
	// The SDK asks for the server more than once per request, so it is built
	// once here and handed over through the request context.
	sdk := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		srv, _ := r.Context().Value(ctxKeyMCPServer).(*mcp.Server)
		return srv
	}, &mcp.StreamableHTTPOptions{
		Stateless: true,
		// One JSON body per POST. No tool streams, and a reverse proxy in
		// front of the relay does not have to pass an event stream through.
		JSONResponse: true,
		// A backstop only: the handler below reads at most MaxBodyBytes+1
		// bytes itself and answers 413 first, so the SDK never sees more.
		MaxRequestBodyBytes: MaxBodyBytes,
		// The SDK refuses a request that arrives on a loopback address with
		// another Host header, to protect unauthenticated servers on a
		// developer's machine from DNS rebinding. A relay behind a reverse
		// proxy on the same machine receives exactly such requests, and this
		// endpoint answers nobody without a bearer token.
		DisableLocalhostProtection: true,
		// No Logger: the SDK is not given a place to write request details.
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := tokenFrom(r.Context())
		if tok == nil {
			sdk.ServeHTTP(w, r)
			return
		}
		// A JSON-RPC batch would let one POST run any number of calls past
		// the limits that count requests. The 2025-03-26 revision allows
		// batches and the SDK serves them, so they are refused here, before
		// the SDK reads the body. At most MaxBodyBytes+1 bytes are read; the
		// SDK gets the body back intact.
		if r.Method == http.MethodPost && r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
			if err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(body) > MaxBodyBytes {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '[' {
				http.Error(w, "batch requests are not supported", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		ctx := context.WithValue(r.Context(), ctxKeyMCPServer, s.serverFor(tok))
		sdk.ServeHTTP(w, r.WithContext(ctx))
	})
}

// serverFor builds the MCP server one request sees: the curated tools tok
// may call, in curatedTools order (the SDK lists them sorted by name).
func (s *Server) serverFor(tok *authedToken) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "burrow",
		Title:   "Burrow",
		Version: version.Version,
	}, &mcp.ServerOptions{
		// Tools only; the list never changes within a request.
		Capabilities:              &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		SupportedProtocolVersions: protocolVersions,
		// The list depends on the caller. Without this the SDK marks it
		// cacheScope "public".
		SetCacheable: func(_ context.Context, _ mcp.Request, c *mcp.Cacheable) {
			c.TTLMs = 0
			c.CacheScope = "private"
		},
	})
	for _, op := range s.curated {
		if !tok.mayUse(op) {
			continue
		}
		srv.AddTool(s.sdkTool(op), s.callHandler(tok, ToolName(op.ID), func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, string, string) {
			res, word := s.invoke(ctx, tok, op, args)
			return res, word, ""
		}))
	}
	for _, g := range s.genericTools(tok) {
		srv.AddTool(g.tool, g.handler)
	}
	srv.AddReceivingMiddleware(s.auditHidden(tok))
	return srv
}

// sdkTool is the MCP descriptor of an operation offered as a tool.
// destructiveHint and openWorldHint are always sent: MCP reads a missing one
// as true, and no tool here reaches beyond the relay.
func (s *Server) sdkTool(op Operation) *mcp.Tool {
	schema, ok := s.toolSchemas[op.ID]
	if !ok {
		schema, _ = json.Marshal(op.InputSchema())
	}
	destructive, openWorld := op.Destructive, false
	return &mcp.Tool{
		Name:        ToolName(op.ID),
		Description: s.description(op),
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    op.Kind() == "read",
			DestructiveHint: &destructive,
			IdempotentHint:  op.Method == http.MethodPut,
			OpenWorldHint:   &openWorld,
		},
	}
}

// toolTimeout bounds one tool call; the JSON API uses the same bound
// (api.JSONHandlerTimeout).
const toolTimeout = 30 * time.Second

// callHandler adapts run to the SDK's raw tool handler and writes the audit
// row: the name run returns (auditName when it returns "") and the word run
// returns. run gets the arguments as an object. A call that fails answers with an isError result, which the model
// can read and act on; it is not a protocol error.
//
// The SDK runs handlers on a goroutine of its own, outside net/http's and the
// router's recovery, so a panic here would end the process. It is caught.
func (s *Server) callHandler(tok *authedToken, auditName string, run func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, string, string)) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (res *mcp.CallToolResult, _ error) {
		defer func() {
			if p := recover(); p != nil {
				// The deferred function must not panic again: a failing audit
				// sink is contained here.
				s.safeAudit(ctx, tok, auditName, "error")
				// The panic value can quote data (an index, a key): its type
				// and the stack, which holds no argument values.
				s.log.Error("mcp tool panicked", "tool", auditName, "panic_type", fmt.Sprintf("%T", p), "stack", string(debug.Stack()))
				res = errorResult(errors.New("internal error"))
			}
		}()
		args := map[string]any{}
		if raw := bytes.TrimSpace(req.Params.Arguments); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			if raw[0] != '{' || json.Unmarshal(raw, &args) != nil || args == nil {
				s.log.Debug("mcp tool call", "tool", auditName, "result", "error", "reason", "arguments are not an object")
				s.emitToolAudit(ctx, tok, auditName, "error")
				return errorResult(errors.New("arguments must be an object")), nil
			}
		}
		// A call that hangs must end: neither a client that went away nor a
		// deadline in front of the handler reaches it otherwise.
		limit := s.toolTimeout
		if limit <= 0 {
			limit = toolTimeout
		}
		callCtx, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		res, word, name := run(callCtx, args)
		if name != "" {
			auditName = name
		}
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			// The log names the tool and the deadline, never an argument.
			s.emitToolAudit(ctx, tok, auditName, "error")
			s.log.Warn("mcp tool call failed", "tool", auditName, "reason", "deadline exceeded", "deadline", limit.String())
			return errorResult(errors.New("timed out")), nil
		}
		// The tool and the outcome, never an argument or the answer.
		s.log.Debug("mcp tool call", "tool", auditName, "result", word)
		s.emitToolAudit(ctx, tok, auditName, word)
		return res, nil
	}
}

// safeAudit writes an audit row and swallows a panic of the audit sink.
func (s *Server) safeAudit(ctx context.Context, tok *authedToken, tool, result string) {
	defer func() { _ = recover() }()
	s.emitToolAudit(ctx, tok, tool, result)
}

// toolResult renders a tool's JSON object twice, as MCP asks: as
// structuredContent, and as text for clients that read only content.
func toolResult(payload json.RawMessage) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(payload)}},
		StructuredContent: payload,
	}
}

// errorResult is a failed call: isError with the message as text.
func errorResult(err error) *mcp.CallToolResult {
	res := &mcp.CallToolResult{}
	res.SetError(err)
	return res
}

// auditHidden records a tools/call on a tool that exists but that tok may
// not use. It does not answer the call: the tool is not registered on this
// request's server, so the SDK's "unknown tool" error follows, and the
// caller cannot tell a tool it may not see from one that does not exist.
func (s *Server) auditHidden(tok *authedToken) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (res mcp.Result, err error) {
			// The audit sink runs on the SDK's goroutine too; its panic must
			// not end the process.
			defer func() {
				if p := recover(); p != nil {
					s.log.Error("mcp request panicked", "method", method, "panic_type", fmt.Sprintf("%T", p), "stack", string(debug.Stack()))
					if method == "tools/call" {
						res, err = errorResult(errors.New("internal error")), nil
						return
					}
					res, err = nil, errors.New("internal error")
				}
			}()
			if method == "tools/call" {
				if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p != nil {
					if op, known := s.curatedByName[p.Name]; known && !tok.mayUse(op) {
						s.emitToolAudit(ctx, tok, p.Name, "denied")
					}
				}
			}
			return next(ctx, method, req)
		}
	}
}
