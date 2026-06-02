package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/redact"
)

// RedactionSettingsStore is the read/write surface the redaction handlers
// use to persist global settings + the custom-rules array. The concrete
// *store.Store satisfies it via the same map[string]string settings table
// used by the cache handlers (Task 4). Two keys are used:
//
//   - "redaction.global"        — the global RedactionSettings JSON blob.
//   - "redaction.custom_rules"  — the custom rules array, JSON-encoded.
//
// Per-service redaction overrides live in service_ai_config.config under
// the .redaction sub-object; Task 24 wires the typed surface. For v0.4.0
// Task 5 the per_service list is rendered from CacheServices (see below).
type RedactionSettingsStore interface {
	GetSettings(ctx context.Context) (map[string]string, error)
	SaveSettings(ctx context.Context, kv map[string]string) error
}

// redactionGlobalKey / redactionCustomRulesKey are the settings row keys
// for the global redaction blob and the custom-rules array, respectively.
// One row, one JSON value — same pattern as cacheSettingsKey.
const (
	redactionGlobalKey      = "redaction.global"
	redactionCustomRulesKey = "redaction.custom_rules"
)

// redactionGlobalJSON is the wire shape of global redaction settings.
// Mirrors the spec's RedactionSettings (web/src/lib/contract.ts:150-156).
// PresidioURL is omitted when empty so the wire JSON matches the
// `presidio_url?: string` optional in the contract.
type redactionGlobalJSON struct {
	Enabled           bool     `json:"enabled"`
	RedactForLogsOnly bool     `json:"redact_for_logs_only"`
	RuleIDs           []string `json:"rule_ids"`
	PresidioEnabled   bool     `json:"presidio_enabled"`
	PresidioURL       string   `json:"presidio_url,omitempty"`
}

// defaultRedactionGlobal is the v0.4.0 default applied when no row exists:
// disabled, log-only off, no rule selection (which means "all built-ins"),
// presidio off. Matches Part B.7 default-fill on RedactionSettings.
var defaultRedactionGlobal = redactionGlobalJSON{
	Enabled:           false,
	RedactForLogsOnly: false,
	RuleIDs:           []string{},
	PresidioEnabled:   false,
}

// redactionRulesResp is the GET /redaction/rules response shape.
type redactionRulesResp struct {
	BuiltIn []redact.Rule `json:"built_in"`
	Custom  []redact.Rule `json:"custom"`
}

// redactionSettingsResp is the GET /redaction/settings response shape.
// PerService is always a non-nil slice (possibly empty); the wire JSON
// renders as [] rather than null for UI ergonomics.
type redactionSettingsResp struct {
	Global     redactionGlobalJSON           `json:"global"`
	PerService []redactionPerServiceSettings `json:"per_service"`
}

// redactionPerServiceSettings is one row of the per_service array. Same
// shape as the global blob plus the override flag (true when the
// per-service AI config has a non-empty .redaction sub-object).
type redactionPerServiceSettings struct {
	ServiceID         string   `json:"service_id"`
	Enabled           bool     `json:"enabled"`
	RedactForLogsOnly bool     `json:"redact_for_logs_only"`
	RuleIDs           []string `json:"rule_ids"`
	PresidioEnabled   bool     `json:"presidio_enabled"`
	PresidioURL       string   `json:"presidio_url,omitempty"`
	Override          bool     `json:"override"`
}

// redactionPreviewReq is the POST /redaction/preview request body. We accept
// the raw body + an optional scope so previews can be evaluated against
// the same scope a live request would see; defaults to "request_body".
type redactionPreviewReq struct {
	Body  string `json:"body"`
	Scope string `json:"scope"` // request_body | response_body | both
}

// redactionPreviewResp is the response shape for POST /redaction/preview.
// matches is always a non-nil slice (possibly empty); a UI that lights up
// matches on a heatmap is the v0.4.0 consumer.
type redactionPreviewResp struct {
	Matches []redactionPreviewMatch `json:"matches"`
}

// redactionPreviewMatch is one regex hit found by the preview engine.
// start/end are byte offsets into the input body (0-indexed, half-open),
// value is the matched substring verbatim.
type redactionPreviewMatch struct {
	Rule  string `json:"rule"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Value string `json:"value"`
}

// redactionRuleReq is the POST/PUT request body. id is server-assigned on
// POST; on PUT the URL param wins and any "id" field in the body is
// ignored.
type redactionRuleReq struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Action  string `json:"action"`
	Scope   string `json:"scope"`
}

// validateRuleReq checks the rule against the closed action/scope enums
// and compiles the regex (so an invalid pattern is caught before persist).
// Returns the cleaned Rule (name trimmed) on success.
func validateRuleReq(in redactionRuleReq) (redact.Rule, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return redact.Rule{}, errors.New("name is required")
	}
	if in.Pattern == "" {
		return redact.Rule{}, errors.New("pattern is required")
	}
	if _, err := regexp.Compile(in.Pattern); err != nil {
		return redact.Rule{}, errors.New("invalid regex")
	}
	switch in.Action {
	case redact.ActionMask, redact.ActionDrop, redact.ActionHash:
	default:
		return redact.Rule{}, errors.New("invalid action")
	}
	switch in.Scope {
	case redact.ScopeRequestBody, redact.ScopeResponseBody, redact.ScopeBoth:
	default:
		return redact.Rule{}, errors.New("invalid scope")
	}
	return redact.Rule{
		Name:    name,
		Pattern: in.Pattern,
		Action:  in.Action,
		Scope:   in.Scope,
	}, nil
}

// loadCustomRules reads the custom rules array out of the settings table.
// Returns an empty slice when no row exists or the row is malformed
// (rather than failing the request — corruption shouldn't take the API
// down). The map is the result of GetSettings; we use the typed wrapper
// to avoid two roundtrips per handler.
func loadCustomRules(m map[string]string) []redact.Rule {
	raw := m[redactionCustomRulesKey]
	if raw == "" {
		return []redact.Rule{}
	}
	var rules []redact.Rule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return []redact.Rule{}
	}
	return rules
}

// saveCustomRules persists the custom rules array under the redaction
// custom_rules key. Marshals with empty JSON-array semantics ("[]" not
// "null") so the GET response is stable.
func saveCustomRules(ctx context.Context, s RedactionSettingsStore, rules []redact.Rule) error {
	if rules == nil {
		rules = []redact.Rule{}
	}
	buf, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	return s.SaveSettings(ctx, map[string]string{
		redactionCustomRulesKey: string(buf),
	})
}

// loadRedactionGlobal decodes the global redaction settings blob from the
// settings map, falling back to defaults on missing/corrupt JSON.
func loadRedactionGlobal(m map[string]string) redactionGlobalJSON {
	raw := m[redactionGlobalKey]
	if raw == "" {
		return defaultRedactionGlobal
	}
	var g redactionGlobalJSON
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		return defaultRedactionGlobal
	}
	if g.RuleIDs == nil {
		g.RuleIDs = []string{}
	}
	return g
}

// GetRedactionRules handles GET /api/v1/redaction/rules.
// Returns the built-in rule set + persisted custom rules. Session-authed
// (any logged-in user may read); no admin gate. The JSON response is
// stable across calls (built-in pack is hard-coded, custom rules come
// straight from the settings table).
func (d Deps) GetRedactionRules(w http.ResponseWriter, r *http.Request) {
	out := redactionRulesResp{
		BuiltIn: redact.BuiltIn,
		Custom:  []redact.Rule{},
	}
	if d.Settings != nil {
		m, err := d.Settings.GetSettings(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "get redaction rules failed")
			return
		}
		out.Custom = loadCustomRules(m)
	}
	writeJSON(w, http.StatusOK, out)
}

// PostRedactionRule handles POST /api/v1/redaction/rules.
// Admin-or-ai:configure:any. On success returns 201 with the persisted Rule
// (id assigned server-side as a UUID). 400 on missing/invalid fields.
func (d Deps) PostRedactionRule(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var in redactionRuleReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	rule, err := validateRuleReq(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if d.Settings == nil {
		writeErr(w, http.StatusInternalServerError, "settings store unavailable")
		return
	}
	m, err := d.Settings.GetSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get redaction rules failed")
		return
	}
	rule.ID = uuid.NewString()
	rules := append(loadCustomRules(m), rule)
	if err := saveCustomRules(r.Context(), d.Settings, rules); err != nil {
		writeErr(w, http.StatusInternalServerError, "save redaction rule failed")
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

// PutRedactionRule handles PUT /api/v1/redaction/rules/{id}.
// Admin-or-ai:configure:any. 400 on invalid regex; 404 on unknown id;
// 409 on attempt to PUT a built-in id (built-ins are read-only).
func (d Deps) PutRedactionRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if redact.IsBuiltIn(id) {
		writeErr(w, http.StatusConflict, "built-in rules are read-only")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var in redactionRuleReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	rule, err := validateRuleReq(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if d.Settings == nil {
		writeErr(w, http.StatusInternalServerError, "settings store unavailable")
		return
	}
	m, err := d.Settings.GetSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get redaction rules failed")
		return
	}
	rules := loadCustomRules(m)
	found := false
	for i := range rules {
		if rules[i].ID == id {
			rule.ID = id
			rules[i] = rule
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	if err := saveCustomRules(r.Context(), d.Settings, rules); err != nil {
		writeErr(w, http.StatusInternalServerError, "save redaction rule failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteRedactionRule handles DELETE /api/v1/redaction/rules/{id}.
// Admin-or-ai:configure:any. 409 when id refers to a built-in rule
// (spec Part B.4); 404 on unknown custom id.
func (d Deps) DeleteRedactionRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if redact.IsBuiltIn(id) {
		writeErr(w, http.StatusConflict, "built-in rules cannot be deleted")
		return
	}
	if d.Settings == nil {
		writeErr(w, http.StatusInternalServerError, "settings store unavailable")
		return
	}
	m, err := d.Settings.GetSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get redaction rules failed")
		return
	}
	rules := loadCustomRules(m)
	out := rules[:0]
	found := false
	for _, ru := range rules {
		if ru.ID == id {
			found = true
			continue
		}
		out = append(out, ru)
	}
	if !found {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	if err := saveCustomRules(r.Context(), d.Settings, out); err != nil {
		writeErr(w, http.StatusInternalServerError, "save redaction rule failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetRedactionSettings handles GET /api/v1/redaction/settings.
// Session-authed. Returns the global blob (or defaults) + a per_service
// list derived from every row in service_ai_config (Task 24 will write
// these; for v0.4.0 Task 5 the list may be empty).
func (d Deps) GetRedactionSettings(w http.ResponseWriter, r *http.Request) {
	resp := redactionSettingsResp{
		Global:     defaultRedactionGlobal,
		PerService: []redactionPerServiceSettings{},
	}
	if d.Settings != nil {
		m, err := d.Settings.GetSettings(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "get redaction settings failed")
			return
		}
		resp.Global = loadRedactionGlobal(m)
	}
	if d.CacheServices != nil {
		// Per-service redaction lives in service_ai_config.config.redaction —
		// reuse the existing CacheServices surface (it already iterates every
		// service_ai_config row) and decode the .redaction sub-object.
		rows, err := d.CacheServices.ListAllServiceAIConfigs(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "list service ai configs failed")
			return
		}
		for _, row := range rows {
			perSvc, override := perServiceRedactionFromConfig(row.Config)
			resp.PerService = append(resp.PerService, redactionPerServiceSettings{
				ServiceID:         row.ServiceID,
				Enabled:           perSvc.Enabled,
				RedactForLogsOnly: perSvc.RedactForLogsOnly,
				RuleIDs:           perSvc.RuleIDs,
				PresidioEnabled:   perSvc.PresidioEnabled,
				PresidioURL:       perSvc.PresidioURL,
				Override:          override,
			})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// perServiceRedactionFromConfig pulls the .redaction sub-object out of a
// service_ai_config.config JSON blob. ok is true when the blob has a
// non-empty redaction block (i.e. the service overrides the global
// defaults). Falls back to defaults on missing / malformed input.
func perServiceRedactionFromConfig(blob []byte) (redactionGlobalJSON, bool) {
	if len(blob) == 0 {
		return defaultRedactionGlobal, false
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(blob, &outer); err != nil {
		return defaultRedactionGlobal, false
	}
	raw, ok := outer["redaction"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return defaultRedactionGlobal, false
	}
	var g redactionGlobalJSON
	if err := json.Unmarshal(raw, &g); err != nil {
		return defaultRedactionGlobal, false
	}
	if g.RuleIDs == nil {
		g.RuleIDs = []string{}
	}
	return g, true
}

// PutRedactionSettings handles PUT /api/v1/redaction/settings.
// Admin-or-ai:configure:any. Validates the global blob structure; 400 on
// malformed JSON. The validated blob is persisted as a single JSON value
// under redactionGlobalKey.
func (d Deps) PutRedactionSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var g redactionGlobalJSON
	if err := json.Unmarshal(raw, &g); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid setting")
		return
	}
	if g.RuleIDs == nil {
		g.RuleIDs = []string{}
	}
	// Re-marshal so we persist a canonical shape (no extra fields).
	canon, err := json.Marshal(g)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal failed")
		return
	}
	if d.Settings == nil {
		writeErr(w, http.StatusInternalServerError, "settings store unavailable")
		return
	}
	if err := d.Settings.SaveSettings(r.Context(), map[string]string{
		redactionGlobalKey: string(canon),
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "save redaction settings failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostRedactionPreview handles POST /api/v1/redaction/preview.
// Admin-or-ai:configure:any (mutation seam — runs regex evaluation, kept
// behind the same gate as rule edits so a curious user can't probe the
// pack). Returns the set of regex matches the engine would have rewritten
// against the supplied body, but does NOT apply rewrites — the UI uses
// this for live highlight.
func (d Deps) PostRedactionPreview(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB preview cap
	var in redactionPreviewReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if in.Body == "" {
		writeJSON(w, http.StatusOK, redactionPreviewResp{Matches: []redactionPreviewMatch{}})
		return
	}
	scope := in.Scope
	if scope == "" {
		scope = redact.ScopeRequestBody
	}
	// Custom rules participate in preview too (so editing a regex shows the
	// expected new matches before persist). Load them best-effort.
	var custom []redact.Rule
	if d.Settings != nil {
		m, err := d.Settings.GetSettings(r.Context())
		if err == nil {
			custom = loadCustomRules(m)
		}
	}
	// NewEngine validates everything (regex compile, action/scope enums); we
	// only need it as a guard against persisted-but-invalid custom rules.
	if _, err := redact.NewEngine(custom); err != nil {
		// A persisted invalid regex would be a server-side defect; surface
		// it cleanly rather than 500.
		writeErr(w, http.StatusInternalServerError, "engine init failed: "+err.Error())
		return
	}
	matches := previewMatches(custom, []byte(in.Body), scope)
	writeJSON(w, http.StatusOK, redactionPreviewResp{Matches: matches})
}

// previewMatches returns one redactionPreviewMatch per (rule, hit) pair in
// the union of BuiltIn + custom rules, filtered by scope. Order is rule
// iteration order followed by ascending match offset within a rule. The
// engine's own iteration order would be name-sorted, but the preview API
// does not require name-sorting; we keep the rule list as built-ins first,
// then custom, which is what a UI heatmap most naturally renders.
//
// credit_card_luhn matches are post-filtered through Luhn so the heatmap
// doesn't light up on order/lot numbers.
func previewMatches(custom []redact.Rule, body []byte, scope string) []redactionPreviewMatch {
	out := []redactionPreviewMatch{}
	all := make([]redact.Rule, 0, len(redact.BuiltIn)+len(custom))
	all = append(all, redact.BuiltIn...)
	all = append(all, custom...)
	for _, r := range all {
		if !redactScopeMatches(r.Scope, scope) {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			continue
		}
		idxs := re.FindAllIndex(body, -1)
		for _, span := range idxs {
			if r.Name == "credit_card_luhn" && !luhnPreview(body[span[0]:span[1]]) {
				continue
			}
			out = append(out, redactionPreviewMatch{
				Rule:  r.Name,
				Start: span[0],
				End:   span[1],
				Value: string(body[span[0]:span[1]]),
			})
		}
	}
	return out
}

// redactScopeMatches mirrors redact.ruleScopeMatches at the API layer
// (the engine helper is unexported). Same semantics: "both" matches any
// caller scope; otherwise scopes must equal.
func redactScopeMatches(ruleScope, callerScope string) bool {
	if ruleScope == redact.ScopeBoth {
		return true
	}
	return ruleScope == callerScope
}

// luhnPreview is a small Luhn-check helper for the preview path so the UI
// doesn't highlight credit-card false positives. Kept separate from the
// engine's private luhnValid to avoid widening the engine's surface.
func luhnPreview(value []byte) bool {
	digits := make([]byte, 0, len(value))
	for _, b := range value {
		switch {
		case b >= '0' && b <= '9':
			digits = append(digits, b-'0')
		case b == ' ' || b == '-':
		default:
			return false
		}
	}
	n := len(digits)
	if n < 13 || n > 16 {
		return false
	}
	sum := 0
	parity := n % 2
	for i, d := range digits {
		dd := int(d)
		if i%2 == parity {
			dd *= 2
			if dd > 9 {
				dd -= 9
			}
		}
		sum += dd
	}
	return sum%10 == 0
}
