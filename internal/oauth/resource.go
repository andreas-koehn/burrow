package oauth

// resource.go — the resource-server side (spec D22, D23, Interfaces): the
// protected resource metadata of each MCP endpoint (RFC 9728), the
// WWW-Authenticate challenge of a 401 there, and the check of an access token
// presented at one of them.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// ProtectedResourcePath is the well-known path of the protected resource
// metadata. The document of an MCP endpoint is at this path followed by the
// endpoint's path ("/.well-known/oauth-protected-resource/mcp/burrow").
const ProtectedResourcePath = "/.well-known/oauth-protected-resource"

// MCP endpoint paths.
const (
	PathBurrow  = "/mcp/burrow"
	PathGateway = "/mcp"
)

// ResourceKindOf is the kind of an MCP endpoint path: ResourceBurrow for
// "/mcp/burrow", ResourceGateway for "/mcp", ResourceSource for "/mcp/<slug>"
// with a valid slug, "" for anything else.
func ResourceKindOf(path string) string {
	switch {
	case path == PathBurrow:
		return ResourceBurrow
	case path == PathGateway:
		return ResourceGateway
	case strings.HasPrefix(path, PathGateway+"/") && store.ValidToolSourceSlug(path[len(PathGateway)+1:]):
		return ResourceSource
	}
	return ""
}

// ResourceMetadataURL is the address of the protected resource metadata of
// the MCP endpoint path, "" when the server is off for the request.
func (s *Server) ResourceMetadataURL(r *http.Request, path string) string {
	iss := s.Issuer(r)
	if iss == "" {
		return ""
	}
	return iss + ProtectedResourcePath + path
}

// DefaultScope is the scope a client asks for at an MCP endpoint of the kind
// when it does not choose: burrow:read at the management server, mcp:tools at
// a gateway resource.
func DefaultScope(kind string) string { return strings.Join(defaultScopes(kind), " ") }

// Challenge is the WWW-Authenticate value of a 401 at the MCP endpoint path:
// the address of its metadata and the default scope, plus
// error="invalid_token" when a credential was presented and refused (RFC
// 6750 3.1: a request without one gets no error code). While the server is
// off for the request there is no metadata to point to: the value is a bare
// "Bearer".
func (s *Server) Challenge(r *http.Request, path string, invalid bool) string {
	prm := s.ResourceMetadataURL(r, path)
	kind := ResourceKindOf(path)
	if prm == "" || kind == "" {
		return "Bearer"
	}
	v := `Bearer resource_metadata="` + prm + `", scope="` + DefaultScope(kind) + `"`
	if invalid {
		v += `, error="invalid_token"`
	}
	return v
}

// ProtectedResource serves the protected resource metadata (RFC 9728) of the
// MCP endpoint path: "" for the document at the root, which describes the
// origin itself. It answers 404 while the server is off for the request, for
// a path that is no MCP endpoint, and for an unknown or disabled tool source
// (no token can be had for it).
func (s *Server) ProtectedResource(w http.ResponseWriter, r *http.Request, path string) {
	iss := s.Issuer(r)
	if iss == "" {
		http.NotFound(w, r)
		return
	}
	meta := oauthex.ProtectedResourceMetadata{
		Resource:               iss + path,
		AuthorizationServers:   []string{iss},
		BearerMethodsSupported: []string{"header"},
	}
	switch ResourceKindOf(path) {
	case ResourceBurrow:
		meta.ScopesSupported = []string{ScopeBurrowRead, ScopeBurrowWrite}
		meta.ResourceName = "Burrow management"
	case ResourceGateway:
		meta.ScopesSupported = []string{ScopeMCPTools}
		meta.ResourceName = "Burrow tools"
	case ResourceSource:
		src, err := s.store.GetMCPSource(r.Context(), path[len(PathGateway)+1:])
		if errors.Is(err, db.ErrNotFound) || (err == nil && !src.Enabled) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.log.Error("oauth: protected resource metadata: read source", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// A generic name: the document is public, and the operator's name
		// of a source is not for unauthenticated callers. That the slug
		// exists (200 vs 404) is visible, and accepted.
		meta.ScopesSupported = []string{ScopeMCPTools}
		meta.ResourceName = "Burrow tools"
	default:
		if path != "" {
			http.NotFound(w, r)
			return
		}
		meta.ScopesSupported = []string{ScopeBurrowRead, ScopeBurrowWrite, ScopeMCPTools}
		meta.ResourceName = "Burrow"
	}
	if s.Configured() {
		w.Header().Set("Cache-Control", "public, max-age=300")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	auth.ProtectedResourceMetadataHandler(&meta).ServeHTTP(w, r)
}

// Verified is an access token accepted at one MCP endpoint, with its user as
// they are now.
type Verified struct {
	AccessToken
	// Kind is the kind of the endpoint (ResourceBurrow, ResourceGateway,
	// ResourceSource).
	Kind string
	User db.User
	// Permissions are, at the management server, the permissions the scopes
	// stand for that the user's role grants now (D23); nil elsewhere.
	Permissions []string
}

// VerifyAccessToken checks an access token presented at the MCP endpoint
// path, every time it is used:
//   - it is an access token, not expired, and its grant is not revoked;
//   - its audience is the resource URL of exactly this endpoint (D22);
//   - its user exists and is not suspended;
//   - at the management server, its permissions are the scopes intersected
//     with the user's role now;
//   - at a gateway resource, it carries mcp:tools and the user's role still
//     grants ai:configure:own or ai:configure:any (D23, D28a).
//
// Any failure is ErrInvalidToken (the resource answers 401 invalid_token),
// except a failure of the store. The grant's last use is recorded only for
// an accepted token.
func (s *Server) VerifyAccessToken(r *http.Request, plain, path string) (Verified, error) {
	kind := ResourceKindOf(path)
	want := s.ResourceURL(r, path)
	if kind == "" || want == "" {
		return Verified{}, ErrInvalidToken
	}
	ctx := r.Context()
	t, g, err := s.accessToken(ctx, plain)
	if err != nil {
		return Verified{}, err
	}
	if g.Resource != want {
		return Verified{}, ErrInvalidToken
	}
	u, err := s.store.GetUserByID(ctx, g.UserID)
	if errors.Is(err, db.ErrNotFound) {
		return Verified{}, ErrInvalidToken
	}
	if err != nil {
		return Verified{}, err
	}
	if u.Status == "suspended" {
		return Verified{}, ErrInvalidToken
	}
	v := Verified{AccessToken: accessTokenOf(t, g), Kind: kind, User: u}
	if kind == ResourceBurrow {
		v.Permissions = []string{}
		for _, p := range ScopePermissions(g.Scopes) {
			if authz.Can(u.Role, p) {
				v.Permissions = append(v.Permissions, string(p))
			}
		}
	} else {
		mayConfigure := authz.Can(u.Role, authz.PermAIConfigureOwn) || authz.Can(u.Role, authz.PermAIConfigureAny)
		if !slices.Contains(g.Scopes, ScopeMCPTools) || !mayConfigure {
			return Verified{}, ErrInvalidToken
		}
	}
	if err := s.store.TouchOAuthGrant(ctx, g.ID, s.now()); err != nil {
		s.log.Warn("oauth: touch grant", "err", err)
	}
	return v, nil
}

// accessToken looks up a live access token and its grant: ErrInvalidToken
// for anything else (not boa_, unknown, a refresh token, expired, revoked).
func (s *Server) accessToken(ctx context.Context, plain string) (db.OAuthToken, db.OAuthGrant, error) {
	if !strings.HasPrefix(plain, AccessTokenPrefix) {
		return db.OAuthToken{}, db.OAuthGrant{}, ErrInvalidToken
	}
	t, g, err := s.store.GetOAuthToken(ctx, hashSecret(plain))
	if errors.Is(err, db.ErrNotFound) {
		return db.OAuthToken{}, db.OAuthGrant{}, ErrInvalidToken
	}
	if err != nil {
		return db.OAuthToken{}, db.OAuthGrant{}, err
	}
	if t.Kind != "access" || g.RevokedAt != nil || !s.now().Before(t.ExpiresAt) {
		return db.OAuthToken{}, db.OAuthGrant{}, ErrInvalidToken
	}
	return t, g, nil
}

func accessTokenOf(t db.OAuthToken, g db.OAuthGrant) AccessToken {
	return AccessToken{
		GrantID: g.ID, ClientID: g.ClientID, ClientName: g.ClientName, UserID: g.UserID, Resource: g.Resource,
		Scopes: g.Scopes, AllowedTools: g.AllowedTools, ToolsReadOnly: g.ToolsReadOnly, ExpiresAt: t.ExpiresAt,
	}
}
