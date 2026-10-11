package api

import (
	"net/http"
	"strings"
	"testing"
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
	// The same through an automation token: what the token declares does not
	// lift what its user's role lacks.
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-cr-none", map[string]string{"role": "admin"},
		h.token(t, "cr-legacy", "users:manage")); rec.Code != http.StatusForbidden {
		t.Errorf("a token giving admin for a user who is none: %d, want 403", rec.Code)
	}
	// An admin's token that declares users:manage gives any role.
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-cr-none", map[string]string{"role": "admin"},
		h.token(t, "admin", "users:manage")); rec.Code != http.StatusNoContent {
		t.Errorf("an admin's token giving admin: %d %s, want 204", rec.Code, rec.Body)
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
