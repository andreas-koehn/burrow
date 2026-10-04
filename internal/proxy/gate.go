// Package proxy — gate.go
//
// # Burrow-Login Forward-Auth Gate (Task 9)
//
// The Gate is an HTTP handler mounted at /__burrow/* on the auth domain. It is
// reached when the proxy's AccessChecker (Task 8) redirects a visitor hitting a
// "burrow_login" service to:
//
//	https://<authDomain>/__burrow/login?next=<url-encoded-original-url>
//
// The Gate renders a Burrow-account login form, authenticates the submission
// using the same argon2 + session primitives as the dashboard API, sets a
// host-only session cookie on the auth domain, and 302s back to the service.
// Services are served under /svc/<slug>/ on that same origin, so the cookie
// reaches every one of them without a Domain attribute.
//
// # Security decisions
//
// CSRF: The Gate's POST endpoints do NOT use a separate CSRF token for v0.3.0.
// Justification:
//  1. The login form is served from the auth domain itself; a CSRF attack from a
//     third-party origin would require a cross-origin form POST, which browsers
//     block for authenticated sessions when SameSite=Lax is set on the session
//     cookie. Lax mode blocks cross-site POSTs originating from a non-same-site
//     context.
//  2. The per-IP rate limiter (10 req/min) caps credential-stuffing and brute-force
//     even if SameSite enforcement is absent (e.g. very old user-agents).
//  3. Logout does not modify sensitive data; the worst outcome is a forced sign-out.
//
// Known limitation: a legacy browser or proxy that does not enforce SameSite=Lax
// is theoretically vulnerable to a CSRF login (login CSRF). A form-token approach
// (HMAC-signed nonce in a short-lived cookie checked on POST) would close this
// gap and is deferred to v0.3.1.
//
// # Access-denied trigger
//
// The Gate performs role-policy checks on the GET /__burrow/login when a valid
// session cookie is present. If the user's role is excluded from the target
// service's policy, the Gate renders the access-denied HTML page (403) instead
// of the login form.
//
// Tradeoff: this is the "gate-side" check described in the Task 9 spec. The
// alternative ("AccessChecker-side") would require the proxy to look up the
// session on every proxied request — a worthwhile optimization for v0.3.1, but
// out of scope here. The current behavior means a logged-in user with the wrong
// role is redirected to the gate once and immediately sees the friendly error.
// A user with the correct role is redirected to the gate, which then immediately
// 302s them back to the service (one extra round-trip, acceptable for v0.3.0).
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/go-chi/httprate"

	"github.com/ankoehn/burrow/internal/db"
)

const (
	gateSessionCookieName = "burrow_session"
	gateCSRFCookieName    = "burrow_csrf"
	gateSessionMaxAge     = 7 * 24 * time.Hour
	// loginRateLimitPerIP mirrors the value from internal/api's LoginRateLimitPerIP
	// constant. The gate gets its own independent limiter instance (not shared
	// state) to keep coupling low, but uses the same N/period (10 req/min per IP).
	gateLoginLimitPerIP  = 10
	gateLoginLimitPeriod = time.Minute
)

// GateStore is the narrow interface the Gate requires from the store layer.
// *store.Store satisfies this interface implicitly (same method signatures).
// Tests use a local fake instead.
type GateStore interface {
	// VerifyUserPassword checks email/password and returns (true, nil) on match.
	VerifyUserPassword(ctx context.Context, email, password string) (bool, error)
	// GetUserByEmail returns the user row or db.ErrNotFound.
	GetUserByEmail(ctx context.Context, email string) (db.User, error)
	// GetUserByID returns the user row or db.ErrNotFound.
	GetUserByID(ctx context.Context, id string) (db.User, error)
	// CreateSession creates a new browser session and returns its ID.
	CreateSession(ctx context.Context, userID, ua, ip string) (id string, err error)
	// ValidateSession returns the owning userID or an error for invalid/expired.
	ValidateSession(ctx context.Context, id string) (userID string, err error)
	// DeleteSession removes the session with the given ID.
	DeleteSession(ctx context.Context, id string) error
	// ServiceForSubdomain returns the db.Service for the given subdomain.
	ServiceForSubdomain(ctx context.Context, sub string) (db.Service, error)
	// RoleAllowed reports whether the given role is in the service's access policy.
	RoleAllowed(ctx context.Context, serviceID, role string) (bool, error)
}

// Gate implements the burrow-login forward-auth gate. It satisfies http.Handler
// and is registered with the Proxy via proxy.WithGate.
type Gate struct {
	st         GateStore
	authDomain string
	secure     bool
	log        *slog.Logger
	mux        *http.ServeMux
}

// NewGate constructs a Gate and registers its routes on an internal ServeMux.
//
//   - st:         GateStore providing auth + session primitives.
//   - authDomain: the Burrow auth domain (e.g. "tunnels.example.com"). Used for
//     next-URL validation and the gate's own redirects.
//   - secure:     when true, cookies carry Secure=true (matches dashboard flag).
//   - log:        structured logger; must not be nil.
func NewGate(st GateStore, authDomain string, secure bool, log *slog.Logger) http.Handler {
	g := &Gate{
		st:         st,
		authDomain: authDomain,
		secure:     secure,
		log:        log,
		mux:        http.NewServeMux(),
	}

	// Build a per-IP rate limiter for the POST endpoints.
	// Mirror of internal/api/router.go loginRateLimiters() constructor.
	limiter := httprate.Limit(
		gateLoginLimitPerIP,
		gateLoginLimitPeriod,
		httprate.WithKeyFuncs(httprate.KeyByIP),
		httprate.WithLimitHandler(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		}),
	)

	g.mux.HandleFunc("GET /__burrow/login", g.handleGetLogin)
	g.mux.Handle("POST /__burrow/login", limiter(http.HandlerFunc(g.handlePostLogin)))
	g.mux.Handle("POST /__burrow/logout", limiter(http.HandlerFunc(g.handlePostLogout)))

	return g
}

// ServeHTTP dispatches to the registered routes.
func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mux.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Route handlers
// ---------------------------------------------------------------------------

// handleGetLogin renders the login form. If the visitor has a valid session
// cookie, the gate performs a role check against the target service:
//   - role allowed → 302 to validated next (bypasses the form entirely).
//   - role denied  → 403 access-denied HTML page.
//   - no session   → render the login form.
func (g *Gate) handleGetLogin(w http.ResponseWriter, r *http.Request) {
	nextRaw := r.URL.Query().Get("next")
	nextURL := g.sanitizeNext(nextRaw)

	// Check for an existing session cookie.
	if c, err := r.Cookie(gateSessionCookieName); err == nil && c.Value != "" {
		uid, err := g.st.ValidateSession(r.Context(), c.Value)
		if err == nil {
			// Valid session — look up the user and check role against target service.
			user, err := g.st.GetUserByID(r.Context(), uid)
			if err == nil && user.Status != "suspended" {
				// Derive service from the /svc/<slug>/ path of the next URL.
				label := slugFromNext(nextURL)
				if label != "" {
					svc, err := g.st.ServiceForSubdomain(r.Context(), label)
					if err == nil {
						allowed, err := g.st.RoleAllowed(r.Context(), svc.ID, user.Role)
						if err == nil {
							if !allowed {
								// Role not in policy → access-denied page.
								g.renderAccessDenied(w, r, user, svc.Name)
								return
							}
							// Role allowed → redirect back to service immediately.
							//nolint:gosec // G710: nextURL is sanitizeNext()-validated to https + authDomain + /svc/ path only; not attacker-controlled.
							http.Redirect(w, r, nextURL, http.StatusFound)
							return
						}
					}
				}
				// No service found or no label — redirect to next anyway (the service
				// access check is best-effort; the proxy will re-evaluate).
				//nolint:gosec // G710: nextURL is sanitizeNext()-validated to https + authDomain + /svc/ path only; not attacker-controlled.
				http.Redirect(w, r, nextURL, http.StatusFound)
				return
			}
		}
	}

	// No valid session (or suspended) → show login form.
	label := slugFromNext(nextURL)
	g.renderLogin(w, nextURL, label, "")
}

// handlePostLogin processes credential submissions.
func (g *Gate) handlePostLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	email := r.FormValue("email")
	password := r.FormValue("password")
	nextRaw := r.FormValue("next")
	nextURL := g.sanitizeNext(nextRaw)

	label := slugFromNext(nextURL)

	// Verify credentials.
	ok, err := g.st.VerifyUserPassword(r.Context(), email, password)
	if err != nil {
		g.log.Error("gate: verify password error", "err", err)
		g.renderLogin(w, nextURL, label, "Invalid email or password")
		return
	}
	if !ok {
		g.log.Warn("gate: login failed", "email", email)
		g.renderLogin(w, nextURL, label, "Invalid email or password")
		return
	}

	// Load the user to check suspension.
	user, err := g.st.GetUserByEmail(r.Context(), email)
	if err != nil {
		g.log.Error("gate: get user by email error", "err", err)
		g.renderLogin(w, nextURL, label, "Invalid email or password")
		return
	}
	if user.Status == "suspended" {
		g.log.Warn("gate: suspended user login attempt", "email", email)
		g.renderLogin(w, nextURL, label, "Invalid email or password")
		return
	}

	// Resolve client IP (same pattern as internal/api/auth_handlers.go Login).
	clientHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		clientHost = r.RemoteAddr
	}

	// Create session.
	sid, err := g.st.CreateSession(r.Context(), user.ID, r.UserAgent(), clientHost)
	if err != nil {
		g.log.Error("gate: create session error", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Set the host-only session cookie. The gate and every path-routed
	// service share the auth domain's origin, so no Domain attribute is
	// needed. The same MaxAge/flags as the dashboard cookie apply.
	g.setSessionCookie(w, sid)
	// On this origin the session cookie is also the dashboard session, and
	// the dashboard API rejects every mutation without the double-submit CSRF
	// cookie. Issue it exactly as the dashboard login does. A failure here
	// only costs dashboard mutations, so the service login still succeeds.
	if token, err := newCSRFToken(); err != nil {
		g.log.Error("gate: csrf token error", "err", err)
	} else {
		g.setCSRFCookie(w, token)
	}

	g.log.Info("gate: login success", "email", email)
	_ = g.st.DeleteSession // best-effort touch is not needed here

	//nolint:gosec // G710: nextURL is sanitizeNext()-validated to https + authDomain + /svc/ path only; not attacker-controlled.
	http.Redirect(w, r, nextURL, http.StatusFound)
}

// handlePostLogout deletes the session from the store and clears the session
// and CSRF cookies. Both happen only when the request carried the session
// cookie: the endpoint has no CSRF token, and SameSite=Lax withholds the
// cookie from a cross-site form POST, so such a request must not be able to
// sign the visitor out of the dashboard.
func (g *Gate) handlePostLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(gateSessionCookieName); err == nil && c.Value != "" {
		_ = g.st.DeleteSession(r.Context(), c.Value)
		g.clearSessionCookie(w)
		g.clearCSRFCookie(w)
	}
	http.Redirect(w, r, "https://"+g.authDomain+"/__burrow/login", http.StatusFound)
}

// ---------------------------------------------------------------------------
// Cookie helpers (mirror internal/api/cookies.go)
// ---------------------------------------------------------------------------

// setSessionCookie sets the burrow_session cookie. It is host-only (no Domain)
// because the gate and every path-routed service share the auth domain's
// origin; it replaces the dashboard cookie of the same name instead of
// shadowing it. All flags (HttpOnly, SameSite=Lax, Secure, MaxAge) mirror the
// dashboard cookie (internal/api/cookies.go).
func (g *Gate) setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     gateSessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(gateSessionMaxAge.Seconds()),
	})
}

// clearSessionCookie expires the host-only burrow_session cookie.
func (g *Gate) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     gateSessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// newCSRFToken returns a 32-byte random token as 64 hex chars. It mirrors
// generateCSRFToken in internal/api/cookies.go, which this package cannot
// import (internal/api imports internal/proxy).
func newCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// setCSRFCookie sets the dashboard's double-submit CSRF cookie with the same
// attributes as internal/api/cookies.go: readable by JavaScript, host-only.
func (g *Gate) setCSRFCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     gateCSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false, // JS-readable: required for the double-submit pattern
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(gateSessionMaxAge.Seconds()),
	})
}

// clearCSRFCookie expires the burrow_csrf cookie.
func (g *Gate) clearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     gateCSRFCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: false,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// ---------------------------------------------------------------------------
// Template rendering helpers
// ---------------------------------------------------------------------------

type loginData struct {
	ServiceLabel string
	Next         string
	AlertMessage string
}

type accessDeniedData struct {
	UserEmail    string
	UserRole     string
	ServiceLabel string
	LogoutAction string
}

func (g *Gate) renderLogin(w http.ResponseWriter, next, serviceLabel, alert string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 200 always — even for re-renders after failed login (it's a form page).
	w.WriteHeader(http.StatusOK)
	_ = loginTmpl.Execute(w, loginData{
		ServiceLabel: serviceLabel,
		Next:         next,
		AlertMessage: alert,
	})
}

func (g *Gate) renderAccessDenied(w http.ResponseWriter, r *http.Request, user db.User, serviceName string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = accessDeniedTmpl.Execute(w, accessDeniedData{
		UserEmail:    user.Email,
		UserRole:     user.Role,
		ServiceLabel: serviceName,
		LogoutAction: "https://" + g.authDomain + "/__burrow/logout",
	})
}

// ---------------------------------------------------------------------------
// next URL sanitisation
// ---------------------------------------------------------------------------

// sanitizeNext validates the `next` parameter. Accepts a URL iff:
//   - it is parseable,
//   - scheme == "https",
//   - host == authDomain exactly (no port, no other case, no trailing dot),
//   - the path is already in its canonical form (no dot segments, doubled
//     slashes, backslashes or percent-encoded separators) and lies under
//     /svc/<slug>, so the slug the gate checks is the one the browser lands on,
//   - no userinfo (user:password@ is stripped out)
//
// Any other value (off-domain, a subdomain of authDomain, http://, another
// path on the auth domain, unparseable) returns the safe default:
// "https://<authDomain>/".
func (g *Gate) sanitizeNext(raw string) string {
	fallback := "https://" + g.authDomain + "/"
	if raw == "" {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return fallback
	}
	if u.Host != g.authDomain {
		return fallback
	}
	// RawPath is set only when the path carries an encoding that differs from
	// the canonical one (%2F, %2e, ...). Browsers treat a backslash as a slash.
	if u.RawPath != "" || strings.Contains(u.Path, `\`) {
		return fallback
	}
	// "/svc" and "/svc/" clean to "/svc" and are rejected with the rest.
	clean := path.Clean(u.Path)
	if clean != strings.TrimSuffix(u.Path, "/") || !strings.HasPrefix(clean, "/svc/") {
		return fallback
	}
	// Strip userinfo for safety.
	u.User = nil
	return u.String()
}

// slugFromNext extracts the service slug from a validated next URL: the
// <slug> segment of its /svc/<slug>/ path. Returns "" for the fallback URL.
func slugFromNext(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Path, "/svc/")
	if !ok {
		return ""
	}
	slug, _, _ := strings.Cut(rest, "/")
	return slug
}
