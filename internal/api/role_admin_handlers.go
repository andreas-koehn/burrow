package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// Spec Part I (v0.4.0 Task 15): editable custom roles + permission matrix.
//
// Permission gating: admin OR authz.PermRolesManage for POST/PUT/DELETE
// mutations; the existing /roles GET routes (registered in router.go under
// the admin-only Group) keep their stricter admin-only stance for v0.4.0 so
// the list/detail wire shape stays consistent across reads. The mutation
// routes accept rolesm:manage so a custom role can be granted curator
// privileges without admin escalation.
//
// All four routes consult the closed authz.AllPermissions() catalog when
// validating role payloads — an unknown key short-circuits with
// 400 {"error":"unknown permission \"<key>\""}.

// requireRolesManage gates the POST/PUT/DELETE routes on admin OR the
// role:manage permission. Mirrors requireWebhooksManage in shape.
func (d Deps) requireRolesManage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, err := d.callerRole(r)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if role == "admin" || authz.Can(role, authz.PermRolesManage) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "roles:manage required")
	})
}

// --- Wire shapes -----------------------------------------------------------

// createRoleReq is the POST /api/v1/roles request body. All four fields are
// validated up-front (name non-empty + slug-shaped, description bounded,
// permissions checked against authz.AllPermissions() in the store). Built-in
// names (admin/user) are rejected at the DB layer with 409 — no special-case
// handling here.
type createRoleReq struct {
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	Permissions        []string `json:"permissions"`
	DefaultForNewUsers bool     `json:"default_for_new_users"`
}

// updateRoleReq is the PUT /api/v1/roles/{name} body. Every field is
// optional (pointer types) so the handler can distinguish "leave unchanged"
// from "set to zero". Built-in roles short-circuit with 409 before any
// field is examined.
type updateRoleReq struct {
	Description        *string   `json:"description,omitempty"`
	Permissions        *[]string `json:"permissions,omitempty"`
	DefaultForNewUsers *bool     `json:"default_for_new_users,omitempty"`
}

// PostRole creates a new non-builtin role. 201 on success (body is the
// roleDetailResp), 400 on a bad payload or unknown perm key (echoed back
// verbatim), 409 on a duplicate / builtin name.
func (d Deps) PostRole(w http.ResponseWriter, r *http.Request) {
	if d.Roles == nil {
		writeErr(w, http.StatusInternalServerError, "role store unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in createRoleReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !isValidRoleName(in.Name) {
		writeErr(w, http.StatusBadRequest, "name must be a slug (lowercase letters, digits, dashes; 1..64 chars)")
		return
	}
	if err := d.Roles.CreateRole(r.Context(), in.Name, in.Description, in.Permissions, in.DefaultForNewUsers); err != nil {
		if writeRoleErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "create role failed")
		return
	}
	// Read back the created row so the response matches the GET shape.
	rd, err := d.Roles.GetRole(r.Context(), in.Name)
	if err != nil {
		// The create succeeded but the read failed — surface a 500 with
		// a clear message; the row is durable, the next GET will see it.
		writeErr(w, http.StatusInternalServerError, "created but readback failed")
		return
	}
	perms := rd.Permissions
	if perms == nil {
		perms = []string{}
	}
	writeJSON(w, http.StatusCreated, roleDetailResp{
		Name:               rd.Name,
		Description:        rd.Description,
		CreatedAt:          rd.CreatedAt,
		Permissions:        perms,
		Builtin:            rd.Builtin,
		DefaultForNewUsers: rd.DefaultForNewUsers,
	})
}

// PutRole patches an existing non-builtin role. 204 on success, 404 if no
// row matches, 409 for built-in rows, 400 on an unknown perm key.
func (d Deps) PutRole(w http.ResponseWriter, r *http.Request) {
	if d.Roles == nil {
		writeErr(w, http.StatusInternalServerError, "role store unavailable")
		return
	}
	name := chi.URLParam(r, "name")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in updateRoleReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := d.Roles.UpdateRole(r.Context(), name, store.RoleUpdate{
		Description:        in.Description,
		Permissions:        in.Permissions,
		DefaultForNewUsers: in.DefaultForNewUsers,
	}); err != nil {
		if writeRoleErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "update role failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteRole removes a non-builtin role. Users on the role are re-assigned
// to the current default-for-new-users role (single tx). 204 on success,
// 404 if no row matches, 409 for built-in rows.
//
// TODO(Task 25): wire audit Logger so we emit one "role.delete" event
// followed by one "user.update" event per affected user (the affected list
// is already plumbed back from the store). Until the Logger is injected
// into Deps the handler simply discards the list.
func (d Deps) DeleteRole(w http.ResponseWriter, r *http.Request) {
	if d.Roles == nil {
		writeErr(w, http.StatusInternalServerError, "role store unavailable")
		return
	}
	name := chi.URLParam(r, "name")
	if _, err := d.Roles.DeleteRole(r.Context(), name); err != nil {
		if writeRoleErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "delete role failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// permissionInfo is one row of the GET /api/v1/roles/permissions response.
// Group is a human-readable section label (e.g. "Tunnels", "AI") used by the
// UI to bucket checkboxes; Description is a short, user-facing blurb.
type permissionInfo struct {
	Key         string `json:"key"`
	Group       string `json:"group"`
	Description string `json:"description"`
}

// permissionCatalog returns one PermissionInfo per entry in
// authz.AllPermissions(). The handler builds the response by iterating
// AllPermissions() in declaration order (the spec's "closed catalog") and
// looking up the human-readable Group/Description from the table below. Any
// permission that lacks an explicit entry falls back to a derived group (the
// first segment of the colon-separated key, title-cased) and the key itself
// as the description — a safe default that keeps the endpoint complete even
// if a new permission is added in code without updating this table.
//
// GET /api/v1/roles/permissions
func (d Deps) GetRolePermissions(w http.ResponseWriter, r *http.Request) {
	all := authz.AllPermissions()
	out := make([]permissionInfo, 0, len(all))
	for _, p := range all {
		info, ok := permissionInfoTable[p]
		if !ok {
			info = permissionInfo{
				Key:         string(p),
				Group:       deriveGroup(string(p)),
				Description: string(p),
			}
		} else {
			info.Key = string(p)
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

// permissionInfoTable maps every permission constant to its UI grouping +
// description. Kept in this file so the wire contract for the matrix is in
// one place. Missing entries fall back to deriveGroup (see GetRolePermissions).
var permissionInfoTable = map[authz.Permission]permissionInfo{
	// Tunnels
	authz.PermTunnelsReadOwn:   {Group: "Tunnels", Description: "Read own tunnels"},
	authz.PermTunnelsReadAny:   {Group: "Tunnels", Description: "Read all tunnels"},
	authz.PermTunnelsManageOwn: {Group: "Tunnels", Description: "Manage own tunnels"},
	authz.PermTunnelsManageAny: {Group: "Tunnels", Description: "Manage all tunnels"},
	// Client tokens
	authz.PermTokensManageOwn: {Group: "Client tokens", Description: "Manage own client tokens"},
	authz.PermTokensManageAny: {Group: "Client tokens", Description: "Manage all client tokens"},
	// Services
	authz.PermServicesConfigureOwn: {Group: "Services", Description: "Configure own services"},
	authz.PermServicesConfigureAny: {Group: "Services", Description: "Configure all services"},
	// Sessions
	authz.PermSessionsManageOwn: {Group: "Sessions", Description: "Manage own browser sessions"},
	authz.PermSessionsManageAny: {Group: "Sessions", Description: "Manage all browser sessions"},
	// Users / roles / settings (global)
	authz.PermUsersRead:      {Group: "Users", Description: "Read users"},
	authz.PermUsersManage:    {Group: "Users", Description: "Manage users"},
	authz.PermRolesRead:      {Group: "Roles", Description: "Read roles"},
	authz.PermRolesManage:    {Group: "Roles", Description: "Create, edit, delete custom roles"},
	authz.PermSettingsManage: {Group: "Settings", Description: "Manage server settings"},
	// AI
	authz.PermAIReadOwn:         {Group: "AI", Description: "Read own AI traffic + settings"},
	authz.PermAIReadAny:         {Group: "AI", Description: "Read all AI traffic + settings"},
	authz.PermAIConfigureOwn:    {Group: "AI", Description: "Configure own AI service settings"},
	authz.PermAIConfigureAny:    {Group: "AI", Description: "Configure all AI service settings"},
	authz.PermAIProvidersManage: {Group: "AI", Description: "Create, change and delete AI providers and their model catalogs"},
	// Quotas + cost
	authz.PermQuotasReadOwn:     {Group: "Quotas", Description: "Read own usage + limits"},
	authz.PermQuotasReadAny:     {Group: "Quotas", Description: "Read all usage + limits"},
	authz.PermQuotasManageAny:   {Group: "Quotas", Description: "Manage rate limits"},
	authz.PermBudgetsManage:     {Group: "Quotas", Description: "Read and manage budgets"},
	authz.PermCostPricingManage: {Group: "Quotas", Description: "Change the pricing table"},
	// Inspector
	authz.PermInspectorReadOwn:   {Group: "Inspector", Description: "Read own request inspector"},
	authz.PermInspectorReadAny:   {Group: "Inspector", Description: "Read all request inspectors"},
	authz.PermInspectorReplayOwn: {Group: "Inspector", Description: "Replay own captured requests"},
	authz.PermInspectorReplayAny: {Group: "Inspector", Description: "Replay any captured request"},
	// Audit
	authz.PermAuditRead: {Group: "Audit", Description: "Read + verify the audit log"},
	// Webhooks
	authz.PermWebhooksManage: {Group: "Webhooks", Description: "Manage outbound webhooks"},
	// Automation tokens
	authz.PermAutomationTokensManageOwn: {Group: "Automation tokens", Description: "Manage own automation tokens"},
	authz.PermAutomationTokensManageAny: {Group: "Automation tokens", Description: "Manage all automation tokens"},
	// Backup / metrics / MCP
	authz.PermBackupRun:    {Group: "Backup", Description: "Run on-demand backups"},
	authz.PermMetricsRead:  {Group: "Metrics", Description: "Read Prometheus metrics + dashboards"},
	authz.PermMcpToolsRead: {Group: "MCP", Description: "Read MCP tool catalog"},
	// mTLS / IP-geo
	authz.PermMtlsManageOwn:  {Group: "mTLS", Description: "Manage own client-certificate lists"},
	authz.PermMtlsManageAny:  {Group: "mTLS", Description: "Manage all client-certificate lists"},
	authz.PermIPGeoManageOwn: {Group: "IP / geo", Description: "Manage own IP/geo allow + block lists"},
	authz.PermIPGeoManageAny: {Group: "IP / geo", Description: "Manage all IP/geo allow + block lists"},
}

// deriveGroup is the fallback for any permission that lacks an explicit
// entry in permissionInfoTable. Splits the key on ':' and title-cases the
// first segment so "ai:read:any" → "Ai" (good enough to surface a stub
// label while the maintainer adds the real entry).
func deriveGroup(key string) string {
	i := strings.IndexByte(key, ':')
	seg := key
	if i > 0 {
		seg = key[:i]
	}
	if seg == "" {
		return ""
	}
	runes := []rune(seg)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// --- Helpers --------------------------------------------------------------

// isValidRoleName enforces a conservative slug shape — lowercase letters,
// digits, and dashes; 1..64 chars; not starting/ending with a dash. This
// keeps role names URL-safe (we use them in path parameters), CSV-safe,
// and unambiguous against the builtin "admin"/"user" rows.
func isValidRoleName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// writeRoleErr maps a store/db error to the appropriate HTTP status + body.
// Returns true when handled — the caller must NOT write a fallback 500.
func writeRoleErr(w http.ResponseWriter, err error) bool {
	var unk store.ErrUnknownPermission
	switch {
	case errors.As(err, &unk):
		// Exact wire contract: {"error": `unknown permission "<key>"`}
		writeErr(w, http.StatusBadRequest, `unknown permission "`+unk.Key+`"`)
		return true
	case errors.Is(err, store.ErrRoleBuiltin):
		writeErr(w, http.StatusConflict, "role is built-in")
		return true
	case errors.Is(err, store.ErrRoleExists):
		writeErr(w, http.StatusConflict, "role already exists")
		return true
	case errors.Is(err, db.ErrNotFound):
		writeErr(w, http.StatusNotFound, "role not found")
		return true
	}
	return false
}
