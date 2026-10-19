package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// maxFormBytes bounds the body of the token and revocation endpoints.
const maxFormBytes = 16 << 10

// pkceVerifier is an RFC 7636 code verifier.
var pkceVerifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// tokenError is an RFC 6749 error of the token endpoint.
type tokenError struct {
	status int
	code   string
	desc   string
}

func tokenErr(code, desc string) *tokenError {
	return &tokenError{status: http.StatusBadRequest, code: code, desc: desc}
}

// tokenResponse is RFC 6749 5.1.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// formRequest reads an application/x-www-form-urlencoded body and the client
// id of a public client: client_id in the body, or HTTP Basic with the id as
// user name and an empty password (what an OAuth library sends when it
// detects the style by itself).
func formRequest(w http.ResponseWriter, r *http.Request) (clientID string, te *tokenError) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/x-www-form-urlencoded" {
		return "", tokenErr("invalid_request", "the body must be application/x-www-form-urlencoded")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return "", tokenErr("invalid_request", "the body could not be read")
	}
	for k, v := range r.PostForm {
		if len(v) > 1 {
			return "", tokenErr("invalid_request", "parameter "+k+" is repeated")
		}
	}
	clientID = r.PostForm.Get("client_id")
	if user, pass, ok := r.BasicAuth(); ok {
		// RFC 6749 2.3.1: both are form-urlencoded before they are joined
		// (an https client_id arrives as https%3A%2F%2F…).
		if u, err := url.QueryUnescape(user); err == nil {
			user = u
		}
		if pass != "" || r.PostForm.Get("client_secret") != "" {
			return "", &tokenError{http.StatusUnauthorized, "invalid_client", "public clients only: no client secret"}
		}
		if clientID != "" && clientID != user {
			return "", tokenErr("invalid_request", "client_id differs from the Authorization header")
		}
		clientID = user
	} else if r.PostForm.Get("client_secret") != "" {
		return "", &tokenError{http.StatusUnauthorized, "invalid_client", "public clients only: no client secret"}
	}
	if clientID == "" {
		return "", tokenErr("invalid_request", "client_id is required")
	}
	return clientID, nil
}

func writeTokenError(w http.ResponseWriter, te *tokenError) {
	if te.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="burrow"`)
	}
	writeJSON(w, te.status, map[string]string{"error": te.code, "error_description": te.desc})
}

// Token serves POST /oauth/token: authorization_code and refresh_token.
func (s *Server) Token(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Access-Control-Allow-Origin", "*")
	clientID, te := formRequest(w, r)
	if te != nil {
		writeTokenError(w, te)
		return
	}
	var resp tokenResponse
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		resp, te = s.exchangeCode(r, clientID)
	case "refresh_token":
		resp, te = s.refresh(r, clientID)
	case "":
		te = tokenErr("invalid_request", "grant_type is required")
	default:
		te = tokenErr("unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
	if te != nil {
		writeTokenError(w, te)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// sameResource compares a resource parameter with the stored audience,
// ignoring one trailing slash.
func sameResource(param, stored string) bool {
	return strings.TrimSuffix(param, "/") == stored
}

// s256 is the S256 code challenge of a verifier.
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Server) exchangeCode(r *http.Request, clientID string) (tokenResponse, *tokenError) {
	ctx := r.Context()
	f := r.PostForm
	code, redirectURI, verifier := f.Get("code"), f.Get("redirect_uri"), f.Get("code_verifier")
	if code == "" || redirectURI == "" || verifier == "" {
		return tokenResponse{}, tokenErr("invalid_request", "code, redirect_uri and code_verifier are required")
	}
	c, err := s.store.RedeemOAuthCode(ctx, hashSecret(code))
	if errors.Is(err, db.ErrNotFound) {
		return tokenResponse{}, tokenErr("invalid_grant", "the code is unknown or expired")
	}
	if err != nil {
		s.log.Error("oauth: redeem code", "err", err)
		return tokenResponse{}, &tokenError{http.StatusInternalServerError, "server_error", "the code could not be checked"}
	}
	if c.Used {
		// A code is used once. A second use may be an attacker who saw it:
		// whatever it was exchanged for ends (RFC 6749 4.1.2).
		if c.GrantID != "" {
			if err := s.store.RevokeOAuthGrant(ctx, c.GrantID, s.now()); err != nil && !errors.Is(err, db.ErrNotFound) {
				s.log.Error("oauth: revoke grant of a reused code", "err", err)
			}
			s.auditEvent(ctx, audit.ActionOAuthRefreshReuse, c.UserID, c.GrantID, c.ClientName, map[string]any{
				"client_id": c.ClientID, "resource": c.Resource, "via": "authorization_code",
			})
		}
		return tokenResponse{}, tokenErr("invalid_grant", "the code was used already")
	}
	now := s.now()
	switch {
	case !now.Before(c.ExpiresAt):
		return tokenResponse{}, tokenErr("invalid_grant", "the code is unknown or expired")
	case c.ClientID != clientID:
		return tokenResponse{}, tokenErr("invalid_grant", "the code was issued to another client")
	case c.RedirectURI != redirectURI:
		return tokenResponse{}, tokenErr("invalid_grant", "redirect_uri differs from the authorization request")
	case !pkceVerifier.MatchString(verifier) ||
		subtle.ConstantTimeCompare([]byte(s256(verifier)), []byte(c.CodeChallenge)) != 1:
		return tokenResponse{}, tokenErr("invalid_grant", "the code_verifier does not match")
	}
	if res := f.Get("resource"); res != "" && !sameResource(res, c.Resource) {
		return tokenResponse{}, tokenErr("invalid_target", "resource differs from the authorization request")
	}
	if te := s.userActive(ctx, c.UserID); te != nil {
		return tokenResponse{}, te
	}
	g := db.OAuthGrant{
		ID: randomID(), ClientID: c.ClientID, ClientName: c.ClientName, UserID: c.UserID, Resource: c.Resource,
		Scopes: c.Scopes, AllowedTools: c.AllowedTools, ToolsReadOnly: c.ToolsReadOnly, CreatedAt: now,
	}
	access, refresh, rows := s.newTokens(g.ID, now)
	if err := s.store.CreateOAuthGrant(ctx, g, c.Hash, rows...); err != nil {
		s.log.Error("oauth: create grant", "err", err)
		return tokenResponse{}, &tokenError{http.StatusInternalServerError, "server_error", "the grant could not be stored"}
	}
	return tokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int(AccessTokenTTL / time.Second),
		RefreshToken: refresh, Scope: strings.Join(g.Scopes, " "),
	}, nil
}

// newTokens mints an access and a refresh token for the grant (family = the
// grant) and returns the plaintexts and the rows to store.
func (s *Server) newTokens(grantID string, now time.Time) (access, refresh string, rows []db.OAuthToken) {
	access, refresh = randomSecret(AccessTokenPrefix), randomSecret(RefreshTokenPrefix)
	return access, refresh, []db.OAuthToken{
		{Hash: hashSecret(access), Kind: "access", GrantID: grantID, FamilyID: grantID, ExpiresAt: now.Add(AccessTokenTTL), CreatedAt: now},
		{Hash: hashSecret(refresh), Kind: "refresh", GrantID: grantID, FamilyID: grantID, ExpiresAt: now.Add(RefreshTokenTTL), CreatedAt: now},
	}
}

// userActive refuses the grant of a user who is gone or suspended.
func (s *Server) userActive(ctx context.Context, userID string) *tokenError {
	u, err := s.store.GetUserByID(ctx, userID)
	if errors.Is(err, db.ErrNotFound) || (err == nil && u.Status == "suspended") {
		return tokenErr("invalid_grant", "the user of this grant cannot sign in")
	}
	if err != nil {
		s.log.Error("oauth: look up user", "err", err)
		return &tokenError{http.StatusInternalServerError, "server_error", "the user could not be checked"}
	}
	return nil
}

func (s *Server) refresh(r *http.Request, clientID string) (tokenResponse, *tokenError) {
	ctx := r.Context()
	f := r.PostForm
	plain := f.Get("refresh_token")
	if plain == "" {
		return tokenResponse{}, tokenErr("invalid_request", "refresh_token is required")
	}
	invalid := tokenErr("invalid_grant", "the refresh token is invalid, expired or revoked")
	if !strings.HasPrefix(plain, RefreshTokenPrefix) {
		return tokenResponse{}, invalid
	}
	hash := hashSecret(plain)
	t, g, err := s.store.GetOAuthToken(ctx, hash)
	if errors.Is(err, db.ErrNotFound) {
		return tokenResponse{}, invalid
	}
	if err != nil {
		s.log.Error("oauth: look up refresh token", "err", err)
		return tokenResponse{}, &tokenError{http.StatusInternalServerError, "server_error", "the token could not be checked"}
	}
	now := s.now()
	if t.Kind != "refresh" || g.RevokedAt != nil || !now.Before(t.ExpiresAt) {
		return tokenResponse{}, invalid
	}
	if g.ClientID != clientID {
		return tokenResponse{}, tokenErr("invalid_grant", "the refresh token was issued to another client")
	}
	if t.UsedAt != nil {
		s.refreshReused(ctx, g)
		return tokenResponse{}, invalid
	}
	if res := f.Get("resource"); res != "" && !sameResource(res, g.Resource) {
		return tokenResponse{}, tokenErr("invalid_target", "resource differs from the grant")
	}
	if sc := parseScope(f.Get("scope")); len(sc) > 0 {
		for _, x := range sc {
			if !slices.Contains(g.Scopes, x) {
				return tokenResponse{}, tokenErr("invalid_scope", "a refresh cannot widen the grant")
			}
		}
	}
	if te := s.userActive(ctx, g.UserID); te != nil {
		return tokenResponse{}, te
	}
	access, refresh, rows := s.newTokens(g.ID, now)
	ok, err := s.store.RotateOAuthRefresh(ctx, hash, now, rows...)
	if err != nil {
		s.log.Error("oauth: rotate refresh token", "err", err)
		return tokenResponse{}, &tokenError{http.StatusInternalServerError, "server_error", "the token could not be rotated"}
	}
	if !ok {
		// Used between the read and the rotation: a second use all the same.
		s.refreshReused(ctx, g)
		return tokenResponse{}, invalid
	}
	return tokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int(AccessTokenTTL / time.Second),
		RefreshToken: refresh, Scope: strings.Join(g.Scopes, " "),
	}, nil
}

// refreshReused ends the grant whose refresh token came back a second time:
// one of the two holders is not the client (RFC 9700 4.14.2).
func (s *Server) refreshReused(ctx context.Context, g db.OAuthGrant) {
	if err := s.store.RevokeOAuthGrant(ctx, g.ID, s.now()); err != nil {
		s.log.Error("oauth: revoke grant after refresh reuse", "err", err)
	}
	s.auditEvent(ctx, audit.ActionOAuthRefreshReuse, g.UserID, g.ID, g.ClientName, map[string]any{
		"client_id": g.ClientID, "resource": g.Resource, "via": "refresh_token",
	})
}

// Revoke serves POST /oauth/revoke (RFC 7009). It answers 200 whatever the
// token was, unless the request itself is malformed. A refresh token ends its
// grant; an access token ends itself. A token of another client is left
// alone.
func (s *Server) Revoke(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", "*")
	clientID, te := formRequest(w, r)
	if te != nil {
		writeTokenError(w, te)
		return
	}
	plain := r.PostForm.Get("token")
	if plain == "" {
		writeTokenError(w, tokenErr("invalid_request", "token is required"))
		return
	}
	ctx := r.Context()
	t, g, err := s.store.GetOAuthToken(ctx, hashSecret(plain))
	switch {
	case errors.Is(err, db.ErrNotFound):
	case err != nil:
		s.log.Error("oauth: look up token to revoke", "err", err)
		writeTokenError(w, &tokenError{http.StatusServiceUnavailable, "temporarily_unavailable", "try again later"})
		return
	case g.ClientID != clientID:
	case t.Kind == "refresh":
		if err := s.store.RevokeOAuthGrant(ctx, g.ID, s.now()); err != nil {
			s.log.Error("oauth: revoke grant", "err", err)
		} else if g.RevokedAt == nil {
			s.auditEvent(ctx, audit.ActionOAuthGrantRevoke, g.UserID, g.ID, g.ClientName, map[string]any{
				"client_id": g.ClientID, "resource": g.Resource, "via": "revocation_endpoint",
			})
		}
	default:
		if err := s.store.DeleteOAuthToken(ctx, t.Hash); err != nil {
			s.log.Error("oauth: delete access token", "err", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// AccessToken is a valid access token as a resource server sees it.
type AccessToken struct {
	GrantID       string
	ClientID      string
	ClientName    string
	UserID        string
	Resource      string // the audience; the token is good at this URL only
	Scopes        []string
	AllowedTools  []string // gateway grants: like a gateway key's allow-list
	ToolsReadOnly bool
	ExpiresAt     time.Time
}

// LookupAccessToken returns the access token for a bearer value, or
// ErrInvalidToken when it is not one, is expired, or its grant was revoked.
// It records the grant's last use (at most once a minute). The caller checks
// the audience (Resource) and the user's role and status.
func (s *Server) LookupAccessToken(ctx context.Context, plain string) (AccessToken, error) {
	if !strings.HasPrefix(plain, AccessTokenPrefix) {
		return AccessToken{}, ErrInvalidToken
	}
	t, g, err := s.store.GetOAuthToken(ctx, hashSecret(plain))
	if errors.Is(err, db.ErrNotFound) {
		return AccessToken{}, ErrInvalidToken
	}
	if err != nil {
		return AccessToken{}, err
	}
	now := s.now()
	if t.Kind != "access" || g.RevokedAt != nil || !now.Before(t.ExpiresAt) {
		return AccessToken{}, ErrInvalidToken
	}
	if err := s.store.TouchOAuthGrant(ctx, g.ID, now); err != nil {
		s.log.Warn("oauth: touch grant", "err", err)
	}
	return AccessToken{
		GrantID: g.ID, ClientID: g.ClientID, ClientName: g.ClientName, UserID: g.UserID, Resource: g.Resource,
		Scopes: g.Scopes, AllowedTools: g.AllowedTools, ToolsReadOnly: g.ToolsReadOnly, ExpiresAt: t.ExpiresAt,
	}, nil
}

// auditEvent appends one event; the actor is the grant's user (the client
// acts in their name).
func (s *Server) auditEvent(ctx context.Context, action, userID, subjectID, label string, payload map[string]any) {
	if s.audit == nil {
		return
	}
	lc := audit.LogContextFrom(ctx)
	result := "ok"
	if action == audit.ActionOAuthRefreshReuse {
		result = "denied"
	}
	if err := s.audit.Append(ctx, audit.Event{
		ActorID: userID, Action: action, SubjectID: subjectID, SubjectLabel: label, Result: result,
		SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
		Payload: audit.MustJSON(payload),
	}); err != nil {
		s.log.Warn("oauth: audit", "action", action, "err", err)
	}
}
