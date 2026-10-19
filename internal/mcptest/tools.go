// Package mcptest is a mock MCP upstream for tests of the tool gateway and of
// `burrow mcp`. It is built on the official Go SDK, so it speaks the real
// protocol, and it is meant to be driven from other packages' tests.
//
// # Remote upstream (HTTP)
//
//	up := mcptest.NewServer(t, mcptest.Opts{})                  // stateless, DefaultTools()
//	up := mcptest.NewServer(t, mcptest.Opts{Stateful: true})    // Mcp-Session-Id sessions
//	up.URL                                                       // Streamable HTTP endpoint
//
// A stateless upstream keeps no session: every POST stands alone. A stateful
// one hands out an Mcp-Session-Id on initialize and answers 404 for an id it
// does not know; the SDK client reports that as mcp.ErrSessionMissing.
// Restart() drops every session (a stateful upstream that was restarted) while
// keeping the URL, the tools and the recordings.
//
// Controls, all safe for concurrent use:
//
//	up.SetTools(tools...)              replace the tool set (list_changed is sent)
//	up.ChangeDescription(name, text)   change one description, like a new upstream release
//	up.Restart()                       forget all sessions
//	up.Requests()                      every HTTP request: method, path, headers (incl. credentials), session id, JSON-RPC method
//	up.Calls()                         every tools/call: tool, arguments, headers, session id
//	up.CallsTo("echo")                 the same, for one tool
//	up.ResetRecords()                  clear both recordings
//	up.Close()                         also registered with t.Cleanup
//
// Opts.Authorize, when set, decides per HTTP request whether it is let in;
// a false answer is a 401 (the request is still recorded). Use it to check
// that the gateway injects the credential.
//
// # Tools
//
// A ToolSpec has a Kind that fixes the behaviour and the argument schema:
//
//	Echo(name)       {text}      returns "echo: <text>"
//	Slow(name)       {ms}        sleeps ms milliseconds (or until cancelled), returns "slept <ms>ms"
//	Fail(name)       {message}   returns an isError result carrying message (default "boom")
//	Big(name, n)     {bytes}     returns bytes (default n) bytes of "x"
//
// .Described(text) and .ReadOnly() adjust a spec; ReadOnly sets the
// readOnlyHint annotation. DefaultTools() is: echo (read-only), slow
// (read-only), fail, big (read-only, 2 MiB by default) and mutate (an echo
// that is not read-only).
//
// # Stdio upstream
//
// BuildMockMCP compiles internal/mcptest/cmd/mockmcp once per test and returns
// the binary; StdioCommand returns an *exec.Cmd for it, ready for
// mcp.CommandTransport. The program serves DefaultTools() on stdin/stdout.
// With --crash-after N it exits with status 3, without answering, when the Nth
// tools/call arrives (N counts calls of this process only; 0 means never).
// With --name it sets the server's implementation name.
package mcptest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Kind selects what a tool does when called.
type Kind string

// Tool kinds.
const (
	KindEcho Kind = "echo"
	KindSlow Kind = "slow"
	KindFail Kind = "fail"
	KindBig  Kind = "big"
)

// DefaultBigBytes is the size of a Big tool's result when the call gives none
// and the spec sets none: over the gateway's 1 MiB result cap.
const DefaultBigBytes = 2 << 20

// ToolSpec defines one tool of the mock.
type ToolSpec struct {
	Name        string
	Description string
	Kind        Kind
	// ReadOnlyHint sets the readOnlyHint annotation.
	ReadOnlyHint bool
	// BigBytes is the default result size of a KindBig tool.
	BigBytes int
}

// Echo returns an echo tool.
func Echo(name string) ToolSpec {
	return ToolSpec{Name: name, Description: "Echo the text back.", Kind: KindEcho}
}

// Slow returns a tool that sleeps for the requested milliseconds.
func Slow(name string) ToolSpec {
	return ToolSpec{Name: name, Description: "Sleep for ms milliseconds.", Kind: KindSlow}
}

// Fail returns a tool whose result is an error.
func Fail(name string) ToolSpec {
	return ToolSpec{Name: name, Description: "Always fails.", Kind: KindFail}
}

// Big returns a tool with a large result of n bytes (DefaultBigBytes if n <= 0).
func Big(name string, n int) ToolSpec {
	return ToolSpec{Name: name, Description: "Return a large result.", Kind: KindBig, BigBytes: n}
}

// Described returns the spec with another description.
func (t ToolSpec) Described(text string) ToolSpec { t.Description = text; return t }

// ReadOnly returns the spec with the readOnlyHint annotation set.
func (t ToolSpec) ReadOnly() ToolSpec { t.ReadOnlyHint = true; return t }

// DefaultTools is the tool set of NewServer when Opts.Tools is empty and of
// the stdio program.
func DefaultTools() []ToolSpec {
	mutate := Echo("mutate")
	mutate.Description = "Echo the text back; not read-only."
	return []ToolSpec{
		Echo("echo").ReadOnly(),
		Slow("slow").ReadOnly(),
		Fail("fail"),
		Big("big", 0).ReadOnly(),
		mutate,
	}
}

// Call is one recorded tools/call.
type Call struct {
	Tool      string
	Args      json.RawMessage
	Header    http.Header // nil for stdio
	SessionID string      // empty for stateless and stdio
}

func (t ToolSpec) tool() *mcp.Tool {
	props := map[string]any{}
	switch t.Kind {
	case KindEcho:
		props["text"] = map[string]any{"type": "string"}
	case KindSlow:
		props["ms"] = map[string]any{"type": "integer", "minimum": 0}
	case KindFail:
		props["message"] = map[string]any{"type": "string"}
	case KindBig:
		props["bytes"] = map[string]any{"type": "integer", "minimum": 0}
	}
	mt := &mcp.Tool{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: map[string]any{"type": "object", "properties": props},
	}
	if t.ReadOnlyHint {
		mt.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
	}
	return mt
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func (t ToolSpec) handler(onCall func(Call)) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw := req.Params.Arguments
		if onCall != nil {
			c := Call{Tool: t.Name, Args: append(json.RawMessage(nil), raw...)}
			if req.Extra != nil {
				c.Header = req.Extra.Header.Clone()
			}
			if req.Session != nil {
				c.SessionID = req.Session.ID()
			}
			onCall(c)
		}
		var a struct {
			Text    string `json:"text"`
			MS      int    `json:"ms"`
			Message string `json:"message"`
			Bytes   *int   `json:"bytes"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, fmt.Errorf("bad arguments: %w", err)
			}
		}
		switch t.Kind {
		case KindEcho:
			return text("echo: " + a.Text), nil
		case KindSlow:
			select {
			case <-time.After(time.Duration(a.MS) * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return text(fmt.Sprintf("slept %dms", a.MS)), nil
		case KindFail:
			msg := a.Message
			if msg == "" {
				msg = "boom"
			}
			r := text(msg)
			r.IsError = true
			return r, nil
		case KindBig:
			n := t.BigBytes
			if n <= 0 {
				n = DefaultBigBytes
			}
			if a.Bytes != nil {
				n = *a.Bytes
			}
			return text(strings.Repeat("x", n)), nil
		}
		return nil, fmt.Errorf("unknown tool kind %q", t.Kind)
	}
}

func addTools(s *mcp.Server, tools []ToolSpec, onCall func(Call)) {
	for _, t := range tools {
		s.AddTool(t.tool(), t.handler(onCall))
	}
}

// NewSDKServer returns an SDK server serving tools (DefaultTools() if empty),
// calling onCall (if not nil) for every tools/call before it runs.
func NewSDKServer(name string, tools []ToolSpec, onCall func(Call)) *mcp.Server {
	if len(tools) == 0 {
		tools = DefaultTools()
	}
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.0.0-test"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
	})
	addTools(s, tools, onCall)
	return s
}
