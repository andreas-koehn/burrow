package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/db"
)

type sessionResp struct {
	ID        string    `json:"id"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Current   bool      `json:"current"`
}

// ListSessions returns the caller's active sessions. GET /api/v1/sessions
// The id of a session is a hash of its cookie value, never the value itself:
// a page that can read the list must not learn a credential.
func (d Deps) ListSessions(w http.ResponseWriter, r *http.Request) {
	uid := userID(r.Context())
	ss, err := d.Sessions.ListSessions(r.Context(), uid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list sessions failed")
		return
	}
	cur := ""
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		cur = sessionRef(c.Value)
	}
	out := make([]sessionResp, 0, len(ss))
	for _, s := range ss {
		out = append(out, sessionResp{
			ID: sessionRef(s.ID), IP: s.IP, UserAgent: s.UserAgent,
			CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt,
			Current: sessionRef(s.ID) == cur,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// RevokeSession deletes one of the caller's sessions. DELETE /api/v1/sessions/{id}
// The id is the one ListSessions returned.
func (d Deps) RevokeSession(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "id")
	uid := userID(r.Context())
	ss, err := d.Sessions.ListSessions(r.Context(), uid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "revoke session failed")
		return
	}
	id := ""
	for _, s := range ss {
		if sessionRef(s.ID) == ref {
			id = s.ID
			break
		}
	}
	if id == "" {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	err = d.Sessions.RevokeSession(r.Context(), id, uid)
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "revoke session failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RevokeAllSessions signs the caller out of every other session (keeps the
// current one). POST /api/v1/sessions/revoke-all
func (d Deps) RevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	keep := ""
	if c, err := r.Cookie(sessionCookieName); err == nil {
		keep = c.Value
	}
	n, err := d.Sessions.RevokeOtherSessions(r.Context(), userID(r.Context()), keep)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "revoke all failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"revoked": n})
}
