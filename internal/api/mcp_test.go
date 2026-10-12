package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ankoehn/burrow/internal/mcpserv"
)

// fakeToolsLister satisfies MCPToolsLister with a canned inventory.
type fakeToolsLister struct{ rows []mcpserv.ToolDescriptor }

func (f *fakeToolsLister) Tools() []mcpserv.ToolDescriptor { return f.rows }

// newMCPTestServer wires the minimal Deps needed for the /api/v1/mcp/*
// surface. role lets the test pick the caller's role; info is the MCPInfo
// the handlers render.
func newMCPTestServer(t *testing.T, role string, info MCPInfo) (*httptest.Server, *authClient) {
	t.Helper()
	d := Deps{
		Log:   discardLog(),
		Users: &fakeUserStore{role: role},
		MCP:   info,
	}
	srv := httptest.NewServer(NewRouter(d))
	t.Cleanup(srv.Close)
	return srv, authedClient(t, srv)
}

func TestMCPStatus_AdminSeesConfig(t *testing.T) {
	_, c := newMCPTestServer(t, "admin", MCPInfo{
		Enabled: true,
		Listen:  ":7800",
		TokenID: "tok-mcp-1",
	})
	r := c.get(t, "/api/v1/mcp/status")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got mcpStatusResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Listen != ":7800" || got.TokenID != "tok-mcp-1" {
		t.Fatalf("status mismatch: %+v", got)
	}
}

func TestMCPStatus_Disabled(t *testing.T) {
	_, c := newMCPTestServer(t, "admin", MCPInfo{})
	r := c.get(t, "/api/v1/mcp/status")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", r.StatusCode)
	}
	var got mcpStatusResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Listen != "" || got.TokenID != "" {
		t.Fatalf("want zero-value status, got %+v", got)
	}
}

func TestMCPTools_AdminSeesInventory(t *testing.T) {
	tools := []mcpserv.ToolDescriptor{
		{Name: "tunnels_list", Description: "x", Permission: "tunnels:read:any",
			ParametersSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "services_list", Description: "y", Permission: "services:configure:own",
			ParametersSchema: json.RawMessage(`{"type":"object"}`)},
	}
	_, c := newMCPTestServer(t, "admin", MCPInfo{Server: &fakeToolsLister{rows: tools}})
	r := c.get(t, "/api/v1/mcp/tools")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got []mcpToolResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tools, want 2", len(got))
	}
	if got[0].Name != "tunnels_list" || got[0].Permission != "tunnels:read:any" {
		t.Errorf("tool[0] mismatch: %+v", got[0])
	}
	if string(got[0].ParametersSchema) != `{"type":"object"}` {
		t.Errorf("schema mismatch: %s", got[0].ParametersSchema)
	}
}

func TestMCPTools_NilServer_ReturnsEmptyArray(t *testing.T) {
	_, c := newMCPTestServer(t, "admin", MCPInfo{})
	r := c.get(t, "/api/v1/mcp/tools")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", r.StatusCode)
	}
	body := readBody(t, r)
	if body != "[]\n" {
		t.Fatalf("body=%q want \"[]\\n\"", body)
	}
}

func TestMCPStatus_UnauthenticatedReturns401(t *testing.T) {
	srv := httptest.NewServer(NewRouter(Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/mcp/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
}

func TestMCPStatus_UserWithoutPermReturns403(t *testing.T) {
	_, c := newMCPTestServer(t, "user", MCPInfo{})
	r := c.get(t, "/api/v1/mcp/status")
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", r.StatusCode, readBody(t, r))
	}
}

func TestMCPTools_UserWithoutPermReturns403(t *testing.T) {
	_, c := newMCPTestServer(t, "user", MCPInfo{})
	r := c.get(t, "/api/v1/mcp/tools")
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", r.StatusCode)
	}
}
