// mcp_handlers.go — `burrowd mcp` dashboard surface (spec Part P).
//
// Two read-only JSON endpoints:
//
//	GET /api/v1/mcp/status  → { enabled, listen, token_id }
//	GET /api/v1/mcp/tools   → [ { name, description, parameters_schema, permission } ]
//
// Both routes are gated by admin OR mcp:tools:read. The actual MCP listener
// (port :7800) is wired by cmd/server (Task 25); these endpoints just
// surface its configuration + the closed tool inventory so the dashboard
// can render a "burrowd mcp" page without speaking JSON-RPC.

package api

import (
	"encoding/json"
	"net/http"

	"github.com/ankoehn/burrow/internal/mcpserv"
)

// MCPInfo is the configuration surface the dashboard endpoints render. All
// fields are inert (no behaviour); cmd/server (Task 25) populates them
// after parsing BURROW_MCP_LISTEN / BURROW_MCP_TOKEN.
type MCPInfo struct {
	// Enabled is true when burrowd mcp is wired (Task 25 has read a non-
	// empty BURROW_MCP_LISTEN). When false, Listen and TokenID are empty.
	Enabled bool
	// Listen is the configured listen address (e.g. ":7800"). Empty when
	// disabled.
	Listen string
	// TokenID is the automation_tokens.id of the bearer token configured
	// via BURROW_MCP_TOKEN. Empty when disabled. The PLAINTEXT secret is
	// NEVER returned here — operators set it server-side, not via the API.
	TokenID string
	// Server is the configured *mcpserv.Server (or anything that can
	// enumerate tools). When non-nil, GET /api/v1/mcp/tools renders its
	// inventory; when nil, the endpoint returns an empty array so the
	// dashboard can degrade gracefully.
	Server MCPToolsLister
}

// MCPToolsLister is the narrow Tools() seam the api package consumes. The
// concrete *mcpserv.Server satisfies it; tests provide a small fake.
type MCPToolsLister interface {
	Tools() []mcpserv.ToolDescriptor
}

// mcpStatusResp is the JSON shape of GET /api/v1/mcp/status.
type mcpStatusResp struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"`
	TokenID string `json:"token_id"`
}

// mcpToolResp is the wire shape of one tool inventory entry. Mirrors
// mcpserv.ToolDescriptor; the json tags are identical so the body is
// byte-equivalent to what the JSON-RPC tools/list response produces.
type mcpToolResp struct {
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	ParametersSchema json.RawMessage `json:"parameters_schema"`
	Permission       string          `json:"permission"`
}

// GetMCPStatus handles GET /api/v1/mcp/status.
//
// Returns the configured MCP listener address + the configured automation
// token's id. The plaintext bearer is NEVER returned — operators set it
// server-side via BURROW_MCP_TOKEN. When the listener is disabled (Task 25
// did not wire it), enabled=false and the listen/token_id fields are empty.
func (d Deps) GetMCPStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, mcpStatusResp{
		Enabled: d.MCP.Enabled,
		Listen:  d.MCP.Listen,
		TokenID: d.MCP.TokenID,
	})
}

// GetMCPTools handles GET /api/v1/mcp/tools.
//
// Returns the closed inventory the JSON-RPC tools/list call exposes. Each
// entry includes the parameters_schema JSON Schema and the permission
// string a caller must hold (transitively via the bearer token) to invoke
// the tool. A nil MCP.Server (Task 25 not yet wired) yields an empty array
// rather than 500 — the dashboard renders a "burrowd mcp not configured"
// notice in that state.
func (d Deps) GetMCPTools(w http.ResponseWriter, r *http.Request) {
	var out []mcpToolResp
	if d.MCP.Server != nil {
		for _, t := range d.MCP.Server.Tools() {
			out = append(out, mcpToolResp{
				Name:             t.Name,
				Description:      t.Description,
				ParametersSchema: t.ParametersSchema,
				Permission:       t.Permission,
			})
		}
	}
	if out == nil {
		out = []mcpToolResp{}
	}
	writeJSON(w, http.StatusOK, out)
}
