package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/ankoehn/burrow/internal/guardrails"
)

// GuardrailSettingsStore is the read/write surface the guardrail handlers
// use to persist global settings. The concrete *store.Store satisfies it
// via GetSettings/SaveSettings (same pattern as cache + redaction); one
// row in the settings table under guardrailsGlobalKey holds the JSON blob.
//
// Per-service overrides live in service_ai_config.config.guardrails (a
// sub-object of the same JSON blob the cache and redaction handlers
// already read via CacheServices). We reuse CacheServiceLookup here
// rather than introduce a new surface — Task 24 wires the typed surface
// across all three subsystems and the per_service list this handler
// renders is read-only for v0.4.0 Task 6.
type GuardrailSettingsStore interface {
	GetSettings(ctx context.Context) (map[string]string, error)
	SaveSettings(ctx context.Context, kv map[string]string) error
}

// guardrailsGlobalKey is the row key in the settings table where the
// global guardrails JSON blob is persisted. One row, one JSON value —
// same pattern as cacheSettingsKey / redactionGlobalKey.
const guardrailsGlobalKey = "guardrails.global"

// guardrailSettingsResp is the wire shape of GET /api/v1/guardrails/settings
// (spec Part B.5). Global is the persisted global blob; per_service is one
// row per service that has an AI-config row, with the override fields
// populated only when the service explicitly overrides global. The plan's
// per_service entry uses optional enabled / action — we emit them as
// pointers so missing values render as JSON `null` rather than the type
// zero-value (which would be ambiguous between "explicitly false" and
// "no override").
type guardrailSettingsResp struct {
	Global     guardrails.Settings           `json:"global"`
	PerService []guardrailPerServiceSettings `json:"per_service"`
}

// guardrailPerServiceSettings is one row of the per_service list. ServiceID
// is always present; Enabled and Action are pointers so a service that
// doesn't override the global blob renders as
// {"service_id":"…","enabled":null,"action":null}. The plan permits this
// optional-field shape ("[{service_id, enabled?, action?}]"); pointers
// give us null-on-omit without a custom MarshalJSON.
type guardrailPerServiceSettings struct {
	ServiceID string  `json:"service_id"`
	Enabled   *bool   `json:"enabled"`
	Action    *string `json:"action"`
}

// guardrailPatternResp is one entry of GET /api/v1/guardrails/patterns. Only
// the stable ID and a human description are exposed. Regex sources are
// deliberately withheld — disclosure of the source would let an attacker
// craft trivial evasions. The same field set is what the UI heatmap and
// the audit log key on.
type guardrailPatternResp struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// loadGuardrailsGlobal decodes the global guardrails blob from the settings
// map, falling back to DefaultSettings on missing/corrupt JSON. Matches
// loadRedactionGlobal's behavior so a malformed row doesn't take the API
// down — settings corruption is a maintainer concern, not a hot-path bug.
func loadGuardrailsGlobal(m map[string]string) guardrails.Settings {
	raw := m[guardrailsGlobalKey]
	if raw == "" {
		return guardrails.DefaultSettings
	}
	var s guardrails.Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return guardrails.DefaultSettings
	}
	if !guardrails.ValidActions[s.Action] {
		// Persisted-but-invalid action: serve defaults rather than echo
		// garbage to the UI. A subsequent PUT with a valid action will
		// repair the row.
		return guardrails.DefaultSettings
	}
	return s
}

// perServiceGuardrailsFromConfig decodes the .guardrails sub-object out of
// a service_ai_config.config JSON blob. Returns (settings, true) when the
// blob has a non-null guardrails block (i.e. the service overrides global);
// (DefaultSettings, false) otherwise. Same shape as
// perServiceCacheFromConfig / perServiceRedactionFromConfig.
func perServiceGuardrailsFromConfig(blob []byte) (guardrails.Settings, bool) {
	if len(blob) == 0 {
		return guardrails.DefaultSettings, false
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(blob, &outer); err != nil {
		return guardrails.DefaultSettings, false
	}
	raw, ok := outer["guardrails"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return guardrails.DefaultSettings, false
	}
	var s guardrails.Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return guardrails.DefaultSettings, false
	}
	return s, true
}

// GetGuardrailSettings handles GET /api/v1/guardrails/settings.
// The route needs ai:read:own or :any, or ai:configure:own or :any (routeAccess). Returns the
// global blob plus a per_service list synthesized from every row in
// service_ai_config (reusing the CacheServiceLookup surface to avoid
// duplicating the listing query). When a service has no .guardrails
// override the enabled/action fields render as JSON null.
func (d Deps) GetGuardrailSettings(w http.ResponseWriter, r *http.Request) {
	resp := guardrailSettingsResp{
		Global:     guardrails.DefaultSettings,
		PerService: []guardrailPerServiceSettings{},
	}
	if d.Settings != nil {
		m, err := d.Settings.GetSettings(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "get guardrail settings failed")
			return
		}
		resp.Global = loadGuardrailsGlobal(m)
	}
	if d.CacheServices != nil {
		rows, err := d.CacheServices.ListAllServiceAIConfigs(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "list service ai configs failed")
			return
		}
		for _, row := range rows {
			perSvc, override := perServiceGuardrailsFromConfig(row.Config)
			entry := guardrailPerServiceSettings{ServiceID: row.ServiceID}
			if override {
				// Take pointers to fresh locals so each row's pointer
				// addresses the per-iteration value (range loop variable
				// is reused — see Go FAQ).
				enabled := perSvc.Enabled
				action := perSvc.Action
				entry.Enabled = &enabled
				entry.Action = &action
			}
			resp.PerService = append(resp.PerService, entry)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// PutGuardrailSettings handles PUT /api/v1/guardrails/settings.
// Needs ai:configure:any (routeAccess in access.go). 400 on invalid action;
// 204 on success. The persisted blob
// is canonicalized (re-marshaled from the typed struct) so extra/unknown
// fields don't survive the round-trip.
func (d Deps) PutGuardrailSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var s guardrails.Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid setting")
		return
	}
	if !guardrails.ValidActions[s.Action] {
		writeErr(w, http.StatusBadRequest, "invalid action")
		return
	}
	if d.Settings == nil {
		writeErr(w, http.StatusInternalServerError, "settings store unavailable")
		return
	}
	canon, err := json.Marshal(s)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal failed")
		return
	}
	if err := d.Settings.SaveSettings(r.Context(), map[string]string{
		guardrailsGlobalKey: string(canon),
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "save guardrail settings failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetGuardrailPatterns handles GET /api/v1/guardrails/patterns.
// The route needs ai:read:own or :any, or ai:configure:own or :any (routeAccess). Returns the closed
// bundled list as [{id, description}] — regex sources are deliberately
// NOT exposed (spec Part B.5: "regex source NOT exposed publicly").
//
// Response order matches guardrails.Patterns iteration order, which is
// the engine's match-priority order — handy for a UI that wants to
// indicate "this pattern fires before that one" without an extra field.
func (d Deps) GetGuardrailPatterns(w http.ResponseWriter, r *http.Request) {
	out := make([]guardrailPatternResp, 0, len(guardrails.Patterns))
	for _, p := range guardrails.Patterns {
		out = append(out, guardrailPatternResp{ID: p.ID, Description: p.Description})
	}
	writeJSON(w, http.StatusOK, out)
}
