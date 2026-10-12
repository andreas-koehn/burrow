package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
)

// mint posts the raw JSON body to the mint endpoint and returns the status,
// the permissions the new token declares and its credentials.
func (h *authzHarness) mint(t *testing.T, body string, auth func(*http.Request)) (int, []string, func(*http.Request)) {
	t.Helper()
	rec := h.do(t, http.MethodPost, "/api/v1/automation/tokens", json.RawMessage(body), auth)
	var out struct {
		Token struct {
			Permissions []string `json:"permissions"`
		} `json:"token"`
		Plaintext string `json:"plaintext"`
	}
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	plain := out.Plaintext
	return rec.Code, out.Token.Permissions, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+plain) }
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func permStrings(ps []authz.Permission) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, string(p))
	}
	return out
}

// The dashboard dialog posts no permissions and promises the token inherits
// the user's: it declares what the caller's role holds now.
func TestMintWithoutPermissionsInheritsTheSessionsRole(t *testing.T) {
	h := newAuthzHarness(t)
	users := apiRoute{http.MethodGet, "/api/v1/users"}

	code, perms, tok := h.mint(t, `{"name":"d"}`, h.session(t, "admin"))
	if code != http.StatusCreated {
		t.Fatalf("admin: %d", code)
	}
	if !sameSet(perms, permStrings(authz.AllPermissions())) {
		t.Errorf("admin token declares %v, want every permission", perms)
	}
	if rec := h.call(t, users, tok); rec.Code != http.StatusOK {
		t.Errorf("admin token on users: %d, want 200", rec.Code)
	}

	ur, _ := authz.Get("user")
	code, perms, tok = h.mint(t, `{"name":"d"}`, h.session(t, "user"))
	if code != http.StatusCreated {
		t.Fatalf("user: %d", code)
	}
	if !sameSet(perms, permStrings(ur.Permissions)) {
		t.Errorf("user token declares %v, want the user role's set", perms)
	}
	if rec := h.call(t, users, tok); rec.Code != http.StatusForbidden {
		t.Errorf("user token on users: %d, want 403", rec.Code)
	}

	code, perms, _ = h.mint(t, `{"name":"d"}`, h.session(t, "cr-legacy"))
	if code != http.StatusCreated || !sameSet(perms, legacyPerms) {
		t.Errorf("custom role: %d %v, want its stored set", code, perms)
	}

	// null is absent; [] is an explicit empty list.
	code, perms, _ = h.mint(t, `{"name":"d","permissions":null}`, h.session(t, "user"))
	if code != http.StatusCreated || !sameSet(perms, permStrings(ur.Permissions)) {
		t.Errorf("null: %d %v, want inherited", code, perms)
	}
	code, perms, tok = h.mint(t, `{"name":"d","permissions":[]}`, h.session(t, "admin"))
	if code != http.StatusCreated || len(perms) != 0 {
		t.Errorf("[]: %d %v, want nothing", code, perms)
	}
	if rec := h.call(t, apiRoute{http.MethodGet, "/api/v1/tunnels"}, tok); rec.Code != http.StatusForbidden {
		t.Errorf("empty token on tunnels: %d, want 403", rec.Code)
	}
}

func TestMintWithoutPermissionsFromATokenInheritsItsEffectivePermissions(t *testing.T) {
	h := newAuthzHarness(t)
	parent := h.token(t, "admin", "automation:tokens:manage:own", "users:manage", "tunnels:read:own")
	code, perms, _ := h.mint(t, `{"name":"c"}`, parent)
	if code != http.StatusCreated {
		t.Fatalf("child: %d", code)
	}
	if !sameSet(perms, []string{"automation:tokens:manage:own", "users:manage", "tunnels:read:own"}) {
		t.Errorf("child declares %v, want exactly the parent's", perms)
	}

	// The minter is demoted: only what the role still grants is handed down.
	if _, err := h.sqldb.ExecContext(context.Background(), `UPDATE users SET role='user' WHERE id='u-admin'`); err != nil {
		t.Fatal(err)
	}
	code, perms, _ = h.mint(t, `{"name":"c"}`, parent)
	if code != http.StatusCreated {
		t.Fatalf("child after demotion: %d", code)
	}
	if !sameSet(perms, []string{"automation:tokens:manage:own", "tunnels:read:own"}) {
		t.Errorf("child after demotion declares %v, want what the user role still grants", perms)
	}
}

func TestMintStillRefusesUnknownFields(t *testing.T) {
	h := newAuthzHarness(t)
	if code, _, _ := h.mint(t, `{"name":"d","bogus":1}`, h.session(t, "admin")); code != http.StatusBadRequest {
		t.Errorf("unknown field: %d, want 400", code)
	}
}

// The events stream is the refresh channel of every page a role may open.
func TestEventsOpenToEveryReadingDashboardRole(t *testing.T) {
	h := newAuthzHarness(t)
	ctx := context.Background()
	if err := h.st.CreateRole(ctx, "cr-airead", "reads own ai", []string{"ai:read:own"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sqldb.ExecContext(ctx,
		`INSERT INTO users(id, email, password_hash, role) VALUES('u-cr-airead','cr@x','x','cr-airead')`); err != nil {
		t.Fatal(err)
	}
	events := apiRoute{http.MethodGet, "/api/v1/events"}
	if rec := h.call(t, events, h.session(t, "cr-airead")); rec.Code == http.StatusForbidden {
		t.Errorf("events for ai:read:own: 403, want it to pass")
	}
	if rec := h.call(t, events, h.session(t, "cr-none")); rec.Code != http.StatusForbidden {
		t.Errorf("events for cr-none: %d, want 403", rec.Code)
	}
	if rec := h.call(t, events, h.token(t, "admin", "tunnels:read:any")); rec.Code != http.StatusForbidden {
		t.Errorf("events for a token: %d, want 403", rec.Code)
	}
}
