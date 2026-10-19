package mcptest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Opts configures NewServer.
type Opts struct {
	// Stateful makes the upstream issue Mcp-Session-Id sessions.
	Stateful bool
	// Tools is the initial tool set; DefaultTools() when empty and NoTools
	// is false.
	Tools []ToolSpec
	// NoTools starts the upstream with no tools at all.
	NoTools bool
	// Authorize, if set, must return true for a request to be served; else 401.
	Authorize func(*http.Request) bool
}

const maxBody = 4 << 20

// Request is one recorded HTTP request.
type Request struct {
	Method    string
	Path      string
	Header    http.Header
	SessionID string // Mcp-Session-Id as sent by the client
	RPC       string // JSON-RPC method of a POST body, "" if none or a batch
}

// Server is a mock MCP upstream on an httptest.Server.
type Server struct {
	*httptest.Server
	opts Opts
	down atomic.Bool

	// opMu serialises the mutating operations (SetTools, ChangeDescription,
	// Restart) so each one is atomic with respect to the others. mu only
	// guards the fields below and is never held across an SDK call.
	opMu    sync.Mutex
	mu      sync.Mutex
	tools   []ToolSpec
	sdk     *mcp.Server
	handler http.Handler
	reqs    []Request
	calls   []Call
}

// NewServer starts the mock and closes it when the test ends.
func NewServer(t testing.TB, opts Opts) *Server {
	t.Helper()
	s := &Server{opts: opts, tools: append([]ToolSpec(nil), opts.Tools...)}
	if len(s.tools) == 0 && !opts.NoTools {
		s.tools = DefaultTools()
	}
	s.sdk, s.handler = s.newHandler(s.tools)
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Close stops the server, dropping open client connections (a stateful
// client's standalone stream would otherwise block it).
func (s *Server) Close() {
	s.CloseClientConnections()
	s.Server.Close()
}

// newHandler creates a fresh SDK server and Streamable HTTP handler for tools.
func (s *Server) newHandler(tools []ToolSpec) (*mcp.Server, http.Handler) {
	sdk := newSDKServer("mcptest", tools, s.record)
	return sdk, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk },
		&mcp.StreamableHTTPOptions{
			Stateless:                  !s.opts.Stateful,
			JSONResponse:               true,
			DisableLocalhostProtection: true,
		})
}

func (s *Server) record(c Call) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	rec := Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), SessionID: r.Header.Get("Mcp-Session-Id")}
	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || len(body) > maxBody {
			// Fail loudly rather than hand the SDK a truncated body.
			http.Error(w, "mcptest: request body over 4 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var m struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		rec.RPC = m.Method
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, rec)
	h := s.handler
	s.mu.Unlock()
	if s.down.Load() {
		http.Error(w, "mcptest: upstream down", http.StatusServiceUnavailable)
		return
	}
	if s.opts.Authorize != nil && !s.opts.Authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.ServeHTTP(w, r)
}

// SetDown makes the upstream answer every request with 503 while down is
// true; setting it back to false restores service at the same URL. Requests
// made while down are still recorded. Sessions survive.
func (s *Server) SetDown(down bool) { s.down.Store(down) }

// SetTools replaces the tool set. Sessions that are open are told with
// notifications/tools/list_changed.
func (s *Server) SetTools(tools ...ToolSpec) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.setToolsLocked(tools)
}

// setToolsLocked needs opMu. The SDK is called without mu held; the new set
// becomes visible to ChangeDescription only after the SDK has it.
func (s *Server) setToolsLocked(tools []ToolSpec) {
	s.mu.Lock()
	sdk, old := s.sdk, s.tools
	s.mu.Unlock()
	var gone []string
	for _, t := range old {
		if !slices.ContainsFunc(tools, func(n ToolSpec) bool { return n.Name == t.Name }) {
			gone = append(gone, t.Name)
		}
	}
	if len(gone) > 0 {
		sdk.RemoveTools(gone...)
	}
	addTools(sdk, tools, s.record)
	s.mu.Lock()
	s.tools = slices.Clone(tools)
	s.mu.Unlock()
}

// ChangeDescription changes one tool's description; it reports whether the
// tool exists. It is atomic against SetTools, Restart and other
// ChangeDescription calls.
func (s *Server) ChangeDescription(name, description string) bool {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	next := slices.Clone(s.tools)
	s.mu.Unlock()
	i := slices.IndexFunc(next, func(t ToolSpec) bool { return t.Name == name })
	if i < 0 {
		return false
	}
	next[i].Description = description
	s.setToolsLocked(next)
	return true
}

// Restart forgets every session, as if a stateful upstream had been
// restarted: the old SDK sessions are closed (no goroutines are left behind),
// and the next request that carries an old Mcp-Session-Id gets 404
// (mcp.ErrSessionMissing on the client). URL, tools and recordings stay.
func (s *Server) Restart() {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	old := s.sdk
	tools := slices.Clone(s.tools)
	s.mu.Unlock()
	sdk, h := s.newHandler(tools)
	s.mu.Lock()
	s.sdk, s.handler = sdk, h
	s.mu.Unlock()
	for ss := range old.Sessions() {
		_ = ss.Close()
	}
}

// Requests returns the recorded HTTP requests.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Calls returns the recorded tools/call requests.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CallsTo returns the recorded calls of one tool.
func (s *Server) CallsTo(tool string) []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

// ResetRecords clears the request and call recordings.
func (s *Server) ResetRecords() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs, s.calls = nil, nil
}
