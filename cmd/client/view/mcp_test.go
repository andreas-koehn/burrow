package view

import (
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
)

func mcpService(tools int, known, down bool) Service {
	return Service{
		Name: "files", Type: "http", Public: GatewayOnlyPublic, Local: "127.0.0.1:41234",
		MCP: true, Tools: tools, ToolsKnown: known, MCPDown: down,
	}
}

func urlService(tools int, down bool) Service {
	s := mcpService(tools, true, down)
	s.MCPURL = true
	return s
}

func TestRender_MCPService(t *testing.T) {
	cases := []struct {
		svc  Service
		want string
	}{
		{mcpService(3, true, false), "  files   AI gateway only  →  MCP · 3 tools"},
		{mcpService(1, true, false), "  files   AI gateway only  →  MCP · 1 tool"},
		{mcpService(0, false, false), "  files   AI gateway only  →  MCP · starting"},
		{mcpService(4, true, true), "  files   AI gateway only  →  MCP · 4 tools · restarting"},
		{urlService(4, true), "  files   AI gateway only  →  MCP · 4 tools · unreachable"},
	}
	for _, tc := range cases {
		lines := Render(connected(tc.svc), 100, false)
		if !hasLine(lines, tc.want) {
			t.Fatalf("no line %q in:\n%s", tc.want, strings.Join(lines, "\n"))
		}
		for _, l := range lines {
			if strings.Contains(l, "41234") {
				t.Fatalf("the bridge address is shown: %q", l)
			}
		}
	}
}

func TestStore_MCPStatusFromTheBridge(t *testing.T) {
	st := &client.MCPStatus{}
	specs := []client.TunnelSpec{{Name: "files", Type: "http", LocalAddr: "127.0.0.1:41234", MCP: &client.MCPSpec{Command: []string{"x"}, Status: st}}}
	s := NewStore("burrow.example.com", "v0.6.0", specs)
	if sv := s.Snapshot().Services[0]; !sv.MCP || sv.ToolsKnown || sv.MCPURL {
		t.Fatalf("before the first status: %+v", sv)
	}
	st.Set(5, true)
	select {
	case <-s.Changed():
	case <-time.After(time.Second):
		t.Fatal("no change signalled")
	}
	if sv := s.Snapshot().Services[0]; sv.Tools != 5 || !sv.ToolsKnown || sv.MCPDown {
		t.Fatalf("after Set(5, true): %+v", sv)
	}
	st.Set(5, false)
	if sv := s.Snapshot().Services[0]; !sv.MCPDown {
		t.Fatalf("after Set(5, false): %+v", sv)
	}
	// The relay's registration keeps the MCP part.
	s.State(client.StateConnected, "", 0)
	s.Registered(client.RegisteredTunnel{TunnelID: "t1", Name: "files", Type: "http", LocalAddr: "127.0.0.1:41234", GatewayOnly: true})
	if sv := s.Snapshot().Services[0]; !sv.MCP || sv.Tools != 5 || sv.Public != GatewayOnlyPublic {
		t.Fatalf("after registration: %+v", sv)
	}
}

func TestStore_MCPURLSource(t *testing.T) {
	specs := []client.TunnelSpec{{Name: "notes", Type: "http", LocalAddr: "127.0.0.1:1", MCP: &client.MCPSpec{URL: "http://localhost:3001/mcp"}}}
	if sv := NewStore("r", "v", specs).Snapshot().Services[0]; !sv.MCP || !sv.MCPURL {
		t.Fatalf("%+v", sv)
	}
}
