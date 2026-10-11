package store

import (
	"context"
	"errors"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// TestRoleCRUDHappyPath covers create → list → get → update → delete with
// the authz cache assertion threaded through: after CreateRole, Can() must
// recognise the new role's permissions; after DeleteRole, it must not.
func TestRoleCRUDHappyPath(t *testing.T) {
	defer authz.SetRoles(nil) // restore clean cache for parallel tests
	ctx := context.Background()
	s := testStore(t)

	if err := s.CreateRole(ctx, "analyst", "AI analyst",
		[]string{"tunnels:read:any", "ai:read:any"}, false); err != nil {
		t.Fatalf("create analyst: %v", err)
	}
	// Cache populated → Can() sees it.
	if !authz.Can("analyst", authz.PermAIReadAny) {
		t.Fatal("authz.Can(analyst, ai:read:any) must be true after CreateRole")
	}
	if !authz.Can("analyst", authz.PermTunnelsReadAny) {
		t.Fatal("authz.Can(analyst, tunnels:read:any) must be true after CreateRole")
	}

	// List includes analyst alongside the two builtins.
	rows, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 roles, got %d", len(rows))
	}

	// Detail surfaces the v0.4.0 fields.
	rd, err := s.GetRole(ctx, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if rd.Builtin {
		t.Fatal("analyst must not be Builtin")
	}
	if len(rd.Permissions) != 2 {
		t.Fatalf("want 2 perms, got %v", rd.Permissions)
	}

	// Patch the description and shrink the permission set.
	newDesc := "AI reviewer"
	newPerms := []string{"ai:read:any"}
	if err := s.UpdateRole(ctx, "analyst", RoleUpdate{
		Description: &newDesc,
		Permissions: &newPerms,
	}); err != nil {
		t.Fatalf("update analyst: %v", err)
	}
	// Cache reflects the shrink: tunnels:read:any no longer held.
	if authz.Can("analyst", authz.PermTunnelsReadAny) {
		t.Fatal("authz.Can(analyst, tunnels:read:any) must be false after shrink")
	}
	if !authz.Can("analyst", authz.PermAIReadAny) {
		t.Fatal("authz.Can(analyst, ai:read:any) must remain true")
	}

	// Delete clears the cache.
	if _, err := s.DeleteRole(ctx, "analyst"); err != nil {
		t.Fatalf("delete analyst: %v", err)
	}
	if authz.Can("analyst", authz.PermAIReadAny) {
		t.Fatal("authz.Can(analyst, *) must be false after DeleteRole")
	}
}

// TestRoleCreateRejectsBuiltinName ensures CreateRole cannot shadow admin/user
// (the DB enforces this via the PK UNIQUE on name; we surface it as
// ErrRoleExists which the handler maps to 409).
func TestRoleCreateRejectsBuiltinName(t *testing.T) {
	defer authz.SetRoles(nil)
	ctx := context.Background()
	s := testStore(t)
	if err := s.CreateRole(ctx, "admin", "fake", nil, false); !errors.Is(err, ErrRoleExists) {
		t.Fatalf("want ErrRoleExists, got %v", err)
	}
}

// An empty or blank role name is refused: nothing is stored.
func TestRoleCreateRejectsEmptyName(t *testing.T) {
	defer authz.SetRoles(nil)
	s := testStore(t)
	for _, name := range []string{"", " ", "\t\n"} {
		if err := s.CreateRole(context.Background(), name, "x", []string{"ai:read:own"}, false); !errors.Is(err, ErrInvalidRole) {
			t.Errorf("CreateRole(%q): %v, want ErrInvalidRole", name, err)
		}
	}
	if authz.Can("", authz.PermAIReadOwn) {
		t.Error("the empty role holds a permission")
	}
}

// TestRoleUpdateBuiltinRefused proves admin/user remain read-only.
func TestRoleUpdateBuiltinRefused(t *testing.T) {
	defer authz.SetRoles(nil)
	ctx := context.Background()
	s := testStore(t)
	desc := "tampered"
	if err := s.UpdateRole(ctx, "admin", RoleUpdate{Description: &desc}); !errors.Is(err, ErrRoleBuiltin) {
		t.Fatalf("want ErrRoleBuiltin, got %v", err)
	}
	if err := s.UpdateRole(ctx, "user", RoleUpdate{Description: &desc}); !errors.Is(err, ErrRoleBuiltin) {
		t.Fatalf("want ErrRoleBuiltin, got %v", err)
	}
	if _, err := s.DeleteRole(ctx, "user"); !errors.Is(err, ErrRoleBuiltin) {
		t.Fatalf("delete builtin: want ErrRoleBuiltin, got %v", err)
	}
}

// TestRoleUnknownPermission proves both Create and Update reject any key
// outside the closed authz catalog, and that the rejected key bubbles up in
// the error message so the handler can echo it back verbatim.
func TestRoleUnknownPermission(t *testing.T) {
	defer authz.SetRoles(nil)
	ctx := context.Background()
	s := testStore(t)

	err := s.CreateRole(ctx, "rogue", "", []string{"ai:read:any", "nope:perm"}, false)
	var unk ErrUnknownPermission
	if !errors.As(err, &unk) {
		t.Fatalf("want ErrUnknownPermission, got %v", err)
	}
	if unk.Key != "nope:perm" {
		t.Fatalf("want offending key 'nope:perm', got %q", unk.Key)
	}

	if err := s.CreateRole(ctx, "analyst", "", []string{"ai:read:any"}, false); err != nil {
		t.Fatal(err)
	}
	bad := []string{"also:bogus"}
	err = s.UpdateRole(ctx, "analyst", RoleUpdate{Permissions: &bad})
	if !errors.As(err, &unk) || unk.Key != "also:bogus" {
		t.Fatalf("UpdateRole: want ErrUnknownPermission{Key:also:bogus}, got %v", err)
	}
}

// TestRoleDefaultForNewUsersClearsPrior — POST/PUT with
// default_for_new_users=true atomically swap the default (single tx).
func TestRoleDefaultForNewUsersClearsPrior(t *testing.T) {
	defer authz.SetRoles(nil)
	ctx := context.Background()
	s := testStore(t)

	if err := s.CreateRole(ctx, "first", "", nil, true); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRole(ctx, "second", "", nil, true); err != nil {
		t.Fatal(err)
	}
	r1, _ := s.GetRole(ctx, "first")
	r2, _ := s.GetRole(ctx, "second")
	if r1.DefaultForNewUsers {
		t.Fatal("first must lose default after second became default")
	}
	if !r2.DefaultForNewUsers {
		t.Fatal("second must be the current default")
	}

	yes := true
	if err := s.UpdateRole(ctx, "first", RoleUpdate{DefaultForNewUsers: &yes}); err != nil {
		t.Fatal(err)
	}
	r1, _ = s.GetRole(ctx, "first")
	r2, _ = s.GetRole(ctx, "second")
	if !r1.DefaultForNewUsers || r2.DefaultForNewUsers {
		t.Fatalf("default did not swap: first=%v second=%v", r1.DefaultForNewUsers, r2.DefaultForNewUsers)
	}
}

// A custom role is assigned like a built-in one, and a role that a user holds
// or an access policy names is not deleted: the users stay where they are.
func TestCustomRoleAssignedAndKeptWhileInUse(t *testing.T) {
	defer authz.SetRoles(nil)
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRole(ctx, "analyst", "", []string{"ai:read:any"}, false); err != nil {
		t.Fatal(err)
	}

	// Created with it, and moved onto it.
	u1, err := s.CreateUser(ctx, "a1@x", "password1", "analyst")
	if err != nil || u1.Role != "analyst" {
		t.Fatalf("create with a custom role: %+v, %v", u1, err)
	}
	u2 := mustCreateUser(t, s, "a2@x", "user")
	if err := s.UpdateUserRole(ctx, u2.ID, "analyst"); err != nil {
		t.Fatalf("update to a custom role: %v", err)
	}
	if got, _ := s.GetUserByID(ctx, u2.ID); got.Role != "analyst" {
		t.Fatalf("stored role = %q", got.Role)
	}
	// A name that is no role is refused both ways.
	if _, err := s.CreateUser(ctx, "a3@x", "password1", "ghost"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("create with an unknown role: %v", err)
	}
	if err := s.UpdateUserRole(ctx, u2.ID, "ghost"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("update to an unknown role: %v", err)
	}

	// An access policy names it.
	owner := mustCreateUser(t, s, "owner@x", "user")
	svc := mustGetOrCreateService(t, s, owner.ID, "web", "http")
	if err := s.SetAccessPolicy(ctx, owner.ID, "user", svc.ID, []string{"analyst", "user"}); err != nil {
		t.Fatalf("policy with a custom role: %v", err)
	}
	if err := s.SetAccessPolicy(ctx, owner.ID, "user", svc.ID, []string{"ghost"}); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("policy with an unknown role: %v", err)
	}
	if users, policies, err := s.RoleUses(ctx, "analyst"); err != nil || users != 2 || policies != 1 {
		t.Fatalf("RoleUses = %d users, %d policies, %v; want 2, 1", users, policies, err)
	}

	// In use: the role stays, and so do its users.
	if _, err := s.DeleteRole(ctx, "analyst"); !errors.Is(err, ErrRoleInUse) {
		t.Fatalf("delete while users hold it: %v, want ErrRoleInUse", err)
	}
	if got, _ := s.GetUserByID(ctx, u1.ID); got.Role != "analyst" {
		t.Fatalf("the refused delete moved a user to %q", got.Role)
	}
	if !authz.Can("analyst", authz.PermAIReadAny) {
		t.Fatal("the refused delete dropped the role from authz")
	}
	for _, id := range []string{u1.ID, u2.ID} {
		if err := s.UpdateUserRole(ctx, id, "user"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteRole(ctx, "analyst"); !errors.Is(err, ErrRoleInUse) {
		t.Fatalf("delete while a policy names it: %v, want ErrRoleInUse", err)
	}
	if err := s.SetAccessPolicy(ctx, owner.ID, "user", svc.ID, []string{"user"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteRole(ctx, "analyst"); err != nil {
		t.Fatalf("delete an unused role: %v", err)
	}
	if _, err := s.GetRole(ctx, "analyst"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("analyst row must be gone, got %v", err)
	}
	// A built-in role answers as before, whoever holds it.
	if _, err := s.DeleteRole(ctx, "user"); !errors.Is(err, ErrRoleBuiltin) {
		t.Fatalf("delete a built-in role: %v, want ErrRoleBuiltin", err)
	}
}
