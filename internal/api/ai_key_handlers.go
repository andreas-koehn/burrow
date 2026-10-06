package api

// ai_key_handlers.go — /ai/keys: gateway keys.
//
// A gateway key opens the dialect endpoints (/openai/v1, /anthropic) and the
// provider paths, optionally restricted to a list of models.  It belongs to
// the user who created it.  The key itself leaves the relay once, in the
// answer to its creation: the database keeps a hash, and no other response,
// audit event or log line carries it.
//
// There is no permission to hold: every signed-in user manages their own
// keys, and an admin lists and revokes every key.  Automation tokens are
// held to less than the session of their user:
//   - a token cannot create a key.  A gateway key does not expire and would
//     outlive the token, whatever the token was minted for;
//   - a token lists and revokes the keys of its user only, also when that
//     user is an admin.

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
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
		AllowedModels: append([]string{}, k.AllowedModels...),
		LastUsed:      k.LastUsed, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt,
	}
}

// postAIKeyReq is the body of POST /ai/keys. There is no field for the
// owner: a key is created for the caller.
type postAIKeyReq struct {
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
}

const (
	maxKeyBody    = 8 << 10
	maxKeyName    = 120
	maxKeyAllowed = 64
	maxKeyID      = 64 // ids are UUIDs

	msgKeyName     = "name must be 1-120 characters without control characters"
	msgKeyAllowed  = `an allowed model must be a model name, "<provider>/<model>" or "<provider>/*"`
	msgKeyTooMany  = "at most 64 allowed models"
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

// keyCallerRole is the role the key store is asked with: the caller's own
// for a dashboard session, "user" for an automation token, so that an
// admin's token reaches the admin's own keys and no one else's.
func (d Deps) keyCallerRole(r *http.Request) (string, error) {
	if bearerTokenID(r.Context()) != "" {
		return "user", nil
	}
	return d.callerRole(r)
}

// GetAIKeys handles GET /api/v1/ai/keys: the caller's gateway keys, newest
// first; every user's for an admin's session.  Revoked keys are listed with
// revoked_at.
func (d Deps) GetAIKeys(w http.ResponseWriter, r *http.Request) {
	out := []aiKeyResp{}
	if d.AIGatewayKeys != nil {
		role, err := d.keyCallerRole(r)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		keys, err := d.AIGatewayKeys.ListGatewayKeys(r.Context(), userID(r.Context()), role)
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

// PostAIKey handles POST /api/v1/ai/keys (dashboard session only; an
// automation token gets 403).  The 201 carries the key; it cannot be read
// again.  allowed_models restricts the key to synthetic model names,
// "<provider>/<model>" and "<provider>/*" entries; empty or left out means
// every model.  An entry need not name something that exists.
func (d Deps) PostAIKey(w http.ResponseWriter, r *http.Request) {
	// The answer holds a secret: no cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	if bearerTokenID(r.Context()) != "" {
		writeErr(w, http.StatusForbidden, msgDashboardSession)
		return
	}
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
	if len(in.AllowedModels) > maxKeyAllowed {
		writeErr(w, http.StatusBadRequest, msgKeyTooMany)
		return
	}
	for _, e := range in.AllowedModels {
		if !store.ValidAllowEntry(e) {
			writeErr(w, http.StatusBadRequest, msgKeyAllowed)
			return
		}
	}
	if d.AIGatewayKeys == nil {
		writeErr(w, http.StatusServiceUnavailable, "gateway keys are not available on this relay")
		return
	}
	k, plaintext, err := d.AIGatewayKeys.CreateGatewayKey(r.Context(), userID(r.Context()), name, in.AllowedModels)
	if err != nil {
		if !mapKeyErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditAI(r, audit.ActionAIGatewayKeyCreate, k.ID, k.Name, map[string]any{
		"name":           k.Name,
		"allowed_models": k.AllowedModels,
		"key_prefix":     k.KeyPrefix,
	})
	writeJSON(w, http.StatusCreated, aiKeyCreatedResp{aiKeyResp: toAIKeyResp(k), Key: plaintext})
}

// DeleteAIKey handles DELETE /api/v1/ai/keys/{id}: revokes the key at once.
// Its owner or an admin's session may; anyone else's key answers 404 like an
// id that does not exist.  Revoking a revoked key is a 204 too.
func (d Deps) DeleteAIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if d.AIGatewayKeys == nil || id == "" || len(id) > maxKeyID {
		writeErr(w, http.StatusNotFound, msgKeyNotFound)
		return
	}
	role, err := d.keyCallerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := d.AIGatewayKeys.RevokeGatewayKey(r.Context(), userID(r.Context()), role, id); err != nil {
		if !mapKeyErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditAI(r, audit.ActionAIGatewayKeyRevoke, id, "", map[string]any{})
	w.WriteHeader(http.StatusNoContent)
}
