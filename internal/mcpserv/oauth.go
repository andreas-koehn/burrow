package mcpserv

// oauth.go — OAuth access tokens at /mcp/burrow (spec D22, D23) and the
// in-process marker that carries their identity into the replayed REST call.

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/ankoehn/burrow/internal/oauth"
)

// OAuthTokens checks OAuth access tokens and words the 401 challenge
// (*oauth.Server).
type OAuthTokens interface {
	VerifyAccessToken(r *http.Request, plain, path string) (oauth.Verified, error)
	Challenge(r *http.Request, path string, invalid bool) string
}

// SetOAuth installs the authorization server whose access tokens, issued for
// /mcp/burrow, the server accepts besides automation tokens; nil accepts
// automation tokens only. Like SetAuditAppender, call it at startup.
func (s *Server) SetOAuth(o OAuthTokens) {
	if s == nil {
		return
	}
	s.oauth = o
}

// ReplayIdentity is who a tool call replayed through the REST API acts as
// when the MCP client authenticated with an OAuth access token: the grant,
// its user, and the permissions the token's scopes stand for that the
// user's role granted when the call was made. The API treats it like an
// automation token (it intersects the permissions with the role again) and
// never sees the token itself.
type ReplayIdentity struct {
	GrantID     string
	UserID      string
	Permissions []string
	// ExpiresAt is the access token's expiry: a token the call mints
	// (automation or client token) expires no later. Other things it
	// creates (service API keys, webhooks, users) outlive the token and the
	// grant, as with an automation token.
	ExpiresAt time.Time
}

// replayKey is unexported, so only this package can put a ReplayIdentity
// into a context: nothing that arrives over HTTP can carry one.
type replayKey struct{}

func withReplayIdentity(ctx context.Context, id ReplayIdentity) context.Context {
	id.Permissions = slices.Clone(id.Permissions)
	return context.WithValue(ctx, replayKey{}, id)
}

// ReplayIdentityFrom returns the identity of a replayed tool call made with
// an OAuth access token, and false for any other request.
func ReplayIdentityFrom(ctx context.Context) (ReplayIdentity, bool) {
	id, ok := ctx.Value(replayKey{}).(ReplayIdentity)
	if !ok || id.GrantID == "" || id.UserID == "" {
		return ReplayIdentity{}, false
	}
	id.Permissions = slices.Clone(id.Permissions)
	return id, true
}
