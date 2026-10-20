package api

// ai_key_handlers.go — /ai/keys: gateway keys.
//
// A gateway key opens the dialect endpoints (/openai/v1, /anthropic) and the
// provider paths, optionally restricted to a list of models, and opens the MCP
// tools its allow-list names (none by default).  It belongs to
// the user who created it.  The key itself leaves the relay once, in the
// answer to its creation: the database keeps a hash, and no other response,
// audit event or log line carries it.
//
// A caller manages their own keys: listing needs ai:read:own / :any or
// ai:configure:own / :any, creating and revoking ai:configure:own / :any
// (routeAccess in access.go).  A key spends AI budget, and the gateway does
// not look at its user's role again, so a role without the permission gets
// none.  An admin lists and revokes every key.  Automation tokens are
// held to less than the session of their user:
//   - a token cannot create a key.  A gateway key does not expire and would
//     outlive the token, whatever the token was minted for;
//   - a token lists and revokes the keys of its user only, also when that
//     user is an admin.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/store"
)

// aiKeyResp is the JSON wire shape of one gateway key. It has no field for
// the key or its hash; key_prefix is the first 8 characters, for display.
type aiKeyResp struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	KeyPrefix     string     `json:"key_prefix"`
	UserID        string     `json:"user_id"`
	AllowedModels []string   `json:"allowed_models"` // empty = every model
	SystemText    string     `json:"system_text"`    // put in front of the app's system content
	AllowedTools  []string   `json:"allowed_tools"`  // "<source>__<tool>" or "<source>__*"; empty = no tools
	ToolsReadOnly bool       `json:"tools_read_only"`
	LastUsed      *time.Time `json:"last_used"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
}

// aiKeyCreatedResp is the answer to a creation: the only shape with the key.
type aiKeyCreatedResp struct {
	aiKeyResp
	Key string `json:"key"`
}

func toAIKeyResp(k store.GatewayKey) aiKeyResp {
	return aiKeyResp{
		ID: k.ID, Name: k.Name, KeyPrefix: k.KeyPrefix, UserID: k.UserID,
		AllowedModels: append([]string{}, k.AllowedModels...), SystemText: k.SystemText,
		AllowedTools: append([]string{}, k.AllowedTools...), ToolsReadOnly: k.ToolsReadOnly,
		LastUsed: k.LastUsed, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt,
	}
}

// postAIKeyReq is the body of POST /ai/keys. There is no field for the
// owner: a key is created for the caller.
type postAIKeyReq struct {
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	AllowedTools  []string `json:"allowed_tools"`
	ToolsReadOnly bool     `json:"tools_read_only"`
	SystemText    string   `json:"system_text"`
}

// putAIKeyReq is the body of PUT /ai/keys/{id}. A field left out stays as it
// is; an allow-list that is given replaces the list, and an empty one clears
// it. The key and its owner cannot be changed.
type putAIKeyReq struct {
	Name          *string   `json:"name"`
	SystemText    *string   `json:"system_text"`
	AllowedModels *[]string `json:"allowed_models"`
	AllowedTools  *[]string `json:"allowed_tools"`
	ToolsReadOnly *bool     `json:"tools_read_only"`
}

const (
	maxKeyBody    = 64 << 10 // a system text of 8192 bytes escaped as \u00XX still fits
	maxKeyName    = 120
	maxKeyAllowed = 64
	maxKeyID      = 64 // ids are UUIDs

	msgKeyName     = "name must be 1-120 characters without control characters"
	msgKeyAllowed  = `an allowed model must be a model name, "<provider>/<model>" or "<provider>/*"`
	msgKeyTooMany  = "at most 64 allowed models"
	msgKeyTool     = `an allowed tool must be "<source>__<tool>" or "<source>__*"`
	msgKeyTools    = "at most 64 allowed tools"
	msgKeyText     = "system_text is longer than 8192 bytes"
	msgKeyNotFound = "key not found"
)

// mapKeyErr writes the HTTP error for a gateway-key store error and reports
// whether it handled it.
func mapKeyErr(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrInvalidKey):
		// The reason names a rule, never the caller's text.
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), store.ErrInvalidKey.Error()+": "))
	case errors.Is(err, store.ErrKeyNotFound):
		writeErr(w, http.StatusNotFound, msgKeyNotFound)
	default:
		return false
	}
	return true
}

// msgDashboardSession is the 403 for a route that an automation token may
// not call (the same words as the client sign-in approval).
const msgDashboardSession = "a dashboard session is required"

// keysReach says how far the caller reaches over gateway keys: all is true
// when the caller may read every user's keys (ai:read:any or ai:configure:any),
// anyKey when it may also change and revoke them (ai:configure:any). Both are
// decided by permission, never by role name. A dashboard session holds what
// its role holds; an automation token holds what it declares and its user's
// role still grants (effectivePerms), so an admin's token that declares only
// :own reaches its user's own keys and no one else's.
func (d Deps) keysReach(r *http.Request) (all, anyKey bool, err error) {
	role, err := d.callerRoleForAuth(r)
	if err != nil {
		return false, false, err
	}
	anyKey = effectivePerms(r.Context(), role, authz.PermAIConfigureAny)
	all = anyKey || effectivePerms(r.Context(), role, authz.PermAIReadAny)
	return all, anyKey, nil
}

// checkKeyAllowLists answers 400 and reports false when a model or tool
// allow-list is malformed or too long. A nil list is not checked.
func checkKeyAllowLists(w http.ResponseWriter, models, tools []string) bool {
	if len(models) > maxKeyAllowed {
		writeErr(w, http.StatusBadRequest, msgKeyTooMany)
		return false
	}
	for _, e := range models {
		if !store.ValidAllowEntry(e) {
			writeErr(w, http.StatusBadRequest, msgKeyAllowed)
			return false
		}
	}
	if len(tools) > maxKeyAllowed {
		writeErr(w, http.StatusBadRequest, msgKeyTools)
		return false
	}
	for _, e := range tools {
		if !store.ValidToolEntry(e) {
			writeErr(w, http.StatusBadRequest, msgKeyTool)
			return false
		}
	}
	return true
}

// GetAIKeys handles GET /api/v1/ai/keys: the caller's gateway keys, newest
// first; every user's for a caller with ai:read:any or ai:configure:any.  Revoked keys are listed with
// revoked_at.
func (d Deps) GetAIKeys(w http.ResponseWriter, r *http.Request) {
	out := []aiKeyResp{}
	if d.AIGatewayKeys != nil {
		all, _, err := d.keysReach(r)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		keys, err := d.AIGatewayKeys.ListGatewayKeys(r.Context(), userID(r.Context()), all)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, k := range keys {
			out = append(out, toAIKeyResp(k))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// PostAIKey handles POST /api/v1/ai/keys (a dashboard session of a role with
// ai:configure:own or :any; the gate of the route answers an automation
// token with 403).  The 201 carries the
// key; it cannot be read again.  allowed_models restricts the key to
// synthetic model names, "<provider>/<model>" and "<provider>/*" entries;
// empty or left out means every model.  An entry need not name something
// that exists.
func (d Deps) PostAIKey(w http.ResponseWriter, r *http.Request) {
	// The answer holds a secret: no cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxKeyBody)
	var in postAIKeyReq
	if msg, _ := decodeStrictJSON(r.Body, &in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	// Checked here as well as in the store, so that no store implementation
	// sees an unbounded list or a malformed entry.
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > maxKeyName || strings.ContainsFunc(name, unicode.IsControl) {
		writeErr(w, http.StatusBadRequest, msgKeyName)
		return
	}
	if len(in.SystemText) > store.MaxSystemTextBytes {
		writeErr(w, http.StatusBadRequest, msgKeyText)
		return
	}
	if !checkKeyAllowLists(w, in.AllowedModels, in.AllowedTools) {
		return
	}
	if !d.keyTextAllowed(w, r, in.SystemText) {
		return
	}
	if d.AIGatewayKeys == nil {
		writeErr(w, http.StatusServiceUnavailable, "gateway keys are not available on this relay")
		return
	}
	k, plaintext, err := d.AIGatewayKeys.CreateGatewayKeyFrom(r.Context(), userID(r.Context()), store.NewGatewayKey{
		Name: name, AllowedModels: in.AllowedModels, AllowedTools: in.AllowedTools, ToolsReadOnly: in.ToolsReadOnly, SystemText: in.SystemText,
	})
	if err != nil {
		if !mapKeyErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditAI(r, audit.ActionAIGatewayKeyCreate, k.ID, k.Name, map[string]any{
		"name":            k.Name,
		"allowed_models":  k.AllowedModels,
		"allowed_tools":   k.AllowedTools,
		"tools_read_only": k.ToolsReadOnly,
		"key_prefix":      k.KeyPrefix,
		"system_text":     len(k.SystemText) > 0,
	})
	writeJSON(w, http.StatusCreated, aiKeyCreatedResp{aiKeyResp: toAIKeyResp(k), Key: plaintext})
}

// DeleteAIKey handles DELETE /api/v1/ai/keys/{id}: revokes the key at once.
// Its owner or a caller with ai:configure:any may; anyone else's key answers 404 like an
// id that does not exist.  Revoking a revoked key is a 204 too.
func (d Deps) DeleteAIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if d.AIGatewayKeys == nil || id == "" || len(id) > maxKeyID {
		writeErr(w, http.StatusNotFound, msgKeyNotFound)
		return
	}
	_, anyKey, err := d.keysReach(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := d.AIGatewayKeys.RevokeGatewayKey(r.Context(), userID(r.Context()), anyKey, id); err != nil {
		if !mapKeyErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditAI(r, audit.ActionAIGatewayKeyRevoke, id, "", map[string]any{})
	w.WriteHeader(http.StatusNoContent)
}

// PutAIKey handles PUT /api/v1/ai/keys/{id}: renames the key, sets its system
// text and replaces its model and tool allow-lists and its read-only switch. Its owner or a caller with ai:configure:any may; anyone else's key answers
// 404 like an id that does not exist. A field left out stays as it is, an
// empty name is refused, an empty system_text clears the text, an empty
// allow-list clears the list (all models, no tools). A revoked key
// can still be changed. The audit event says which fields changed and carries the
// new allow-lists, never the text.
func (d Deps) PutAIKey(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxKeyBody)
	var in putAIKeyReq
	if msg, _ := decodeStrictJSON(r.Body, &in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > maxKeyName || strings.ContainsFunc(name, unicode.IsControl) {
			writeErr(w, http.StatusBadRequest, msgKeyName)
			return
		}
	}
	if in.SystemText != nil && len(*in.SystemText) > store.MaxSystemTextBytes {
		writeErr(w, http.StatusBadRequest, msgKeyText)
		return
	}
	var models, tools []string
	if in.AllowedModels != nil {
		models = *in.AllowedModels
	}
	if in.AllowedTools != nil {
		tools = *in.AllowedTools
	}
	if !checkKeyAllowLists(w, models, tools) {
		return
	}
	if in.SystemText != nil && !d.keyTextAllowed(w, r, *in.SystemText) {
		return
	}
	id := chi.URLParam(r, "id")
	if d.AIGatewayKeys == nil || id == "" || len(id) > maxKeyID {
		writeErr(w, http.StatusNotFound, msgKeyNotFound)
		return
	}
	_, anyKey, err := d.keysReach(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	k, err := d.AIGatewayKeys.UpdateGatewayKey(r.Context(), userID(r.Context()), anyKey, id, store.GatewayKeyUpdate{
		Name: in.Name, SystemText: in.SystemText, AllowedModels: in.AllowedModels, AllowedTools: in.AllowedTools, ToolsReadOnly: in.ToolsReadOnly,
	})
	if err != nil {
		if !mapKeyErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	// The allow-lists are not secret: the event carries the new lists, so that
	// it shows what a key was widened to.
	changes := map[string]any{
		"name":        k.Name,
		"renamed":     in.Name != nil,
		"system_text": in.SystemText != nil,
	}
	if in.AllowedModels != nil {
		changes["allowed_models"] = k.AllowedModels
	}
	if in.AllowedTools != nil {
		changes["allowed_tools"] = k.AllowedTools
	}
	if in.ToolsReadOnly != nil {
		changes["tools_read_only"] = k.ToolsReadOnly
	}
	d.auditAI(r, audit.ActionAIGatewayKeyUpdate, k.ID, k.Name, changes)
	writeJSON(w, http.StatusOK, toAIKeyResp(k))
}

// anyServiceGuardrails reports whether any service has guardrails of its own
// switched on. A config that does not decode counts as none (the chain's
// loader skips it as well).
func (d Deps) anyServiceGuardrails(ctx context.Context) (bool, error) {
	if d.ServiceAIConfigs == nil {
		return false, nil
	}
	raws, err := d.ServiceAIConfigs.ListServiceAIConfigsRaw(ctx)
	if err != nil {
		return false, err
	}
	for _, raw := range raws {
		var cfg struct {
			Guardrails *struct {
				Enabled bool `json:"enabled"`
			} `json:"guardrails"`
		}
		if json.Unmarshal(raw, &cfg) == nil && cfg.Guardrails != nil && cfg.Guardrails.Enabled {
			return true, nil
		}
	}
	return false, nil
}

// keyPatterns is the guardrails engine the key handlers check system text
// with: the bundled request patterns compiled once, the custom ones cached.
var keyPatterns = sync.OnceValue(guardrails.NewEngine)

// keyTextAllowed checks a gateway key's system text against the request
// patterns, bundled and custom, where request guardrails apply to the key,
// and writes the 400 naming the pattern that matched. The chain checks the
// text on every request as well (aigw.WithKeySystemText); refusing it here
// tells the key's owner at once. A model's system text is the operator's and
// is not checked.
//
// A gateway key serves no single service, so the check runs when the global
// request guardrails are on, or when any service has guardrails of its own
// switched on (they override the global switch for that service, see
// guardrails.Effective, and use the global custom patterns).
func (d Deps) keyTextAllowed(w http.ResponseWriter, r *http.Request, text string) bool {
	if text == "" || d.Settings == nil {
		return true
	}
	m, err := d.Settings.GetSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get guardrail settings failed")
		return false
	}
	g := loadGuardrailsGlobal(m)
	if !g.Enabled {
		on, err := d.anyServiceGuardrails(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "get guardrail settings failed")
			return false
		}
		if !on {
			return true
		}
	}
	if hit, id := keyPatterns().InspectRequest([]byte(text), g.CustomPatterns); hit {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("system_text matches the guardrail pattern %q", id))
		return false
	}
	return true
}
