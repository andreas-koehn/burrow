// Package authz holds Burrow's code-defined role→permission map. In v0.2.0
// only the built-in admin/user roles exist and authorization still keys off
// the role string; this map is the read-only extension seam that custom roles
// (v0.4) will grow from. The :own/:any scope split is committed from day one
// so the permission enum never has to change incompatibly later.
//
// v0.4.0 Task 15: custom (non-builtin) roles persist their permission set in
// the roles.permissions JSON column. The store loads the table at startup and
// re-publishes the map after every CreateRole / UpdateRole / DeleteRole via
// SetRoles below. Can() consults that map for non-builtin role names, keeping
// the admin/user hot path on the hardcoded constants. Mutex-protected so a
// concurrent SetRoles cannot race a hot-path Can().
package authz

import "sync"

// Permission is a scoped capability string ("<domain>:<action>[:own|:any]").
type Permission string

// Permission constants. ":own" = act on resources you own; ":any" = act on
// anyone's. Global capabilities (users/roles/settings) have no scope suffix.
const (
	PermTunnelsReadOwn   Permission = "tunnels:read:own"
	PermTunnelsReadAny   Permission = "tunnels:read:any"
	PermTunnelsManageOwn Permission = "tunnels:manage:own"
	PermTunnelsManageAny Permission = "tunnels:manage:any"

	PermTokensManageOwn Permission = "tokens:manage:own"
	PermTokensManageAny Permission = "tokens:manage:any"

	PermServicesConfigureOwn Permission = "services:configure:own"
	PermServicesConfigureAny Permission = "services:configure:any"

	PermSessionsManageOwn Permission = "sessions:manage:own"
	PermSessionsManageAny Permission = "sessions:manage:any"

	PermUsersRead      Permission = "users:read"
	PermUsersManage    Permission = "users:manage"
	PermRolesRead      Permission = "roles:read"
	PermSettingsManage Permission = "settings:manage"

	// v0.4.0: AI, quotas, inspector, audit, webhooks, custom roles,
	// automation tokens, backups, metrics, mTLS, IP-geo, MCP tools.
	PermAIReadOwn      Permission = "ai:read:own"
	PermAIReadAny      Permission = "ai:read:any"
	PermAIConfigureOwn Permission = "ai:configure:own"
	PermAIConfigureAny Permission = "ai:configure:any"

	PermQuotasReadOwn   Permission = "quotas:read:own"
	PermQuotasReadAny   Permission = "quotas:read:any"
	PermQuotasManageAny Permission = "quotas:manage:any"

	PermInspectorReadOwn   Permission = "inspector:read:own"
	PermInspectorReadAny   Permission = "inspector:read:any"
	PermInspectorReplayOwn Permission = "inspector:replay:own"
	PermInspectorReplayAny Permission = "inspector:replay:any"

	PermAuditRead      Permission = "audit:read"
	PermWebhooksManage Permission = "webhooks:manage"
	PermRolesManage    Permission = "roles:manage"

	PermAutomationTokensManageOwn Permission = "automation:tokens:manage:own"
	PermAutomationTokensManageAny Permission = "automation:tokens:manage:any"

	PermBackupRun    Permission = "backup:run"
	PermMetricsRead  Permission = "metrics:read"
	PermMcpToolsRead Permission = "mcp:tools:read"

	PermMtlsManageOwn  Permission = "mtls:manage:own"
	PermMtlsManageAny  Permission = "mtls:manage:any"
	PermIPGeoManageOwn Permission = "ipgeo:manage:own"
	PermIPGeoManageAny Permission = "ipgeo:manage:any"

	// v0.10.0: what only the admin role could do until the permission gate.
	// Global capabilities, held by no role but admin unless a custom role
	// names them.
	PermAIProvidersManage Permission = "ai:providers:manage"
	PermBudgetsManage     Permission = "budgets:manage"
	PermCostPricingManage Permission = "cost:pricing:manage"

	// v0.12.0: the MCP gateway's tool sources. Held by no role but admin
	// unless a custom role names them.
	PermMcpSourcesRead   Permission = "mcp:sources:read"
	PermMcpSourcesManage Permission = "mcp:sources:manage"
)

// Role is a named permission set (built-in only in v0.2.0).
type Role struct {
	Name        string
	Description string
	Permissions []Permission
}

// adminPerms is every permission (admin = full :any access).
var adminPerms = []Permission{
	PermTunnelsReadAny, PermTunnelsReadOwn,
	PermTunnelsManageAny, PermTunnelsManageOwn,
	PermTokensManageAny, PermTokensManageOwn,
	PermServicesConfigureAny, PermServicesConfigureOwn,
	PermSessionsManageAny, PermSessionsManageOwn,
	PermUsersRead, PermUsersManage,
	PermRolesRead, PermSettingsManage,
	// v0.4.0 admin :any + admin-only globals.
	PermAIReadAny, PermAIReadOwn,
	PermAIConfigureAny, PermAIConfigureOwn,
	PermQuotasReadAny, PermQuotasReadOwn, PermQuotasManageAny,
	PermInspectorReadAny, PermInspectorReadOwn,
	PermInspectorReplayAny, PermInspectorReplayOwn,
	PermAuditRead, PermWebhooksManage, PermRolesManage,
	PermAutomationTokensManageAny, PermAutomationTokensManageOwn,
	PermBackupRun, PermMetricsRead, PermMcpToolsRead,
	PermMtlsManageAny, PermMtlsManageOwn,
	PermIPGeoManageAny, PermIPGeoManageOwn,
	// v0.10.0
	PermAIProvidersManage, PermBudgetsManage, PermCostPricingManage,
	// v0.12.0
	PermMcpSourcesRead, PermMcpSourcesManage,
}

// userPerms is the standard-user :own subset.
var userPerms = []Permission{
	PermTunnelsReadOwn,
	PermTunnelsManageOwn,
	PermTokensManageOwn,
	PermServicesConfigureOwn,
	PermSessionsManageOwn,
	// v0.4.0 additive :own perms (read/configure own AI, inspect own
	// traffic, manage own automation tokens, read own quota, manage own
	// mTLS/IP-geo lists).
	PermAIReadOwn, PermAIConfigureOwn,
	PermQuotasReadOwn,
	PermInspectorReadOwn, PermInspectorReplayOwn,
	PermAutomationTokensManageOwn,
	PermMtlsManageOwn, PermIPGeoManageOwn,
}

var builtin = map[string]Role{
	"admin": {
		Name:        "admin",
		Description: "Full administrative access to all tunnels, client tokens, users, roles, and settings.",
		Permissions: adminPerms,
	},
	"user": {
		Name:        "user",
		Description: "Standard user: manage own tunnels and own client tokens.",
		Permissions: userPerms,
	},
}

// order is the stable presentation order for Roles().
var order = []string{"admin", "user"}

// Roles returns the built-in roles in stable order.
func Roles() []Role {
	out := make([]Role, 0, len(order))
	for _, n := range order {
		out = append(out, builtin[n])
	}
	return out
}

// Get returns the named built-in role and whether it exists.
func Get(name string) (Role, bool) {
	r, ok := builtin[name]
	return r, ok
}

// AllPermissions returns the closed set of every permission Burrow defines,
// in declaration order. Used by the custom-roles API (v0.4 Task 15) to
// validate role definitions against the code-defined enum so the wire
// contract can never drift from the constants above.
func AllPermissions() []Permission {
	return []Permission{
		// v0.2.0 / v0.3.0
		PermTunnelsReadOwn, PermTunnelsReadAny,
		PermTunnelsManageOwn, PermTunnelsManageAny,
		PermTokensManageOwn, PermTokensManageAny,
		PermServicesConfigureOwn, PermServicesConfigureAny,
		PermSessionsManageOwn, PermSessionsManageAny,
		PermUsersRead, PermUsersManage,
		PermRolesRead, PermSettingsManage,
		// v0.4.0
		PermAIReadOwn, PermAIReadAny,
		PermAIConfigureOwn, PermAIConfigureAny,
		PermQuotasReadOwn, PermQuotasReadAny, PermQuotasManageAny,
		PermInspectorReadOwn, PermInspectorReadAny,
		PermInspectorReplayOwn, PermInspectorReplayAny,
		PermAuditRead, PermWebhooksManage, PermRolesManage,
		PermAutomationTokensManageOwn, PermAutomationTokensManageAny,
		PermBackupRun, PermMetricsRead, PermMcpToolsRead,
		PermMtlsManageOwn, PermMtlsManageAny,
		PermIPGeoManageOwn, PermIPGeoManageAny,
		// v0.10.0
		PermAIProvidersManage, PermBudgetsManage, PermCostPricingManage,
		// v0.12.0
		PermMcpSourcesRead, PermMcpSourcesManage,
	}
}

// customRoles is the process-wide cache of non-builtin role→permission sets,
// populated by SetRoles after every store-side write. Reads on the hot path
// take an RLock; writes (cache repopulation) take a Lock. Empty map (the
// zero value here) means "no custom roles defined yet", which keeps Can()
// returning false for unknown role names — the safe default.
var (
	customRolesMu sync.RWMutex
	customRoles   = map[string][]Permission{}
)

// SetRoles installs a fresh map of custom (non-builtin) role→permissions.
// The store calls this after every CreateRole / UpdateRole / DeleteRole so
// subsequent Can() lookups see the new shape. Built-in admin/user are NOT
// in this map (their permissions are hard-coded in builtin above); callers
// MUST NOT pass them through here. A nil map is treated as "no custom
// roles" (everything falls back to the safe-default deny).
//
// The slice values are copied to insulate the cache from later mutation by
// the caller — the store may reuse its source slice for the next refresh.
func SetRoles(roles map[string][]Permission) {
	customRolesMu.Lock()
	defer customRolesMu.Unlock()
	customRoles = make(map[string][]Permission, len(roles))
	for name, perms := range roles {
		if _, isBuiltin := builtin[name]; isBuiltin {
			// Defensive: never let a custom-roles refresh shadow the
			// hardcoded admin/user paths. Skip silently — callers that
			// pass builtin names did so by accident and the hot path is
			// unaffected.
			continue
		}
		cp := make([]Permission, len(perms))
		copy(cp, perms)
		customRoles[name] = cp
	}
}

// Can reports whether the named role holds permission p. Unknown roles get
// nothing (safe default). Built-in admin/user resolve via the hard-coded
// table; any other role name falls back to the custom-roles cache populated
// by SetRoles.
func Can(role string, p Permission) bool {
	if r, ok := builtin[role]; ok {
		for _, have := range r.Permissions {
			if have == p {
				return true
			}
		}
		return false
	}
	customRolesMu.RLock()
	perms, ok := customRoles[role]
	customRolesMu.RUnlock()
	if !ok {
		return false
	}
	for _, have := range perms {
		if have == p {
			return true
		}
	}
	return false
}
