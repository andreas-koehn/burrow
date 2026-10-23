package client

import "regexp"

// accessModes maps the names a person uses for an access mode (--access, and
// access: in burrow.yaml) to the relay's.
var accessModes = map[string]string{
	"open":    "open",
	"login":   "burrow_login",
	"api-key": "api_key",
}

// The access modes of a tcp service. Their names are the relay's.
var tcpAccessModes = map[string]bool{"open": true, "ip": true, "private": true}

// The rules of the two wishes, as the messages state them.
const (
	SlugRule      = "must be 3 to 40 characters: lowercase letters, digits and hyphens, starting and ending with a letter or digit"
	AccessRule    = "must be one of: open, login, api-key"
	TCPAccessRule = "must be one of: open, ip, private"
)

// TCPAccessMode returns the relay's access mode of a tcp service for the
// name a person uses: open, ip or private.
func TCPAccessMode(name string) (mode string, ok bool) {
	if tcpAccessModes[name] {
		return name, true
	}
	return "", false
}

// slugRe is the relay's rule for a service slug (auth.ValidSlug). It is
// repeated here so that the client does not link the relay's auth package;
// the relay checks the slug again.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

// ValidSlug reports whether s can be the slug of a service.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

// AccessMode returns the relay's access mode for the name a person uses:
// open, login or api-key.
func AccessMode(name string) (mode string, ok bool) {
	mode, ok = accessModes[name]
	return mode, ok
}

// AccessName returns the name a person uses for a relay access mode. A mode
// without one (mtls) keeps the relay's name.
func AccessName(mode string) string {
	for name, m := range accessModes {
		if m == mode {
			return name
		}
	}
	return mode
}

// tcpAccessRank orders the tcp modes by strictness: open < ip < private. An
// unknown mode ranks as open, the least strict.
func tcpAccessRank(mode string) int {
	switch mode {
	case "ip":
		return 1
	case "private":
		return 2
	}
	return 0
}
