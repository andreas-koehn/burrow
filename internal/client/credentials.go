package client

import (
	"errors"
	"fmt"
	"log/slog"
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

	c := Credentials{Control: controls[ci], Token: tokens[ti], Source: names[min(ci, ti)]}
	if names[ci] == SourceUserConfig {
		c.Relay = s.User.Relay
	}
	if names[ti] == SourceUserConfig {
		c.TokenName = s.User.TokenName
	}
	return c, nil
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
