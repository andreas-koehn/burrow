// tools.go — the closed 12-tool MCP inventory (spec Part P.2).
//
// Each tool wraps an existing store / recorder method through a narrow
// ToolStore interface. No business logic is duplicated here — the wrappers
// resolve callerID/callerRole from the request ctx (set by Server) and
// delegate verbatim. Adding a 13th tool requires a doc PR and a
// corresponding plan-task in v0.5.

package mcpserv

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// Tool is one entry in the MCP registry. Description and ParamsSchema are
// surfaced via tools/list; Call is invoked by tools/call after the bearer
// token's permission set has been validated against Permission.
type Tool struct {
	Name         string
	Description  string
	Permission   authz.Permission
	ParamsSchema json.RawMessage
	// Call executes the tool. args is the raw JSON arguments from the
	// tools/call params.arguments field; the returned bytes are placed
	// verbatim in the JSON-RPC result. Implementations resolve callerID /
	// callerRole via mcpserv.CallerID / mcpserv.CallerRole on the ctx.
	Call func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// ToolStore is the narrow seam between mcpserv and the real *store.Store +
// metrics.Recorder. cmd/server (Task 25) constructs a single adapter that
// satisfies every method; tests supply an in-memory fake.
//
// The interface is intentionally read-heavy plus the three mutating tools
// the spec requires (services.set_access_mode, api_keys.create/delete,
// tokens.mint). No method here is async — all the listed REST surfaces are
// synchronous, so the MCP tool wrappers do not need a streaming protocol.
type ToolStore interface {
	// Tunnels
	ListUserTunnels(ctx context.Context, callerID, callerRole string) ([]TunnelInfo, error)

	// Services
	ListServices(ctx context.Context, callerID, callerRole string) ([]store.ServiceView, error)
	GetService(ctx context.Context, callerID, callerRole, serviceID string) (store.ServiceDetail, error)
	SetServiceAccessMode(ctx context.Context, callerID, callerRole, serviceID, mode, apiKeyHeader string, mtlsCAPEM []byte) error

	// API keys (per service)
	ListAPIKeys(ctx context.Context, callerID, callerRole, serviceID string) ([]db.ServiceAPIKey, error)
	CreateAPIKey(ctx context.Context, callerID, callerRole, serviceID, name string) (id, plaintext string, err error)
	DeleteAPIKey(ctx context.Context, callerID, callerRole, serviceID, keyID string) error

	// Tokens (client tokens, the burrow CLI's bearer)
	ListClientTokens(ctx context.Context, userID string) ([]db.ClientToken, error)
	IssueClientToken(ctx context.Context, userID, name string) (plaintext string, err error)

	// Users (admin / users:read)
	ListUsers(ctx context.Context, q string, limit, offset int) ([]db.User, int, error)

	// Audit
	SearchAuditEvents(ctx context.Context, q db.AuditQuery) ([]db.AuditEvent, error)

	// Metrics — returns the Prometheus 0.0.4 plain-text body so operators
	// can ingest it client-side. The Snapshot() helper on the recorder
	// would be a nicer JSON shape, but it isn't part of the closed metric
	// recorder API — calling WriteText keeps the seam minimal and the
	// integration test easy.
	MetricsText() (string, error)
}

// TunnelInfo is the narrow tunnel view the tunnels.list tool returns. It
// mirrors api.TunnelView but lives in mcpserv so the package has no
// dependency on api. cmd/server's adapter populates it from the registry.
type TunnelInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	RemotePort int    `json:"remote_port"`
	LocalAddr  string `json:"local_addr"`
	BytesIn    uint64 `json:"bytes_in"`
	BytesOut   uint64 `json:"bytes_out"`
	Connected  bool   `json:"connected"`
}

// ----- ParamsSchemas (closed set; lowercase keys) --------------------------

// schemaEmpty is the canonical "no parameters" schema. Used by every
// no-argument tool to keep the surface consistent.
var schemaEmpty = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

// schemaServiceID is the schema for tools that take only a serviceID.
var schemaServiceID = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`)

// schemaSetAccessMode is the schema for services.set_access_mode.
var schemaSetAccessMode = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"mode":{"type":"string","enum":["open","api_key","burrow_login","mtls"]},"api_key_header":{"type":"string"}},"required":["id","mode"],"additionalProperties":false}`)

// schemaServiceIDOnly is the schema for api_keys.list / .create / .delete.
var schemaAPIKeysList = json.RawMessage(`{"type":"object","properties":{"service_id":{"type":"string"}},"required":["service_id"],"additionalProperties":false}`)
var schemaAPIKeysCreate = json.RawMessage(`{"type":"object","properties":{"service_id":{"type":"string"},"name":{"type":"string"}},"required":["service_id","name"],"additionalProperties":false}`)
var schemaAPIKeysDelete = json.RawMessage(`{"type":"object","properties":{"service_id":{"type":"string"},"id":{"type":"string"}},"required":["service_id","id"],"additionalProperties":false}`)

// schemaMintToken is the schema for tokens.mint — only `name` is required.
var schemaMintToken = json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`)

// schemaUsersList is the schema for users.list.
var schemaUsersList = json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":1000},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`)

// schemaAuditSearch is the schema for audit.search.
var schemaAuditSearch = json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"},"since":{"type":"string","format":"date-time"},"until":{"type":"string","format":"date-time"},"limit":{"type":"integer","minimum":1,"maximum":1000}},"additionalProperties":false}`)

// schemaTunnelsList is the schema for tunnels.list — optional filter string.
var schemaTunnelsList = json.RawMessage(`{"type":"object","properties":{"filter":{"type":"string"}},"additionalProperties":false}`)

// ----- buildTools ----------------------------------------------------------

// orderedTools is the stable declaration order — tools/list emits the
// inventory in this order, and the api package's mcp/tools endpoint shares
// it. Mirrors spec Part P.2.
var orderedTools = []string{
	"tunnels.list",
	"services.list",
	"services.get",
	"services.set_access_mode",
	"api_keys.list",
	"api_keys.create",
	"api_keys.delete",
	"tokens.list",
	"tokens.mint",
	"users.list",
	"audit.search",
	"metrics.snapshot",
}

// buildTools returns the 12-tool registry, wrapping toolStore methods. Pass
// nil for toolStore to get tools whose Call returns ErrForbidden — useful in
// tests that exercise only the registry shape (tools/list, schema).
func buildTools(s ToolStore) map[string]Tool {
	m := make(map[string]Tool, len(orderedTools))
	add := func(t Tool) { m[t.Name] = t }

	add(Tool{
		Name:         "tunnels.list",
		Description:  "List all live tunnels visible to the caller. Optional 'filter' parameter does a substring match on tunnel name (case-insensitive).",
		Permission:   authz.PermTunnelsReadAny,
		ParamsSchema: schemaTunnelsList,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			var in struct {
				Filter string `json:"filter"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			rows, err := s.ListUserTunnels(ctx, CallerID(ctx), CallerRole(ctx))
			if err != nil {
				return nil, err
			}
			if in.Filter != "" {
				filtered := rows[:0]
				for _, r := range rows {
					if containsFold(r.Name, in.Filter) {
						filtered = append(filtered, r)
					}
				}
				rows = filtered
			}
			return marshalJSON(rows)
		},
	})

	add(Tool{
		Name:         "services.list",
		Description:  "List services. Admin / services:configure:any callers see every service; others see only their own.",
		Permission:   authz.PermServicesConfigureOwn,
		ParamsSchema: schemaEmpty,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			svcs, err := s.ListServices(ctx, CallerID(ctx), CallerRole(ctx))
			if err != nil {
				return nil, err
			}
			// Re-shape to lowercase wire keys consistent with the REST
			// surface. The store-side ServiceView uses Go-default
			// capitalised keys; the MCP envelope normalises them.
			type svcOut struct {
				ID           string    `json:"id"`
				UserID       string    `json:"user_id"`
				Name         string    `json:"name"`
				Type         string    `json:"type"`
				Subdomain    string    `json:"subdomain"`
				AccessMode   string    `json:"access_mode"`
				APIKeyHeader string    `json:"api_key_header"`
				CreatedAt    time.Time `json:"created_at"`
			}
			out := make([]svcOut, 0, len(svcs))
			for _, sv := range svcs {
				out = append(out, svcOut{
					ID: sv.ID, UserID: sv.UserID, Name: sv.Name,
					Type: sv.Type, Subdomain: sv.Subdomain,
					AccessMode: sv.AccessMode, APIKeyHeader: sv.APIKeyHeader,
					CreatedAt: sv.CreatedAt,
				})
			}
			return marshalJSON(out)
		},
	})

	add(Tool{
		Name:         "services.get",
		Description:  "Get a single service by id. Includes api_key_count and access_policy. 'id' is required.",
		Permission:   authz.PermServicesConfigureOwn,
		ParamsSchema: schemaServiceID,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			var in struct {
				ID string `json:"id"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			if in.ID == "" {
				return nil, errors.New("id is required")
			}
			det, err := s.GetService(ctx, CallerID(ctx), CallerRole(ctx), in.ID)
			if err != nil {
				return nil, mapStoreErr(err)
			}
			policy := det.AccessPolicy
			if policy == nil {
				policy = []string{}
			}
			type svcDetailOut struct {
				ID           string    `json:"id"`
				UserID       string    `json:"user_id"`
				Name         string    `json:"name"`
				Type         string    `json:"type"`
				Subdomain    string    `json:"subdomain"`
				AccessMode   string    `json:"access_mode"`
				APIKeyHeader string    `json:"api_key_header"`
				CreatedAt    time.Time `json:"created_at"`
				APIKeyCount  int       `json:"api_key_count"`
				AccessPolicy []string  `json:"access_policy"`
			}
			return marshalJSON(svcDetailOut{
				ID: det.ID, UserID: det.UserID, Name: det.Name,
				Type: det.Type, Subdomain: det.Subdomain,
				AccessMode: det.AccessMode, APIKeyHeader: det.APIKeyHeader,
				CreatedAt:   det.CreatedAt,
				APIKeyCount: det.APIKeyCount, AccessPolicy: policy,
			})
		},
	})

	add(Tool{
		Name:         "services.set_access_mode",
		Description:  "Set a service's access mode. mode is one of open / api_key / burrow_login / mtls. api_key_header is honored only for api_key mode.",
		Permission:   authz.PermServicesConfigureOwn,
		ParamsSchema: schemaSetAccessMode,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			var in struct {
				ID           string `json:"id"`
				Mode         string `json:"mode"`
				APIKeyHeader string `json:"api_key_header"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			if in.ID == "" || in.Mode == "" {
				return nil, errors.New("id and mode are required")
			}
			header := ""
			if in.Mode == "api_key" {
				header = in.APIKeyHeader
			}
			// mtls / burrow_login pem inputs are out of scope for MCP in
			// v0.4.0 — the operator-supplied PEM bundle does not belong
			// in a JSON-RPC argument list; reject mtls here so callers do
			// not accidentally clear the existing CA.
			if in.Mode == "mtls" {
				return nil, errors.New("mtls access mode requires the REST surface (mtls_ca_pem upload)")
			}
			if err := s.SetServiceAccessMode(ctx, CallerID(ctx), CallerRole(ctx), in.ID, in.Mode, header, nil); err != nil {
				return nil, mapStoreErr(err)
			}
			return json.RawMessage(`{"ok":true}`), nil
		},
	})

	add(Tool{
		Name:         "api_keys.list",
		Description:  "List the API keys provisioned for a service. service_id is required.",
		Permission:   authz.PermServicesConfigureOwn,
		ParamsSchema: schemaAPIKeysList,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			var in struct {
				ServiceID string `json:"service_id"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			if in.ServiceID == "" {
				return nil, errors.New("service_id is required")
			}
			rows, err := s.ListAPIKeys(ctx, CallerID(ctx), CallerRole(ctx), in.ServiceID)
			if err != nil {
				return nil, mapStoreErr(err)
			}
			// Redact key_hash from the wire surface — the plaintext was
			// surfaced only at create time; the hash adds no operator
			// value and is sensitive.
			type apiKeyOut struct {
				ID        string     `json:"id"`
				Name      string     `json:"name"`
				LastUsed  *time.Time `json:"last_used"`
				CreatedAt time.Time  `json:"created_at"`
			}
			out := make([]apiKeyOut, 0, len(rows))
			for _, k := range rows {
				out = append(out, apiKeyOut{
					ID: k.ID, Name: k.Name,
					LastUsed: k.LastUsed, CreatedAt: k.CreatedAt,
				})
			}
			return marshalJSON(out)
		},
	})

	add(Tool{
		Name:         "api_keys.create",
		Description:  "Create a new API key for a service. service_id and name are required. The plaintext is returned EXACTLY ONCE in the response.",
		Permission:   authz.PermServicesConfigureOwn,
		ParamsSchema: schemaAPIKeysCreate,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			var in struct {
				ServiceID string `json:"service_id"`
				Name      string `json:"name"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			if in.ServiceID == "" || in.Name == "" {
				return nil, errors.New("service_id and name are required")
			}
			id, plaintext, err := s.CreateAPIKey(ctx, CallerID(ctx), CallerRole(ctx), in.ServiceID, in.Name)
			if err != nil {
				return nil, mapStoreErr(err)
			}
			return marshalJSON(struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Key  string `json:"key"`
			}{ID: id, Name: in.Name, Key: plaintext})
		},
	})

	add(Tool{
		Name:         "api_keys.delete",
		Description:  "Delete an API key. service_id and id are required.",
		Permission:   authz.PermServicesConfigureOwn,
		ParamsSchema: schemaAPIKeysDelete,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			var in struct {
				ServiceID string `json:"service_id"`
				ID        string `json:"id"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			if in.ServiceID == "" || in.ID == "" {
				return nil, errors.New("service_id and id are required")
			}
			if err := s.DeleteAPIKey(ctx, CallerID(ctx), CallerRole(ctx), in.ServiceID, in.ID); err != nil {
				return nil, mapStoreErr(err)
			}
			return json.RawMessage(`{"ok":true}`), nil
		},
	})

	add(Tool{
		Name:         "tokens.list",
		Description:  "List the caller's own client tokens (the burrow CLI's bearer).",
		Permission:   authz.PermTokensManageOwn,
		ParamsSchema: schemaEmpty,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			rows, err := s.ListClientTokens(ctx, CallerID(ctx))
			if err != nil {
				return nil, err
			}
			// Redact token_hash — never surface the irreversible secret.
			type tokenOut struct {
				ID        string     `json:"id"`
				Name      string     `json:"name"`
				LastUsed  *time.Time `json:"last_used"`
				CreatedAt time.Time  `json:"created_at"`
			}
			out := make([]tokenOut, 0, len(rows))
			for _, t := range rows {
				out = append(out, tokenOut{
					ID: t.ID, Name: t.Name,
					LastUsed: t.LastUsed, CreatedAt: t.CreatedAt,
				})
			}
			return marshalJSON(out)
		},
	})

	add(Tool{
		Name:         "tokens.mint",
		Description:  "Issue a new client token for the caller. name is required. The plaintext is returned EXACTLY ONCE.",
		Permission:   authz.PermTokensManageOwn,
		ParamsSchema: schemaMintToken,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			var in struct {
				Name string `json:"name"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Name == "" {
				return nil, errors.New("name is required")
			}
			plaintext, err := s.IssueClientToken(ctx, CallerID(ctx), in.Name)
			if err != nil {
				return nil, err
			}
			return marshalJSON(struct {
				Name  string `json:"name"`
				Token string `json:"token"`
			}{Name: in.Name, Token: plaintext})
		},
	})

	add(Tool{
		Name:         "users.list",
		Description:  "List users (admin / users:read). Optional q (substring), limit (1..1000, default 50), offset.",
		Permission:   authz.PermUsersRead,
		ParamsSchema: schemaUsersList,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			var in struct {
				Q      string `json:"q"`
				Limit  int    `json:"limit"`
				Offset int    `json:"offset"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Limit == 0 {
				in.Limit = 50
			}
			if in.Limit > 1000 {
				in.Limit = 1000
			}
			if in.Offset < 0 {
				in.Offset = 0
			}
			rows, total, err := s.ListUsers(ctx, in.Q, in.Limit, in.Offset)
			if err != nil {
				return nil, err
			}
			// Redact password_hash by mapping to a small public shape.
			type userOut struct {
				ID        string     `json:"id"`
				Email     string     `json:"email"`
				Role      string     `json:"role"`
				Status    string     `json:"status"`
				LastLogin *time.Time `json:"last_login"`
				CreatedAt time.Time  `json:"created_at"`
			}
			out := make([]userOut, 0, len(rows))
			for _, u := range rows {
				out = append(out, userOut{
					ID:        u.ID,
					Email:     u.Email,
					Role:      u.Role,
					Status:    u.Status,
					LastLogin: u.LastLogin,
					CreatedAt: u.CreatedAt,
				})
			}
			return marshalJSON(struct {
				Users []userOut `json:"users"`
				Total int       `json:"total"`
			}{Users: out, Total: total})
		},
	})

	add(Tool{
		Name:         "audit.search",
		Description:  "Search the audit log. q does a substring match (action / subject_label / actor_email); since/until are RFC3339; limit caps at 1000.",
		Permission:   authz.PermAuditRead,
		ParamsSchema: schemaAuditSearch,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			var in struct {
				Q     string     `json:"q"`
				Since *time.Time `json:"since"`
				Until *time.Time `json:"until"`
				Limit int        `json:"limit"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Limit == 0 {
				in.Limit = 100
			}
			if in.Limit > 1000 {
				in.Limit = 1000
			}
			rows, err := s.SearchAuditEvents(ctx, db.AuditQuery{
				Q:     in.Q,
				Since: in.Since,
				Until: in.Until,
				Limit: in.Limit,
			})
			if err != nil {
				return nil, err
			}
			// Mirror api.auditEventResp so callers see the same shape
			// they'd get from GET /api/v1/audit/events; Payload is
			// preserved as JSON (RawMessage) so structured fields are
			// not double-escaped.
			type auditOut struct {
				ID           string          `json:"id"`
				Ts           time.Time       `json:"ts"`
				ActorID      string          `json:"actor_id"`
				ActorEmail   string          `json:"actor_email"`
				Action       string          `json:"action"`
				SubjectID    string          `json:"subject_id"`
				SubjectLabel string          `json:"subject_label"`
				Result       string          `json:"result"`
				SourceIP     string          `json:"source_ip"`
				UserAgent    string          `json:"user_agent"`
				RequestID    string          `json:"request_id"`
				Payload      json.RawMessage `json:"payload"`
				PrevHash     string          `json:"prev_hash"`
				Hash         string          `json:"hash"`
			}
			out := make([]auditOut, 0, len(rows))
			for _, e := range rows {
				pl := json.RawMessage(e.Payload)
				if len(pl) == 0 {
					pl = json.RawMessage(`{}`)
				}
				out = append(out, auditOut{
					ID: e.ID, Ts: e.Ts,
					ActorID: e.ActorID, ActorEmail: e.ActorEmail,
					Action: e.Action, SubjectID: e.SubjectID,
					SubjectLabel: e.SubjectLabel, Result: e.Result,
					SourceIP: e.SourceIP, UserAgent: e.UserAgent,
					RequestID: e.RequestID, Payload: pl,
					PrevHash: e.PrevHash, Hash: e.Hash,
				})
			}
			return marshalJSON(out)
		},
	})

	add(Tool{
		Name:         "metrics.snapshot",
		Description:  "Return the current Prometheus 0.0.4 text-format metrics body. Operators parse the response client-side.",
		Permission:   authz.PermMetricsRead,
		ParamsSchema: schemaEmpty,
		Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if s == nil {
				return nil, errStoreUnavailable
			}
			body, err := s.MetricsText()
			if err != nil {
				return nil, err
			}
			return marshalJSON(struct {
				Format string `json:"format"`
				Body   string `json:"body"`
			}{Format: "prometheus-0.0.4", Body: body})
		},
	})

	return m
}

// ----- helpers -------------------------------------------------------------

// errStoreUnavailable is the sentinel returned by every tool's Call when the
// server was constructed with a nil ToolStore — degraded mode for the
// dashboard's tools/list view (which doesn't need to invoke anything).
var errStoreUnavailable = errors.New("tool store unavailable")

// decodeArgs unmarshals args into v. An empty args body (nil or zero-length)
// is accepted as an all-defaults call — schemas mark required fields, but
// the handler chooses what to validate inside Call so the error messages
// can be specific.
func decodeArgs(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil
	}
	return json.Unmarshal(args, v)
}

// marshalJSON is a tiny helper that returns the JSON bytes of v as a
// json.RawMessage so Tool.Call can compose results without each call site
// type-asserting json.Marshal's []byte return.
func marshalJSON(v any) (json.RawMessage, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// mapStoreErr translates the store sentinels into the in-package
// ErrForbidden the JSON-RPC handler maps to -32603 "forbidden". Every
// other error is passed through unchanged.
func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrForbidden):
		return ErrForbidden
	case errors.Is(err, db.ErrNotFound):
		return errors.New("not found")
	}
	return err
}

// containsFold returns true when needle is a case-insensitive substring of
// hay. Used by tunnels.list's filter param. ASCII-only is fine for tunnel
// names (the constraint is enforced upstream).
func containsFold(hay, needle string) bool {
	if needle == "" {
		return true
	}
	hay, needle = toLowerASCII(hay), toLowerASCII(needle)
	return indexOf(hay, needle) >= 0
}

// toLowerASCII is an allocation-light ASCII downcaser. tunnel.Name is
// constrained to ASCII at create time (see store validation) so this is safe.
func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// indexOf is a small wrapper around strings.Index without importing strings
// for one call site — keeps the tools file self-contained at the cost of a
// trivial helper. Returns the first index of needle in hay, or -1.
func indexOf(hay, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
