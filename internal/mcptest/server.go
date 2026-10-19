package mcptest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Opts configures NewServer.
type Opts struct {
	// Stateful makes the upstream issue Mcp-Session-Id sessions.
	Stateful bool
	// Tools is the initial tool set; DefaultTools() when empty.
	Tools []ToolSpec
	// Authorize, if set, must return true for a request to be served; else 401.
	Authorize func(*http.Request) bool
}

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
	if len(s.tools) == 0 {
		s.tools = DefaultTools()
	}
	s.build()
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

// build creates a fresh SDK server and handler from the current tools.
// Caller holds mu, or is the constructor.
func (s *Server) build() {
	s.sdk = NewSDKServer("mcptest", s.tools, s.record)
	sdk := s.sdk
	s.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk },
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
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
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
	if s.opts.Authorize != nil && !s.opts.Authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.ServeHTTP(w, r)
}

// SetTools replaces the tool set. Sessions that are open are told with
// notifications/tools/list_changed.
func (s *Server) SetTools(tools ...ToolSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := map[string]bool{}
	for _, t := range tools {
		keep[t.Name] = true
	}
	var gone []string
	for _, t := range s.tools {
		if !keep[t.Name] {
			gone = append(gone, t.Name)
		}
	}
	if len(gone) > 0 {
		s.sdk.RemoveTools(gone...)
	}
	s.tools = append([]ToolSpec(nil), tools...)
	addTools(s.sdk, s.tools, s.record)
}

// ChangeDescription changes one tool's description; it reports whether the
// tool exists.
func (s *Server) ChangeDescription(name, description string) bool {
	s.mu.Lock()
	var next []ToolSpec
	found := false
	for _, t := range s.tools {
		if t.Name == name {
			t.Description = description
			found = true
		}
		next = append(next, t)
	}
	s.mu.Unlock()
	if found {
		s.SetTools(next...)
	}
	return found
}

// Restart forgets every session, as if a stateful upstream had been
// restarted: the next request that carries an old Mcp-Session-Id gets 404
// (mcp.ErrSessionMissing on the client). URL, tools and recordings stay.
func (s *Server) Restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.build()
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
