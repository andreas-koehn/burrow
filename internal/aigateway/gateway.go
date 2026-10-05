package aigateway

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
)

// ProviderStore resolves a provider slug to its record.
type ProviderStore interface {
	ProviderBySlug(ctx context.Context, slug string) (db.AIProvider, error) // db.ErrNotFound
}

// KeyValidator checks a presented API key against a service's keys.
type KeyValidator interface {
	ValidateAPIKey(ctx context.Context, serviceID, presented string) (keyID string, ok bool, err error)
}

// TunnelDialer reaches the live tunnel of a service, addressed by service id.
type TunnelDialer interface {
	LookupByServiceID(ctx context.Context, serviceID string) (*proxy.Resolved, error) // proxy.ErrNotFound
	DialTunnelStreamByServiceID(ctx context.Context, serviceID string) (net.Conn, error)
}

// Chain is the AI chain (cache, limits, usage) a request runs through.
type Chain interface {
	// Dispatch is pass-through for a service without AI config (no usage row).
	Dispatch(w http.ResponseWriter, r *http.Request, serviceID, localHost, apiKeyHeader, apiKeyID string, upstream http.Handler)
	// DispatchMetered always runs the chain and records usage.
	DispatchMetered(w http.ResponseWriter, r *http.Request, serviceID, localHost, apiKeyHeader, apiKeyID string, upstream http.Handler)
}

// Gateway serves requests under /ai/<provider>/.
type Gateway struct {
	Providers  ProviderStore
	Keys       KeyValidator
	Tunnels    TunnelDialer
	Chain      Chain                                           // nil = call the upstream directly
	IPGeoDeny  func(res *proxy.Resolved, r *http.Request) bool // nil = no policy check
	PublicHost string                                          // auth domain, for X-Forwarded-Host
	Log        *slog.Logger
}

// Serve handles one request for provider slug. r.URL.Path must already have
// the "/ai/<slug>" prefix removed.
func (g *Gateway) Serve(w http.ResponseWriter, r *http.Request, slug string) {
	ctx := r.Context()

	p, err := g.Providers.ProviderBySlug(ctx, slug)
	if errors.Is(err, db.ErrNotFound) {
		WriteError(w, http.StatusNotFound, "provider_not_found", "unknown provider "+slug)
		return
	}
	if err != nil {
		g.Log.Error("aigateway: provider lookup failed", "provider", slug, "err", err)
		WriteError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	keyID, ok := g.authenticate(w, r, p)
	if !ok {
		return
	}
	// Tell the caller which provider answered; set only once the caller is
	// known to hold a valid key.
	w.Header().Set("Burrow-Provider", p.Slug)

	upstream, host, ok := g.upstreamFor(w, r, p)
	if !ok {
		return
	}

	// The Burrow key and the dashboard's cookies stop here.
	r.Header.Del("Authorization")
	r.Header.Del("X-Api-Key")
	r.Header.Del("Cookie")

	if g.Chain == nil {
		upstream.ServeHTTP(w, r)
		return
	}
	// Only inference calls are metered. A model listing or a health probe
	// must not show up as usage.
	if r.Method == http.MethodPost {
		g.Chain.DispatchMetered(w, r, p.ServiceID, host, "Authorization", keyID, upstream)
		return
	}
	g.Chain.Dispatch(w, r, p.ServiceID, host, "Authorization", keyID, upstream)
}

// authenticate checks the presented key against the provider's backing
// service. It writes the error response itself and reports ok=false.
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request, p db.AIProvider) (string, bool) {
	presented := presentedKey(r)
	if presented == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		WriteError(w, http.StatusUnauthorized, "invalid_api_key", "missing API key")
		return "", false
	}
	keyID, ok, err := g.Keys.ValidateAPIKey(r.Context(), p.ServiceID, presented)
	if err != nil {
		g.Log.Warn("aigateway: api key validation failed", "provider", p.Slug, "err", err)
		WriteError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return "", false
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		WriteError(w, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
		return "", false
	}
	return keyID, true
}

// presentedKey reads the caller's key from "Authorization: Bearer …" (OpenAI
// clients) or "X-Api-Key" (Anthropic clients).
func presentedKey(r *http.Request) string {
	if tok, ok := strings.CutPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "); ok {
		return strings.TrimSpace(tok)
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// upstreamFor returns the handler that reaches the provider's upstream and the
// upstream host (for diagnostics). It writes the error response itself.
func (g *Gateway) upstreamFor(w http.ResponseWriter, r *http.Request, p db.AIProvider) (http.Handler, string, bool) {
	switch p.Kind {
	case "tunnel":
		return g.tunnelUpstream(w, r, p)
	default:
		WriteError(w, http.StatusServiceUnavailable, "provider_unavailable", "provider kind is not supported by this relay")
		return nil, "", false
	}
}

func (g *Gateway) tunnelUpstream(w http.ResponseWriter, r *http.Request, p db.AIProvider) (http.Handler, string, bool) {
	res, err := g.Tunnels.LookupByServiceID(r.Context(), p.ServiceID)
	if errors.Is(err, proxy.ErrNotFound) {
		WriteError(w, http.StatusBadGateway, "provider_offline", "the client serving this provider is not connected")
		return nil, "", false
	}
	if err != nil {
		g.Log.Error("aigateway: tunnel lookup failed", "provider", p.Slug, "err", err)
		WriteError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return nil, "", false
	}
	// The AI namespace is key-authenticated only. A service switched to
	// another mode is no longer meant to be reached with an API key.
	if res.AccessMode != "api_key" {
		WriteError(w, http.StatusForbidden, "provider_unavailable", "this provider's service is not in API-key mode")
		return nil, "", false
	}
	if g.IPGeoDeny != nil && g.IPGeoDeny(res, r) {
		WriteError(w, http.StatusForbidden, "forbidden", "your address is not allowed to use this provider")
		return nil, "", false
	}

	serviceID, host, publicHost := p.ServiceID, res.LocalHost, g.PublicHost
	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // token streams must not be buffered
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header.Del("X-Forwarded-Port")
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", publicHost)
			pr.Out.URL = &url.URL{Scheme: "http", Host: host, Path: pr.In.URL.Path, RawPath: pr.In.URL.RawPath, RawQuery: pr.In.URL.RawQuery}
			pr.Out.Host = host
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return g.Tunnels.DialTunnelStreamByServiceID(ctx, serviceID)
			},
			DisableCompression: true,
			DisableKeepAlives:  true, // one tunnel stream per request
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			g.Log.Warn("aigateway: tunnel upstream error", "provider", p.Slug, "err", err)
			if errors.Is(err, proxy.ErrNotFound) {
				WriteError(w, http.StatusBadGateway, "provider_offline", "the client serving this provider is not connected")
				return
			}
			WriteError(w, http.StatusBadGateway, "upstream_unavailable", "the provider did not answer")
		},
	}
	return rp, host, true
}
