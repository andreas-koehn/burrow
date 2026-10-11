package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/store"
)

// A custom role is a role like the built-in ones: a user can hold it and a
// service's access policy can name it. A name that is no role is refused.
func TestCustomRoleCanBeAssigned(t *testing.T) {
	h := newAuthzHarness(t)
	admin := h.session(t, "admin")

	rec := h.do(t, http.MethodPost, "/api/v1/users",
		map[string]string{"email": "new@x", "password": "password1", "role": "cr-none"}, admin)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"role":"cr-none"`) {
		t.Fatalf("create with a custom role: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-user", map[string]string{"role": "cr-legacy"}, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("update to a custom role: %d %s", rec.Code, rec.Body)
	}
	var role string
	if err := h.sqldb.QueryRow(`SELECT role FROM users WHERE id='u-user'`).Scan(&role); err != nil || role != "cr-legacy" {
		t.Fatalf("stored role = %q, %v", role, err)
	}
	for _, body := range []map[string]string{
		{"email": "a@x", "password": "password1", "role": "ghost"},
	} {
		if rec := h.do(t, http.MethodPost, "/api/v1/users", body, admin); rec.Code != http.StatusBadRequest {
			t.Errorf("create with an unknown role: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-user", map[string]string{"role": "ghost"}, admin); rec.Code != http.StatusBadRequest {
		t.Errorf("update to an unknown role: %d %s", rec.Code, rec.Body)
	}

	policy := "/api/v1/services/" + authzServiceID + "/access-policy"
	if rec := h.do(t, http.MethodPut, policy, map[string][]string{"roles": {"user", "cr-none"}}, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("policy with a custom role: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(t, http.MethodGet, policy, nil, admin); !strings.Contains(rec.Body.String(), "cr-none") {
		t.Errorf("policy read back: %s", rec.Body)
	}
	if rec := h.do(t, http.MethodPut, policy, map[string][]string{"roles": {"ghost"}}, admin); rec.Code != http.StatusBadRequest {
		t.Errorf("policy with an unknown role: %d %s", rec.Code, rec.Body)
	}
}

// Nobody hands out a permission their own role does not hold: not through a
// role given to a user, not through the permissions written into a role.
// "cr-legacy" may manage users and roles, but it lacks the three permissions
// of v0.10.
func TestNobodyGrantsWhatTheirRoleLacks(t *testing.T) {
	h := newAuthzHarness(t)
	legacy := h.session(t, "cr-legacy")
	patch := func(role string) int {
		return h.do(t, http.MethodPatch, "/api/v1/users/u-cr-none", map[string]string{"role": role}, legacy).Code
	}
	if code := patch("admin"); code != http.StatusForbidden {
		t.Errorf("giving admin to a user without being one: %d, want 403", code)
	}
	if code := patch("user"); code != http.StatusNoContent {
		t.Errorf("giving a role that is no wider than one's own: %d, want 204", code)
	}
	if rec := h.do(t, http.MethodPost, "/api/v1/users",
		map[string]string{"email": "boss@x", "password": "password1", "role": "admin"}, legacy); rec.Code != http.StatusForbidden {
		t.Errorf("creating an admin without being one: %d, want 403", rec.Code)
	}
	// The same through an automation token.
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-cr-none", map[string]string{"role": "admin"},
		h.token(t, "cr-legacy", "users:manage")); rec.Code != http.StatusForbidden {
		t.Errorf("a token giving admin for a user who is none: %d, want 403", rec.Code)
	}
	// A token is held to what it declares, not to its minter's whole role: an
	// admin's token with users:manage only cannot hand out admin.
	narrow := h.token(t, "admin", "users:manage")
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-cr-none", map[string]string{"role": "admin"}, narrow); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "token") {
		t.Errorf("a narrow admin token updating to admin: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := h.do(t, http.MethodPost, "/api/v1/users",
		map[string]string{"email": "boss2@x", "password": "password1", "role": "admin"}, narrow); rec.Code != http.StatusForbidden {
		t.Errorf("a narrow admin token creating an admin: %d, want 403", rec.Code)
	}
	userPerms := []string{"users:manage"}
	if ar, ok := authz.Get("user"); ok {
		for _, p := range ar.Permissions {
			userPerms = append(userPerms, string(p))
		}
	}
	if rec := h.do(t, http.MethodPost, "/api/v1/users",
		map[string]string{"email": "plain@x", "password": "password1", "role": "user"}, h.token(t, "admin", userPerms...)); rec.Code != http.StatusCreated {
		t.Errorf("a token declaring the user role's permissions creating a user: %d %s, want 201", rec.Code, rec.Body)
	}
	// A token that declares every permission of admin may hand admin out.
	var all []string
	for _, p := range authz.AllPermissions() {
		if authz.Can("admin", p) {
			all = append(all, string(p))
		}
	}
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-cr-none", map[string]string{"role": "admin"},
		h.token(t, "admin", all...)); rec.Code != http.StatusNoContent {
		t.Errorf("a full admin token giving admin: %d %s, want 204", rec.Code, rec.Body)
	}
	// roles:manage alone does not write other permissions into a role.
	rolesOnly := h.token(t, "admin", "roles:manage")
	if rec := h.do(t, http.MethodPost, "/api/v1/roles",
		map[string]any{"name": "tok-made", "permissions": []string{"ai:read:any"}}, rolesOnly); rec.Code != http.StatusForbidden {
		t.Errorf("a roles:manage token creating a role with an undeclared permission: %d, want 403", rec.Code)
	}
	if rec := h.do(t, http.MethodPut, "/api/v1/roles/cr-none",
		map[string]any{"permissions": []string{"ai:read:any"}}, rolesOnly); rec.Code != http.StatusForbidden {
		t.Errorf("a roles:manage token widening a role: %d, want 403", rec.Code)
	}

	if rec := h.do(t, http.MethodPost, "/api/v1/roles",
		map[string]any{"name": "treasurer", "permissions": []string{"budgets:manage"}}, legacy); rec.Code != http.StatusForbidden {
		t.Errorf("a role with a permission the caller lacks: %d, want 403", rec.Code)
	}
	if rec := h.do(t, http.MethodPost, "/api/v1/roles",
		map[string]any{"name": "auditor", "permissions": []string{"audit:read"}}, legacy); rec.Code != http.StatusCreated {
		t.Errorf("a role within the caller's own permissions: %d %s, want 201", rec.Code, rec.Body)
	}
	// Widening one's own role is the same thing.
	if rec := h.do(t, http.MethodPut, "/api/v1/roles/cr-legacy",
		map[string]any{"permissions": append([]string{"ai:providers:manage"}, legacyPerms...)}, legacy); rec.Code != http.StatusForbidden {
		t.Errorf("widening one's own role: %d, want 403", rec.Code)
	}
	// An unknown key stays a 400 that names it.
	if rec := h.do(t, http.MethodPost, "/api/v1/roles",
		map[string]any{"name": "odd", "permissions": []string{"no:such:thing"}}, legacy); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), `unknown permission`) {
		t.Errorf("unknown permission key: %d %s", rec.Code, rec.Body)
	}
}

// A role that a user holds or an access policy names is not deleted.
func TestDeleteRoleInUseIsRefused(t *testing.T) {
	h := newAuthzHarness(t)
	admin := h.session(t, "admin")
	del := func(name string) (int, string) {
		rec := h.do(t, http.MethodDelete, "/api/v1/roles/"+name, nil, admin)
		return rec.Code, rec.Body.String()
	}
	if code, body := del("cr-none"); code != http.StatusConflict || !strings.Contains(body, "1 user(s)") {
		t.Fatalf("deleting a role a user holds: %d %s, want 409 naming the user count", code, body)
	}
	var role string
	if err := h.sqldb.QueryRow(`SELECT role FROM users WHERE id='u-cr-none'`).Scan(&role); err != nil || role != "cr-none" {
		t.Fatalf("the user was moved off the role: %q, %v", role, err)
	}
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-cr-none", map[string]string{"role": "user"}, admin); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code, rec.Body)
	}
	policy := "/api/v1/services/" + authzServiceID + "/access-policy"
	if rec := h.do(t, http.MethodPut, policy, map[string][]string{"roles": {"cr-none"}}, admin); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code, rec.Body)
	}
	if code, body := del("cr-none"); code != http.StatusConflict || !strings.Contains(body, "1 access policy(ies)") {
		t.Fatalf("deleting a role a policy names: %d %s, want 409 naming the policy count", code, body)
	}
	if rec := h.do(t, http.MethodPut, policy, map[string][]string{"roles": {}}, admin); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code, rec.Body)
	}
	if code, body := del("cr-none"); code != http.StatusNoContent {
		t.Fatalf("deleting an unused role: %d %s", code, body)
	}
	if code, _ := del("admin"); code != http.StatusConflict {
		t.Errorf("deleting a built-in role: %d, want 409", code)
	}
}

// failingBuiltinRoles is a RoleStore as on Postgres, where the built-in roles
// have no row.
type failingBuiltinRoles struct{ RoleStore }

func (f failingBuiltinRoles) GetRole(ctx context.Context, name string) (store.RoleDetail, error) {
	if name == "admin" || name == "user" {
		return store.RoleDetail{}, errors.New("no row")
	}
	return f.RoleStore.GetRole(ctx, name)
}

// A built-in role is resolved from authz, not from the roles table.
func TestAssigningABuiltInRoleNeedsNoRoleRow(t *testing.T) {
	h := newAuthzHarness(t, func(d *Deps) { d.Roles = failingBuiltinRoles{d.Roles} })
	rec := h.do(t, http.MethodPost, "/api/v1/users",
		map[string]string{"email": "p@x", "password": "password1", "role": "user"}, h.session(t, "admin"))
	if rec.Code != http.StatusCreated {
		t.Errorf("creating a user without a row for the role: %d %s, want 201", rec.Code, rec.Body)
	}
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-cr-none", map[string]string{"role": "admin"}, h.session(t, "cr-legacy")); rec.Code != http.StatusForbidden {
		t.Errorf("admin for a caller who lacks it: %d, want 403", rec.Code)
	}
}

// Whoever manages users must hold what the target's role holds: otherwise
// users:manage would let the caller demote, suspend or delete every admin.
func TestUserManagersAreHeldToTheTargetsRole(t *testing.T) {
	h := newAuthzHarness(t)
	ctx := context.Background()
	if err := h.st.CreateRole(ctx, "cr-users", "", []string{"users:manage", "users:read"}, false); err != nil {
		t.Fatal(err)
	}
	for id, role := range map[string]string{"u-cr-users": "cr-users", "u-admin2": "admin", "u-admin3": "admin"} {
		if _, err := h.sqldb.ExecContext(ctx, `INSERT INTO users(id, email, password_hash, role) VALUES(?,?,?,?)`,
			id, id+"@x", "x", role); err != nil {
			t.Fatal(err)
		}
	}
	managers := h.session(t, "cr-users")
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/api/v1/users/u-admin2", map[string]string{"role": "user"}},
		{http.MethodPatch, "/api/v1/users/u-admin2", map[string]string{"status": "suspended"}},
		{http.MethodDelete, "/api/v1/users/u-admin2", nil},
		{http.MethodPatch, "/api/v1/users/u-user", map[string]string{"status": "suspended"}},
	} {
		rec := h.do(t, c.method, c.path, c.body, managers)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "does not hold") {
			t.Errorf("%s %s by a users:manage role: %d %s, want 403", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// Only the target guard can refuse this one: the new role is the caller's
	// own, the target's current role (admin) is not.
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-admin2", map[string]string{"role": "cr-users"}, managers); rec.Code != http.StatusForbidden {
		t.Errorf("moving an admin onto the caller's own role: %d %s, want 403", rec.Code, rec.Body)
	}
	var role, status string
	if err := h.sqldb.QueryRow(`SELECT role, status FROM users WHERE id='u-admin2'`).Scan(&role, &status); err != nil || role != "admin" || status != "active" {
		t.Errorf("the refused calls changed the admin: %q %q %v", role, status, err)
	}
	// A target whose role the caller holds is still managed.
	legacy := h.session(t, "cr-legacy")
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-cr-none", map[string]string{"status": "suspended"}, legacy); rec.Code != http.StatusNoContent {
		t.Errorf("managing a user whose role is within one's own: %d %s, want 204", rec.Code, rec.Body)
	}
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/nobody", map[string]string{"status": "suspended"}, managers); rec.Code != http.StatusNotFound {
		t.Errorf("a target that does not exist: %d, want 404", rec.Code)
	}
	// An admin's token that declares users:manage only is refused for an admin.
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-admin2", map[string]string{"status": "suspended"},
		h.token(t, "admin", "users:manage")); rec.Code != http.StatusForbidden {
		t.Errorf("a narrow admin token suspending an admin: %d, want 403", rec.Code)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/users/u-admin2", nil, h.token(t, "admin", "users:manage")); rec.Code != http.StatusForbidden {
		t.Errorf("a narrow admin token deleting an admin: %d, want 403", rec.Code)
	}
	// An admin session is unaffected.
	admin := h.session(t, "admin")
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-admin2", map[string]string{"role": "user"}, admin); rec.Code != http.StatusNoContent {
		t.Errorf("admin demoting an admin: %d %s, want 204", rec.Code, rec.Body)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/users/u-admin3", nil, admin); rec.Code != http.StatusNoContent {
		t.Errorf("admin deleting an admin: %d %s, want 204", rec.Code, rec.Body)
	}
}

// Changing or deleting a role takes the permissions the role holds now, or
// "PUT permissions: []" would empty the guard on its users.
func TestChangingARoleNeedsWhatItHoldsNow(t *testing.T) {
	h := newAuthzHarness(t)
	ctx := context.Background()
	for name, perms := range map[string][]string{
		"cr-mgr": {"roles:manage", "users:manage", "users:read"},
		"small":  {"users:read"},
		"small2": {"users:read"},
	} {
		if err := h.st.CreateRole(ctx, name, "", perms, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.sqldb.ExecContext(ctx, `INSERT INTO users(id, email, password_hash, role) VALUES('u-cr-mgr','m@x','x','cr-mgr')`); err != nil {
		t.Fatal(err)
	}
	mgr := h.session(t, "cr-mgr")

	if rec := h.do(t, http.MethodPut, "/api/v1/roles/cr-legacy", map[string]any{"permissions": []string{}}, mgr); rec.Code != http.StatusForbidden {
		t.Errorf("emptying a role the caller does not fully hold: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/users/u-cr-legacy", nil, mgr); rec.Code != http.StatusForbidden {
		t.Errorf("deleting a holder of that role: %d, want 403", rec.Code)
	}
	var n int
	if err := h.sqldb.QueryRow(`SELECT COUNT(*) FROM users WHERE id='u-cr-legacy'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("the user is gone: %d %v", n, err)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/roles/cr-legacy", nil, mgr); rec.Code != http.StatusForbidden {
		t.Errorf("deleting a role the caller does not fully hold: %d, want 403", rec.Code)
	}
	if rec := h.do(t, http.MethodPut, "/api/v1/roles/small", map[string]any{"permissions": []string{}}, mgr); rec.Code != http.StatusNoContent {
		t.Errorf("editing a role within one's own: %d %s, want 204", rec.Code, rec.Body)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/roles/small", nil, mgr); rec.Code != http.StatusNoContent {
		t.Errorf("deleting a role within one's own: %d %s, want 204", rec.Code, rec.Body)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/roles/ghost", nil, mgr); rec.Code != http.StatusNotFound {
		t.Errorf("deleting a role that does not exist: %d, want 404", rec.Code)
	}

	admin := h.session(t, "admin")
	if rec := h.do(t, http.MethodPut, "/api/v1/roles/cr-legacy", map[string]any{"description": "d"}, admin); rec.Code != http.StatusNoContent {
		t.Errorf("admin editing a role: %d %s, want 204", rec.Code, rec.Body)
	}
	// An admin's token that declares roles:manage only does not reach a role
	// that holds more.
	tok := h.token(t, "admin", "roles:manage")
	if rec := h.do(t, http.MethodPut, "/api/v1/roles/small2", map[string]any{"description": "d"}, tok); rec.Code != http.StatusForbidden {
		t.Errorf("a narrow token editing a role: %d, want 403", rec.Code)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/roles/small2", nil, tok); rec.Code != http.StatusForbidden {
		t.Errorf("a narrow token deleting a role: %d, want 403", rec.Code)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/roles/small2", nil, admin); rec.Code != http.StatusNoContent {
		t.Errorf("admin deleting a role: %d %s, want 204", rec.Code, rec.Body)
	}
}
