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
	"github.com/ankoehn/burrow/internal/store"
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
	// trustReportedCost lets the upstream's own cost figure into the usage row.
	DispatchMetered(w http.ResponseWriter, r *http.Request, serviceID, localHost, apiKeyHeader, apiKeyID string, trustReportedCost bool, upstream http.Handler)
}

// ModelLister reads a provider's stored model list.
type ModelLister interface {
	ListProviderModels(ctx context.Context, slug string) ([]db.AIProviderModel, error)
}

// Gateway serves requests under /ai/<provider>/ and the dialect endpoints
// (/openai/v1, /ai/v1, /anthropic).
type Gateway struct {
	Providers  ProviderStore
	Keys       KeyValidator
	Tunnels    TunnelDialer
	Chain      Chain                                           // nil = call the upstream directly
	Synthetic  SyntheticModels                                 // nil = no synthetic models
	IPGeoDeny  func(res *proxy.Resolved, r *http.Request) bool // nil = no policy check
	PublicHost string                                          // auth domain, for X-Forwarded-Host
	Log        *slog.Logger
	Models     ModelLister // nil = /v1/models is always forwarded

	// GatewayKeys validates gateway keys ("bgw_…"). nil = none is accepted.
	GatewayKeys GatewayKeys
	// Catalog feeds the model list of a dialect endpoint. nil = empty list.
	Catalog Catalog
	// MaxBody is the largest request body, in bytes, a dialect endpoint (or
	// a gateway key on a provider path) accepts. 0 = 8 MiB.
	MaxBody int64

	// Direct builds the upstream handler for a provider the relay calls
	// itself. nil = direct providers are not available on this relay.
	// ew writes the errors that handler originates, in the caller's dialect.
	Direct func(p db.AIProvider, ew aiprovider.ErrorWriter) (http.Handler, error)
	// ServicePolicy reads the access mode and IP/geo policy of a direct
	// provider's backing service; such a provider has no tunnel to read
	// them from. Required for direct providers.
	ServicePolicy func(ctx context.Context, serviceID string) (*proxy.Resolved, error)
}

// Serve handles one request for provider slug. r.URL.Path must already have
// the "/ai/<slug>" prefix removed.
func (g *Gateway) Serve(w http.ResponseWriter, r *http.Request, slug string) {
	// Everything the gateway and the chain write from here on (rate limit,
	// body cap, guardrails) is in the /ai/ shape.
	r = r.WithContext(aigw.WithErrorWriter(r.Context(), WriteError))
	ctx := r.Context()

	p, err := g.Providers.ProviderBySlug(ctx, slug)
	if errors.Is(err, db.ErrNotFound) {
		g.fail(w, r, http.StatusNotFound, "provider_not_found", "unknown provider "+slug)
		return
	}
	if err != nil {
		g.Log.Error("aigateway: provider lookup failed", "provider", slug, "err", err)
		g.fail(w, r, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Resolve the upstream first: the backing service's IP/geo policy is
	// enforced there, so a blocked address can neither probe keys nor touch
	// a key's last-used time.
	upstream, host, ok := g.upstreamFor(w, r, p)
	if !ok {
		return
	}

	r, keyID, ok := g.authenticate(w, r, p)
	if !ok {
		return
	}
	if g.serveModels(w, r, p) {
		return
	}
	if p.Kind == "direct" {
		// Built only now: it reads the upstream credential, which is not
		// done for a caller without a valid key.
		if upstream, ok = g.directUpstream(w, r, p); !ok {
			return
		}
	}
	// Tell the caller which provider answered; set only once the caller is
	// known to hold a valid key.
	w.Header().Set(headerProvider, p.Slug)

	// The Burrow key and the dashboard's cookies stop here.
	stripCredentials(r)

	if g.Chain == nil {
		upstream.ServeHTTP(w, r)
		return
	}
	// Only inference calls are metered. A model listing or a health probe
	// must not show up as usage.
	if r.Method == http.MethodPost {
		// Only an upstream the relay calls itself is believed about what a
		// request cost. A tunnelled model is run by whoever holds the tunnel.
		g.Chain.DispatchMetered(w, r, p.ServiceID, host, "Authorization", keyID, p.Kind == "direct", upstream)
		return
	}
	g.Chain.Dispatch(w, r, p.ServiceID, host, "Authorization", keyID, upstream)
}

// fail writes an error the gateway originates, in the shape of the request's
// dialect. Entry points put that writer into the request context.
func (g *Gateway) fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if ew := aigw.ErrorWriterFrom(r.Context()); ew != nil {
		ew(w, status, code, message)
		return
	}
	WriteError(w, status, code, message)
}

// requestErrorWriter returns the error writer of r's dialect.
func requestErrorWriter(r *http.Request) aiprovider.ErrorWriter {
	if ew := aigw.ErrorWriterFrom(r.Context()); ew != nil {
		return aiprovider.ErrorWriter(ew)
	}
	return WriteError
}

// stripCredentials removes the caller's Burrow credentials before a request
// leaves the relay.
func stripCredentials(r *http.Request) {
	r.Header.Del("Authorization")
	r.Header.Del("X-Api-Key")
	r.Header.Del("Cookie")
	r.Header.Del("Proxy-Authorization")
}

// authenticate checks the presented key. A service key is checked against the
// provider's backing service. A gateway key is checked against its allow-list
// for this provider; it has no service key id, so the returned id is "" and
// the gateway key travels in the request's route. It writes the error response
// itself and reports ok=false.
//
// A gateway key that may use every model of the provider (empty allow-list,
// or "<slug>/*") has the whole provider path. Any other gateway key gets two
// things: POST on an inference path of the provider's dialect with an allowed
// "model" in the body, and, when its list names this provider at all, GET of
// the model list, answered from the filtered catalog. Everything else is
// refused: a native API can name a model where Burrow does not look (the URL,
// "source", "from", a batch file), so a body with an allowed model proves
// nothing there. For the same reason such a key may not send a "model" query
// parameter next to the body.
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request, p db.AIProvider) (*http.Request, string, bool) {
	presented := presentedKey(r)
	if !strings.HasPrefix(presented, gatewayKeyPrefix) {
		keyID, ok := g.authenticateServiceKey(w, r, p, presented)
		return r, keyID, ok
	}
	key, ok := g.authenticateGatewayKey(w, r)
	if !ok {
		return r, "", false
	}
	deny := func() (*http.Request, string, bool) {
		g.fail(w, r, http.StatusForbidden, "model_not_allowed", msgModelNotAllowed)
		return r, "", false
	}
	full := store.ModelAllowed(key.AllowedModels, p.Slug+"/*")
	model := ""
	switch {
	case !full && isModelList(r) && namesProvider(key.AllowedModels, p.Slug):
		// serveModels answers it, filtered; it is never forwarded. A key
		// with no entry for this provider has no list here to be shown.
	case r.Method == http.MethodPost && (full || providerInference(p, r)):
		if !full && hasModelQuery(r) {
			return deny()
		}
		body, ok := g.readBodyOrFail(w, r)
		if !ok {
			return r, "", false
		}
		setBody(r, body.Raw()) // byte for byte what the client sent
		model = body.Model()
		if model == "" && body.HasModelKey() {
			// "Model", or a model that is not a string: an upstream may
			// still read it, and Burrow could neither check nor meter it.
			g.fail(w, r, http.StatusBadRequest, "model_required", `the request needs a "model"`)
			return r, "", false
		}
		if !full && !store.ModelAllowed(key.AllowedModels, p.Slug+"/"+model) {
			return deny()
		}
	case !full:
		return deny()
	}
	route := aigw.NewRoute(key.ID, p.APIFormat, model, w.Header().Get(headerRequestID))
	route.SetTarget(p.Slug, model)
	ctx := aigw.WithRoute(r.Context(), route)
	return r.WithContext(context.WithValue(ctx, allowListKey{}, key.AllowedModels)), "", true
}

// namesProvider reports whether an allow-list has an entry for a model of
// provider slug ("<slug>/<model>" or "<slug>/*").
func namesProvider(allowed []string, slug string) bool {
	for _, e := range allowed {
		if strings.HasPrefix(e, slug+"/") {
			return true
		}
	}
	return false
}

// hasModelQuery reports whether r's query string names a model, in any
// letter case, or cannot be read the way an upstream might read it. The
// allow-list is checked on the body's "model" alone.
func hasModelQuery(r *http.Request) bool {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return true
	}
	for k := range q {
		if strings.EqualFold(strings.TrimSpace(k), "model") {
			return true
		}
	}
	return false
}

// allowListKey carries a gateway key's allow-list to the model listing of a
// provider path.
type allowListKey struct{}

// isModelList reports whether r asks for the model list, written plainly.
func isModelList(r *http.Request) bool {
	path, plain := plainPath(r)
	return plain && r.Method == http.MethodGet && strings.TrimSuffix(path, "/") == "/v1/models"
}

// providerInference reports whether r's path, written plainly, is an
// inference path of the dialect p speaks.
func providerInference(p db.AIProvider, r *http.Request) bool {
	d, ok := DialectByName(p.APIFormat)
	if !ok {
		return false
	}
	path, plain := plainPath(r)
	return plain && d.inference(strings.TrimSuffix(path, "/"))
}

// plainPath returns r's path and whether it is written plainly: nothing
// percent-encoded that need not be, no empty segment, no "." or "..". Only a
// plain path is compared with the paths Burrow knows; an upstream may read
// any other spelling differently than Burrow does.
func plainPath(r *http.Request) (string, bool) {
	path := r.URL.Path
	if r.URL.RawPath != "" || !strings.HasPrefix(path, "/") {
		return path, false
	}
	segments := strings.Split(strings.TrimSuffix(path[1:], "/"), "/")
	for _, seg := range segments {
		if (seg == "" && len(segments) > 1) || seg == "." || seg == ".." {
			return path, false
		}
	}
	return path, true
}

// authenticateServiceKey checks the presented key against the provider's
// backing service. It writes the error response itself and reports ok=false.
func (g *Gateway) authenticateServiceKey(w http.ResponseWriter, r *http.Request, p db.AIProvider, presented string) (string, bool) {
	if presented == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		g.fail(w, r, http.StatusUnauthorized, "invalid_api_key", "missing API key")
		return "", false
	}
	keyID, ok, err := g.Keys.ValidateAPIKey(r.Context(), p.ServiceID, presented)
	if err != nil {
		g.Log.Warn("aigateway: api key validation failed", "provider", p.Slug, "err", err)
		g.fail(w, r, http.StatusInternalServerError, "internal_error", "internal error")
		return "", false
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		g.fail(w, r, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
		return "", false
	}
	return keyID, true
}

// serveModels answers GET /v1/models from the stored catalog. It reports
// false when the request is something else or the catalog is empty, in which
// case the request is forwarded like any other. Ids are stored data; the
// encoder escapes them.
//
// A gateway key that may not use every model of this provider is shown the
// catalog entries it may use and is never forwarded: the upstream's own list
// would name the rest.
func (g *Gateway) serveModels(w http.ResponseWriter, r *http.Request, p db.AIProvider) bool {
	if !isModelList(r) {
		return false
	}
	allowed, _ := r.Context().Value(allowListKey{}).([]string)
	restricted := !store.ModelAllowed(allowed, p.Slug+"/*")
	var models []db.AIProviderModel
	if g.Models != nil {
		var err error
		if models, err = g.Models.ListProviderModels(r.Context(), p.Slug); err != nil {
			g.Log.Warn("aigateway: model catalog not read", "provider", p.Slug, "err", err)
			models = nil
		}
	}
	if len(models) == 0 && !restricted {
		return false
	}
	items := make([]modelItem, 0, len(models))
	for _, m := range models {
		if !restricted || store.ModelAllowed(allowed, p.Slug+"/"+m.ModelID) {
			items = append(items, modelItem{ID: m.ModelID, OwnedBy: p.Slug, DisplayName: m.DisplayName})
		}
	}
	w.Header().Set(headerProvider, p.Slug)
	writeOpenAIModels(w, items)
	return true
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
		g.fail(w, r, http.StatusServiceUnavailable, "provider_unavailable", "provider kind is not supported by this relay")
		return nil, "", false
	}
}

func (g *Gateway) tunnelUpstream(w http.ResponseWriter, r *http.Request, p db.AIProvider) (http.Handler, string, bool) {
	res, err := g.Tunnels.LookupByServiceID(r.Context(), p.ServiceID)
	if errors.Is(err, proxy.ErrNotFound) {
		g.fail(w, r, http.StatusBadGateway, "provider_offline", "the client serving this provider is not connected")
		return nil, "", false
	}
	if err != nil {
		g.Log.Error("aigateway: tunnel lookup failed", "provider", p.Slug, "err", err)
		g.fail(w, r, http.StatusInternalServerError, "internal_error", "internal error")
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
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			g.Log.Warn("aigateway: tunnel upstream error", "provider", p.Slug, "err", err)
			if errors.Is(err, proxy.ErrNotFound) {
				g.fail(w, req, http.StatusBadGateway, "provider_offline", "the client serving this provider is not connected")
				return
			}
			g.fail(w, req, http.StatusBadGateway, "upstream_unavailable", "the provider did not answer")
		},
	}
	return rp, host, true
}

// policyAllows enforces the backing service's access mode and IP/geo policy,
// the same for every provider kind. It writes the error response itself.
func (g *Gateway) policyAllows(w http.ResponseWriter, r *http.Request, p db.AIProvider, res *proxy.Resolved) bool {
	if res.AccessMode != "api_key" {
		g.fail(w, r, http.StatusForbidden, "provider_unavailable", "this provider's service is not in API-key mode")
		return false
	}
	if g.IPGeoDeny != nil && g.IPGeoDeny(res, r) {
		g.Log.Info("aigateway: ip-geo deny", "provider", p.Slug, "service_id", res.ServiceID, "remote_addr", r.RemoteAddr)
		g.fail(w, r, http.StatusForbidden, "forbidden", "your address is not allowed to use this provider")
		return false
	}
	return true
}

// directAllowed is the policy step for a direct provider. It reads the
// backing service's row, never the tunnel registry.
func (g *Gateway) directAllowed(w http.ResponseWriter, r *http.Request, p db.AIProvider) bool {
	if g.Direct == nil || g.ServicePolicy == nil {
		g.fail(w, r, http.StatusServiceUnavailable, "provider_unavailable", "direct providers are not available on this relay")
		return false
	}
	res, err := g.ServicePolicy(r.Context(), p.ServiceID)
	if errors.Is(err, proxy.ErrNotFound) {
		// The backing service is gone or is not this provider's own row.
		g.Log.Error("aigateway: direct provider has no backing service of type direct", "provider", p.Slug, "service_id", p.ServiceID)
		g.fail(w, r, http.StatusServiceUnavailable, "provider_misconfigured", "this provider's configuration is invalid")
		return false
	}
	if err != nil || res == nil {
		g.Log.Error("aigateway: service policy lookup failed", "provider", p.Slug, "err", err)
		g.fail(w, r, http.StatusInternalServerError, "internal_error", "internal error")
		return false
	}
	return g.policyAllows(w, r, p, res)
}

func (g *Gateway) directUpstream(w http.ResponseWriter, r *http.Request, p db.AIProvider) (http.Handler, bool) {
	h, err := g.Direct(p, requestErrorWriter(r))
	switch {
	case errors.Is(err, aiprovider.ErrNotConfigured):
		g.Log.Warn("aigateway: credential slot is not set", "provider", p.Slug, "slot", p.CredentialSlot)
		g.fail(w, r, http.StatusServiceUnavailable, "provider_not_configured", "this provider has no upstream credential configured")
		return nil, false
	case err != nil || h == nil:
		// err names the provider and the faulty setting, never the
		// credential or the base URL.
		g.Log.Error("aigateway: provider configuration is invalid", "provider", p.Slug, "err", err)
		g.fail(w, r, http.StatusServiceUnavailable, "provider_misconfigured", "this provider's configuration is invalid")
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
func DirectUpstreams(v aiprovider.Vault, rt http.RoundTripper) func(db.AIProvider, aiprovider.ErrorWriter) (http.Handler, error) {
	return func(p db.AIProvider, ew aiprovider.ErrorWriter) (http.Handler, error) {
		return aiprovider.NewUpstream(aiprovider.Config{
			Slug:           p.Slug,
			BaseURL:        p.BaseURL,
			CredentialSlot: p.CredentialSlot,
			AuthHeader:     p.AuthHeader,
			AuthFormat:     p.AuthFormat,
			ExtraHeaders:   p.ExtraHeaders,
		}, v, rt, ew)
	}
}
