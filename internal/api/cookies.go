package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/ankoehn/burrow/internal/auth"
)

// sessionCookieName and csrfCookieName are the base names of the dashboard's
// two cookies. With Secure cookies on they are written with the __Host-
// prefix (auth.CookieName); the bare names are read too, for one release,
// while the prefixed ones are absent (auth.ReadCookie).
const sessionCookieName = auth.SessionCookie
const csrfCookieName = auth.CSRFCookie
const sessionMaxAge = 7 * 24 * time.Hour

// sessionID returns the dashboard session id the request's cookie carries,
// "" when it carries none.
func sessionID(r *http.Request) string {
	v, _ := auth.ReadCookie(r, sessionCookieName)
	return v
}

// csrfCookieValue returns the CSRF cookie that belongs with the request's
// session cookie: the __Host- one next to a __Host- session, the bare one
// next to a bare session. "" when there is none.
func csrfCookieValue(r *http.Request) string {
	_, name := auth.ReadCookie(r, sessionCookieName)
	if name == "" {
		name = sessionCookieName
	}
	return auth.ReadCookieLike(r, csrfCookieName, name)
}

func setSessionCookie(w http.ResponseWriter, id string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName(sessionCookieName, secure),
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionMaxAge.Seconds()),
	})
	if secure {
		// The bare cookies of an earlier version would otherwise stay
		// behind until they expire.
		expireCookie(w, sessionCookieName, true, true)
		expireCookie(w, csrfCookieName, false, true)
	}
}

// clearSessionCookie expires the session cookie under both names.
func clearSessionCookie(w http.ResponseWriter, secure bool) {
	expireCookie(w, sessionCookieName, true, secure)
	if secure {
		expireCookie(w, auth.CookieName(sessionCookieName, true), true, true)
	}
}

// expireCookie writes a Set-Cookie that removes the host-only cookie name.
func expireCookie(w http.ResponseWriter, name string, httpOnly, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: httpOnly,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// generateCSRFToken returns a 32-byte random token encoded as a 64-char hex string.
func generateCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// setCSRFCookie sets the CSRF cookie (burrow_csrf, __Host-burrow_csrf with
// Secure cookies). It is intentionally NOT HttpOnly so that JavaScript can
// read and send it back as the X-CSRF-Token header (double-submit cookie
// pattern). SameSite and Secure mirror the session cookie.
func setCSRFCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName(csrfCookieName, secure),
		Value:    token,
		Path:     "/",
		HttpOnly: false, // JS-readable: required for the double-submit pattern
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionMaxAge.Seconds()),
	})
}

// clearCSRFCookie expires the CSRF cookie under both names (called on Logout).
func clearCSRFCookie(w http.ResponseWriter, secure bool) {
	expireCookie(w, csrfCookieName, false, secure)
	if secure {
		expireCookie(w, auth.CookieName(csrfCookieName, true), false, true)
	}
}
