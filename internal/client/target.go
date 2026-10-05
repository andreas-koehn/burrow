package client

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// TargetError explains why a target was not accepted. Its message is shown to
// the user as is and always lists the accepted forms.
type TargetError struct{ Input, Reason string }

func (e *TargetError) Error() string {
	return "cannot use " + strconv.Quote(e.Input) + " as a target: " + e.Reason +
		"\nAccepted forms: 3000   localhost:3000   192.168.1.20:8080   http://localhost:3000"
}

// ParseTarget turns what the user typed into the local address to forward to.
// A bare port means 127.0.0.1.
func ParseTarget(s string) (string, error) {
	in := strings.TrimSpace(s)
	// shown holds the input as echoed in errors: a typed password is never repeated.
	shown := s
	if u, err := url.Parse(in); err == nil && u.User != nil {
		shown = u.Redacted()
	}
	bad := func(reason string) (string, error) { return "", &TargetError{Input: shown, Reason: reason} }

	if strings.Contains(in, "://") {
		u, err := url.Parse(in)
		if err != nil {
			return bad("it is not a valid address with a port")
		}
		switch {
		case u.Scheme == "https":
			return bad("HTTPS upstreams are not supported; point at the plain HTTP port of the local service")
		case u.Scheme != "http":
			return bad("only http:// addresses are understood")
		case u.User != nil:
			return bad("an address must not contain credentials")
		case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
			return bad("an address must not contain a path; the whole service is exposed")
		}
		host, port := u.Hostname(), u.Port()
		if port == "" {
			port = "80"
		}
		return joinTarget(shown, host, port)
	}
	if !strings.Contains(in, ":") {
		// a bare port
		return joinTarget(shown, "127.0.0.1", in)
	}
	host, port, err := net.SplitHostPort(in)
	if err != nil {
		return bad("it needs the form host:port with a numeric port")
	}
	return joinTarget(shown, host, port)
}

func joinTarget(input, host, port string) (string, error) {
	n, err := strconv.Atoi(port)
	if err != nil || !plainDigits(port) || n < 1 || n > 65535 {
		return "", &TargetError{Input: input, Reason: "the port must be a number between 1 and 65535"}
	}
	if host == "" || strings.ContainsAny(host, "/") || strings.IndexFunc(host, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return "", &TargetError{Input: input, Reason: "the host is missing or not valid"}
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// plainDigits reports whether s is ASCII digits only, without a leading zero.
func plainDigits(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// DefaultName is the service name used when the user gives none. It depends
// only on the machine's name and the local port, so running the same command
// again reuses the same service, URL and access settings.
func DefaultName(hostname, localAddr string) string {
	host := hostname
	if i := strings.IndexByte(host, '.'); i >= 0 {
		host = host[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	host = strings.Trim(b.String(), "-")
	_, port, err := net.SplitHostPort(localAddr)
	if err != nil || port == "" {
		port = ""
	}
	// The whole name stays within 40 characters so it is a valid slug.
	limit := 40
	if port != "" {
		limit = 40 - 1 - len(port)
	}
	if len(host) > limit {
		host = strings.TrimRight(host[:limit], "-")
	}
	if host == "" {
		host = "burrow"
	}
	if port == "" {
		return host
	}
	return host + "-" + port
}
