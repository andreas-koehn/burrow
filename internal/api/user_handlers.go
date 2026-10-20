package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// changePasswordReq is the request body for POST /api/v1/auth/change-password.
type changePasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword lets the authenticated user change their own password.
// 401 on wrong current password, 400 on short/missing new password, 204 on success.
func (d Deps) ChangePassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in changePasswordReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.CurrentPassword == "" || in.NewPassword == "" {
		writeErr(w, http.StatusBadRequest, "current_password and new_password are required")
		return
	}
	uid := userID(r.Context())
	// Wrong current passwords count against the session's allowance, shared
	// with /auth/reauth (a bearer request has no session to count against).
	key := ""
	if c, cerr := r.Cookie(sessionCookieName); cerr == nil && c.Value != "" && bearerTokenID(r.Context()) == "" {
		key = sessionRef(c.Value)
	}
	if key != "" && d.reauthGuesses.blocked(key) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "too many wrong passwords; try again in a minute")
		return
	}
	err := d.Users.ChangePassword(r.Context(), uid, in.CurrentPassword, in.NewPassword)
	if errors.Is(err, store.ErrInvalidCredentials) {
		d.noteWrongPassword(key, uid)
		d.auditPasswordChange(r, "denied")
		writeErr(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if errors.Is(err, store.ErrPasswordTooShort) {
		writeErr(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "change password failed")
		return
	}
	d.auditPasswordChange(r, "ok")
	d.endOtherSessions(r, uid)
	w.WriteHeader(http.StatusNoContent)
}

// auditPasswordChange records an attempt to change the caller's password.
func (d Deps) auditPasswordChange(r *http.Request, result string) {
	email := ""
	if d.Users != nil {
		if u, err := d.Users.GetUserByID(r.Context(), userID(r.Context())); err == nil {
			email = u.Email
		}
	}
	d.auditAuth(r, audit.ActionAuthPasswordChange, userID(r.Context()), email, result)
}

// endOtherSessions signs the user out of every session but the caller's own
// and closes their re-authentication windows: whoever held the old password
// loses the sessions it opened.
func (d Deps) endOtherSessions(r *http.Request, uid string) {
	if d.Sessions == nil {
		return
	}
	keep := ""
	if c, err := r.Cookie(sessionCookieName); err == nil {
		keep = c.Value
	}
	if ss, err := d.Sessions.ListSessions(r.Context(), uid); err == nil {
		for _, s := range ss {
			if s.ID != keep {
				d.reauth.forget(sessionRef(s.ID))
			}
		}
	}
	_, _ = d.Sessions.RevokeOtherSessions(r.Context(), uid, keep)
}

// userAdminResp is returned by admin user endpoints (never password_hash).
type userAdminResp struct {
	ID        string     `json:"id"`
	Email     string     `json:"email"`
	Role      string     `json:"role"`
	Status    string     `json:"status"`
	LastLogin *time.Time `json:"last_login"`
	CreatedAt time.Time  `json:"created_at"`
}

func toUserAdminResp(u db.User) userAdminResp {
	return userAdminResp{
		ID: u.ID, Email: u.Email, Role: u.Role,
		Status: u.Status, LastLogin: u.LastLogin, CreatedAt: u.CreatedAt,
	}
}

type usersPageResp struct {
	Users []userAdminResp `json:"users"`
	Total int             `json:"total"`
}

// AdminListUsers returns a filtered page of users (users:read or users:manage).
// GET /api/v1/users?q=&limit=&offset=
func (d Deps) AdminListUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	users, total, err := d.Users.ListUsersPage(r.Context(), q, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list users failed")
		return
	}
	out := make([]userAdminResp, 0, len(users))
	for _, u := range users {
		out = append(out, toUserAdminResp(u))
	}
	writeJSON(w, http.StatusOK, usersPageResp{Users: out, Total: total})
}

// atoiDefault parses a base-10 int, returning def for empty/invalid input.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// msgUnknownUserRole is the 400 for a role that is neither built-in nor a
// stored custom role.
const msgUnknownUserRole = "role must be a built-in role or an existing custom role"

// mayGrant reports whether the caller holds every one of perms, and answers
// 403 itself when not: nobody hands out a permission they lack. A session
// holds what its role holds; a token only what it declares and its user's
// role grants (effectivePerms), so a token hands out no more than it can use.
func (d Deps) mayGrant(w http.ResponseWriter, r *http.Request, perms []string) bool {
	role, err := d.callerRoleForAuth(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return false
	}
	if p := firstNotHeld(r.Context(), role, perms); p != "" {
		who := "your role"
		if bearerTokenID(r.Context()) != "" {
			who = "this token"
		}
		writeErr(w, http.StatusForbidden, who+" does not hold "+p+" and cannot grant it")
		return false
	}
	return true
}

// rolePerms returns the permissions of a role: a built-in role from authz
// (it may have no row in the roles table), any other from the role store.
func (d Deps) rolePerms(ctx context.Context, name string) ([]string, error) {
	if ar, ok := authz.Get(name); ok {
		var perms []string
		for _, p := range ar.Permissions {
			perms = append(perms, string(p))
		}
		return perms, nil
	}
	if d.Roles == nil {
		return nil, nil
	}
	rd, err := d.Roles.GetRole(ctx, name)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rd.Permissions, nil
}

// mayAssignRole reports whether the caller may give a user the named role
// (mayGrant over the role's permissions). A role that does not exist passes:
// the store refuses it with ErrInvalidRole.
func (d Deps) mayAssignRole(w http.ResponseWriter, r *http.Request, name string) bool {
	perms, err := d.rolePerms(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return false
	}
	return d.mayGrant(w, r, perms)
}

// mayManageUser reports whether the caller may change or delete the target
// user: the caller must hold every permission of the role the target holds
// now, so that users:manage alone does not reach the admins. A target that
// does not exist is a 404.
func (d Deps) mayManageUser(w http.ResponseWriter, r *http.Request, targetID string) bool {
	target, err := d.Users.GetUserByID(r.Context(), targetID)
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "user not found")
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return false
	}
	return d.mayAssignRole(w, r, target.Role)
}

type updateUserReq struct {
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

// AdminUpdateUser changes a user's role and/or status (users:manage). The
// role is a built-in role or a stored custom role, and one whose permissions
// the caller's own role holds (mayAssignRole).
// PATCH /api/v1/users/{id}. An admin cannot change their own status (lockout
// guard, mirrors the self-delete guard).
func (d Deps) AdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "id")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in updateUserReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || (in.Role == nil && in.Status == nil) {
		writeErr(w, http.StatusBadRequest, "role and/or status required")
		return
	}
	if !d.mayManageUser(w, r, targetID) {
		return
	}
	if in.Role != nil && !d.mayAssignRole(w, r, *in.Role) {
		return
	}
	if in.Status != nil {
		if *in.Status != "active" && *in.Status != "suspended" {
			writeErr(w, http.StatusBadRequest, "status must be 'active' or 'suspended'")
			return
		}
		if targetID == userID(r.Context()) {
			writeErr(w, http.StatusBadRequest, "cannot change your own status")
			return
		}
		// Read the tokens before anything changes: a failing list answers
		// 500 and leaves the user as it was.
		var tokenIDs []string
		if *in.Status == "suspended" {
			ids, err := d.clientTokenIDs(r, targetID)
			if err != nil {
				d.Log.Error("list client tokens failed", "user_id", targetID, "err", err)
				writeErr(w, http.StatusInternalServerError, "update status failed")
				return
			}
			tokenIDs = ids
		}
		if err := d.Users.SetUserStatus(r.Context(), targetID, *in.Status); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "user not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, "update status failed")
			return
		}
		// The store ended the user's dashboard sessions; the clients that
		// are connected with the user's tokens go too.
		d.closeClientSessions(tokenIDs...)
	}
	if in.Role != nil {
		if err := d.Users.UpdateUserRole(r.Context(), targetID, *in.Role); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "user not found")
				return
			}
			if errors.Is(err, store.ErrInvalidRole) {
				writeErr(w, http.StatusBadRequest, msgUnknownUserRole)
				return
			}
			writeErr(w, http.StatusInternalServerError, "update role failed")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// createUserReq is the request body for POST /api/v1/users.
type createUserReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// AdminCreateUser creates a new user account (users:manage).
// POST /api/v1/users
func (d Deps) AdminCreateUser(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in createUserReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Email == "" {
		writeErr(w, http.StatusBadRequest, "email, password, and role are required")
		return
	}
	if in.Password == "" {
		writeErr(w, http.StatusBadRequest, "email, password, and role are required")
		return
	}
	if in.Role == "" {
		writeErr(w, http.StatusBadRequest, "email, password, and role are required")
		return
	}
	if !d.mayAssignRole(w, r, in.Role) {
		return
	}
	u, err := d.Users.CreateUser(r.Context(), in.Email, in.Password, in.Role)
	if errors.Is(err, store.ErrEmailConflict) {
		writeErr(w, http.StatusConflict, "email already in use")
		return
	}
	if errors.Is(err, store.ErrPasswordTooShort) {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if errors.Is(err, store.ErrInvalidRole) {
		writeErr(w, http.StatusBadRequest, msgUnknownUserRole)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create user failed")
		return
	}
	writeJSON(w, http.StatusCreated, toUserAdminResp(u))
}

// AdminDeleteUser removes a user (users:manage).
// DELETE /api/v1/users/{id}
// Returns 400 if the admin attempts to delete themselves (lockout guard).
func (d Deps) AdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "id")
	if targetID == userID(r.Context()) {
		writeErr(w, http.StatusBadRequest, "cannot delete yourself")
		return
	}
	if !d.mayManageUser(w, r, targetID) {
		return
	}
	// Read before the delete: the tokens go with the user.
	tokenIDs, err := d.clientTokenIDs(r, targetID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "delete user failed")
		return
	}
	err = d.Users.DeleteUser(r.Context(), targetID)
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	if errors.Is(err, store.ErrProviderInUse) {
		// The user owns the service behind a provider that a synthetic model
		// targets; deleting the user would take the provider with it.
		writeErr(w, http.StatusConflict, "the user owns a provider that is used by model(s): "+providerInUseModels(err))
		return
	}
	if errors.Is(err, store.ErrUserHasMCPSource) {
		// The cascade would take the sources, their tools and approvals.
		writeErr(w, http.StatusConflict, "the user owns the service of tool source(s): "+errDetail(err, store.ErrUserHasMCPSource)+"; delete the tool sources first")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "delete user failed")
		return
	}
	d.closeClientSessions(tokenIDs...)
	w.WriteHeader(http.StatusNoContent)
}
