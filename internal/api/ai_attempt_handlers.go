package api

// ai_attempt_handlers.go — /ai/requests/{requestID}/attempts: what one
// gateway request tried.
//
// The attempt log names, per attempt, the provider, the provider's own model
// id, the upstream status, a failure code and how long it took. It holds no
// credential, slot name, header or upstream body, and the response adds
// nothing to it.

import (
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
)

// aiAttemptResp is one attempt of a gateway request. position is the order
// the attempts were made in; it is not the position of a target in the
// model's list (a provider with several credential slots is tried once per
// slot, and targets the endpoint does not fit are left out).
type aiAttemptResp struct {
	Position   int       `json:"position"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Status     int       `json:"status"` // upstream HTTP status, 0 when none was received
	ErrorCode  string    `json:"error_code"`
	DurationMs int64     `json:"duration_ms"`
	Ts         time.Time `json:"ts"`
}

const (
	// maxAttemptRequestID bounds the id that is looked up; the relay's own
	// ids are far shorter.
	maxAttemptRequestID = 128

	msgAttemptRequestID = "request id must be 1-128 characters without control characters"
)

// attemptRequestID reads the request id from the path. The relay's ids hold a
// "/", which a caller sends as %2F; the router then matches the escaped path
// and hands the segment over still escaped.
func attemptRequestID(r *http.Request) (string, bool) {
	id := chi.URLParam(r, "requestID")
	if r.URL.RawPath != "" {
		unescaped, err := url.PathUnescape(id)
		if err != nil {
			return "", false
		}
		id = unescaped
	}
	if id == "" || len(id) > maxAttemptRequestID || strings.ContainsFunc(id, unicode.IsControl) {
		return "", false
	}
	return id, true
}

// GetAIRequestAttempts handles GET /api/v1/ai/requests/{requestID}/attempts
// (an admin's dashboard session).  The log covers the requests of every user
// and gateway key, so an automation token does not open it, whatever it
// declares.  An id without a log answers an empty list: a log is kept only
// for requests that needed more than one attempt or failed.
func (d Deps) GetAIRequestAttempts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if bearerTokenID(r.Context()) != "" {
		writeErr(w, http.StatusForbidden, "a dashboard session is required")
		return
	}
	id, ok := attemptRequestID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, msgAttemptRequestID)
		return
	}
	out := []aiAttemptResp{}
	if d.AIAttempts != nil {
		rows, err := d.AIAttempts.AttemptsForRequest(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, a := range rows { // ordered by position
			out = append(out, aiAttemptResp{
				Position: a.Position, Provider: a.ProviderSlug, Model: a.TargetModel,
				Status: a.Status, ErrorCode: a.ErrorCode, DurationMs: a.DurationMs, Ts: a.Ts,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
