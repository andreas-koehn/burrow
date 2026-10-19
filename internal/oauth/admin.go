package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/ankoehn/burrow/internal/db"
)

// maxClientRedirects bounds the redirect URIs of a pre-registered client.
const maxClientRedirects = 16

// ErrInvalidClientInput is wrapped with the reason a client was refused.
var ErrInvalidClientInput = errors.New("oauth: invalid client")

// NewClient is a pre-registered client to create.
type NewClient struct {
	Name         string
	RedirectURIs []string
	CreatedBy    string
}

// CreateClient registers a public client and returns it with its new id.
func (s *Server) CreateClient(ctx context.Context, in NewClient) (db.OAuthClient, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 120 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return db.OAuthClient{}, fmt.Errorf("%w: name must be 1 to 120 characters without control characters", ErrInvalidClientInput)
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > maxClientRedirects {
		return db.OAuthClient{}, fmt.Errorf("%w: give 1 to %d redirect URIs", ErrInvalidClientInput, maxClientRedirects)
	}
	uris := make([]string, 0, len(in.RedirectURIs))
	for _, u := range in.RedirectURIs {
		if !ValidRedirectURI(u) {
			return db.OAuthClient{}, fmt.Errorf("%w: a redirect URI must be https, http on a loopback address, or an app scheme such as com.example.app:/callback; no fragment", ErrInvalidClientInput)
		}
		if !containsStr(uris, u) {
			uris = append(uris, u)
		}
	}
	c := db.OAuthClient{
		ClientID: ClientIDPrefix + randomID(), Name: name, RedirectURIs: uris,
		CreatedBy: in.CreatedBy, CreatedAt: s.now().UTC(),
	}
	if err := s.store.CreateOAuthClient(ctx, c); err != nil {
		return db.OAuthClient{}, err
	}
	return c, nil
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ListClients returns the pre-registered clients.
func (s *Server) ListClients(ctx context.Context) ([]db.OAuthClient, error) {
	return s.store.ListOAuthClients(ctx)
}

// DeleteClient removes a pre-registered client and revokes its grants.
// ErrNotFound when there is none; the deleted client is returned.
func (s *Server) DeleteClient(ctx context.Context, id string) (db.OAuthClient, error) {
	c, err := s.store.GetOAuthClient(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return db.OAuthClient{}, ErrNotFound
	}
	if err != nil {
		return db.OAuthClient{}, err
	}
	if err := s.store.DeleteOAuthClient(ctx, id, s.now()); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.OAuthClient{}, ErrNotFound
		}
		return db.OAuthClient{}, err
	}
	s.dropPendingOfClient(id)
	return c, nil
}

// ListGrants returns the grants that are not revoked: the user's, or every
// user's when userID is empty.
func (s *Server) ListGrants(ctx context.Context, userID string) ([]db.OAuthGrant, error) {
	return s.store.ListOAuthGrants(ctx, userID)
}

// RevokeGrant ends a grant and its tokens at once. ownerID non-empty limits
// it to that user's grants (another user's is ErrNotFound). A grant that was
// revoked already is ErrNotFound too.
func (s *Server) RevokeGrant(ctx context.Context, id, ownerID string) (db.OAuthGrant, error) {
	g, err := s.store.GetOAuthGrant(ctx, id)
	if errors.Is(err, db.ErrNotFound) || (err == nil && (g.RevokedAt != nil || (ownerID != "" && g.UserID != ownerID))) {
		return db.OAuthGrant{}, ErrNotFound
	}
	if err != nil {
		return db.OAuthGrant{}, err
	}
	if err := s.store.RevokeOAuthGrant(ctx, id, s.now()); err != nil {
		return db.OAuthGrant{}, err
	}
	return g, nil
}

// RevokeResourceGrants revokes every live grant issued for the MCP endpoint
// path ("/mcp/<slug>"), whatever origin it was issued under, and returns how
// many it revoked. Deleting a tool source calls it, so that a later source
// of the same slug is not reached by grants given for the old one.
func (s *Server) RevokeResourceGrants(ctx context.Context, path string) (int, error) {
	grants, err := s.store.ListOAuthGrants(ctx, "")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, g := range grants {
		u, err := url.Parse(g.Resource)
		if err != nil || strings.TrimSuffix(u.Path, "/") != path {
			continue
		}
		if err := s.store.RevokeOAuthGrant(ctx, g.ID, s.now()); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
