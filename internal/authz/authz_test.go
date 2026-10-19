package authz

import (
	"slices"
	"testing"
)

func TestPermsV040(t *testing.T) {
	for _, p := range []Permission{
		PermAIReadAny, PermAIConfigureAny, PermAuditRead,
		PermWebhooksManage, PermRolesManage, PermBackupRun,
		PermMetricsRead, PermMtlsManageAny, PermIPGeoManageAny,
		PermInspectorReadAny, PermInspectorReplayAny,
		PermAutomationTokensManageAny, PermQuotasManageAny,
		PermMcpToolsRead,
	} {
		if !Can("admin", p) {
			t.Fatalf("admin should have %s", p)
		}
	}
	for _, p := range []Permission{
		PermAIReadOwn, PermAIConfigureOwn, PermInspectorReadOwn,
		PermInspectorReplayOwn, PermAutomationTokensManageOwn,
		PermQuotasReadOwn, PermMtlsManageOwn, PermIPGeoManageOwn,
	} {
		if !Can("user", p) {
			t.Fatalf("user should have %s", p)
		}
	}
}

func TestBuiltinRoles(t *testing.T) {
	rs := Roles()
	if len(rs) != 2 {
		t.Fatalf("want 2 builtin roles, got %d", len(rs))
	}
	if _, ok := Get("admin"); !ok {
		t.Fatal("admin role missing")
	}
	if _, ok := Get("nope"); ok {
		t.Fatal("unknown role must not resolve")
	}
}

// TestCustomRoleCache covers SetRoles + Can() for non-builtin roles. After
// SetRoles publishes {"analyst": [ai:read:any]}, Can("analyst", PermAIReadAny)
// must return true; passing the empty map restores the deny-default. The
// builtin admin/user paths must remain unaffected throughout.
func TestCustomRoleCache(t *testing.T) {
	// Reset at end so other tests see a clean cache.
	defer SetRoles(nil)

	// Empty state: any custom role is unknown.
	if Can("analyst", PermAIReadAny) {
		t.Fatal("analyst must deny before SetRoles")
	}

	SetRoles(map[string][]Permission{
		"analyst": {PermAIReadAny, PermAuditRead},
	})
	if !Can("analyst", PermAIReadAny) {
		t.Fatal("analyst should hold ai:read:any after SetRoles")
	}
	if !Can("analyst", PermAuditRead) {
		t.Fatal("analyst should hold audit:read after SetRoles")
	}
	if Can("analyst", PermUsersManage) {
		t.Fatal("analyst must NOT hold users:manage (not in its set)")
	}

	// Builtin admin/user must still resolve from the hard-coded table.
	if !Can("admin", PermUsersManage) {
		t.Fatal("builtin admin path must be unaffected by SetRoles")
	}
	if Can("user", PermUsersManage) {
		t.Fatal("builtin user path must be unaffected by SetRoles")
	}

	// Attempting to overwrite a builtin via SetRoles is a silent no-op.
	SetRoles(map[string][]Permission{"admin": {}, "analyst": {PermAuditRead}})
	if !Can("admin", PermUsersManage) {
		t.Fatal("builtin admin must not be shadowed by SetRoles")
	}
	if Can("analyst", PermAIReadAny) {
		t.Fatal("analyst's perm set should have shrunk to {audit:read} only")
	}
	if !Can("analyst", PermAuditRead) {
		t.Fatal("analyst should still hold audit:read after refresh")
	}
}

func TestCanScopes(t *testing.T) {
	cases := []struct {
		role string
		p    Permission
		want bool
	}{
		{"admin", PermTunnelsManageAny, true},
		{"admin", PermUsersManage, true},
		{"admin", PermSettingsManage, true},
		{"admin", PermServicesConfigureAny, true},
		{"admin", PermTunnelsManageOwn, true},
		{"user", PermTunnelsReadOwn, true},
		{"user", PermTunnelsManageOwn, true},
		{"user", PermTokensManageOwn, true},
		{"user", PermServicesConfigureOwn, true},
		{"user", PermSessionsManageOwn, true},
		{"user", PermTunnelsManageAny, false},
		{"user", PermUsersManage, false},
		{"user", PermUsersRead, false},
		{"user", PermSettingsManage, false},
		{"user", PermServicesConfigureAny, false},
		{"nope", PermTunnelsReadOwn, false},
	}
	for _, c := range cases {
		if got := Can(c.role, c.p); got != c.want {
			t.Errorf("Can(%q,%q)=%v want %v", c.role, c.p, got, c.want)
		}
	}
}

// TestPermissionSetIsClosed is a golden-list guard: a permission is added on
// purpose, here and in AllPermissions, never by accident.
func TestPermissionSetIsClosed(t *testing.T) {
	// Do NOT replace the literal with AllPermissions() — that would be tautological.
	expected := []Permission{
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
		PermOAuthClientsManage,
	}
	got := AllPermissions()
	if !slices.Equal(got, expected) {
		t.Errorf("permissions were added, removed or reordered\n  got  (%d): %v\n  want (%d): %v",
			len(got), got, len(expected), expected)
	}
}

// The admin role holds every permission, and each exactly once: the gate of
// the API relies on it (a route that only an admin could call now asks for a
// permission, and the admin must still pass).
func TestAdminHoldsEveryPermission(t *testing.T) {
	admin, _ := Get("admin")
	seen := map[Permission]int{}
	for _, p := range admin.Permissions {
		seen[p]++
	}
	for _, p := range AllPermissions() {
		if seen[p] != 1 {
			t.Errorf("admin holds %s %d times, want once", p, seen[p])
		}
		if !Can("admin", p) {
			t.Errorf("Can(admin, %s) = false", p)
		}
	}
	if len(admin.Permissions) != len(AllPermissions()) {
		t.Errorf("admin holds %d permissions, AllPermissions has %d", len(admin.Permissions), len(AllPermissions()))
	}
}

// What only an admin could do before v0.10 stays out of the user role.
func TestUserRoleLacksTheAdminOnlyPermissions(t *testing.T) {
	for _, p := range []Permission{
		PermAIProvidersManage, PermBudgetsManage, PermCostPricingManage,
		PermMcpSourcesRead, PermMcpSourcesManage, PermOAuthClientsManage,
		PermUsersRead, PermUsersManage, PermRolesRead, PermRolesManage, PermSettingsManage,
		PermTunnelsReadAny, PermServicesConfigureAny, PermAIReadAny,
	} {
		if Can("user", p) {
			t.Errorf("the user role holds %s", p)
		}
	}
}
