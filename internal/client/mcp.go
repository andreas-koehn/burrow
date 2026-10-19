package client

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// MCPSpec is the local MCP server behind a tunnel: a command speaking stdio,
// or the Streamable HTTP endpoint of a server that already runs. Exactly one
// of Command and URL is set.
type MCPSpec struct {
	// Command is the program and its arguments.
	Command []string
	// Env holds KEY=VALUE pairs the child gets on top of this process's
	// environment.
	Env []string
	// URL is the local server's endpoint, http:// or https://.
	URL string
	// RestartMax is how often an ended child is started again; negative is
	// without limit.
	RestartMax int
	// Status is set by the command that starts the bridge, for the status
	// view; nil until then.
	Status *MCPStatus
}

// MCPStatus is what a bridge reports for the status view: how many tools it
// serves and whether the local server answers. Its methods are safe for
// concurrent use and on a nil *MCPStatus.
type MCPStatus struct {
	mu       sync.Mutex
	tools    int
	running  bool
	known    bool
	watchers []func(tools int, running bool)
}

// Set records the bridge's status and tells the watchers.
func (s *MCPStatus) Set(tools int, running bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.tools, s.running, s.known = tools, running, true
	ws := append([]func(tools int, running bool){}, s.watchers...)
	s.mu.Unlock()
	for _, f := range ws {
		f(tools, running)
	}
}

// Get returns the last status; known is false before the first Set.
func (s *MCPStatus) Get() (tools int, running, known bool) {
	if s == nil {
		return 0, false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tools, s.running, s.known
}

// OnChange calls f on every Set, and at once when a status is known.
func (s *MCPStatus) OnChange(f func(tools int, running bool)) {
	if s == nil || f == nil {
		return
	}
	s.mu.Lock()
	s.watchers = append(s.watchers, f)
	tools, running, known := s.tools, s.running, s.known
	s.mu.Unlock()
	if known {
		f(tools, running)
	}
}

// ValidMCPURL reports whether u can be the endpoint of a local MCP server.
func ValidMCPURL(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// ValidEnvKey reports whether k can name an environment variable for a child.
func ValidEnvKey(k string) bool {
	return k != "" && !strings.ContainsAny(k, "=\x00")
}

// mcpFromFile builds the MCP part of a burrow.yaml service of type mcp.
func mcpFromFile(svc rawService) (*MCPSpec, error) {
	cmd := strings.TrimSpace(svc.Command)
	switch {
	case svc.Local != "":
		return nil, fmt.Errorf("local does not apply to type mcp: the bridge picks its own address")
	case svc.RemotePort != 0:
		return nil, fmt.Errorf("remote does not apply to type mcp")
	case (cmd == "") == (svc.URL == ""):
		return nil, fmt.Errorf("type mcp needs exactly one of command or url")
	}
	if svc.URL != "" {
		if len(svc.Args) > 0 {
			return nil, fmt.Errorf("args applies to command, not to url")
		}
		if len(svc.Env) > 0 {
			return nil, fmt.Errorf("env applies to command, not to url")
		}
		if !ValidMCPURL(svc.URL) {
			return nil, fmt.Errorf("url must be an http:// or https:// address, for example http://localhost:3001/mcp")
		}
		return &MCPSpec{URL: svc.URL, RestartMax: -1}, nil
	}
	spec := &MCPSpec{Command: append([]string{cmd}, svc.Args...), RestartMax: -1}
	keys := make([]string, 0, len(svc.Env))
	for k := range svc.Env {
		if !ValidEnvKey(k) {
			return nil, fmt.Errorf("env: %q cannot name an environment variable", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec.Env = append(spec.Env, k+"="+svc.Env[k])
	}
	return spec, nil
}
