package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/db"
)

type tokenResp struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	LastUsed  *time.Time `json:"last_used"`
	CreatedAt time.Time  `json:"created_at"`
	// ExpiresAt is when the token stops working; null for one that never does.
	ExpiresAt *time.Time `json:"expires_at"`
}

// ListTokens returns the caller's client tokens (never the hash).
func (d Deps) ListTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := d.Users.ListClientTokens(r.Context(), userID(r.Context()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]tokenResp, 0, len(ts))
	for _, t := range ts {
		out = append(out, tokenResp{ID: t.ID, Name: t.Name, LastUsed: t.LastUsed, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt})
	}
	writeJSON(w, http.StatusOK, out)
}

type createTokenReq struct {
	Name string `json:"name"`
	// ExpiresAt (RFC 3339) is optional: in the future and at most
	// maxClientTokenLife ahead.
	ExpiresAt *string `json:"expires_at"`
}

// maxClientTokenLife is how far ahead a client token may be set to expire.
const maxClientTokenLife = 365 * 24 * time.Hour

// newTokenResp is returned by CreateToken — name, the one-time plaintext and
// the expiry the token got (null: never).
type newTokenResp struct {
	Name      string     `json:"name"`
	Token     string     `json:"token"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// CreateToken mints a new client token and returns the plaintext exactly once.
func (d Deps) CreateToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in createTokenReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	now := time.Now()
	var exp *time.Time
	if in.ExpiresAt != nil {
		at, err := time.Parse(time.RFC3339, *in.ExpiresAt)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "expires_at must be an RFC 3339 time")
			return
		}
		if !at.After(now) {
			writeErr(w, http.StatusBadRequest, "expires_at must be in the future")
			return
		}
		if at.After(now.Add(maxClientTokenLife)) {
			writeErr(w, http.StatusBadRequest, "expires_at must be at most 365 days ahead")
			return
		}
		exp = &at
	}
	// A token that expires mints nothing that outlives it: the new token
	// expires with it at the latest, and by default exactly then.
	if limit, ok := bearerExpiry(r.Context()); ok && (exp == nil || exp.After(limit)) {
		// The default is capped like a requested time: at most 365 days.
		if top := now.Add(maxClientTokenLife); limit.After(top) {
			limit = top
		}
		exp = &limit
	}
	pt, err := d.Users.IssueClientTokenUntil(r.Context(), userID(r.Context()), in.Name, exp)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issue failed")
		return
	}
	writeJSON(w, http.StatusCreated, newTokenResp{Name: in.Name, Token: pt, ExpiresAt: exp})
}

// RevokeToken deletes one of the caller's tokens and ends the sessions that
// are connected with it.
func (d Deps) RevokeToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	err := d.Users.RevokeClientToken(r.Context(), id, userID(r.Context()))
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	// A token that is gone must not keep a tunnel up.
	d.closeClientSessions(id)
	w.WriteHeader(http.StatusNoContent)
}

// closeClientSessions ends the live sessions of the client tokens.
func (d Deps) closeClientSessions(tokenIDs ...string) {
	if d.ClientSessions == nil {
		return
	}
	for _, id := range tokenIDs {
		if n := d.ClientSessions.CloseByToken(id); n > 0 {
			d.Log.Info("client sessions closed", "token_id", id, "sessions", n)
		}
	}
}

// clientTokenIDs returns the ids of the user's client tokens.
func (d Deps) clientTokenIDs(r *http.Request, userID string) ([]string, error) {
	ts, err := d.Users.ListClientTokens(r.Context(), userID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	return ids, nil
}
