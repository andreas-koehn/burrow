package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
)

type presetResp struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

func getPresets(t *testing.T, h *authzHarness, auth func(*http.Request)) (int, []presetResp) {
	t.Helper()
	rec := h.do(t, http.MethodGet, "/api/v1/automation/presets", nil, auth)
	var out []presetResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec.Code, out
}

func TestAutomationPresets(t *testing.T) {
	h := newAuthzHarness(t)

	t.Run("an admin gets every preset whole", func(t *testing.T) {
		code, ps := getPresets(t, h, h.session(t, "admin"))
		if code != http.StatusOK || len(ps) != 3 {
			t.Fatalf("got %d %+v", code, ps)
		}
		for i, want := range authz.Presets() {
			if ps[i].Name != want.Name || len(ps[i].Permissions) != len(want.Permissions) {
				t.Errorf("preset %d = %s with %d permissions, want %s with %d", i, ps[i].Name, len(ps[i].Permissions), want.Name, len(want.Permissions))
			}
		}
	})

	t.Run("a user gets each preset cut to the user role", func(t *testing.T) {
		code, ps := getPresets(t, h, h.session(t, "user"))
		if code != http.StatusOK {
			t.Fatalf("got %d", code)
		}
		for _, p := range ps {
			for _, perm := range p.Permissions {
				if !authz.Can("user", authz.Permission(perm)) {
					t.Errorf("%s offers %s to a user", p.Name, perm)
				}
			}
		}
		full := ps[2].Permissions
		if slices.Contains(full, string(authz.PermUsersManage)) || !slices.Contains(full, string(authz.PermAIReadOwn)) {
			t.Errorf("user's full preset = %v", full)
		}
	})

	t.Run("a token gets each preset cut to what it declares", func(t *testing.T) {
		declared := []string{string(authz.PermAutomationTokensManageOwn), string(authz.PermAIReadOwn)}
		code, ps := getPresets(t, h, h.token(t, "admin", declared...))
		if code != http.StatusOK {
			t.Fatalf("got %d", code)
		}
		for _, p := range ps {
			for _, perm := range p.Permissions {
				if !slices.Contains(declared, perm) {
					t.Errorf("%s offers %s to a token that does not declare it", p.Name, perm)
				}
			}
		}
	})

	t.Run("without the token permission there are no presets", func(t *testing.T) {
		if code, _ := getPresets(t, h, h.token(t, "admin", string(authz.PermAIReadAny))); code != http.StatusForbidden {
			t.Errorf("got %d, want 403", code)
		}
	})
}

func TestAutomationPresetsOfADemotedMinter(t *testing.T) {
	h := newAuthzHarness(t)
	token := h.token(t, "admin", string(authz.PermAutomationTokensManageOwn), string(authz.PermAIReadOwn), string(authz.PermUsersManage))
	if _, err := h.sqldb.Exec(`UPDATE users SET role='user' WHERE id='u-admin'`); err != nil {
		t.Fatal(err)
	}
	code, ps := getPresets(t, h, token)
	if code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	for _, p := range ps {
		if slices.Contains(p.Permissions, string(authz.PermUsersManage)) {
			t.Errorf("%s offers users:manage to a token whose user lost it", p.Name)
		}
	}
	if full := ps[2].Permissions; !slices.Contains(full, string(authz.PermAIReadOwn)) {
		t.Errorf("full = %v, want it to keep ai:read:own", full)
	}
}
