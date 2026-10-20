package auth

import (
	"net/http"
	"strings"
)

// Burrow's own cookies, by base name. With Secure cookies on, each is written
// under HostCookiePrefix + its base name (see CookieName).
const (
	// SessionCookie and CSRFCookie are the dashboard's session and its
	// double-submit CSRF token (on the dashboard's host, which path-routed
	// services share).
	SessionCookie = "burrow_session"
	CSRFCookie    = "burrow_csrf"
	// ServicesSessionCookie is the services host's session; ServicesNonceCookie
	// the name stem of the sign-in handoff's nonce cookies.
	ServicesSessionCookie = "burrow_svc_session"
	ServicesNonceCookie   = "burrow_svc_nonce"
)

// HostCookiePrefix is the cookie name prefix browsers accept only with
// Secure, Path=/ and no Domain. A page on another host of the same site (a
// services host, a tunnelled app on a sibling name) can therefore never set a
// cookie of that name for the dashboard's host, as it can with Domain= for a
// bare name.
const HostCookiePrefix = "__Host-"

// CookieName is the name a cookie of base name is written under: with
// HostCookiePrefix when cookies are Secure, the bare base name otherwise
// (plain http in development, where browsers refuse the prefix).
func CookieName(base string, secure bool) string {
	if secure {
		return HostCookiePrefix + base
	}
	return base
}

// IsHostCookie reports whether name carries HostCookiePrefix.
func IsHostCookie(name string) bool { return strings.HasPrefix(name, HostCookiePrefix) }

// BaseCookieName is name without HostCookiePrefix.
func BaseCookieName(name string) string { return strings.TrimPrefix(name, HostCookiePrefix) }

// ReadCookie returns the value of the cookie of base name the request
// carries, and the name it was read under: the HostCookiePrefix one when it
// is there and not empty, else the bare one. The bare name is what Burrow
// wrote before v0.12.1 (and still writes without Secure cookies); it is
// accepted while the prefixed one is absent, so that signed-in browsers are
// not signed out by the upgrade. ("", "") when neither is there.
func ReadCookie(r *http.Request, base string) (value, name string) {
	if c, err := r.Cookie(HostCookiePrefix + base); err == nil && c.Value != "" {
		return c.Value, c.Name
	}
	if c, err := r.Cookie(base); err == nil && c.Value != "" {
		return c.Value, c.Name
	}
	return "", ""
}

// ReadCookieLike returns the value of the cookie of base name in the name
// family of like (a name ReadCookie returned): prefixed when like is, bare
// when like is bare. The CSRF token is read this way next to the session
// cookie that authenticated the request, so that a bare cookie another host
// of the site planted never pairs with a __Host- session.
func ReadCookieLike(r *http.Request, base, like string) string {
	name := base
	if IsHostCookie(like) {
		name = HostCookiePrefix + base
	}
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}
