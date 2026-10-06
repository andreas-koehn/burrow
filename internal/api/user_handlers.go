package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

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
	err := d.Users.ChangePassword(r.Context(), userID(r.Context()), in.CurrentPassword, in.NewPassword)
	if errors.Is(err, store.ErrInvalidCredentials) {
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
	w.WriteHeader(http.StatusNoContent)
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

// AdminListUsers returns a filtered page of users (admin only).
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

type updateUserReq struct {
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

// AdminUpdateUser changes a user's role and/or status (admin only).
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
	if in.Status != nil {
		if *in.Status != "active" && *in.Status != "suspended" {
			writeErr(w, http.StatusBadRequest, "status must be 'active' or 'suspended'")
			return
		}
		if targetID == userID(r.Context()) {
			writeErr(w, http.StatusBadRequest, "cannot change your own status")
			return
		}
		if err := d.Users.SetUserStatus(r.Context(), targetID, *in.Status); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "user not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, "update status failed")
			return
		}
	}
	if in.Role != nil {
		if *in.Role != "admin" && *in.Role != "user" {
			writeErr(w, http.StatusBadRequest, "role must be 'admin' or 'user'")
			return
		}
		if err := d.Users.UpdateUserRole(r.Context(), targetID, *in.Role); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "user not found")
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

// AdminCreateUser creates a new user account (admin only).
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
		writeErr(w, http.StatusBadRequest, "role must be 'admin' or 'user'")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create user failed")
		return
	}
	writeJSON(w, http.StatusCreated, toUserAdminResp(u))
}

// AdminDeleteUser removes a user (admin only).
// DELETE /api/v1/users/{id}
// Returns 400 if the admin attempts to delete themselves (lockout guard).
func (d Deps) AdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "id")
	if targetID == userID(r.Context()) {
		writeErr(w, http.StatusBadRequest, "cannot delete yourself")
		return
	}
	err := d.Users.DeleteUser(r.Context(), targetID)
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
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "delete user failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
