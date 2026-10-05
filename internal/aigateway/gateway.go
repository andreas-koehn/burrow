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

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/aiprovider"
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
	Aliases    AliasStore                                      // nil = aliases are not applied
	IPGeoDeny  func(res *proxy.Resolved, r *http.Request) bool // nil = no policy check
	PublicHost string                                          // auth domain, for X-Forwarded-Host
	Log        *slog.Logger

	// Direct builds the upstream handler for a provider the relay calls
	// itself. nil = direct providers are not available on this relay.
	Direct func(p db.AIProvider) (http.Handler, error)
	// ServicePolicy reads the access mode and IP/geo policy of a direct
	// provider's backing service; such a provider has no tunnel to read
	// them from. Required for direct providers.
	ServicePolicy func(ctx context.Context, serviceID string) (*proxy.Resolved, error)
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

	// Resolve the upstream first: the backing service's IP/geo policy is
	// enforced there, so a blocked address can neither probe keys nor touch
	// a key's last-used time.
	upstream, host, ok := g.upstreamFor(w, r, p)
	if !ok {
		return
	}

	keyID, ok := g.authenticate(w, r, p)
	if !ok {
		return
	}
	if p.Kind == "direct" {
		// Built only now: it reads the upstream credential, which is not
		// done for a caller without a valid key.
		if upstream, ok = g.directUpstream(w, p); !ok {
			return
		}
	}
	// Tell the caller which provider answered; set only once the caller is
	// known to hold a valid key.
	w.Header().Set("Burrow-Provider", p.Slug)

	// The Burrow key and the dashboard's cookies stop here.
	r.Header.Del("Authorization")
	r.Header.Del("X-Api-Key")
	r.Header.Del("Cookie")

	if err := rewriteModelAlias(r, p.ServiceID, g.Aliases); err != nil {
		// Forwarding the request as sent is better than failing it.
		// err is a failed body read or a failed alias lookup.
		g.Log.Warn("aigateway: model alias not applied", "provider", p.Slug, "err", err)
	}

	if g.Chain == nil {
		upstream.ServeHTTP(w, r)
		return
	}
	// Errors the chain writes itself (rate limit, body cap, guardrails) take
	// the /ai/ shape too.
	r = r.WithContext(aigw.WithErrorWriter(r.Context(), WriteError))
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
// clients) or "X-Api-Key" (Anthropic clients). The scheme name is matched
// without regard to case.
func presentedKey(r *http.Request) string {
	const scheme = "Bearer "
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); len(auth) > len(scheme) && strings.EqualFold(auth[:len(scheme)], scheme) {
		return strings.TrimSpace(auth[len(scheme):])
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// upstreamFor returns the handler that reaches the provider's upstream and the
// upstream host (for diagnostics). It writes the error response itself. For a
// direct provider it checks the policy only and returns no handler; Serve
// builds that one after the key check.
func (g *Gateway) upstreamFor(w http.ResponseWriter, r *http.Request, p db.AIProvider) (http.Handler, string, bool) {
	switch p.Kind {
	case "tunnel":
		return g.tunnelUpstream(w, r, p)
	case "direct":
		if !g.directAllowed(w, r, p) {
			return nil, "", false
		}
		return nil, upstreamHost(p.BaseURL), true
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
	if !g.policyAllows(w, r, p, res) {
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
		ModifyResponse: func(resp *http.Response) error {
			aiprovider.DropSetCookie(resp.Header)
			return nil
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

// policyAllows enforces the backing service's access mode and IP/geo policy,
// the same for every provider kind. It writes the error response itself.
func (g *Gateway) policyAllows(w http.ResponseWriter, r *http.Request, p db.AIProvider, res *proxy.Resolved) bool {
	if res.AccessMode != "api_key" {
		WriteError(w, http.StatusForbidden, "provider_unavailable", "this provider's service is not in API-key mode")
		return false
	}
	if g.IPGeoDeny != nil && g.IPGeoDeny(res, r) {
		g.Log.Info("aigateway: ip-geo deny", "provider", p.Slug, "service_id", res.ServiceID, "remote_addr", r.RemoteAddr)
		WriteError(w, http.StatusForbidden, "forbidden", "your address is not allowed to use this provider")
		return false
	}
	return true
}

// directAllowed is the policy step for a direct provider. It reads the
// backing service's row, never the tunnel registry.
func (g *Gateway) directAllowed(w http.ResponseWriter, r *http.Request, p db.AIProvider) bool {
	if g.Direct == nil || g.ServicePolicy == nil {
		WriteError(w, http.StatusServiceUnavailable, "provider_unavailable", "direct providers are not available on this relay")
		return false
	}
	res, err := g.ServicePolicy(r.Context(), p.ServiceID)
	if err != nil || res == nil {
		g.Log.Error("aigateway: service policy lookup failed", "provider", p.Slug, "err", err)
		WriteError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return false
	}
	return g.policyAllows(w, r, p, res)
}

func (g *Gateway) directUpstream(w http.ResponseWriter, p db.AIProvider) (http.Handler, bool) {
	h, err := g.Direct(p)
	switch {
	case errors.Is(err, aiprovider.ErrNotConfigured):
		g.Log.Warn("aigateway: credential slot is not set", "provider", p.Slug, "slot", p.CredentialSlot)
		WriteError(w, http.StatusServiceUnavailable, "provider_not_configured", "this provider has no upstream credential configured")
		return nil, false
	case err != nil || h == nil:
		// err names the provider and the faulty setting, never the
		// credential or the base URL.
		g.Log.Error("aigateway: provider configuration is invalid", "provider", p.Slug, "err", err)
		WriteError(w, http.StatusServiceUnavailable, "provider_misconfigured", "this provider's configuration is invalid")
		return nil, false
	}
	return h, true
}

// upstreamHost is the host part of a base URL, for diagnostics only.
func upstreamHost(base string) string {
	if u, err := url.Parse(base); err == nil {
		return u.Host
	}
	return ""
}

// DirectUpstreams returns the factory used for Gateway.Direct. The handler is
// built per request from the provider row and the vault, so nothing holding a
// credential outlives the request and an edited or deleted provider leaves no
// handler behind. rt is shared by all providers and carries no credential.
func DirectUpstreams(v aiprovider.Vault, rt http.RoundTripper) func(db.AIProvider) (http.Handler, error) {
	return func(p db.AIProvider) (http.Handler, error) {
		return aiprovider.NewUpstream(aiprovider.Config{
			Slug:           p.Slug,
			BaseURL:        p.BaseURL,
			CredentialSlot: p.CredentialSlot,
			AuthHeader:     p.AuthHeader,
			AuthFormat:     p.AuthFormat,
			ExtraHeaders:   p.ExtraHeaders,
		}, v, rt, WriteError)
	}
}
