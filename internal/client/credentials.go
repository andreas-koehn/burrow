package client

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
)

// The places a value can come from, as Credentials.Source names them.
const (
	SourceFlags       = "flags"
	SourceEnvironment = "environment"
	SourceFile        = "burrow.yaml"
	SourceUserConfig  = "user config"
)

// Credentials is what a command needs to connect.
//
// Like UserConfig, it never prints its token.
type Credentials struct {
	Control   string `json:"control"` // host:port of the control endpoint
	Token     string `json:"-" yaml:"-"`
	TokenName string `json:"token_name"` // "" when the token did not come from the user config
	Relay     string `json:"relay"`      // dashboard URL, "" when unknown
	Source    string `json:"source"`     // "flags" | "environment" | "burrow.yaml" | "user config", for doctor and status
}

// String renders the credentials without the token.
func (c Credentials) String() string {
	return fmt.Sprintf("Credentials{Control:%q Token:%s TokenName:%q Relay:%q Source:%q}",
		c.Control, redactToken(c.Token), c.TokenName, c.Relay, c.Source)
}

// GoString keeps %#v from printing the token.
func (c Credentials) GoString() string { return c.String() }

// LogValue keeps slog from printing the token.
func (c Credentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("control", c.Control),
		slog.String("token", redactToken(c.Token)),
		slog.String("token_name", c.TokenName),
		slog.String("relay", c.Relay),
		slog.String("source", c.Source),
	)
}

// Sources are the places a control endpoint and a token can come from, highest precedence first.
type Sources struct {
	FlagServer string      `json:"flag_server"`
	FlagToken  string      `json:"-" yaml:"-"`
	EnvServer  string      `json:"env_server"`  // BURROW_SERVER
	EnvToken   string      `json:"-" yaml:"-"`  // BURROW_TOKEN (BURROW_TOKEN_FILE already resolved by the caller)
	FileServer string      `json:"file_server"` // burrow.yaml, for `up` only
	FileToken  string      `json:"-" yaml:"-"`
	User       *UserConfig `json:"user,omitempty" yaml:"-"` // its token is written as YAML, so it stays out as a whole
}

// String renders the sources without any token.
func (s Sources) String() string {
	user := "<nil>"
	if s.User != nil {
		user = s.User.String()
	}
	return fmt.Sprintf("Sources{FlagServer:%q FlagToken:%s EnvServer:%q EnvToken:%s FileServer:%q FileToken:%s User:%s}",
		s.FlagServer, redactToken(s.FlagToken), s.EnvServer, redactToken(s.EnvToken),
		s.FileServer, redactToken(s.FileToken), user)
}

// GoString keeps %#v from printing a token.
func (s Sources) GoString() string { return s.String() }

// LogValue keeps slog from printing a token.
func (s Sources) LogValue() slog.Value { return slog.StringValue(s.String()) }

// ErrNotSignedIn is returned when no source provides both a control endpoint
// and a token. Its text is the line the command prints before exit code 3.
var ErrNotSignedIn = errors.New("Not signed in. Run: burrow login <your relay address>") //nolint:staticcheck // the wording is the spec's

// Resolve picks the control endpoint and the token, each on its own, from the
// first source that has it: flags, then the environment, then burrow.yaml, then
// the user config. It returns ErrNotSignedIn when either is missing.
//
// Values are trimmed, and one that is blank counts as not set.
//
// A token from the user config is used only with the user config's own control
// endpoint, or with a control endpoint from another source that is the same
// one; otherwise the result is a *RelayMismatchError. A token given explicitly
// may go with any control endpoint.
//
// Source names the highest-precedence place that contributed a value.
// TokenName is set only when the token itself came from the user config, and
// Relay only when the control endpoint did: the stored name and dashboard URL
// describe the stored values, not ones given some other way.
func Resolve(s Sources) (Credentials, error) {
	var userControl, userToken string
	if s.User != nil {
		userControl, userToken = s.User.Control, s.User.Token
	}
	trim := strings.TrimSpace
	names := [...]string{SourceFlags, SourceEnvironment, SourceFile, SourceUserConfig}
	controls := [...]string{trim(s.FlagServer), trim(s.EnvServer), trim(s.FileServer), trim(userControl)}
	tokens := [...]string{trim(s.FlagToken), trim(s.EnvToken), trim(s.FileToken), trim(userToken)}

	ci, ti := firstSet(controls[:]), firstSet(tokens[:])
	if ci < 0 || ti < 0 {
		return Credentials{}, ErrNotSignedIn
	}

	// The stored token belongs to the relay it was stored with. A server named
	// some other way gets it only when it is that same control endpoint.
	if names[ti] == SourceUserConfig && names[ci] != SourceUserConfig && !sameEndpoint(controls[ci], controls[3]) {
		return Credentials{}, &RelayMismatchError{
			Control: controls[ci], StoredControl: controls[3], StoredRelay: trim(s.User.Relay),
		}
	}

	c := Credentials{Control: controls[ci], Token: tokens[ti], Source: names[min(ci, ti)]}
	if names[ci] == SourceUserConfig {
		c.Relay = s.User.Relay
	}
	if names[ti] == SourceUserConfig {
		c.TokenName = s.User.TokenName
	}
	return c, nil
}

// RelayMismatchError says that the only token at hand is the stored one and
// that it was stored for another relay than the one asked for. The token is
// not sent there. It is a kind of ErrNotSignedIn; its message names the relay
// and the command that signs in to it, and never the token.
type RelayMismatchError struct {
	Control       string // the control endpoint asked for
	StoredControl string // the control endpoint of the stored sign-in, "" when it has none
	StoredRelay   string // the dashboard URL of the stored sign-in, "" when it has none
}

func (e *RelayMismatchError) Error() string {
	stored := "The stored sign-in names no relay."
	if at := cmp.Or(e.StoredRelay, e.StoredControl); at != "" {
		stored = "The stored sign-in is for " + at + "."
	}
	login := "<its address>"
	if host, port, err := net.SplitHostPort(e.Control); err == nil && host != "" {
		login = host
		if strings.Contains(host, ":") {
			login = "[" + host + "]"
		}
		if port != "7000" {
			login += " --control " + e.Control
		}
	}
	return "Not signed in to " + e.Control + ". " + stored + "\nRun: burrow login " + login
}

// Is makes the error count as ErrNotSignedIn.
func (e *RelayMismatchError) Is(target error) bool { return target == ErrNotSignedIn }

// sameEndpoint reports whether two host:port values name the same control
// endpoint: the host without regard to case or a trailing dot, the same port.
func sameEndpoint(a, b string) bool {
	norm := func(s string) string {
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			return strings.ToLower(s)
		}
		if n, err := strconv.Atoi(port); err == nil {
			port = strconv.Itoa(n)
		}
		return net.JoinHostPort(strings.TrimSuffix(strings.ToLower(host), "."), port)
	}
	return a != "" && b != "" && norm(a) == norm(b)
}

// firstSet returns the index of the first non-empty value, or -1.
func firstSet(values []string) int {
	for i, v := range values {
		if v != "" {
			return i
		}
	}
	return -1
}

// TokenTail returns the last four characters of a token for display, "" for a
// shorter one. A token of exactly four characters also gives "": its tail
// would be the whole token.
func TokenTail(token string) string {
	r := []rune(token)
	if len(r) <= 4 {
		return ""
	}
	return string(r[len(r)-4:])
}

// ValidToken reports whether s can be a client token: printable ASCII without
// spaces, and not empty. It is the one rule for a token a person pastes and
// for one a relay hands out.
func ValidToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}
