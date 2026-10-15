package api

import (
	"net/http"

	"github.com/ankoehn/burrow/internal/authz"
)

type automationPresetResp struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

// GetAutomationPresets handles GET /api/v1/automation/presets: the presets a
// token can be made from, each cut to what the caller holds. A preset is a
// starting point for the dialog; the token stores the list.
func (d Deps) GetAutomationPresets(w http.ResponseWriter, r *http.Request) {
	role, err := d.callerRoleForAuth(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	presets := authz.Presets()
	out := make([]automationPresetResp, 0, len(presets))
	for _, p := range presets {
		held := []string{}
		for _, perm := range p.Permissions {
			if effectivePerms(r.Context(), role, perm) {
				held = append(held, string(perm))
			}
		}
		out = append(out, automationPresetResp{Name: p.Name, Description: p.Description, Permissions: held})
	}
	writeJSON(w, http.StatusOK, out)
}
