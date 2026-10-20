package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
)

// ReauthWindow is how long a password confirmation (POST /auth/reauth) keeps
// the guarded routes open for the session that gave it.
const ReauthWindow = 10 * time.Minute

// reauthRequired is the error code of a guarded route called outside the
// window. The dashboard answers it with its password dialog.
const reauthRequired = "reauth_required"

// sessionRef names a session in the API and keys the re-authentication
// windows; see audit.SessionRef.
func sessionRef(sid string) string { return audit.SessionRef(sid) }

// reauthWindows remembers, in memory, which sessions confirmed their password
// recently. A restart closes every window, which is the safe direction.
type reauthWindows struct {
	mu    sync.Mutex
	now   func() time.Time
	until map[string]time.Time
}

func newReauthWindows(now func() time.Time) *reauthWindows {
	if now == nil {
		now = time.Now
	}
	return &reauthWindows{now: now, until: map[string]time.Time{}}
}

// open starts a window for the session and returns when it ends.
func (w *reauthWindows) open(ref string) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	for k, t := range w.until {
		if !t.After(now) {
			delete(w.until, k)
		}
	}
	end := now.Add(ReauthWindow)
	w.until[ref] = end
	return end
}

// forget closes the windows of the given sessions.
func (w *reauthWindows) forget(refs ...string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range refs {
		delete(w.until, r)
	}
}

// active reports whether the session's window is still open. A nil value has
// no windows.
func (w *reauthWindows) active(ref string) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.until[ref]
	return ok && w.now().Before(t)
}

// recentAuth reports whether the request's session confirmed its password
// within the window. Always false for a request without a session cookie.
func (d Deps) recentAuth(r *http.Request) bool {
	sid := sessionID(r)
	if sid == "" {
		return false
	}
	return d.reauth.active(sessionRef(sid))
}

// RequireRecentAuth guards a route that mints a secret or changes who may do
// what. While no services domain is set a tunnelled app shares the
// dashboard's origin and can send any write as the signed-in visitor; these
// routes then need the password confirmed within the last ten minutes
// (POST /auth/reauth) and answer 403 {"error":"reauth_required"} otherwise.
// With a services domain no app runs on the dashboard's origin and the
// guard is off. Bearer (automation token) requests are exempt: they carry no
// cookie that a page could ride on. It belongs after the session and CSRF
// checks of a route.
func (d Deps) RequireRecentAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.ServicesDomain != "" || bearerTokenID(r.Context()) != "" || d.recentAuth(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, reauthRequired)
	})
}

type reauthReq struct {
	Password string `json:"password"`
}

type reauthResp struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// PostReauth confirms the caller's password and opens the re-authentication
// window for the session. POST /auth/reauth. Rate-limited like sign-in.
func (d Deps) PostReauth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in reauthReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Password == "" {
		writeErr(w, http.StatusBadRequest, "password required")
		return
	}
	// The window belongs to a browser session; a token has none.
	sid := sessionID(r)
	if bearerTokenID(r.Context()) != "" || sid == "" {
		writeErr(w, http.StatusForbidden, "a dashboard session is required")
		return
	}
	u, err := d.Users.GetUserByID(r.Context(), userID(r.Context()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reauth failed")
		return
	}
	// Wrong passwords are counted per session (this one and the password
	// change share the count), next to the per-address limit. Not per user: a
	// script on the dashboard's origin sends requests as the signed-in user
	// and would otherwise lock the user out everywhere.
	ref := sessionRef(sid)
	if d.reauthGuesses.blocked(ref) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "too many wrong passwords; try again in a minute")
		return
	}
	ok, err := d.Users.VerifyUserPassword(r.Context(), u.Email, in.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reauth failed")
		return
	}
	if !ok {
		d.noteWrongPassword(ref, u.ID)
		d.auditAuth(r, audit.ActionAuthReauth, u.ID, u.Email, "denied")
		writeErr(w, http.StatusForbidden, "password is incorrect")
		return
	}
	end := d.reauth.open(sessionRef(sid))
	d.auditAuth(r, audit.ActionAuthReauth, u.ID, u.Email, "ok")
	writeJSON(w, http.StatusOK, reauthResp{ExpiresAt: end.UTC()})
}

// ReauthGuessLimit is how many wrong passwords one session may send to
// /auth/reauth and /auth/change-password together per minute.
const ReauthGuessLimit = 10

// ReauthUserCeiling is how many wrong passwords one user's sessions together
// may send per hour before the relay flags it in the audit log. It never
// refuses anything: a refusal keyed by user would let a script lock the
// user out.
const ReauthUserCeiling = 100

// noteWrongPassword counts a wrong password against the session and the user.
func (d Deps) noteWrongPassword(sessionKey, uid string) {
	if sessionKey != "" {
		d.reauthGuesses.fail(sessionKey)
	}
	d.reauthUserWrong.fail(uid)
}

// overUserCeiling reports whether the user is past ReauthUserCeiling.
func (d Deps) overUserCeiling(uid string) bool { return d.reauthUserWrong.blocked(uid) }

func (d Deps) auditAuth(r *http.Request, action, id, email, result string) {
	if d.AuditAppender == nil {
		return
	}
	var payload json.RawMessage
	if d.overUserCeiling(id) {
		payload = json.RawMessage(`{"over_user_ceiling":true}`)
	}
	lc := audit.LogContextFrom(r.Context())
	_ = d.AuditAppender.Append(r.Context(), audit.Event{
		ActorID: id, ActorEmail: email, Action: action,
		SubjectID: id, SubjectLabel: email, Result: result,
		SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID, Payload: payload,
	})
}
