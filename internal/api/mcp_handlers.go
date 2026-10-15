// mcp_handlers.go — the dashboard's view of the management MCP server
// (spec Part P).
//
// Two read-only JSON endpoints:
//
//	GET /api/v1/mcp/status  → { enabled, listen, token_id, endpoint, operations }
//	GET /api/v1/mcp/tools   → [ { name, description, parameters_schema, permission,
//	                              permissions, kind, generic } ]
//
// Both routes need mcp:tools:read (routeAccess in access.go), from a session
// through its role and from a token that declares it. The MCP server itself
// (internal/mcpserv) is mounted at /mcp/burrow by NewRouter; these endpoints
// tell the dashboard where it is and list the tools it offers by name, so
// that it can render a page without speaking MCP.

package api

import (
	"encoding/json"
	"net/http"

	"github.com/ankoehn/burrow/internal/mcpserv"
)

// mcpEndpointPath is where NewRouter mounts MCPInfo.Handler.
const mcpEndpointPath = mcpserv.EndpointPath

// MCPInfo is what the router needs of the management MCP server: the
// handler it mounts at /mcp/burrow and the metadata the two dashboard
// endpoints render. cmd/server populates it.
type MCPInfo struct {
	// Enabled is rendered as "enabled" by GET /api/v1/mcp/status. cmd/server
	// sets it to true: the endpoint is part of every relay.
	Enabled bool
	// Listen and TokenID are rendered as "listen" and "token_id". Both
	// described the separate MCP listener (BURROW_MCP_LISTEN,
	// BURROW_MCP_TOKEN), which is gone; cmd/server leaves them empty. The
	// keys stay in the status body for clients written against them.
	Listen  string
	TokenID string
	// Server is the configured *mcpserv.Server (or anything that can
	// enumerate tools). When non-nil, GET /api/v1/mcp/tools renders its
	// inventory; when nil, the endpoint returns an empty array so the
	// dashboard can degrade gracefully.
	Server MCPToolsLister
	// Handler serves /mcp/burrow: the same *mcpserv.Server. It brings its
	// own authentication (automation tokens), origin check and limits. Nil
	// leaves the route answering 503.
	Handler http.Handler
	// RateLimitPerIP overrides MCPRateLimitPerIP, the requests per minute
	// and client address the router admits before the handler sees them.
	// Zero or negative means the default.
	RateLimitPerIP int
}

// MCPToolsLister is the narrow seam the api package consumes. The concrete
// *mcpserv.Server satisfies it; tests provide a small fake.
type MCPToolsLister interface {
	Tools() []mcpserv.ToolDescriptor
	// OperationCount is the number of operations the generic tools reach.
	OperationCount() int
}

// mcpStatusResp is the JSON shape of GET /api/v1/mcp/status. Endpoint is
// the path of the MCP endpoint on this origin; Operations the number of
// operations the generic tools reach.
type mcpStatusResp struct {
	Enabled    bool   `json:"enabled"`
	Listen     string `json:"listen"`
	TokenID    string `json:"token_id"`
	Endpoint   string `json:"endpoint"`
	Operations int    `json:"operations"`
}

// mcpToolResp is the wire shape of one tool inventory entry. Mirrors
// mcpserv.ToolDescriptor. It is Burrow's own shape, not MCP's: an MCP
// client reads the tools from /mcp/burrow, filtered by its token.
type mcpToolResp struct {
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	ParametersSchema json.RawMessage `json:"parameters_schema"`
	Permission       string          `json:"permission"`
	Permissions      []string        `json:"permissions"`
	Kind             string          `json:"kind"`
	Generic          bool            `json:"generic"`
}

// GetMCPStatus handles GET /api/v1/mcp/status.
//
// Returns where the MCP endpoint is: "endpoint" is its path on this origin.
// "listen" and "token_id" are kept for older clients and are empty on a
// relay started by cmd/server. "operations" is 0 without a server.
func (d Deps) GetMCPStatus(w http.ResponseWriter, r *http.Request) {
	ops := 0
	if d.MCP.Server != nil {
		ops = d.MCP.Server.OperationCount()
	}
	writeJSON(w, http.StatusOK, mcpStatusResp{
		Enabled:    d.MCP.Enabled,
		Listen:     d.MCP.Listen,
		TokenID:    d.MCP.TokenID,
		Endpoint:   mcpEndpointPath,
		Operations: ops,
	})
}

// GetMCPTools handles GET /api/v1/mcp/tools.
//
// Returns the tools the server offers by name, unfiltered. Each entry
// includes the parameters_schema JSON Schema, the permissions of which a
// token needs one to see and invoke the tool (permission is the first),
// whether it reads, writes or deletes, and whether it is a generic tool. A
// nil MCP.Server yields an empty array rather than 500.
func (d Deps) GetMCPTools(w http.ResponseWriter, r *http.Request) {
	var out []mcpToolResp
	if d.MCP.Server != nil {
		for _, t := range d.MCP.Server.Tools() {
			perms := t.Permissions
			if perms == nil {
				perms = []string{}
			}
			out = append(out, mcpToolResp{
				Name:             t.Name,
				Description:      t.Description,
				ParametersSchema: t.ParametersSchema,
				Permission:       t.Permission,
				Permissions:      perms,
				Kind:             t.Kind,
				Generic:          t.Generic,
			})
		}
	}
	if out == nil {
		out = []mcpToolResp{}
	}
	writeJSON(w, http.StatusOK, out)
}
