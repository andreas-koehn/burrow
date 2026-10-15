package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
)

func getMe(t *testing.T, h *authzHarness, auth func(*http.Request)) (role string, perms []string) {
	t.Helper()
	rec := h.do(t, http.MethodGet, "/api/v1/me", nil, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("/me: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Role        string   `json:"role"`
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Permissions == nil {
		t.Fatal("/me has no permissions field")
	}
	return body.Role, body.Permissions
}

func TestMePermissions(t *testing.T) {
	h := newAuthzHarness(t)

	_, admin := getMe(t, h, h.session(t, "admin"))
	if len(admin) != len(authz.AllPermissions()) {
		t.Errorf("admin session has %d permissions, want %d", len(admin), len(authz.AllPermissions()))
	}

	_, user := getMe(t, h, h.session(t, "user"))
	for _, p := range authz.AllPermissions() {
		if slices.Contains(user, string(p)) != authz.Can("user", p) {
			t.Errorf("user session: %s listed = %v", p, slices.Contains(user, string(p)))
		}
	}

	// A token: only what it declares (a token cannot declare more than its
	// user's role holds), in AllPermissions order.
	_, tok := getMe(t, h, h.token(t, "user", string(authz.PermAutomationTokensManageOwn), string(authz.PermAIReadOwn)))
	want := []string{string(authz.PermAIReadOwn), string(authz.PermAutomationTokensManageOwn)}
	if !slices.Equal(tok, want) {
		t.Errorf("token permissions = %v, want %v", tok, want)
	}

	// A custom role is read from the database.
	ctx := context.Background()
	if err := h.st.CreateRole(ctx, "cr-audit", "audit only", []string{"audit:read"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sqldb.ExecContext(ctx,
		`INSERT INTO users(id, email, password_hash, role) VALUES('u-cr-audit','cr-audit@x','x','cr-audit')`); err != nil {
		t.Fatal(err)
	}
	role, custom := getMe(t, h, h.session(t, "cr-audit"))
	if role != "cr-audit" || !slices.Equal(custom, []string{"audit:read"}) {
		t.Errorf("custom role session: role %q permissions %v, want cr-audit [audit:read]", role, custom)
	}
}

// A token holds what it declares AND what its user's role still grants: after
// the minter is demoted, /me no longer lists what the old role gave.
func TestMePermissionsOfADemotedMinter(t *testing.T) {
	h := newAuthzHarness(t)
	token := h.token(t, "admin", string(authz.PermAIReadOwn), string(authz.PermUsersManage))
	if _, err := h.sqldb.Exec(`UPDATE users SET role='user' WHERE id='u-admin'`); err != nil {
		t.Fatal(err)
	}
	_, perms := getMe(t, h, token)
	if !slices.Equal(perms, []string{string(authz.PermAIReadOwn)}) {
		t.Errorf("token permissions = %v, want [ai:read:own]", perms)
	}
}
