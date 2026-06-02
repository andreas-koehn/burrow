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
// using the same argon2 + session primitives as the dashboard API, sets an
// auth-domain-scoped session cookie, and 302s back to the service.
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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/httprate"

	"github.com/ankoehn/burrow/internal/db"
)

const (
	gateSessionCookieName = "burrow_session"
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
//     cookie Domain scoping and next-URL validation.
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
				// Derive service from the next URL subdomain.
				label := subdomainLabel(nextURL, g.authDomain)
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
							//nolint:gosec // G710: nextURL is sanitizeNext()-validated to https + authDomain/sub-of-authDomain only; not attacker-controlled.
							http.Redirect(w, r, nextURL, http.StatusFound)
							return
						}
					}
				}
				// No service found or no label — redirect to next anyway (the service
				// access check is best-effort; the proxy will re-evaluate).
				//nolint:gosec // G710: nextURL is sanitizeNext()-validated to https + authDomain/sub-of-authDomain only; not attacker-controlled.
				http.Redirect(w, r, nextURL, http.StatusFound)
				return
			}
		}
	}

	// No valid session (or suspended) → show login form.
	label := subdomainLabel(nextURL, g.authDomain)
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

	label := subdomainLabel(nextURL, g.authDomain)

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

	// Set the auth-domain-scoped session cookie. Domain=authDomain means
	// the cookie is sent to all subdomains of authDomain — this is the key
	// difference from the dashboard cookie (which has no Domain set, making
	// it host-only). The same MaxAge/flags as the dashboard cookie apply.
	g.setSessionCookie(w, sid)

	g.log.Info("gate: login success", "email", email)
	_ = g.st.DeleteSession // best-effort touch is not needed here

	//nolint:gosec // G710: nextURL is sanitizeNext()-validated to https + authDomain/sub-of-authDomain only; not attacker-controlled.
	http.Redirect(w, r, nextURL, http.StatusFound)
}

// handlePostLogout clears the session cookie and deletes the session from the store.
func (g *Gate) handlePostLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(gateSessionCookieName); err == nil && c.Value != "" {
		_ = g.st.DeleteSession(r.Context(), c.Value)
	}
	g.clearSessionCookie(w)
	http.Redirect(w, r, "https://"+g.authDomain+"/__burrow/login", http.StatusFound)
}

// ---------------------------------------------------------------------------
// Cookie helpers (domain-scoped variants of internal/api/cookies.go)
// ---------------------------------------------------------------------------

// setSessionCookie sets the burrow_session cookie scoped to authDomain so it is
// shared across all subdomains (SSO). The dashboard cookie (internal/api/cookies.go)
// uses no Domain (host-only); we add Domain here for gate-specific SSO scoping.
// All other flags (HttpOnly, SameSite=Lax, Secure, MaxAge) mirror the dashboard.
func (g *Gate) setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     gateSessionCookieName,
		Value:    id,
		Domain:   g.authDomain,
		Path:     "/",
		HttpOnly: true,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(gateSessionMaxAge.Seconds()),
	})
}

// clearSessionCookie expires the burrow_session cookie on the auth domain.
func (g *Gate) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     gateSessionCookieName,
		Value:    "",
		Domain:   g.authDomain,
		Path:     "/",
		HttpOnly: true,
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
//   - host == authDomain OR host ends with "."+authDomain.
//   - no userinfo (user:password@ is stripped out)
//
// Any other value (off-domain, http://, unparseable) returns the safe default:
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
	host := u.Hostname() // strips port
	if host != g.authDomain && !strings.HasSuffix(host, "."+g.authDomain) {
		return fallback
	}
	// Strip userinfo for safety.
	u.User = nil
	return u.String()
}

// subdomainLabel extracts the first DNS label from a URL's host if the host is
// a direct subdomain of authDomain (e.g. "app.tunnels.example.com" → "app").
// Returns "" if the URL is unparseable, is the auth domain itself, or the label
// contains a dot (multi-level subdomain).
func subdomainLabel(rawURL, authDomain string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	suffix := "." + authDomain
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return ""
	}
	return label
}
