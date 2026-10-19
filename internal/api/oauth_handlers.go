package api

// oauth_handlers.go — the API side of the OAuth authorization server
// (internal/oauth): the endpoints an MCP client calls (metadata, authorize,
// token, revoke; outside /api/v1, no session), the consent decisions of the
// dashboard (a session only, the password while no services domain is set,
// spec D24), pre-registered clients (oauth:clients:manage) and connected
// apps (grants: a user's own; every user's with oauth:clients:manage).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/oauth"
)

// Per-address limits of the endpoints an MCP client calls without a
// session. The authorization endpoint may fetch a client metadata document;
// the token endpoint hashes and looks up what it is given.
const (
	OAuthAuthorizeRatePerIP = 60
	OAuthTokenRatePerIP     = 120
)

// oauthBodyLimit bounds the JSON bodies of the consent and client routes.
const oauthBodyLimit = 16 << 10

func oauthLimiter(perMinute int) func(http.Handler) http.Handler {
	return httprate.Limit(perMinute, time.Minute,
		httprate.WithKeyFuncs(httprate.KeyByIP),
		httprate.WithLimitHandler(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": "temporarily_unavailable", "error_description": "too many requests; try again in a minute",
			})
		}))
}

// oauthEndpoint wraps one of the server's handlers: 404 while no
// authorization server is configured.
func (d Deps) oauthEndpoint(h func(*oauth.Server) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.OAuth == nil {
			http.NotFound(w, r)
			return
		}
		h(d.OAuth)(w, r)
	}
}

// mountOAuthEndpoints registers the authorization server's own endpoints on
// the root router (before the dashboard's catch-all).
func (d Deps) mountOAuthEndpoints(r chi.Router) {
	r.Get("/.well-known/oauth-authorization-server", d.oauthEndpoint(func(s *oauth.Server) http.HandlerFunc { return s.Metadata }))
	r.With(oauthLimiter(OAuthAuthorizeRatePerIP)).
		Get("/oauth/authorize", d.oauthEndpoint(func(s *oauth.Server) http.HandlerFunc { return s.Authorize }))
	r.With(oauthLimiter(OAuthTokenRatePerIP), d.withAuditContext).
		Post("/oauth/token", d.oauthEndpoint(func(s *oauth.Server) http.HandlerFunc { return s.Token }))
	r.With(oauthLimiter(OAuthTokenRatePerIP), d.withAuditContext).
		Post("/oauth/revoke", d.oauthEndpoint(func(s *oauth.Server) http.HandlerFunc { return s.Revoke }))
}

// requireOAuth answers 404 while no authorization server is configured and
// marks the answer as not cacheable.
func (d Deps) requireOAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if d.OAuth == nil {
			writeErr(w, http.StatusNotFound, "oauth is not configured")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- consent ---------------------------------------------------------------

type oauthRequestResp struct {
	ID                string            `json:"id"`
	ClientID          string            `json:"client_id"`
	ClientName        string            `json:"client_name"`
	ClientKind        string            `json:"client_kind"`
	RedirectURI       string            `json:"redirect_uri"`
	RedirectHost      string            `json:"redirect_host"`
	LocalhostRedirect bool              `json:"localhost_redirect"`
	Resource          string            `json:"resource"`
	ResourceKind      string            `json:"resource_kind"`
	Source            string            `json:"source"`
	Scopes            []string          `json:"scopes"`
	Sources           []oauth.SourceRef `json:"sources"`
	CanGrantTools     bool              `json:"can_grant_tools"`
	PasswordRequired  bool              `json:"password_required"`
	CreatedAt         time.Time         `json:"created_at"`
	ExpiresAt         time.Time         `json:"expires_at"`
}

const msgOAuthRequestNotFound = "authorization request not found or expired"

// GetOAuthRequest shows a pending authorization request to the consent page.
// GET /oauth/requests/{id}.
func (d Deps) GetOAuthRequest(w http.ResponseWriter, r *http.Request) {
	req, err := d.OAuth.PendingRequest(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, oauth.ErrNotFound) {
		writeErr(w, http.StatusNotFound, msgOAuthRequestNotFound)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	role, err := d.callerRoleForAuth(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, oauthRequestResp{
		ID: req.ID, ClientID: req.ClientID, ClientName: req.ClientName, ClientKind: req.ClientKind,
		RedirectURI: req.RedirectURI, RedirectHost: req.RedirectHost, LocalhostRedirect: req.LocalhostRedirect,
		Resource: req.Resource, ResourceKind: req.ResourceKind, Source: req.Source,
		Scopes: req.Scopes, Sources: req.Sources,
		CanGrantTools:    effectivePerms(r.Context(), role, authz.PermAIConfigureOwn) || effectivePerms(r.Context(), role, authz.PermAIConfigureAny),
		PasswordRequired: d.passwordRequired(),
		CreatedAt:        req.CreatedAt, ExpiresAt: req.ExpiresAt,
	})
}

type oauthApproveReq struct {
	// Scopes replaces the requested scopes (/mcp/burrow: burrow:read,
	// burrow:write or permission strings). Absent: as requested.
	Scopes []string `json:"scopes"`
	// AllowedTools is a gateway grant's allow-list. Absent: every source the
	// request lists; [] means no tools.
	AllowedTools *[]string `json:"allowed_tools"`
	// ToolsReadOnly limits a gateway grant to read-only tools. Absent: true.
	ToolsReadOnly *bool `json:"tools_read_only"`
	// Password is accepted instead of a recent POST /auth/reauth while no
	// services domain is set.
	Password string `json:"password"`
}

type oauthRedirectResp struct {
	RedirectURL string `json:"redirect_url"`
}

// PostOAuthRequestApprove approves a pending request in the caller's name and
// answers the address to send the browser to. While no services domain is
// set the caller needs the password (inline or a recent confirmation).
// POST /oauth/requests/{id}/approve.
func (d Deps) PostOAuthRequestApprove(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, oauthBodyLimit)
	var in oauthApproveReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !d.checkApproverPassword(w, r, in.Password) {
		return
	}
	role, err := d.callerRoleForAuth(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "approve failed")
		return
	}
	uid := userID(r.Context())
	got, err := d.OAuth.Approve(r.Context(), chi.URLParam(r, "id"), oauth.Decision{
		UserID: uid, Role: role, Scopes: in.Scopes, AllowedTools: in.AllowedTools, ToolsReadOnly: in.ToolsReadOnly,
	})
	switch {
	case errors.Is(err, oauth.ErrNotFound):
		writeErr(w, http.StatusNotFound, msgOAuthRequestNotFound)
		return
	case errors.Is(err, oauth.ErrInvalidScope):
		writeErr(w, http.StatusBadRequest, "a scope is not offered by this resource")
		return
	case errors.Is(err, oauth.ErrScopeNotInRole):
		writeErr(w, http.StatusForbidden, "a scope names a permission your role does not grant")
		return
	case errors.Is(err, oauth.ErrToolsForbidden):
		writeErr(w, http.StatusForbidden, "ai:configure:own or ai:configure:any required")
		return
	case errors.Is(err, oauth.ErrInvalidTools):
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), oauth.ErrInvalidTools.Error()+": "))
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "approve failed")
		return
	}
	payload := map[string]any{
		"client_id": got.ClientID, "client_kind": got.ClientKind, "resource": got.Resource, "scopes": got.Scopes,
	}
	if got.ResourceKind != oauth.ResourceBurrow {
		payload["allowed_tools"] = got.AllowedTools
		payload["tools_read_only"] = got.ToolsReadOnly
	}
	d.auditOAuth(r, audit.ActionOAuthGrantApprove, got.ClientID, got.ClientName, payload)
	writeJSON(w, http.StatusOK, oauthRedirectResp{RedirectURL: got.RedirectURL})
}

// PostOAuthRequestDeny denies a pending request; the client is told
// access_denied. POST /oauth/requests/{id}/deny. No body.
func (d Deps) PostOAuthRequestDeny(w http.ResponseWriter, r *http.Request) {
	got, err := d.OAuth.Deny(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, oauth.ErrNotFound) {
		writeErr(w, http.StatusNotFound, msgOAuthRequestNotFound)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "deny failed")
		return
	}
	writeJSON(w, http.StatusOK, oauthRedirectResp{RedirectURL: got.RedirectURL})
}

// --- grants (connected apps) -------------------------------------------------

type oauthGrantResp struct {
	ID            string     `json:"id"`
	ClientID      string     `json:"client_id"`
	ClientName    string     `json:"client_name"`
	UserID        string     `json:"user_id"`
	UserEmail     string     `json:"user_email"`
	Resource      string     `json:"resource"`
	Scopes        []string   `json:"scopes"`
	AllowedTools  []string   `json:"allowed_tools"`
	ToolsReadOnly bool       `json:"tools_read_only"`
	CreatedAt     time.Time  `json:"created_at"`
	LastUsed      *time.Time `json:"last_used"`
}

// mayManageAllGrants: every user's connected apps are for the holders of
// oauth:clients:manage.
func (d Deps) mayManageAllGrants(r *http.Request) (bool, error) {
	role, err := d.callerRoleForAuth(r)
	if err != nil {
		return false, err
	}
	return effectivePerms(r.Context(), role, authz.PermOAuthClientsManage), nil
}

// GetOAuthGrants lists the caller's connected apps, or with ?all=1 every
// user's (oauth:clients:manage). GET /oauth/grants.
func (d Deps) GetOAuthGrants(w http.ResponseWriter, r *http.Request) {
	owner := userID(r.Context())
	if all := r.URL.Query().Get("all"); all == "1" || all == "true" {
		ok, err := d.mayManageAllGrants(r)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if !ok {
			writeErr(w, http.StatusForbidden, "oauth:clients:manage required")
			return
		}
		owner = ""
	}
	grants, err := d.OAuth.ListGrants(r.Context(), owner)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	emails := map[string]string{}
	out := make([]oauthGrantResp, 0, len(grants))
	for _, g := range grants {
		email, seen := emails[g.UserID]
		if !seen && d.Users != nil {
			if u, err := d.Users.GetUserByID(r.Context(), g.UserID); err == nil {
				email = u.Email
			}
			emails[g.UserID] = email
		}
		out = append(out, oauthGrantResp{
			ID: g.ID, ClientID: g.ClientID, ClientName: g.ClientName, UserID: g.UserID, UserEmail: email,
			Resource: g.Resource, Scopes: g.Scopes, AllowedTools: g.AllowedTools, ToolsReadOnly: g.ToolsReadOnly,
			CreatedAt: g.CreatedAt, LastUsed: g.LastUsed,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// DeleteOAuthGrant revokes a connected app: its tokens stop working at once.
// A user revokes their own; oauth:clients:manage revokes anyone's. Another
// user's grant is 404 otherwise. DELETE /oauth/grants/{id}.
func (d Deps) DeleteOAuthGrant(w http.ResponseWriter, r *http.Request) {
	owner := userID(r.Context())
	all, err := d.mayManageAllGrants(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	if all {
		owner = ""
	}
	g, err := d.OAuth.RevokeGrant(r.Context(), chi.URLParam(r, "id"), owner)
	if errors.Is(err, oauth.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "grant not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	d.auditOAuth(r, audit.ActionOAuthGrantRevoke, g.ID, g.ClientName, map[string]any{
		"client_id": g.ClientID, "resource": g.Resource, "user_id": g.UserID,
	})
	w.WriteHeader(http.StatusNoContent)
}

// --- pre-registered clients ----------------------------------------------------

type oauthClientResp struct {
	ClientID     string    `json:"client_id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

func oauthClientOf(c db.OAuthClient) oauthClientResp {
	return oauthClientResp{ClientID: c.ClientID, Name: c.Name, RedirectURIs: c.RedirectURIs, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt}
}

type postOAuthClientReq struct {
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// GetOAuthClients lists the pre-registered clients. GET /oauth/clients.
func (d Deps) GetOAuthClients(w http.ResponseWriter, r *http.Request) {
	cs, err := d.OAuth.ListClients(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]oauthClientResp, 0, len(cs))
	for _, c := range cs {
		out = append(out, oauthClientOf(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// PostOAuthClient registers a public client for an MCP client that has no
// metadata document. POST /oauth/clients.
func (d Deps) PostOAuthClient(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, oauthBodyLimit)
	var in postOAuthClientReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	c, err := d.OAuth.CreateClient(r.Context(), oauth.NewClient{
		Name: in.Name, RedirectURIs: in.RedirectURIs, CreatedBy: userID(r.Context()),
	})
	if errors.Is(err, oauth.ErrInvalidClientInput) {
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), oauth.ErrInvalidClientInput.Error()+": "))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create failed")
		return
	}
	d.auditOAuth(r, audit.ActionOAuthClientCreate, c.ClientID, c.Name, map[string]any{"redirect_uris": c.RedirectURIs})
	writeJSON(w, http.StatusCreated, oauthClientOf(c))
}

// DeleteOAuthClient removes a pre-registered client and revokes every grant
// it holds. DELETE /oauth/clients/{id}.
func (d Deps) DeleteOAuthClient(w http.ResponseWriter, r *http.Request) {
	c, err := d.OAuth.DeleteClient(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, oauth.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "client not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "delete failed")
		return
	}
	d.auditOAuth(r, audit.ActionOAuthClientDelete, c.ClientID, c.Name, map[string]any{"redirect_uris": c.RedirectURIs})
	w.WriteHeader(http.StatusNoContent)
}

// auditOAuth appends one OAuth event in the caller's name (best-effort).
func (d Deps) auditOAuth(r *http.Request, action, subjectID, label string, payload map[string]any) {
	if d.AuditAppender == nil {
		return
	}
	lc := audit.LogContextFrom(r.Context())
	_ = d.AuditAppender.Append(r.Context(), audit.Event{
		ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
		Action: action, SubjectID: subjectID, SubjectLabel: label, Result: "ok",
		SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
		Payload: audit.MustJSON(payload),
	})
}
