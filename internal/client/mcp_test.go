package client

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "burrow.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFileConfig_MCPServices(t *testing.T) {
	p := writeYAML(t, `services:
  - name: files
    type: mcp
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/srv/docs"]
    env:
      LOG_LEVEL: debug
      API_KEY: k
  - name: notes
    type: mcp
    url: http://localhost:3001/mcp
    slug: notes-mcp
  - { name: web, local: 3000, type: http }
`)
	c, err := LoadFileConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tunnels) != 3 {
		t.Fatalf("tunnels = %+v", c.Tunnels)
	}
	files := c.Tunnels[0]
	if files.Name != "files" || files.Type != "http" || files.LocalAddr != "" || files.MCP == nil {
		t.Fatalf("files = %+v", files)
	}
	want := MCPSpec{
		Command:    []string{"npx", "-y", "@modelcontextprotocol/server-filesystem", "/srv/docs"},
		Env:        []string{"API_KEY=k", "LOG_LEVEL=debug"}, // sorted: the order of a map is not kept
		RestartMax: -1,
	}
	if !reflect.DeepEqual(*files.MCP, want) {
		t.Fatalf("files.MCP = %+v, want %+v", *files.MCP, want)
	}
	notes := c.Tunnels[1]
	if notes.Type != "http" || notes.Slug != "notes-mcp" || notes.MCP == nil ||
		notes.MCP.URL != "http://localhost:3001/mcp" || len(notes.MCP.Command) != 0 || notes.MCP.RestartMax != -1 {
		t.Fatalf("notes = %+v (%+v)", notes, notes.MCP)
	}
	if c.Tunnels[2].MCP != nil {
		t.Fatalf("an http service got an MCP part: %+v", c.Tunnels[2])
	}
}

func TestLoadFileConfig_MCPValidation(t *testing.T) {
	cases := []struct{ name, svc, want string }{
		{"neither", "{ name: m, type: mcp }", "exactly one of command or url"},
		{"both", "{ name: m, type: mcp, command: x, url: 'http://localhost:1/mcp' }", "exactly one of command or url"},
		{"local", "{ name: m, type: mcp, command: x, local: 3000 }", "local does not apply to type mcp"},
		{"remote", "{ name: m, type: mcp, command: x, remote: 9000 }", "remote does not apply to type mcp"},
		{"args with url", "{ name: m, type: mcp, url: 'http://localhost:1/mcp', args: [a] }", "args applies to command"},
		{"env with url", "{ name: m, type: mcp, url: 'http://localhost:1/mcp', env: { A: b } }", "env applies to command"},
		{"bad url", "{ name: m, type: mcp, url: 'localhost:1/mcp' }", "http:// or https://"},
		{"blank command", "{ name: m, type: mcp, command: '  ' }", "exactly one of command or url"},
		{"bad env key", "{ name: m, type: mcp, command: x, env: { 'A=B': c } }", "env"},
		{"command on http", "{ name: m, type: http, local: 3000, command: x }", "command applies to type mcp"},
		{"url on tcp", "{ name: m, type: tcp, local: 3000, url: 'http://x/' }", "url applies to type mcp"},
		{"unknown type names mcp", "{ name: m, type: grpc, local: 3000 }", "tcp, http or mcp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFileConfig(writeYAML(t, "services:\n  - "+tc.svc+"\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// `burrow connect --config` runs no bridge: it says where type mcp works.
func TestLoadCompleteFileConfig_RefusesMCP(t *testing.T) {
	_, err := LoadCompleteFileConfig(writeYAML(t, "server: r:7000\ntoken: t\nservices:\n  - { name: m, type: mcp, command: x }\n"))
	if err == nil || !strings.Contains(err.Error(), "burrow up") {
		t.Fatalf("err = %v, want one that points to burrow up", err)
	}
}

func TestMCPStatus(t *testing.T) {
	var s MCPStatus
	if _, _, known := s.Get(); known {
		t.Fatal("a fresh status is known")
	}
	var mu sync.Mutex
	var seen []string
	record := func(tools int, running bool) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, strings.Repeat("t", tools)+map[bool]string{true: "+", false: "-"}[running])
	}
	s.OnChange(record) // nothing known yet: not called
	s.Set(3, true)
	s.Set(3, false)
	if tools, running, known := s.Get(); tools != 3 || running || !known {
		t.Fatalf("Get = %d %v %v", tools, running, known)
	}
	// A late watcher hears the current status at once.
	var late []string
	s.OnChange(func(tools int, running bool) { late = append(late, "x") })
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(seen, []string{"ttt+", "ttt-"}) || len(late) != 1 {
		t.Fatalf("seen %v, late %v", seen, late)
	}
	// A nil status is safe to use.
	var none *MCPStatus
	none.Set(1, true)
	none.OnChange(record)
	if _, _, known := none.Get(); known {
		t.Fatal("nil status is known")
	}
}
