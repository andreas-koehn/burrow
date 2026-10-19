package api

import (
	"crypto/sha256"
	"encoding/hex"
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

// sessionRef is the public, non-secret name of a session: a hash of the
// session id (the cookie value). It names a session in the API and keys the
// re-authentication windows, so the cookie value itself never leaves the
// browser's cookie jar.
func sessionRef(sid string) string {
	sum := sha256.Sum256([]byte("burrow-session-ref\x00" + sid))
	return hex.EncodeToString(sum[:16])
}

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
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return d.reauth.active(sessionRef(c.Value))
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
	c, err := r.Cookie(sessionCookieName)
	if bearerTokenID(r.Context()) != "" || err != nil || c.Value == "" {
		writeErr(w, http.StatusForbidden, "a dashboard session is required")
		return
	}
	u, err := d.Users.GetUserByID(r.Context(), userID(r.Context()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reauth failed")
		return
	}
	ok, err := d.Users.VerifyUserPassword(r.Context(), u.Email, in.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reauth failed")
		return
	}
	if !ok {
		d.auditReauth(r, u.ID, u.Email, "denied")
		writeErr(w, http.StatusForbidden, "password is incorrect")
		return
	}
	end := d.reauth.open(sessionRef(c.Value))
	d.auditReauth(r, u.ID, u.Email, "ok")
	writeJSON(w, http.StatusOK, reauthResp{ExpiresAt: end.UTC()})
}

func (d Deps) auditReauth(r *http.Request, id, email, result string) {
	if d.AuditAppender == nil {
		return
	}
	lc := audit.LogContextFrom(r.Context())
	_ = d.AuditAppender.Append(r.Context(), audit.Event{
		ActorID: id, ActorEmail: email, Action: audit.ActionAuthReauth,
		SubjectID: id, SubjectLabel: email, Result: result,
		SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
	})
}
