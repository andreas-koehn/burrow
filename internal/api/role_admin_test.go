package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// fakeRoleStore is the RoleStore fake for the v0.4.0 Task 15 handler tests.
// It mirrors *store.Store's external behaviour: builtin guard, single-default
// invariant, unknown-permission rejection, and cascade re-assign on delete.
type fakeRoleStore struct {
	mu    sync.Mutex
	rows  map[string]*fakeRoleRow
	users map[string]string // userID → role (used by DeleteRole cascade)
}

type fakeRoleRow struct {
	Description        string
	Permissions        []string
	Builtin            bool
	DefaultForNewUsers bool
	CreatedAt          time.Time
}

func newFakeRoleStore() *fakeRoleStore {
	rs := &fakeRoleStore{
		rows:  map[string]*fakeRoleRow{},
		users: map[string]string{},
	}
	now := time.Now().UTC()
	rs.rows["admin"] = &fakeRoleRow{Description: "Full admin", Builtin: true, CreatedAt: now}
	rs.rows["user"] = &fakeRoleRow{Description: "Standard user", Builtin: true, CreatedAt: now}
	return rs
}

func (f *fakeRoleStore) ListRoles(_ context.Context) ([]db.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.Role, 0, len(f.rows))
	for name, r := range f.rows {
		out = append(out, db.Role{
			Name:               name,
			Description:        r.Description,
			CreatedAt:          r.CreatedAt,
			Builtin:            r.Builtin,
			Permissions:        append([]string(nil), r.Permissions...),
			DefaultForNewUsers: r.DefaultForNewUsers,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeRoleStore) GetRole(_ context.Context, name string) (store.RoleDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[name]
	if !ok {
		return store.RoleDetail{}, db.ErrNotFound
	}
	perms := append([]string(nil), r.Permissions...)
	if r.Builtin && name == "admin" {
		// Mimic the real store: builtin admin's perms come from the authz
		// table, not the DB row. For tests we surface a fixed non-empty
		// set so the existing role_test.go assertions still hold.
		perms = []string{"users:manage"}
	}
	return store.RoleDetail{
		Name:               name,
		Description:        r.Description,
		CreatedAt:          r.CreatedAt,
		Permissions:        perms,
		Builtin:            r.Builtin,
		DefaultForNewUsers: r.DefaultForNewUsers,
	}, nil
}

func (f *fakeRoleStore) CreateRole(_ context.Context, name, description string, permissions []string, defaultForNewUsers bool) error {
	// Validate against the closed catalog (mirrors the real store).
	if err := validatePerms(permissions); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.rows[name]; exists {
		return store.ErrRoleExists
	}
	if defaultForNewUsers {
		for _, r := range f.rows {
			r.DefaultForNewUsers = false
		}
	}
	if permissions == nil {
		permissions = []string{}
	}
	f.rows[name] = &fakeRoleRow{
		Description:        description,
		Permissions:        permissions,
		DefaultForNewUsers: defaultForNewUsers,
		CreatedAt:          time.Now().UTC(),
	}
	return nil
}

func (f *fakeRoleStore) UpdateRole(_ context.Context, name string, u store.RoleUpdate) error {
	if u.Permissions != nil {
		if err := validatePerms(*u.Permissions); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[name]
	if !ok {
		return db.ErrNotFound
	}
	if r.Builtin {
		return store.ErrRoleBuiltin
	}
	if u.Description != nil {
		r.Description = *u.Description
	}
	if u.Permissions != nil {
		r.Permissions = append([]string(nil), (*u.Permissions)...)
	}
	if u.DefaultForNewUsers != nil {
		if *u.DefaultForNewUsers {
			for n, x := range f.rows {
				if n != name {
					x.DefaultForNewUsers = false
				}
			}
		}
		r.DefaultForNewUsers = *u.DefaultForNewUsers
	}
	return nil
}

func (f *fakeRoleStore) DeleteRole(_ context.Context, name string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[name]
	if !ok {
		return nil, db.ErrNotFound
	}
	if r.Builtin {
		return nil, store.ErrRoleBuiltin
	}
	// Find the cascade target: current default; else fall back to "user".
	fallback := "user"
	for n, x := range f.rows {
		if x.DefaultForNewUsers && n != name {
			fallback = n
		}
	}
	var affected []string
	for uid, role := range f.users {
		if role == name {
			affected = append(affected, uid)
			f.users[uid] = fallback
		}
	}
	sort.Strings(affected)
	delete(f.rows, name)
	return affected, nil
}

// validatePerms mirrors the real store's check so the fake stays in sync.
func validatePerms(perms []string) error {
	allowed := authz.AllPermissions()
	for _, k := range perms {
		ok := false
		for _, a := range allowed {
			if string(a) == k {
				ok = true
				break
			}
		}
		if !ok {
			return store.ErrUnknownPermission{Key: k}
		}
	}
	return nil
}

// --- Handler tests ---------------------------------------------------------

// TestRolePostHappyPath proves the POST analyst → 201 + roleDetailResp shape,
// and a subsequent GET /api/v1/roles surfaces the new row with builtin=false.
func TestRolePostHappyPath(t *testing.T) {
	rs := newFakeRoleStore()
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	body := createRoleReq{
		Name:               "analyst",
		Description:        "AI analyst",
		Permissions:        []string{"tunnels:read:any", "ai:read:any"},
		DefaultForNewUsers: false,
	}
	r := c.post(t, "/api/v1/roles", body)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/roles status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got roleDetailResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if got.Name != "analyst" || got.Builtin {
		t.Fatalf("unexpected create response: %+v", got)
	}
	if len(got.Permissions) != 2 {
		t.Fatalf("want 2 perms in response, got %v", got.Permissions)
	}

	// GET /api/v1/roles must list it with builtin=false.
	r = c.get(t, "/api/v1/roles")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", r.StatusCode)
	}
	var list []roleResp
	_ = json.NewDecoder(r.Body).Decode(&list)
	r.Body.Close()
	var found *roleResp
	for i := range list {
		if list[i].Name == "analyst" {
			found = &list[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("analyst missing from list: %+v", list)
	}
	if found.Builtin {
		t.Fatal("analyst must be builtin=false")
	}
}

// TestRolePutBuiltinRejected proves PUT /api/v1/roles/admin → 409 with the
// exact spec-mandated error body.
func TestRolePutBuiltinRejected(t *testing.T) {
	rs := newFakeRoleStore()
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	desc := "tampered"
	r := c.put(t, "/api/v1/roles/admin", updateRoleReq{Description: &desc})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("PUT admin status=%d want 409", r.StatusCode)
	}
	var resp map[string]string
	_ = json.NewDecoder(r.Body).Decode(&resp)
	r.Body.Close()
	if resp["error"] != "role is built-in" {
		t.Fatalf(`want {"error":"role is built-in"}, got %q`, resp["error"])
	}
}

// TestRolePutUnknownPermission — PUT with a stray perm key → 400 with the
// exact spec-mandated error body (key quoted verbatim).
func TestRolePutUnknownPermission(t *testing.T) {
	rs := newFakeRoleStore()
	_ = rs.CreateRole(context.Background(), "analyst", "",
		[]string{"ai:read:any"}, false)
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	bad := []string{"nope:perm"}
	r := c.put(t, "/api/v1/roles/analyst", updateRoleReq{Permissions: &bad})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s want 400", r.StatusCode, readBody(t, r))
	}
	var resp map[string]string
	_ = json.NewDecoder(r.Body).Decode(&resp)
	r.Body.Close()
	want := `unknown permission "nope:perm"`
	if resp["error"] != want {
		t.Fatalf("want %q, got %q", want, resp["error"])
	}
}

// TestRolePostUnknownPermission proves the same 400 shape on POST.
func TestRolePostUnknownPermission(t *testing.T) {
	rs := newFakeRoleStore()
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.post(t, "/api/v1/roles", createRoleReq{
		Name: "rogue", Permissions: []string{"ai:read:any", "also:bogus"},
	})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", r.StatusCode)
	}
	var resp map[string]string
	_ = json.NewDecoder(r.Body).Decode(&resp)
	r.Body.Close()
	if resp["error"] != `unknown permission "also:bogus"` {
		t.Fatalf("unexpected body: %s", resp["error"])
	}
}

// TestRoleDeleteCascade — DELETE re-assigns affected users to the default
// role in one (logical) operation and returns 204.
func TestRoleDeleteCascade(t *testing.T) {
	rs := newFakeRoleStore()
	_ = rs.CreateRole(context.Background(), "analyst", "",
		[]string{"ai:read:any"}, false)
	rs.users["u1"] = "analyst"
	rs.users["u2"] = "analyst"
	rs.users["u3"] = "user"

	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.delete(t, "/api/v1/roles/analyst")
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status=%d body=%s want 204", r.StatusCode, readBody(t, r))
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.users["u1"] != "user" || rs.users["u2"] != "user" {
		t.Fatalf("affected users not re-assigned: %v", rs.users)
	}
	if rs.users["u3"] != "user" {
		t.Fatal("uninvolved user must keep its role")
	}
	if _, ok := rs.rows["analyst"]; ok {
		t.Fatal("analyst row must be gone")
	}
}

// TestRoleDeleteBuiltin — DELETE /api/v1/roles/admin → 409.
func TestRoleDeleteBuiltin(t *testing.T) {
	rs := newFakeRoleStore()
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.delete(t, "/api/v1/roles/admin")
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("DELETE admin status=%d want 409", r.StatusCode)
	}
}

// TestRolePostDefaultClearsPrior — POSTing a second default_for_new_users
// role atomically clears the prior default (single tx).
func TestRolePostDefaultClearsPrior(t *testing.T) {
	rs := newFakeRoleStore()
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/roles", createRoleReq{
		Name: "first", DefaultForNewUsers: true,
		Permissions: []string{"ai:read:any"},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("first status=%d", r.StatusCode)
	}
	r.Body.Close()
	r = c.post(t, "/api/v1/roles", createRoleReq{
		Name: "second", DefaultForNewUsers: true,
		Permissions: []string{"ai:read:any"},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("second status=%d", r.StatusCode)
	}
	r.Body.Close()

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.rows["first"].DefaultForNewUsers {
		t.Fatal("first must lose default after second became default")
	}
	if !rs.rows["second"].DefaultForNewUsers {
		t.Fatal("second must be the current default")
	}
}

// TestRolePermissionsEndpoint — GET /api/v1/roles/permissions returns the
// closed catalog grouped for the matrix UI.
func TestRolePermissionsEndpoint(t *testing.T) {
	rs := newFakeRoleStore()
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "user"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/roles/permissions")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []permissionInfo
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(out) != len(authz.AllPermissions()) {
		t.Fatalf("want %d perm entries, got %d", len(authz.AllPermissions()), len(out))
	}
	// Sanity-check a couple of well-known entries.
	wantGroup := map[string]string{
		"tunnels:read:any": "Tunnels",
		"ai:read:any":      "AI",
		"audit:read":       "Audit",
		"roles:manage":     "Roles",
	}
	for _, p := range out {
		if g, ok := wantGroup[p.Key]; ok && p.Group != g {
			t.Errorf("%s: group=%q want %q", p.Key, p.Group, g)
		}
		if p.Key == "" || p.Group == "" || p.Description == "" {
			t.Errorf("entry incomplete: %+v", p)
		}
	}
}

// TestRoleNonAdminForbiddenWithoutRolesManage proves the non-admin caller
// without roles:manage gets 403 on every mutation.
func TestRoleNonAdminForbiddenWithoutRolesManage(t *testing.T) {
	defer authz.SetRoles(nil)
	rs := newFakeRoleStore()
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "user"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	// POST forbidden.
	r := c.post(t, "/api/v1/roles", createRoleReq{Name: "x"})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("POST as user status=%d want 403", r.StatusCode)
	}
	r.Body.Close()
	// PUT forbidden.
	desc := ""
	r = c.put(t, "/api/v1/roles/x", updateRoleReq{Description: &desc})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT as user status=%d want 403", r.StatusCode)
	}
	r.Body.Close()
	// DELETE forbidden.
	r = c.delete(t, "/api/v1/roles/x")
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("DELETE as user status=%d want 403", r.StatusCode)
	}
	r.Body.Close()

	// Now grant the caller's role roles:manage via the authz cache. The
	// fakeUserStore role is "user", but we can swap to a custom role to
	// prove the roles:manage gate honours the cache. Set the user's role
	// to "curator" and grant it roles:manage.
	authz.SetRoles(map[string][]authz.Permission{
		"curator": {authz.PermRolesManage, authz.PermAIReadAny},
	})
	d.Users = &fakeUserStore{role: "curator"}
	srv2 := httptest.NewServer(NewRouter(d))
	defer srv2.Close()
	c2 := authedClient(t, srv2)
	r = c2.post(t, "/api/v1/roles", createRoleReq{
		Name: "ok-role", Permissions: []string{"ai:read:any"},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST as curator status=%d body=%s want 201", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
}

// TestRolePostBadName — slug guard rejects names with whitespace/symbols.
func TestRolePostBadName(t *testing.T) {
	rs := newFakeRoleStore()
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	for _, bad := range []string{"", "Bad Name", "Trail-", "-lead", "UPPER", "white space"} {
		r := c.post(t, "/api/v1/roles", createRoleReq{Name: bad})
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: status=%d want 400", bad, r.StatusCode)
		}
		r.Body.Close()
	}
}

// TestRolePostDuplicate — duplicate name → 409.
func TestRolePostDuplicate(t *testing.T) {
	rs := newFakeRoleStore()
	_ = rs.CreateRole(context.Background(), "dup", "",
		[]string{"ai:read:any"}, false)
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.post(t, "/api/v1/roles", createRoleReq{
		Name: "dup", Permissions: []string{"ai:read:any"},
	})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("dup status=%d want 409", r.StatusCode)
	}
}

// TestRolePutNotFound — PUT on a missing role → 404.
func TestRolePutNotFound(t *testing.T) {
	rs := newFakeRoleStore()
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	desc := "x"
	r := c.put(t, "/api/v1/roles/ghost", updateRoleReq{Description: &desc})
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", r.StatusCode)
	}
}

// Sanity: the fake's wrap of ErrUnknownPermission is the same type the
// real store returns — guards against a future refactor breaking the
// matrix of error checks at the handler boundary.
func TestStoreErrUnknownPermissionIsValueType(t *testing.T) {
	var unk store.ErrUnknownPermission
	err := store.ErrUnknownPermission{Key: "x"}
	if !errors.As(err, &unk) || unk.Key != "x" {
		t.Fatalf("errors.As broke: unk=%+v", unk)
	}
}

// recordedRechecks is a ForwardRechecker that records whom it was asked about.
type recordedRechecks struct {
	mu       sync.Mutex
	users    []string
	services []string
}

func (r *recordedRechecks) RecheckForwards(_ context.Context, serviceID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services = append(r.services, serviceID)
	return 0
}

func (r *recordedRechecks) RecheckUserForwards(_ context.Context, userID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = append(r.users, userID)
	return 0
}

// A user the delete moved to the fallback role gets their live forwards
// checked again, as a role change through PATCH /users does; nobody else.
func TestRoleDelete_MovedUsersHaveTheirForwardsRechecked(t *testing.T) {
	rs := newFakeRoleStore()
	_ = rs.CreateRole(context.Background(), "analyst", "", []string{"ai:read:any"}, false)
	rs.users["u1"] = "analyst"
	rs.users["u2"] = "analyst"
	rs.users["u3"] = "user"
	rec := &recordedRechecks{}
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Roles: rs, Forwards: rec}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	if r := c.delete(t, "/api/v1/roles/analyst"); r.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status=%d body=%s want 204", r.StatusCode, readBody(t, r))
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if strings.Join(rec.users, ",") != "u1,u2" {
		t.Fatalf("rechecked users %v, want u1,u2", rec.users)
	}
}
