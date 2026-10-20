package audit

import (
	"context"
	"encoding/json"
)

// ctxKey is the unexported key type for LogContext values stashed on a
// context.Context. Defined as an unexported empty struct so two callers in
// different packages cannot collide.
type ctxKey struct{}

var logContextKey ctxKey

// WithLogContext returns a new context carrying lc. HTTP middleware in
// internal/api derives lc from the authenticated session + request headers
// and threads it through to store methods so mutations can append typed
// audit events without re-parsing the request inside the store.
func WithLogContext(parent context.Context, lc LogContext) context.Context {
	return context.WithValue(parent, logContextKey, lc)
}

// LogContextFrom extracts the LogContext stashed by WithLogContext. Returns
// a zero value when none is present (the typed helpers all accept that
// zero value — actor_id/actor_email simply land empty, which is correct
// for CLI-driven and system-triggered audit rows).
func LogContextFrom(ctx context.Context) LogContext {
	if ctx == nil {
		return LogContext{}
	}
	v, _ := ctx.Value(logContextKey).(LogContext)
	return v
}

// Credential is the automation token or the OAuth grant that made a
// management tool call (/mcp/burrow). The call is replayed through the REST
// API in-process; every row written on the way names the credential in its
// payload (token_id, oauth_grant_id), as the mcp.tool.call row does.
type Credential struct {
	TokenID      string
	OAuthGrantID string
}

type credentialKey struct{}

// WithCredential returns a context carrying c. Only the management MCP
// server sets it, on the context of a replayed call: no HTTP request can.
func WithCredential(parent context.Context, c Credential) context.Context {
	return context.WithValue(parent, credentialKey{}, c)
}

// CredentialFrom returns the credential WithCredential put on ctx.
func CredentialFrom(ctx context.Context) (Credential, bool) {
	if ctx == nil {
		return Credential{}, false
	}
	c, ok := ctx.Value(credentialKey{}).(Credential)
	return c, ok && (c.TokenID != "" || c.OAuthGrantID != "")
}

// withCredential adds c to an object payload under token_id and
// oauth_grant_id, unless the payload names them itself. A payload that is
// not an object stays as it is.
func withCredential(p json.RawMessage, c Credential) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(p, &m); err != nil || m == nil {
		return p
	}
	add := func(k, v string) {
		if _, has := m[k]; !has && v != "" {
			m[k] = mustJSON(v)
		}
	}
	add("token_id", c.TokenID)
	add("oauth_grant_id", c.OAuthGrantID)
	b, err := json.Marshal(m)
	if err != nil {
		return p
	}
	return b
}
