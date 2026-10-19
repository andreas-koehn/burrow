// Package oauth is Burrow's OAuth 2.1 authorization server for MCP clients
// (spec D19-D25): authorization code with PKCE S256, public clients only
// (pre-registered or identified by a Client ID Metadata Document), RFC 8414
// metadata, RFC 9207 iss, RFC 8707 resource indicators and RFC 7009
// revocation. Tokens are opaque and stored hashed: access tokens boa_ (1 h),
// refresh tokens bor_ (30 days, rotated on every use; reuse revokes the
// grant). The resource-server side (which MCP endpoint accepts which token)
// asks LookupAccessToken.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// Lifetimes (spec D21, Data).
const (
	AccessTokenTTL  = time.Hour
	RefreshTokenTTL = 30 * 24 * time.Hour
	CodeTTL         = 60 * time.Second
	RequestTTL      = 10 * time.Minute
)

// Token prefixes: secret-scanning friendly, and how a resource server tells
// an OAuth access token from Burrow's other bearer credentials.
const (
	AccessTokenPrefix  = "boa_"
	RefreshTokenPrefix = "bor_"
	// ClientIDPrefix starts the id of a pre-registered client.
	ClientIDPrefix = "bcl_"
)

// Scopes (spec D23).
const (
	ScopeBurrowRead  = "burrow:read"
	ScopeBurrowWrite = "burrow:write"
	ScopeMCPTools    = "mcp:tools"
)

// Resource kinds: what a resource (audience) URL names.
const (
	ResourceBurrow  = "burrow"  // <issuer>/mcp/burrow, the management server
	ResourceGateway = "gateway" // <issuer>/mcp, every tool source
	ResourceSource  = "source"  // <issuer>/mcp/<slug>, one tool source
)

// Store is the slice of *db.DB the server uses.
type Store interface {
	GetUserByID(ctx context.Context, id string) (db.User, error)
	GetMCPSource(ctx context.Context, slug string) (db.MCPSource, error)
	ListMCPSources(ctx context.Context) ([]db.MCPSource, error)

	CreateOAuthClient(ctx context.Context, c db.OAuthClient) error
	GetOAuthClient(ctx context.Context, clientID string) (db.OAuthClient, error)
	ListOAuthClients(ctx context.Context) ([]db.OAuthClient, error)
	DeleteOAuthClient(ctx context.Context, clientID string, now time.Time) error

	CreateOAuthCode(ctx context.Context, c db.OAuthCode) error
	GetOAuthCode(ctx context.Context, hash string) (db.OAuthCode, error)
	RedeemOAuthCode(ctx context.Context, hash string) (db.OAuthCode, error)
	CreateOAuthGrant(ctx context.Context, g db.OAuthGrant, codeHash string, tokens ...db.OAuthToken) error
	GetOAuthGrant(ctx context.Context, id string) (db.OAuthGrant, error)
	ListOAuthGrants(ctx context.Context, userID string) ([]db.OAuthGrant, error)
	RevokeOAuthGrant(ctx context.Context, id string, now time.Time) error
	TouchOAuthGrant(ctx context.Context, id string, now time.Time) error
	GetOAuthToken(ctx context.Context, hash string) (db.OAuthToken, db.OAuthGrant, error)
	RotateOAuthRefresh(ctx context.Context, oldHash string, now time.Time, tokens ...db.OAuthToken) (bool, error)
	DeleteOAuthToken(ctx context.Context, hash string) error
	PruneOAuth(ctx context.Context, now time.Time) error
}

// Appender is the audit log (*audit.Logger).
type Appender interface {
	Append(ctx context.Context, e audit.Event) error
}

// Config is what New needs.
type Config struct {
	Store Store
	// Issuer is the dashboard's origin ("https://burrow.example.com"), the
	// issuer identifier and the base of every resource URL. Empty: taken
	// from each request (scheme and Host), for a relay without a configured
	// public name.
	Issuer string
	// Audit records oauth.refresh.reuse and revocations through the
	// revocation endpoint. nil: not recorded.
	Audit Appender
	Log   *slog.Logger
	// CIMDClient fetches client metadata documents. nil: a client that
	// connects to public addresses only (whatever the private-upstream
	// switch says), with a 5 s timeout and no redirects. Tests replace it.
	CIMDClient *http.Client
	// Now is the clock (tests). nil: time.Now.
	Now func() time.Time
}

// Server is the authorization server. Its HTTP handlers (Metadata,
// Authorize, Token, Revoke) are mounted by the API router; the consent
// decisions (Approve, Deny) are called by the API's session-protected
// handlers.
type Server struct {
	store  Store
	issuer string
	audit  Appender
	log    *slog.Logger
	now    func() time.Time
	cimd   *cimdFetcher

	mu             sync.Mutex
	pending        map[string]*pendingRequest
	pendingPerAddr map[string]int
}

// New builds a server.
func New(cfg Config) *Server {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{
		store:   cfg.Store,
		issuer:  strings.TrimSuffix(cfg.Issuer, "/"),
		audit:   cfg.Audit,
		log:     log,
		now:     now,
		cimd:    newCIMDFetcher(cfg.CIMDClient, now),
		pending: map[string]*pendingRequest{},

		pendingPerAddr: map[string]int{},
	}
}

// Issuer is the issuer identifier for a request, "" when the server is off.
// It is the configured issuer (the dashboard's https origin) whatever the
// request's Host says. Without one the server is off, except for a request
// sent to a loopback host (localhost, 127.0.0.1, [::1]): for development the
// issuer is then that origin. Every resource URL starts with the issuer.
func (s *Server) Issuer(r *http.Request) string {
	if s.issuer != "" {
		return s.issuer
	}
	// Development: the request must be loopback by its Host and by its peer
	// (after the router's trusted-proxy handling), so that a remote client
	// cannot switch this on with "Host: localhost".
	if !isLoopbackHost(hostOnly(r.Host)) || !loopbackPeer(r) {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// Configured reports whether an issuer is configured; without one only
// loopback requests are served.
func (s *Server) Configured() bool { return s.issuer != "" }

// Available reports whether the server serves the request: an issuer is
// configured, or the request is loopback by Host and peer (development).
func (s *Server) Available(r *http.Request) bool { return s.Issuer(r) != "" }

// loopbackPeer reports whether the request's peer address is a loopback one.
func loopbackPeer(r *http.Request) bool {
	ip := net.ParseIP(hostOnly(r.RemoteAddr))
	return ip != nil && ip.IsLoopback()
}

// hostOnly is a Host header without its port and brackets.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
}

// endpoint starts an answer of the server's own endpoints: the issuer, or a
// 404 when the server is off for this request. An issuer taken from a
// loopback request is not for a cache to keep.
func (s *Server) endpoint(w http.ResponseWriter, r *http.Request) (string, bool) {
	iss := s.Issuer(r)
	if iss == "" {
		http.NotFound(w, r)
		return "", false
	}
	if !s.Configured() {
		w.Header().Set("Cache-Control", "no-store")
	}
	return iss, true
}

// ResourceURL is the resource (audience) URL of an MCP endpoint path such as
// "/mcp/burrow", "/mcp" or "/mcp/<slug>" for the request's issuer; "" when
// the server is off for the request.
func (s *Server) ResourceURL(r *http.Request, path string) string {
	iss := s.Issuer(r)
	if iss == "" {
		return ""
	}
	return iss + path
}

// metadata is the RFC 8414 document.
type metadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethods     []string `json:"revocation_endpoint_auth_methods_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported"`
	AuthorizationResponseIssSupported bool     `json:"authorization_response_iss_parameter_supported"`
	ResourceIndicatorsSupported       bool     `json:"resource_indicators_supported"`
	ResponseModesSupported            []string `json:"response_modes_supported"`
}

// Metadata serves GET /.well-known/oauth-authorization-server.
func (s *Server) Metadata(w http.ResponseWriter, r *http.Request) {
	iss, ok := s.endpoint(w, r)
	if !ok {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	if s.Configured() {
		h.Set("Cache-Control", "public, max-age=300")
	}
	writeJSON(w, http.StatusOK, metadata{
		Issuer:                            iss,
		AuthorizationEndpoint:             iss + "/oauth/authorize",
		TokenEndpoint:                     iss + "/oauth/token",
		RevocationEndpoint:                iss + "/oauth/revoke",
		ResponseTypesSupported:            []string{"code"},
		ResponseModesSupported:            []string{"query"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		RevocationEndpointAuthMethods:     []string{"none"},
		ScopesSupported:                   []string{ScopeBurrowRead, ScopeBurrowWrite, ScopeMCPTools},
		ClientIDMetadataDocumentSupported: true,
		AuthorizationResponseIssSupported: true,
		ResourceIndicatorsSupported:       true,
	})
}

// --- small helpers ---------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// hashSecret is the stored form of a code or token: sha256, hex.
func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// randomSecret is prefix + 32 random bytes in base32 (52 characters).
func randomSecret(prefix string) string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // never fails (crypto/rand panics instead)
	return prefix + b32.EncodeToString(raw)
}

// randomID is 16 random bytes in lower-case hex.
func randomID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// Errors of the management functions; the API maps them.
var (
	ErrNotFound       = errors.New("oauth: not found")
	ErrInvalidClient  = errors.New("oauth: invalid client")
	ErrInvalidScope   = errors.New("oauth: invalid scope")
	ErrScopeNotInRole = errors.New("oauth: scope not granted by the role")
	ErrToolsForbidden = errors.New("oauth: granting tools needs ai:configure:own or ai:configure:any")
	ErrInvalidTools   = errors.New("oauth: invalid allowed_tools")
	ErrInvalidToken   = errors.New("oauth: invalid token")
)
