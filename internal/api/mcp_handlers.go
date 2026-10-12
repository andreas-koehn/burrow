// mcp_handlers.go — the dashboard's view of the management MCP server
// (spec Part P).
//
// Two read-only JSON endpoints:
//
//	GET /api/v1/mcp/status  → { enabled, listen, token_id, endpoint }
//	GET /api/v1/mcp/tools   → [ { name, description, parameters_schema, permission } ]
//
// Both routes are gated by admin OR mcp:tools:read. The MCP server itself
// (internal/mcpserv) is mounted at /mcp/burrow by NewRouter; these endpoints
// tell the dashboard where it is and list the closed tool inventory, so that
// it can render a page without speaking MCP.

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
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

// MCPToolsLister is the narrow Tools() seam the api package consumes. The
// concrete *mcpserv.Server satisfies it; tests provide a small fake.
type MCPToolsLister interface {
	Tools() []mcpserv.ToolDescriptor
}

// mcpStatusResp is the JSON shape of GET /api/v1/mcp/status. Endpoint is
// the path of the MCP endpoint on this origin.
type mcpStatusResp struct {
	Enabled  bool   `json:"enabled"`
	Listen   string `json:"listen"`
	TokenID  string `json:"token_id"`
	Endpoint string `json:"endpoint"`
}

// mcpToolResp is the wire shape of one tool inventory entry. Mirrors
// mcpserv.ToolDescriptor. It is Burrow's own shape, not MCP's: an MCP
// client reads the tools from /mcp/burrow, filtered by its token.
type mcpToolResp struct {
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	ParametersSchema json.RawMessage `json:"parameters_schema"`
	Permission       string          `json:"permission"`
}

// requireMCPRead is the admin OR mcp:tools:read gate. Cookie callers
// resolve their role via callerRoleForAuth (bearer-set ctx wins;
// otherwise fresh GetUserByID). Same shape as requireMetricsRead.
func (d Deps) requireMCPRead(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, err := d.callerRoleForAuth(r)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if role == "admin" || effectivePerms(r.Context(), role, authz.PermMcpToolsRead) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "mcp:tools:read required")
	})
}

// GetMCPStatus handles GET /api/v1/mcp/status.
//
// Returns where the MCP endpoint is: "endpoint" is its path on this origin.
// "listen" and "token_id" are kept for older clients and are empty on a
// relay started by cmd/server.
func (d Deps) GetMCPStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, mcpStatusResp{
		Enabled:  d.MCP.Enabled,
		Listen:   d.MCP.Listen,
		TokenID:  d.MCP.TokenID,
		Endpoint: mcpEndpointPath,
	})
}

// GetMCPTools handles GET /api/v1/mcp/tools.
//
// Returns the whole closed inventory, unfiltered. Each entry includes the
// parameters_schema JSON Schema and the permission string a caller must
// hold (transitively via the bearer token) to see and invoke the tool. A
// nil MCP.Server yields an empty array rather than 500.
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
