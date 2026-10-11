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
		out = append(out, tokenResp{ID: t.ID, Name: t.Name, LastUsed: t.LastUsed, CreatedAt: t.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

type createTokenReq struct {
	Name string `json:"name"`
}

// newTokenResp is returned by CreateToken — name and the one-time plaintext.
type newTokenResp struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// CreateToken mints a new client token and returns the plaintext exactly once.
func (d Deps) CreateToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in createTokenReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	pt, err := d.Users.IssueClientToken(r.Context(), userID(r.Context()), in.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issue failed")
		return
	}
	writeJSON(w, http.StatusCreated, newTokenResp{Name: in.Name, Token: pt})
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
