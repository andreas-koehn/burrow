package oauth

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// maxPending caps the authorization requests waiting for a decision. Anyone
// can open one, so the cap keeps memory bounded; expired ones go first.
const maxPending = 10000

// maxAuthorizeQuery bounds the query of an authorization request.
const maxAuthorizeQuery = 8 << 10

// maxGrantTools bounds a grant's tool allow-list, like a gateway key's.
const maxGrantTools = 64

// pkceChallenge is an S256 code challenge: base64url of 32 bytes is 43
// characters; RFC 7636 allows 43 to 128 of the unreserved set.
var pkceChallenge = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// clientInfo is a resolved client: pre-registered or from its document.
type clientInfo struct {
	ID           string
	Name         string
	Kind         string // "registered" or "metadata_document"
	RedirectURIs []string
}

// pendingRequest is an authorization request waiting for the user's
// decision on the consent page.
type pendingRequest struct {
	ID            string
	Issuer        string
	Client        clientInfo
	RedirectURI   string
	State         string
	CodeChallenge string
	Resource      string
	ResourceKind  string
	Source        string // slug, for ResourceSource
	Scopes        []string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// SourceRef names a tool source on the consent page.
type SourceRef struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Request is what the consent page shows of a pending request.
type Request struct {
	ID                string
	ClientID          string
	ClientName        string
	ClientKind        string // "registered" or "metadata_document"
	RedirectURI       string
	RedirectHost      string
	LocalhostRedirect bool
	Resource          string
	ResourceKind      string
	Source            string
	Scopes            []string
	// Sources are the tool sources a gateway grant can name ("<slug>__*"):
	// every enabled one for /mcp, the one for /mcp/<slug>.
	Sources   []SourceRef
	CreatedAt time.Time
	ExpiresAt time.Time
}

// authzError is an RFC 6749 error of the authorization endpoint.
type authzError struct{ code, desc string }

func (e authzError) Error() string { return e.code + ": " + e.desc }

// Authorize serves GET /oauth/authorize. It checks the client and its
// redirect URI first: until both are known good, errors are shown as a page
// and never sent to the redirect URI. After that every error goes back to the
// client with error, state and iss. A valid request is stored for ten minutes
// and the browser is sent to the consent page.
func (s *Server) Authorize(w http.ResponseWriter, r *http.Request) {
	// A pending request is kept in memory: its size is bounded here.
	if len(r.URL.RawQuery) > maxAuthorizeQuery {
		s.errorPage(w, "The request is too long.")
		return
	}
	q := r.URL.Query()
	for _, k := range []string{"client_id", "redirect_uri"} {
		if len(q[k]) > 1 {
			s.errorPage(w, "The request repeats "+k+".")
			return
		}
	}
	client, err := s.resolveClient(r.Context(), q.Get("client_id"))
	if err != nil {
		s.errorPage(w, clientErrorText(err))
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" {
		s.errorPage(w, "The request names no redirect_uri.")
		return
	}
	if !redirectAllowed(client.RedirectURIs, redirectURI) {
		s.errorPage(w, "The redirect_uri is not registered for this client.")
		return
	}
	iss := s.Issuer(r)
	state := q.Get("state")
	fail := func(code, desc string) {
		http.Redirect(w, r, withParams(redirectURI, map[string]string{
			"error": code, "error_description": desc, "state": state, "iss": iss,
		}), http.StatusFound)
	}
	for k, v := range q {
		if len(v) > 1 {
			fail("invalid_request", "parameter "+k+" is repeated")
			return
		}
	}
	if rt := q.Get("response_type"); rt != "code" {
		fail("unsupported_response_type", "response_type must be code")
		return
	}
	if q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method S256 is required")
		return
	}
	challenge := q.Get("code_challenge")
	if !pkceChallenge.MatchString(challenge) {
		fail("invalid_request", "code_challenge is missing or malformed")
		return
	}
	resource, kind, source, err := s.resolveResource(r.Context(), iss, q.Get("resource"))
	if err != nil {
		var ae authzError
		if errors.As(err, &ae) {
			fail(ae.code, ae.desc)
			return
		}
		s.log.Error("oauth: resolve resource", "err", err)
		fail("server_error", "the resource could not be checked")
		return
	}
	scopes := parseScope(q.Get("scope"))
	if len(scopes) == 0 {
		scopes = defaultScopes(kind)
	}
	if !validScopes(kind, scopes) {
		fail("invalid_scope", "a requested scope is not offered by this resource")
		return
	}
	now := s.now()
	p := &pendingRequest{
		ID: randomID(), Issuer: iss, Client: client, RedirectURI: redirectURI, State: state,
		CodeChallenge: challenge, Resource: resource, ResourceKind: kind, Source: source, Scopes: scopes,
		CreatedAt: now, ExpiresAt: now.Add(RequestTTL),
	}
	if !s.addPending(p) {
		fail("temporarily_unavailable", "too many authorization requests are waiting; try again later")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, iss+"/oauth/consent?request="+url.QueryEscape(p.ID), http.StatusFound)
}

// errorPage answers an authorization request that cannot be sent back to its
// client: a short plain page.
func (s *Server) errorPage(w http.ResponseWriter, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>Authorization failed</title>"+
		"<h1>Authorization failed</h1><p>%s</p><p>Return to the application and try again.</p>", html.EscapeString(msg))
}

func clientErrorText(err error) string {
	var ce errCIMD
	switch {
	case errors.As(err, &ce):
		return "The client's " + ce.Error() + "."
	case errors.Is(err, ErrInvalidClient):
		return "The client_id is unknown."
	default:
		return "The client could not be checked."
	}
}

// resolveClient finds a pre-registered client, or fetches the metadata
// document of an https client_id.
func (s *Server) resolveClient(ctx context.Context, id string) (clientInfo, error) {
	if id == "" || len(id) > 2048 {
		return clientInfo{}, ErrInvalidClient
	}
	if IsMetadataClientID(id) {
		doc, err := s.cimd.fetch(ctx, id)
		if err != nil {
			return clientInfo{}, err
		}
		return clientInfo{ID: id, Name: doc.ClientName, Kind: "metadata_document", RedirectURIs: doc.RedirectURIs}, nil
	}
	c, err := s.store.GetOAuthClient(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return clientInfo{}, ErrInvalidClient
	}
	if err != nil {
		return clientInfo{}, err
	}
	return clientInfo{ID: c.ClientID, Name: c.Name, Kind: "registered", RedirectURIs: c.RedirectURIs}, nil
}

// resolveResource checks a resource parameter (RFC 8707) against the MCP
// endpoints of this issuer and returns it normalised (no trailing slash)
// with its kind and source slug.
func (s *Server) resolveResource(ctx context.Context, iss, raw string) (resource, kind, source string, err error) {
	if raw == "" {
		return "", "", "", authzError{"invalid_target", "the resource parameter is required"}
	}
	res := strings.TrimSuffix(raw, "/")
	rest, ok := strings.CutPrefix(res, iss+"/mcp")
	switch {
	case !ok:
	case rest == "":
		return res, ResourceGateway, "", nil
	case rest == "/burrow":
		return res, ResourceBurrow, "", nil
	case strings.HasPrefix(rest, "/") && store.ValidToolSourceSlug(rest[1:]):
		slug := rest[1:]
		_, err := s.store.GetMCPSource(ctx, slug)
		if errors.Is(err, db.ErrNotFound) {
			break
		}
		if err != nil {
			return "", "", "", err
		}
		return res, ResourceSource, slug, nil
	}
	return "", "", "", authzError{"invalid_target", "the resource is not an MCP endpoint of this relay"}
}

// withParams adds the parameters (empty values left out) to a redirect URI
// that may have a query of its own.
func withParams(raw string, params map[string]string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for _, k := range []string{"code", "state", "error", "error_description", "iss"} {
		if v := params[k]; v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// --- pending requests ------------------------------------------------------

func (s *Server) addPending(p *pendingRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) >= maxPending {
		now := s.now()
		for id, e := range s.pending {
			if !now.Before(e.ExpiresAt) {
				delete(s.pending, id)
			}
		}
		if len(s.pending) >= maxPending {
			return false
		}
	}
	s.pending[p.ID] = p
	return true
}

// getPending returns a live request, or nil.
func (s *Server) getPending(id string) *pendingRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[id]
	if p == nil {
		return nil
	}
	if !s.now().Before(p.ExpiresAt) {
		delete(s.pending, id)
		return nil
	}
	return p
}

// takePending removes and returns a live request, or nil: a request is
// decided once.
func (s *Server) takePending(id string) *pendingRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[id]
	delete(s.pending, id)
	if p == nil || !s.now().Before(p.ExpiresAt) {
		return nil
	}
	return p
}

// PendingRequest returns what the consent page shows of a request, or
// ErrNotFound when it is unknown, decided or expired.
func (s *Server) PendingRequest(ctx context.Context, id string) (Request, error) {
	p := s.getPending(id)
	if p == nil {
		return Request{}, ErrNotFound
	}
	out := Request{
		ID: p.ID, ClientID: p.Client.ID, ClientName: p.Client.Name, ClientKind: p.Client.Kind,
		RedirectURI: p.RedirectURI, RedirectHost: redirectHost(p.RedirectURI),
		LocalhostRedirect: IsLoopbackRedirect(p.RedirectURI),
		Resource:          p.Resource, ResourceKind: p.ResourceKind, Source: p.Source,
		Scopes:    slices.Clone(p.Scopes),
		Sources:   []SourceRef{},
		CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
	}
	if p.ResourceKind != ResourceBurrow {
		srcs, err := s.grantableSources(ctx, p)
		if err != nil {
			return Request{}, err
		}
		for _, src := range srcs {
			out.Sources = append(out.Sources, SourceRef{Slug: src.Slug, Name: src.Name})
		}
	}
	return out, nil
}

// grantableSources are the sources a gateway grant of the request may name.
func (s *Server) grantableSources(ctx context.Context, p *pendingRequest) ([]db.MCPSource, error) {
	if p.ResourceKind == ResourceSource {
		src, err := s.store.GetMCPSource(ctx, p.Source)
		if errors.Is(err, db.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []db.MCPSource{src}, nil
	}
	all, err := s.store.ListMCPSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]db.MCPSource, 0, len(all))
	for _, src := range all {
		if src.Enabled {
			out = append(out, src)
		}
	}
	return out, nil
}

// Decision is the user's answer on the consent page.
type Decision struct {
	UserID string
	Role   string // the user's role now
	// Scopes replaces the requested scopes when non-nil (/mcp/burrow: the
	// read-only switch chooses burrow:read).
	Scopes []string
	// AllowedTools is the gateway grant's tool allow-list ("<source>__*",
	// "<source>__<tool>"); nil: every grantable source. Empty: no tools.
	AllowedTools *[]string
	// ToolsReadOnly limits a gateway grant to read-only tools; nil: true.
	ToolsReadOnly *bool
}

// Approved is what an approval granted, for the audit entry.
type Approved struct {
	ClientID      string
	ClientName    string
	ClientKind    string
	Resource      string
	ResourceKind  string
	Scopes        []string
	AllowedTools  []string
	ToolsReadOnly bool
	RedirectURL   string
}

// Approve decides a request in the user's favour: it checks the decision,
// stores a 60-second authorization code holding the grant to be, and returns
// the redirect to the client (code, state, iss). An invalid decision leaves
// the request waiting. Errors: ErrNotFound, ErrInvalidScope,
// ErrScopeNotInRole, ErrToolsForbidden, ErrInvalidTools.
func (s *Server) Approve(ctx context.Context, id string, d Decision) (Approved, error) {
	p := s.getPending(id)
	if p == nil {
		return Approved{}, ErrNotFound
	}
	scopes := p.Scopes
	if d.Scopes != nil {
		scopes = parseScope(strings.Join(d.Scopes, " "))
	}
	if !validScopes(p.ResourceKind, scopes) {
		return Approved{}, ErrInvalidScope
	}
	var tools []string
	readOnly := false
	if p.ResourceKind == ResourceBurrow {
		for _, sc := range scopes {
			if sc != ScopeBurrowRead && sc != ScopeBurrowWrite && !authz.Can(d.Role, authz.Permission(sc)) {
				return Approved{}, ErrScopeNotInRole
			}
		}
	} else {
		if !authz.Can(d.Role, authz.PermAIConfigureOwn) && !authz.Can(d.Role, authz.PermAIConfigureAny) {
			return Approved{}, ErrToolsForbidden
		}
		var err error
		if tools, err = s.grantTools(ctx, p, d.AllowedTools); err != nil {
			return Approved{}, err
		}
		readOnly = d.ToolsReadOnly == nil || *d.ToolsReadOnly
	}
	if p = s.takePending(id); p == nil {
		return Approved{}, ErrNotFound
	}
	code := randomSecret("")
	now := s.now()
	if err := s.store.CreateOAuthCode(ctx, db.OAuthCode{
		Hash: hashSecret(code), ClientID: p.Client.ID, ClientName: p.Client.Name, UserID: d.UserID,
		RedirectURI: p.RedirectURI, CodeChallenge: p.CodeChallenge, Resource: p.Resource,
		Scopes: scopes, AllowedTools: tools, ToolsReadOnly: readOnly,
		ExpiresAt: now.Add(CodeTTL), CreatedAt: now,
	}); err != nil {
		return Approved{}, err
	}
	if err := s.store.PruneOAuth(ctx, now); err != nil {
		s.log.Warn("oauth: prune", "err", err)
	}
	return Approved{
		ClientID: p.Client.ID, ClientName: p.Client.Name, ClientKind: p.Client.Kind,
		Resource: p.Resource, ResourceKind: p.ResourceKind, Scopes: scopes, AllowedTools: tools, ToolsReadOnly: readOnly,
		RedirectURL: withParams(p.RedirectURI, map[string]string{"code": code, "state": p.State, "iss": p.Issuer}),
	}, nil
}

// grantTools checks a gateway grant's allow-list; nil chooses every source
// the request can name.
func (s *Server) grantTools(ctx context.Context, p *pendingRequest, chosen *[]string) ([]string, error) {
	if chosen == nil {
		srcs, err := s.grantableSources(ctx, p)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(srcs))
		for _, src := range srcs {
			out = append(out, src.Slug+"__*")
		}
		return out, nil
	}
	if len(*chosen) > maxGrantTools {
		return nil, fmt.Errorf("%w: at most %d entries", ErrInvalidTools, maxGrantTools)
	}
	out := make([]string, 0, len(*chosen))
	for _, e := range *chosen {
		if !store.ValidToolEntry(e) {
			return nil, fmt.Errorf(`%w: an entry must be "<source>__<tool>" or "<source>__*"`, ErrInvalidTools)
		}
		if p.ResourceKind == ResourceSource && !strings.HasPrefix(e, p.Source+"__") {
			return nil, fmt.Errorf("%w: every entry must name the source %s", ErrInvalidTools, p.Source)
		}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out, nil
}

// Denied is what a denial answers.
type Denied struct {
	ClientID    string
	ClientName  string
	RedirectURL string
}

// Deny decides a request against the client: the redirect carries
// error=access_denied, state and iss. ErrNotFound when it is not pending.
func (s *Server) Deny(_ context.Context, id string) (Denied, error) {
	p := s.takePending(id)
	if p == nil {
		return Denied{}, ErrNotFound
	}
	return Denied{
		ClientID: p.Client.ID, ClientName: p.Client.Name,
		RedirectURL: withParams(p.RedirectURI, map[string]string{
			"error": "access_denied", "error_description": "the user denied the request", "state": p.State, "iss": p.Issuer,
		}),
	}, nil
}
