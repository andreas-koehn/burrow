package proxy

// gate_services.go — the gate with a services origin (BURROW_SERVICES_DOMAIN).
//
// The tunnelled apps live on the services host, the dashboard on the auth
// domain. The dashboard's cookies are host-only there and never reach the
// services host, so a burrow_login service needs a session of its own on the
// services host. Nobody types a password on the services host (a tunnelled
// app shares that origin and could read the form); instead:
//
//  1. The access checker sends a visitor without a services session to
//     https://<services>/__burrow/login?next=<service URL>, which sends them on
//     to https://<dashboard>/__burrow/handoff?return=<service URL>.
//  2. The dashboard host needs its own session (it shows a sign-in form when
//     there is none), then issues a one-time code: 60 s, single use, in memory,
//     bound to the user and to the exact return address. It redirects to
//     https://<services>/__burrow/callback?code=…&return=….
//  3. The callback takes the code, creates a session row for the user and sets
//     the host-only cookie burrow_svc_session (HttpOnly, Secure, SameSite=Lax),
//     then redirects to the return address.
//
// Every return address must be a canonical /svc/<slug> address on the services
// host (svcURLOn), at the handoff and at the callback.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

const (
	// handoffCodeTTL is how long a handoff code can be exchanged.
	handoffCodeTTL = 60 * time.Second
	// handoffCodesMax caps the codes held at once; only signed-in users can
	// make the relay issue one, so the cap is a backstop, not a limit anyone
	// meets.
	handoffCodesMax = 10000
)

// registerServicesRoutes registers the services-origin routes. Each handler
// answers on its own host only and 404s on the other one.
func (g *Gate) registerServicesRoutes(limiter func(http.Handler) http.Handler) {
	g.mux.HandleFunc("GET /__burrow/login", g.handleServicesLogin)
	// No password form on the services host: an explicit 404 instead of
	// the mux's 405 for a POST to a GET route.
	g.mux.HandleFunc("POST /__burrow/login", http.NotFound)
	g.mux.Handle("POST /__burrow/logout", limiter(http.HandlerFunc(g.handleServicesLogout)))
	g.mux.HandleFunc("GET /__burrow/callback", g.handleCallback)
	g.mux.HandleFunc("GET /__burrow/handoff", g.handleHandoff)
	g.mux.Handle("POST /__burrow/handoff", limiter(http.HandlerFunc(g.handlePostHandoff)))
}

// onServicesHost reports whether the request arrived on the services host.
func (g *Gate) onServicesHost(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(host, g.servicesDomain)
}

// dashboardRoot is where a visitor goes when there is nowhere better.
func (g *Gate) dashboardRoot() string { return "https://" + g.authDomain + "/" }

// privatePage marks an answer that carries or leads to a one-time code: never
// cached, never framed, and its address never passed on as a referrer.
func privatePage(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
}

// sessionUser returns the active user of the session in the named cookie.
func (g *Gate) sessionUser(r *http.Request, cookie string) (db.User, bool) {
	c, err := r.Cookie(cookie)
	if err != nil || c.Value == "" {
		return db.User{}, false
	}
	uid, err := g.st.ValidateSession(r.Context(), c.Value)
	if err != nil {
		return db.User{}, false
	}
	user, err := g.st.GetUserByID(r.Context(), uid)
	if err != nil || user.Status == "suspended" {
		return db.User{}, false
	}
	return user, true
}

// handleServicesLogin is GET /__burrow/login on the services host: on with a
// services session (or the access-denied page for a role the service does not
// allow), to the dashboard's handoff without one.
func (g *Gate) handleServicesLogin(w http.ResponseWriter, r *http.Request) {
	if !g.onServicesHost(r) {
		http.NotFound(w, r)
		return
	}
	next, ok := svcURLOn(r.URL.Query().Get("next"), g.servicesDomain)
	if !ok {
		http.Redirect(w, r, g.dashboardRoot(), http.StatusFound)
		return
	}
	if user, ok := g.sessionUser(r, ServicesSessionCookie); ok {
		if svc, err := g.st.ServiceForSubdomain(r.Context(), slugFromNext(next)); err == nil && !svc.GatewayOnly {
			if allowed, err := g.st.RoleAllowed(r.Context(), svc.ID, user.Role); err == nil && !allowed {
				g.renderAccessDenied(w, r, user, svc.Name)
				return
			}
		}
		// Allowed, or no such service: the proxy decides again.
		//nolint:gosec // G710: next is svcURLOn-validated to https + the services host + a /svc/ path.
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	http.Redirect(w, r, "https://"+g.authDomain+"/__burrow/handoff?return="+url.QueryEscape(next), http.StatusFound)
}

// handleServicesLogout is POST /__burrow/logout on the services host. It ends
// the services session only; the dashboard session is the dashboard's. As in
// single-origin mode, only a request that carries the cookie changes anything.
func (g *Gate) handleServicesLogout(w http.ResponseWriter, r *http.Request) {
	if !g.onServicesHost(r) {
		http.NotFound(w, r)
		return
	}
	if c, err := r.Cookie(ServicesSessionCookie); err == nil && c.Value != "" {
		_ = g.st.DeleteSession(r.Context(), c.Value)
		g.setServicesCookie(w, "", -1)
	}
	http.Redirect(w, r, g.dashboardRoot(), http.StatusFound)
}

// handleHandoff is GET /__burrow/handoff on the dashboard's host: a code for
// a signed-in user, the sign-in form for anybody else.
func (g *Gate) handleHandoff(w http.ResponseWriter, r *http.Request) {
	if g.onServicesHost(r) {
		http.NotFound(w, r)
		return
	}
	privatePage(w)
	ret, ok := svcURLOn(r.URL.Query().Get("return"), g.servicesDomain)
	if !ok {
		http.Error(w, "invalid return address", http.StatusBadRequest)
		return
	}
	user, ok := g.sessionUser(r, gateSessionCookieName)
	if !ok {
		g.renderHandoffLogin(w, ret, "")
		return
	}
	g.issueHandoff(w, r, user, ret)
}

// handlePostHandoff is the sign-in form of the handoff on the dashboard's
// host. It signs in exactly as the dashboard does (session and CSRF cookie on
// the dashboard's host), then continues with a code.
func (g *Gate) handlePostHandoff(w http.ResponseWriter, r *http.Request) {
	if g.onServicesHost(r) {
		http.NotFound(w, r)
		return
	}
	privatePage(w)
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ret, ok := svcURLOn(r.FormValue("return"), g.servicesDomain)
	if !ok {
		http.Error(w, "invalid return address", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	user, ok := g.checkCredentials(r.Context(), email, r.FormValue("password"))
	if !ok {
		g.renderHandoffLogin(w, ret, "Invalid email or password")
		return
	}
	sid, err := g.st.CreateSession(r.Context(), user.ID, r.UserAgent(), clientHost(r))
	if err != nil {
		g.log.Error("gate: create session error", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	g.setSessionCookie(w, sid)
	if token, err := newCSRFToken(); err != nil {
		g.log.Error("gate: csrf token error", "err", err)
	} else {
		g.setCSRFCookie(w, token)
	}
	g.log.Info("gate: login success", "email", email)
	g.issueHandoff(w, r, user, ret)
}

// checkCredentials returns the active user with this email and password.
func (g *Gate) checkCredentials(ctx context.Context, email, password string) (db.User, bool) {
	ok, err := g.st.VerifyUserPassword(ctx, email, password)
	if err != nil {
		g.log.Error("gate: verify password error", "err", err)
		return db.User{}, false
	}
	if !ok {
		g.log.Warn("gate: login failed", "email", email)
		return db.User{}, false
	}
	user, err := g.st.GetUserByEmail(ctx, email)
	if err != nil || user.Status == "suspended" {
		g.log.Warn("gate: login refused", "email", email)
		return db.User{}, false
	}
	return user, true
}

// issueHandoff issues a code for user and ret and sends the browser to the
// callback on the services host.
func (g *Gate) issueHandoff(w http.ResponseWriter, r *http.Request, user db.User, ret string) {
	code, err := g.codes.issue(user.ID, ret, g.now())
	if err != nil {
		g.log.Warn("gate: handoff code not issued", "err", err)
		http.Error(w, "try again in a minute", http.StatusServiceUnavailable)
		return
	}
	target := "https://" + g.servicesDomain + "/__burrow/callback?code=" + code + "&return=" + url.QueryEscape(ret)
	//nolint:gosec // G710: the services host is configuration, ret is svcURLOn-validated, code is hex.
	http.Redirect(w, r, target, http.StatusFound)
}

// handleCallback is GET /__burrow/callback on the services host: it exchanges
// the code for a services session and sends the browser to the return address.
func (g *Gate) handleCallback(w http.ResponseWriter, r *http.Request) {
	if !g.onServicesHost(r) {
		http.NotFound(w, r)
		return
	}
	privatePage(w)
	q := r.URL.Query()
	ret, ok := svcURLOn(q.Get("return"), g.servicesDomain)
	if !ok {
		http.Error(w, "invalid return address", http.StatusBadRequest)
		return
	}
	uid, ok := g.codes.take(q.Get("code"), ret, g.now())
	if !ok {
		http.Error(w, "This sign-in link is not valid or has expired. Open the service again.", http.StatusBadRequest)
		return
	}
	user, err := g.st.GetUserByID(r.Context(), uid)
	if err != nil || user.Status == "suspended" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	sid, err := g.st.CreateSession(r.Context(), user.ID, r.UserAgent(), clientHost(r))
	if err != nil {
		g.log.Error("gate: create services session error", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	g.setServicesCookie(w, sid, int(gateSessionMaxAge.Seconds()))
	audit.LogAuthHandoff(r.Context(), g.audit, audit.LogContext{
		ActorID: user.ID, ActorEmail: user.Email, SourceIP: clientHost(r), UserAgent: r.UserAgent(),
	}, g.servicesDomain, slugFromNext(ret))
	//nolint:gosec // G710: ret is svcURLOn-validated to https + the services host + a /svc/ path.
	http.Redirect(w, r, ret, http.StatusFound)
}

// setServicesCookie sets (maxAge > 0) or clears (maxAge < 0) the services
// session cookie: host-only on the services host, HttpOnly, SameSite=Lax, and
// always Secure, since every services-host address Burrow builds is https.
func (g *Gate) setServicesCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     ServicesSessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	})
}

func (g *Gate) renderHandoffLogin(w http.ResponseWriter, ret, alert string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = loginTmpl.Execute(w, loginData{
		ServiceLabel: slugFromNext(ret),
		Next:         ret,
		AlertMessage: alert,
		Action:       "/__burrow/handoff",
		NextField:    "return",
	})
}

// clientHost is the client address of r without its port.
func clientHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handoffCodes holds the issued, not yet exchanged handoff codes. They live in
// memory: a code is exchanged within a minute on the relay that issued it.
type handoffCodes struct {
	mu sync.Mutex
	m  map[string]handoffCode
}

type handoffCode struct {
	userID  string
	ret     string
	expires time.Time
}

var errHandoffFull = errors.New("too many handoff codes outstanding")

func newHandoffCodes() *handoffCodes { return &handoffCodes{m: map[string]handoffCode{}} }

// issue returns a new code for userID, good for ret until now+handoffCodeTTL.
func (c *handoffCodes) issue(userID, ret string, now time.Time) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := hex.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.m {
		if !now.Before(v.expires) {
			delete(c.m, k)
		}
	}
	if len(c.m) >= handoffCodesMax {
		return "", errHandoffFull
	}
	c.m[code] = handoffCode{userID: userID, ret: ret, expires: now.Add(handoffCodeTTL)}
	return code, nil
}

// take spends code: whatever the outcome, it cannot be presented again. It
// returns the user the code was issued to when the code is known, not
// expired, and was issued for exactly ret.
func (c *handoffCodes) take(code, ret string, now time.Time) (string, bool) {
	if code == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[code]
	delete(c.m, code)
	if !ok || !now.Before(e.expires) || e.ret != ret {
		return "", false
	}
	return e.userID, true
}
