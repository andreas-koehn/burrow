// automation_handlers.go — automation token JSON API (spec Part M.1).
//
//	GET    /api/v1/automation/tokens
//	POST   /api/v1/automation/tokens
//	DELETE /api/v1/automation/tokens/{id}
//
// Permission model:
//   - automation:tokens:manage:own  — list/create/revoke OWN tokens
//   - automation:tokens:manage:any  — manage anyone's (admin always has both)
//
// The POST handler validates requested perms ⊆ caller's current role perms
// at the store layer (ErrPermissionNotInRole → 403). The plaintext bearer
// secret "bua_<token>" is returned EXACTLY ONCE on the POST response; GET
// responses redact it (only id/name/prefix/perms/expires/last_used remain).

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// AutomationStore is the CRUD + lookup surface the automation handlers
// consume. *store.Store satisfies it directly.
type AutomationStore interface {
	MintAutomationToken(
		ctx context.Context,
		callerID, callerRole, name string,
		permissions []string,
		expiresAt *time.Time,
	) (store.AutomationTokenView, string, error)
	ListAutomationTokensForCaller(
		ctx context.Context,
		callerID, callerRole string,
	) ([]store.AutomationTokenView, error)
	RevokeAutomationToken(
		ctx context.Context,
		callerID, callerRole, id string,
	) error
}

// --- Wire shapes ----------------------------------------------------------

// automationTokenResp is the wire view of one automation_tokens row. The
// plaintext is intentionally absent — it lives only on the POST response.
type automationTokenResp struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Prefix      string     `json:"prefix"`
	UserID      string     `json:"user_id"`
	RoleAtMint  string     `json:"role_at_mint"`
	Permissions []string   `json:"permissions"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsed    *time.Time `json:"last_used,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// createTokenWrap is the POST response: the token view plus the one-time
// plaintext. The plaintext key is fixed by spec Part M.1.
type createTokenWrap struct {
	Token     automationTokenResp `json:"token"`
	Plaintext string              `json:"plaintext"`
}

// postAutomationTokenReq is the POST request body.
type postAutomationTokenReq struct {
	Name        string     `json:"name"`
	Permissions []string   `json:"permissions"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

func toAutomationTokenResp(v store.AutomationTokenView) automationTokenResp {
	perms := v.Permissions
	if perms == nil {
		perms = []string{}
	}
	return automationTokenResp{
		ID:          v.ID,
		Name:        v.Name,
		Prefix:      v.Prefix,
		UserID:      v.UserID,
		RoleAtMint:  v.RoleAtMint,
		Permissions: perms,
		ExpiresAt:   v.ExpiresAt,
		LastUsed:    v.LastUsed,
		CreatedAt:   v.CreatedAt,
	}
}

// --- Permission ------------------------------------------------------------
//
// All three routes need automation:tokens:manage:own or :any (routeAccess in
// access.go). Anyone with the :own perm may at least try — the store narrows
// what they see/revoke.

// callerRoleForAuth returns the role attached to the request. For bearer-
// authed calls the role lives in ctx (set by RequireBearerOrSession); for
// cookie-authed calls the existing service_handlers helper does a fresh
// GetUserByID.
func (d Deps) callerRoleForAuth(r *http.Request) (string, error) {
	if role := callerRoleFromCtx(r.Context()); role != "" {
		return role, nil
	}
	return d.callerRole(r)
}

// --- Handlers -------------------------------------------------------------

// GetAutomationTokens handles GET /api/v1/automation/tokens. The response is
// always a JSON array (non-nil, possibly empty). Plaintext is never present
// in this surface.
func (d Deps) GetAutomationTokens(w http.ResponseWriter, r *http.Request) {
	if d.Automation == nil {
		writeErr(w, http.StatusInternalServerError, "automation store unavailable")
		return
	}
	uid := userID(r.Context())
	role, err := d.callerRoleForAuth(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	rows, err := d.Automation.ListAutomationTokensForCaller(r.Context(), uid, role)
	if err != nil {
		if errors.Is(err, store.ErrForbidden) {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]automationTokenResp, 0, len(rows))
	for _, v := range rows {
		out = append(out, toAutomationTokenResp(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// PostAutomationToken handles POST /api/v1/automation/tokens. 201 on success
// with the new row + one-time plaintext; 400 on validation, 403 on a
// permission outside the caller's current role.
func (d Deps) PostAutomationToken(w http.ResponseWriter, r *http.Request) {
	if d.Automation == nil {
		writeErr(w, http.StatusInternalServerError, "automation store unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var in postAutomationTokenReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}

	uid := userID(r.Context())
	role, err := d.callerRoleForAuth(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	view, plaintext, err := d.Automation.MintAutomationToken(r.Context(), uid, role, in.Name, in.Permissions, in.ExpiresAt)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrPermissionNotInRole):
			writeErr(w, http.StatusForbidden, "requested permission not granted by caller role")
		case errors.Is(err, store.ErrTokenNameRequired):
			writeErr(w, http.StatusBadRequest, "name is required")
		default:
			writeErr(w, http.StatusInternalServerError, "mint failed")
		}
		return
	}
	writeJSON(w, http.StatusCreated, createTokenWrap{
		Token:     toAutomationTokenResp(view),
		Plaintext: plaintext,
	})
}

// DeleteAutomationToken handles DELETE /api/v1/automation/tokens/{id}.
// 204 on success, 404 if the row does not exist or the caller may not
// see it, 403 if the caller lacks any manage permission at all.
func (d Deps) DeleteAutomationToken(w http.ResponseWriter, r *http.Request) {
	if d.Automation == nil {
		writeErr(w, http.StatusInternalServerError, "automation store unavailable")
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	uid := userID(r.Context())
	role, err := d.callerRoleForAuth(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	err = d.Automation.RevokeAutomationToken(r.Context(), uid, role, id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, db.ErrNotFound):
		writeErr(w, http.StatusNotFound, "token not found")
	default:
		writeErr(w, http.StatusInternalServerError, "revoke failed")
	}
}
