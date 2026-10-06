package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"

	"github.com/ankoehn/burrow/internal/api/install"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// Browser-approved client sign-in (the shape of RFC 8628). Five endpoints:
//
//	POST /client/login/start                         client, no authentication
//	POST /client/login/poll                          client, no authentication
//	GET  /client/login/requests/{user_code}          dashboard session
//	POST /client/login/requests/{user_code}/approve  dashboard session + client-token permission
//	POST /client/login/requests/{user_code}/deny     dashboard session
//
// start and poll are public because a client has no token yet; they are
// rate-limited per source IP and say nothing about users. The other three sit
// in the session group, behind the CSRF check, and refuse automation bearer
// tokens: approving a sign-in is something a person does in the dashboard.

const (
	// ClientLoginStartRateLimitPerIP is how many sign-in requests one source
	// IP may start per minute.
	ClientLoginStartRateLimitPerIP = 10
	// ClientLoginPollRateLimitPerIP is how many polls one source IP may send
	// per minute. One client polls every two seconds: thirty a minute.
	ClientLoginPollRateLimitPerIP = 60
	// ClientLoginGuessLimit is how many wrong user codes one user may send
	// per minute before the request endpoints refuse that user.
	ClientLoginGuessLimit = 20

	clientLoginBodyLimit = 4096
)

// ClientLoginStore is the sign-in request surface. *store.Store satisfies it.
type ClientLoginStore interface {
	StartClientLogin(ctx context.Context, m store.LoginMeta) (store.LoginStart, error)
	GetClientLogin(ctx context.Context, userCode string) (store.LoginRequestView, error)
	ApproveClientLogin(ctx context.Context, userCode, approverID, tokenName string) (store.LoginRequestView, error)
	DenyClientLogin(ctx context.Context, userCode, userID string) (store.LoginRequestView, error)
	PollClientLogin(ctx context.Context, deviceCode string) (store.LoginResult, error)
}

// clientLoginRateLimiters builds the per-IP limiters of start and poll.
// Per-IP accuracy comes from TrustedProxyMiddleware, as for login.
func (d Deps) clientLoginRateLimiters() (start, poll func(http.Handler) http.Handler) {
	build := func(limit, override int) func(http.Handler) http.Handler {
		if override > 0 {
			limit = override
		}
		return httprate.Limit(limit, time.Minute,
			httprate.WithKeyFuncs(httprate.KeyByIP),
			httprate.WithLimitHandler(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				writeErr(w, http.StatusTooManyRequests, "too many requests")
			}),
		)
	}
	return build(ClientLoginStartRateLimitPerIP, d.ClientLoginStartLimitOverride),
		build(ClientLoginPollRateLimitPerIP, d.ClientLoginPollLimitOverride)
}

// clientLoginAnon prepares a request of the two public endpoints: never
// cacheable, and with the caller's address in the audit context (there is no
// actor).
func (d Deps) clientLoginAnon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		lc := audit.LogContext{
			SourceIP:  remoteIP(r),
			UserAgent: r.UserAgent(),
			RequestID: middleware.GetReqID(r.Context()),
		}
		next.ServeHTTP(w, r.WithContext(audit.WithLogContext(r.Context(), lc)))
	})
}

// remoteIP is the caller's address without the port, as
// TrustedProxyMiddleware left it.
func remoteIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

type clientLoginStartReq struct {
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	ClientVersion string `json:"client_version"`
	TokenName     string `json:"token_name"`
}

type clientLoginStartResp struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// PostClientLoginStart opens a sign-in request. POST /client/login/start, no
// authentication. The strings in the body are untrusted; the store cleans and
// caps them.
func (d Deps) PostClientLoginStart(w http.ResponseWriter, r *http.Request) {
	if d.ClientLogins == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, clientLoginBodyLimit)
	var in clientLoginStartReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// The link the user opens points at this relay as the client reached it.
	// The scheme is the relay's own knowledge, not a forwarded header (see
	// installParams).
	scheme := "http"
	if r.TLS != nil || d.HTTPSEnabled || d.SecureCookies {
		scheme = "https"
	}
	origin, ok := install.Origin(scheme, r.Host)
	if !ok {
		writeErr(w, http.StatusBadRequest, "this address cannot be used to sign in; ask by the relay's host name")
		return
	}
	st, err := d.ClientLogins.StartClientLogin(r.Context(), store.LoginMeta{
		Hostname: in.Hostname, OS: in.OS, Arch: in.Arch, ClientVersion: in.ClientVersion,
		SourceIP: remoteIP(r), TokenName: in.TokenName,
	})
	if errors.Is(err, store.ErrLoginTooMany) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "too many pending sign-in requests")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "start failed")
		return
	}
	writeJSON(w, http.StatusOK, clientLoginStartResp{
		DeviceCode:      st.DeviceCode,
		UserCode:        st.UserCode,
		VerificationURL: origin + "/link?code=" + st.UserCode,
		ExpiresIn:       st.ExpiresIn,
		Interval:        st.Interval,
	})
}

type clientLoginPollReq struct {
	DeviceCode string `json:"device_code"`
}

type clientLoginPollResp struct {
	Token     string `json:"token"`
	TokenName string `json:"token_name"`
	Email     string `json:"email"`
}

// PostClientLoginPoll is the client asking for the outcome of its request.
// POST /client/login/poll, no authentication: the device code is the
// credential.
//
//	202 {"status":"pending"}       no decision yet
//	200 {token, token_name, email} approved; given once
//	403 {"error":"access_denied"}  denied
//	410 {"error":"expired_token"}  unknown, expired or collected already
//	429 {"error":"slow_down"}      polled less than the interval ago
func (d Deps) PostClientLoginPoll(w http.ResponseWriter, r *http.Request) {
	if d.ClientLogins == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, clientLoginBodyLimit)
	var in clientLoginPollReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.DeviceCode == "" {
		writeErr(w, http.StatusBadRequest, "device_code is required")
		return
	}
	res, err := d.ClientLogins.PollClientLogin(r.Context(), in.DeviceCode)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, clientLoginPollResp{Token: res.Token, TokenName: res.TokenName, Email: res.Email})
	case errors.Is(err, store.ErrLoginPending):
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
	case errors.Is(err, store.ErrLoginSlowDown):
		w.Header().Set("Retry-After", "2")
		writeErr(w, http.StatusTooManyRequests, "slow_down")
	case errors.Is(err, store.ErrLoginDenied):
		writeErr(w, http.StatusForbidden, "access_denied")
	case errors.Is(err, store.ErrLoginNotFound):
		writeErr(w, http.StatusGone, "expired_token")
	default:
		writeErr(w, http.StatusInternalServerError, "poll failed")
	}
}

// clientLoginRequestResp is a sign-in request as the dashboard shows it.
// Every string in it that came from the client is plain text to be rendered
// as text.
type clientLoginRequestResp struct {
	UserCode           string    `json:"user_code"`
	Hostname           string    `json:"hostname"`
	OS                 string    `json:"os"`
	Arch               string    `json:"arch"`
	ClientVersion      string    `json:"client_version"`
	SourceIP           string    `json:"source_ip"`
	Status             string    `json:"status"`
	SuggestedTokenName string    `json:"suggested_token_name"`
	AgeSeconds         int       `json:"age_seconds"`
	CreatedAt          time.Time `json:"created_at"`
	ExpiresAt          time.Time `json:"expires_at"`
}

func clientLoginRequestOf(v store.LoginRequestView) clientLoginRequestResp {
	return clientLoginRequestResp{
		UserCode: v.UserCode, Hostname: v.Hostname, OS: v.OS, Arch: v.Arch,
		ClientVersion: v.ClientVersion, SourceIP: v.SourceIP, Status: v.Status,
		SuggestedTokenName: v.SuggestedTokenName,
		AgeSeconds:         int(v.Age / time.Second),
		CreatedAt:          v.CreatedAt, ExpiresAt: v.ExpiresAt,
	}
}

// requireDashboardSession lets cookie sessions through and refuses automation
// bearer tokens. A bearer request skips the CSRF check by design, and
// approving a sign-in mints a client token: that is for a person in the
// dashboard to do. It also carries the wrong-code gate of the three request
// endpoints and marks their answers as not cacheable.
func (d Deps) requireDashboardSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if bearerTokenID(r.Context()) != "" {
			writeErr(w, http.StatusForbidden, "a dashboard session is required")
			return
		}
		if d.ClientLogins == nil {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		if d.clientLoginGuesses.blocked(userID(r.Context())) {
			w.Header().Set("Retry-After", "60")
			writeErr(w, http.StatusTooManyRequests, "too many wrong codes; try again in a minute")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireClientTokensManage gates the approval on the permission to manage
// client tokens (tokens:manage:own or :any; admin always passes): approving
// a sign-in creates a client token for the approver.
func (d Deps) requireClientTokensManage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, err := d.callerRoleForAuth(r)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if role == "admin" ||
			effectivePerms(r.Context(), role, authz.PermTokensManageAny) ||
			effectivePerms(r.Context(), role, authz.PermTokensManageOwn) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "tokens:manage required")
	})
}

// writeClientLoginErr maps a store error of the three request endpoints. A
// wrong code and an expired one are the same 404, and each counts against the
// caller's allowance of wrong codes.
func (d Deps) writeClientLoginErr(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	switch {
	case errors.Is(err, store.ErrLoginNotFound):
		d.clientLoginGuesses.fail(userID(r.Context()))
		writeErr(w, http.StatusNotFound, "sign-in request not found or expired")
	case errors.Is(err, store.ErrLoginConflict):
		writeErr(w, http.StatusConflict, "sign-in request was already decided")
	case errors.Is(err, store.ErrLoginTokenName):
		writeErr(w, http.StatusBadRequest, "token_name must be 1 to 120 characters without control characters")
	case errors.Is(err, store.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden")
	default:
		writeErr(w, http.StatusInternalServerError, fallback)
	}
}

// GetClientLoginRequest shows a sign-in request to a signed-in user.
// GET /client/login/requests/{user_code}.
func (d Deps) GetClientLoginRequest(w http.ResponseWriter, r *http.Request) {
	v, err := d.ClientLogins.GetClientLogin(r.Context(), chi.URLParam(r, "user_code"))
	if err != nil {
		d.writeClientLoginErr(w, r, err, "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, clientLoginRequestOf(v))
}

type clientLoginApproveReq struct {
	TokenName string `json:"token_name"`
}

// PostClientLoginApprove approves a pending sign-in request in the caller's
// name. POST /client/login/requests/{user_code}/approve. The client token is
// minted when the client collects it, for this user, with this name.
func (d Deps) PostClientLoginApprove(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, clientLoginBodyLimit)
	var in clientLoginApproveReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "token_name is required")
		return
	}
	v, err := d.ClientLogins.ApproveClientLogin(r.Context(), chi.URLParam(r, "user_code"), userID(r.Context()), in.TokenName)
	if err != nil {
		d.writeClientLoginErr(w, r, err, "approve failed")
		return
	}
	writeJSON(w, http.StatusOK, clientLoginRequestOf(v))
}

// PostClientLoginDeny denies a pending sign-in request.
// POST /client/login/requests/{user_code}/deny. No body.
func (d Deps) PostClientLoginDeny(w http.ResponseWriter, r *http.Request) {
	v, err := d.ClientLogins.DenyClientLogin(r.Context(), chi.URLParam(r, "user_code"), userID(r.Context()))
	if err != nil {
		d.writeClientLoginErr(w, r, err, "deny failed")
		return
	}
	writeJSON(w, http.StatusOK, clientLoginRequestOf(v))
}

// guessLimiter counts wrong user codes per user in a fixed window. At the
// limit the user is refused until the window ends, right codes included, so
// that guessing gains nothing in that time. Keyed by user, not by session: a
// second session does not buy a second allowance.
type guessLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	seen   map[string]guessWindow
}

type guessWindow struct {
	start time.Time
	n     int
}

func newGuessLimiter(limit int, window time.Duration, now func() time.Time) *guessLimiter {
	if now == nil {
		now = time.Now
	}
	return &guessLimiter{limit: limit, window: window, now: now, seen: map[string]guessWindow{}}
}

// blocked reports whether the user has used up the allowance of this window.
// A nil limiter blocks nobody.
func (g *guessLimiter) blocked(user string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	w, ok := g.seen[user]
	if !ok {
		return false
	}
	if g.now().Sub(w.start) >= g.window {
		delete(g.seen, user)
		return false
	}
	return w.n >= g.limit
}

// fail records one wrong code of the user.
func (g *guessLimiter) fail(user string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	// Keep the map as small as the number of users who guessed lately.
	if len(g.seen) >= 1024 {
		for k, w := range g.seen {
			if now.Sub(w.start) >= g.window {
				delete(g.seen, k)
			}
		}
	}
	w, ok := g.seen[user]
	if !ok || now.Sub(w.start) >= g.window {
		w = guessWindow{start: now}
	}
	w.n++
	g.seen[user] = w
}
