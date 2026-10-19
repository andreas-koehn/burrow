package oauth

import (
	"net"
	"net/url"
	"regexp"
	"strings"
)

// privateScheme is a private-use URI scheme of a native app (RFC 8252 7.1):
// a reverse domain name, so it holds a dot ("com.example.app").
var privateScheme = regexp.MustCompile(`^[a-z][a-z0-9+-]*(\.[a-z0-9+-]+)+$`)

// maxRedirectURI bounds one redirect URI.
const maxRedirectURI = 2048

// ValidRedirectURI reports whether raw may be registered as a redirect URI:
// an absolute https URL, an http URL on a loopback address (a native app's
// local listener), or a private-use scheme of a native app. No fragment, no
// user information.
func ValidRedirectURI(raw string) bool {
	if raw == "" || len(raw) > maxRedirectURI || strings.ContainsAny(raw, " \t\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.User != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != "" && u.Hostname() != ""
	case "http":
		return isLoopbackHost(u.Hostname())
	default:
		return privateScheme.MatchString(u.Scheme)
	}
}

// isLoopbackHost reports whether host names this machine: localhost or a
// loopback address.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsLoopbackRedirect reports whether the redirect URI goes to this machine:
// the consent page warns about a client whose redirect is local only.
func IsLoopbackRedirect(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http" && isLoopbackHost(u.Hostname())
}

// redirectHost is what the consent page names as the redirect's host.
func redirectHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Host != "" {
		return u.Hostname()
	}
	return u.Scheme + ":"
}

// redirectAllowed reports whether requested matches one of the registered
// redirect URIs: exactly, or, for an http loopback URI, in everything but the
// port (RFC 8252 7.3: a native app picks its port when it starts).
func redirectAllowed(registered []string, requested string) bool {
	if !ValidRedirectURI(requested) {
		return false
	}
	for _, reg := range registered {
		if reg == requested {
			return true
		}
	}
	req, err := url.Parse(requested)
	if err != nil || req.Scheme != "http" || !isLoopbackHost(req.Hostname()) {
		return false
	}
	for _, reg := range registered {
		u, err := url.Parse(reg)
		if err != nil || u.Scheme != "http" || !isLoopbackHost(u.Hostname()) {
			continue
		}
		if strings.EqualFold(u.Hostname(), req.Hostname()) && u.EscapedPath() == req.EscapedPath() && u.RawQuery == req.RawQuery {
			return true
		}
	}
	return false
}
