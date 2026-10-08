package aigateway

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/store"
)

const (
	defaultMaxBody   = 8 << 20
	gatewayKeyPrefix = "bgw_"
	headerRequestID  = "Burrow-Request-Id"
	headerProvider   = "Burrow-Provider"
	headerModel      = "Burrow-Model"

	// msgModelNotAllowed is the one text of every allow-list denial: it does
	// not say what was asked for or whether it exists.
	msgModelNotAllowed = "this key may not use this model"
)

// GatewayKeys validates a presented gateway key. It must not cache: a revoked
// key fails on the next request.
type GatewayKeys interface {
	ValidateGatewayKey(ctx context.Context, presented string) (store.GatewayKey, bool, error)
}

// Catalog is what a dialect's model list is built from.
type Catalog interface {
	ListModels(ctx context.Context) ([]db.AIModel, error)
	ListProviders(ctx context.Context) ([]db.AIProvider, error)
	ListProviderModels(ctx context.Context, slug string) ([]db.AIProviderModel, error)
}

// ServeDialect handles a dialect endpoint: one base URL for every model. The
// request's "model" names a synthetic model or "<provider>/<model>".
// r.URL.Path must start at "/v1".
//
// The key is read from "Authorization: Bearer" or, as Anthropic clients send
// it, "x-api-key". A bearer token, when one is sent, is the key and x-api-key
// is then not looked at; x-api-key is the key only when Authorization carries
// no bearer token (it is absent, uses another scheme, or is "Bearer" with
// nothing after it). Neither header reaches an upstream.
//
// Order of checks: gateway key, the key's allow-list on the name the client
// asked for, the daily budget of the key and of that name, model resolution,
// which targets offer the endpoint (or, for a model that opted in and has
// none, which can be reached through a translating pair), the first
// target's service policy (access mode, IP/geo), then the chain, and inside
// it, per attempt, that target's policy and only then its upstream
// credential (see failover). Nothing of the request but its key is looked at
// before the key is accepted.
//
// A synthetic model's targets are tried in order; a direct address has one
// target and so no fallback. Burrow-Provider, Burrow-Model and Burrow-Attempts
// describe the attempt that answered.
func (g *Gateway) ServeDialect(w http.ResponseWriter, r *http.Request, d *Dialect) {
	// Everything the gateway and the chain write from here on is in d's shape.
	// The URL prefix, not a header, says which format the request is in.
	r = r.WithContext(aigw.WithKind(aigw.WithErrorWriter(r.Context(), d.WriteError), aigw.Kind(d.Name)))

	key, ok := g.authenticateGatewayKey(w, r)
	if !ok {
		return
	}
	if isModelList(r) {
		g.serveDialectModels(w, r, d, key)
		return
	}
	// Only a plainly written path is an endpoint: "/v1//x", "/v1/./x" or an
	// encoded slash may mean something else to the upstream than to Burrow.
	path, plain := plainPath(r)
	path = strings.TrimSuffix(path, "/")
	if !plain || !d.inference(path) {
		g.fail(w, r, http.StatusNotFound, "endpoint_not_found", "this endpoint does not exist in the "+d.Name+" API of this gateway")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		g.fail(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}

	// A key held to some models names its model in the body and nowhere
	// else: an upstream may read a "model" parameter of the URL instead.
	if len(key.AllowedModels) > 0 && hasModelQuery(r) {
		g.fail(w, r, http.StatusForbidden, "model_not_allowed", msgModelNotAllowed)
		return
	}

	body, ok := g.readBodyOrFail(w, r)
	if !ok {
		return
	}
	requested := body.Model()
	if requested == "" {
		g.fail(w, r, http.StatusBadRequest, "model_required", `the request needs a "model"`)
		return
	}
	// Allow-list before lookup: a restricted key learns nothing about models
	// outside its list, not even whether they exist. The answer is the same
	// for every such name.
	if !store.ModelAllowed(key.AllowedModels, requested) {
		g.fail(w, r, http.StatusForbidden, "model_not_allowed", msgModelNotAllowed)
		return
	}
	// A key or a model over its daily budget reaches no upstream, whether or
	// not the call would be metered. After the allow-list, so a key learns
	// nothing about the budget of a model it may not use; before resolution,
	// so nothing is read for a request that will not be served.
	if g.overBudget(w, r, key.ID, requested) {
		return
	}
	res, ok := g.resolveOrFail(w, r, requested, d)
	if !ok {
		return
	}

	// Only targets that offer the requested endpoint are candidates; a
	// provider that does not offer it is not called and nothing of it is
	// read. A model that has none and has translation turned on gets
	// translated candidates instead (see candidatesForRequest); without the
	// flag the answers below are what they always were.
	candidates, why := g.candidatesForRequest(r.Context(), res, d, path)
	if len(candidates) == 0 {
		if t, ok := g.estimateTarget(res, d, path); ok {
			g.serveEstimate(w, r, d, key, requested, t, body)
			return
		}
		g.failNoCandidate(w, r, res, d, requested, why)
		return
	}
	// All of a request's candidates are translated, or none is.
	translated := candidates[0].pair != nil
	first := Target{Provider: candidates[0].provider, Model: candidates[0].model}

	requestID := w.Header().Get(headerRequestID)
	route := aigw.NewRoute(key.ID, d.Name, requested, requestID)
	// Until an attempt answers, the route names the first target: an answer
	// that comes from the chain itself (a cached one) is accounted to it.
	route.SetTarget(first.Provider.Slug, first.Model)
	r = r.WithContext(aigw.WithRoute(r.Context(), route))

	// The chain runs under the first target's service: its AI config, its
	// limits, its cache. That service's policy is therefore checked before
	// the chain, as it is for a single target; every target's own policy is
	// checked again when its turn comes, before its credential is read.
	host, checked, ok := g.firstTargetPolicy(w, r, first.Provider)
	if !ok {
		return
	}
	if !checked || translated {
		// In the context, not in a header: the request's headers go on to
		// the upstream. A translated answer is never cached: the key is the
		// first target's service and model, which a request in that target's
		// own format shares, and one of the two would be served the other's
		// format.
		r = r.WithContext(aigw.WithoutCache(r.Context()))
	}
	// Each attempt applies the upstream credential of its own target (see
	// (*failover).attempt). The chain, which runs under the first target's
	// service, must not put that service's credential on a request that may
	// go to another target.
	r = r.WithContext(aigw.WithOwnCredential(r.Context()))

	// The forwarded body differs from the client's in the bytes of the
	// "model" value only. It has exactly one top-level "model" key in any
	// letter case, so the model the chain reads from it is the first
	// target's. The failover handler sets each attempt's own model.
	setBody(r, body.WithModel(first.Model))
	// The gateway key and the dashboard's cookies stop here.
	stripCredentials(r)
	fo := g.newFailover(res, candidates, route, requestID)
	if g.Chain == nil {
		fo.ServeHTTP(w, r)
		return
	}
	if !d.metered(path) {
		// No usage row, and nothing else is left out: limits, redaction and
		// guardrails apply to a prompt that is only counted, too.
		r = r.WithContext(aigw.WithoutUsage(r.Context()))
	}
	// A reported cost is believed only when every candidate is an upstream
	// the relay calls itself: the chain cannot tell which one answered. It
	// is not believed for a translated answer: the chain reads the caller's
	// side of it, where no upstream's figure stands.
	g.Chain.DispatchMetered(w, r, first.Provider.ServiceID, host, "Authorization", "", allDirect(candidates) && !translated, fo)
}

// failNoCandidate answers a request whose model has nothing to try on this
// endpoint. A synthetic name does not tell what stands behind it, and a key
// may be allowed that name alone: a provider is named only to a client that
// named it itself.
func (g *Gateway) failNoCandidate(w http.ResponseWriter, r *http.Request, res Resolution, d *Dialect, requested, why string) {
	switch why {
	case whyEndpointUnsupported:
		msg := "model " + shownName(requested) + " is not available on the Responses API; use /v1/chat/completions"
		if !res.Synthetic {
			msg = "provider " + res.Targets[0].Provider.Slug + " does not offer the Responses API; use /v1/chat/completions for model " + shownName(requested)
		}
		g.fail(w, r, http.StatusBadRequest, "endpoint_unsupported", msg)
	case whyFormatMismatch:
		g.failFormatMismatch(w, r, requested, d, res.Other[0].Provider.APIFormat)
	default:
		g.fail(w, r, http.StatusNotFound, "model_not_found", "unknown model "+shownName(requested))
	}
}

func (g *Gateway) failFormatMismatch(w http.ResponseWriter, r *http.Request, requested string, d *Dialect, servedBy string) {
	g.fail(w, r, http.StatusBadRequest, "format_mismatch",
		"model "+shownName(requested)+" is not served in the "+d.Name+" format; use "+g.dialectBaseURL(servedBy))
}

// firstTargetPolicy checks the policy (access mode, IP/geo) of the service a
// fallback chain runs under: one lookup and the verdict, no upstream handler
// and no credential. A refusal is written to the client and ends the request:
// ok is false. When the policy cannot be read (the tunnel is offline, a
// lookup failed) that is not a refusal, and the failover handler meets it
// again as a failed attempt and moves on; but the caller was then not checked
// against this service's policy (checked is false), so the request must not
// be answered from its cache.
func (g *Gateway) firstTargetPolicy(w http.ResponseWriter, r *http.Request, p db.AIProvider) (host string, checked, ok bool) {
	var res *proxy.Resolved
	switch p.Kind {
	case "tunnel":
		if found, err := g.Tunnels.LookupByServiceID(r.Context(), p.ServiceID); err == nil && found != nil {
			res, host = found, found.LocalHost
		}
	case "direct":
		host = upstreamHost(p.BaseURL)
		if g.Direct != nil && g.ServicePolicy != nil {
			if found, err := g.ServicePolicy(r.Context(), p.ServiceID); err == nil {
				res = found
			}
		}
	}
	if res == nil {
		return host, false, true
	}
	return host, true, g.policyAllows(w, r, p, res)
}

func (g *Gateway) maxBody() int64 {
	if g.MaxBody > 0 {
		return g.MaxBody
	}
	return defaultMaxBody
}

// readBodyOrFail buffers the request body once, within the size limit, and
// writes the error itself. A body that is refused is never forwarded.
func (g *Gateway) readBodyOrFail(w http.ResponseWriter, r *http.Request) (*requestBody, bool) {
	body, err := readRequestBody(r, g.maxBody())
	switch {
	case err == nil:
		return body, true
	case errors.Is(err, errBodyTooLarge):
		g.fail(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
	case errors.Is(err, errDuplicateModel):
		// Which of the fields an upstream reads is not defined.
		g.fail(w, r, http.StatusBadRequest, "invalid_request", `the request has more than one "model" field`)
	default:
		g.fail(w, r, http.StatusBadRequest, "invalid_request", "could not read the request body")
	}
	return nil, false
}

// resolveOrFail resolves the requested model for d and writes the error
// itself. A model with translation turned on may come back without a target
// of d (Resolution.Other): what a request can do with it depends on its
// endpoint, see candidatesForRequest.
func (g *Gateway) resolveOrFail(w http.ResponseWriter, r *http.Request, requested string, d *Dialect) (Resolution, bool) {
	res, err := g.resolve(r.Context(), requested, d.Name)
	var mismatch *formatMismatchError
	switch {
	case err == nil && len(res.Targets)+len(res.Other) > 0:
		return res, true
	case err == nil || errors.Is(err, errModelNotFound):
		g.fail(w, r, http.StatusNotFound, "model_not_found", "unknown model "+shownName(requested))
	case errors.As(err, &mismatch):
		g.failFormatMismatch(w, r, requested, d, mismatch.ServedBy)
	default:
		g.Log.Error("aigateway: model resolution failed", "model", shownName(requested), "err", err)
		g.fail(w, r, http.StatusInternalServerError, "internal_error", "internal error")
	}
	return Resolution{}, false
}

// shownName is a requested model name as it is echoed in an error or a log
// line: a name longer than any that can exist is cut.
func shownName(name string) string {
	if len(name) > maxDirectAddressLen {
		return strings.ToValidUTF8(name[:maxDirectAddressLen], "") + "…"
	}
	return name
}

// dialectBaseURL is the base URL a client is given for a dialect.
func (g *Gateway) dialectBaseURL(dialect string) string {
	if dialect == "anthropic" {
		return "https://" + g.PublicHost + "/anthropic"
	}
	return "https://" + g.PublicHost + "/openai/v1"
}

// targetUpstream applies the target's policy and builds its upstream handler.
// The policy (access mode, IP/geo of the backing service) is checked first;
// a direct provider's credential is read only after it passed.
func (g *Gateway) targetUpstream(w http.ResponseWriter, r *http.Request, p db.AIProvider) (http.Handler, string, bool) {
	upstream, host, ok := g.upstreamFor(w, r, p)
	if !ok {
		return nil, "", false
	}
	if p.Kind == "direct" {
		if upstream, ok = g.directUpstream(w, r, p); !ok {
			return nil, "", false
		}
	}
	return upstream, host, true
}

// authenticateGatewayKey accepts only gateway keys. It writes the error
// itself. A missing, malformed, unknown or revoked key gets one and the same
// answer, and the key is validated on every request: nothing is cached here.
// The presented key is never logged or echoed.
func (g *Gateway) authenticateGatewayKey(w http.ResponseWriter, r *http.Request) (store.GatewayKey, bool) {
	deny := func() (store.GatewayKey, bool) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		g.fail(w, r, http.StatusUnauthorized, "invalid_api_key", "a valid gateway key is required")
		return store.GatewayKey{}, false
	}
	presented := presentedKey(r)
	if g.GatewayKeys == nil || !strings.HasPrefix(presented, gatewayKeyPrefix) {
		return deny()
	}
	key, ok, err := g.GatewayKeys.ValidateGatewayKey(r.Context(), presented)
	if err != nil {
		g.Log.Warn("aigateway: gateway key validation failed", "err", err)
		g.fail(w, r, http.StatusInternalServerError, "internal_error", "internal error")
		return store.GatewayKey{}, false
	}
	if !ok {
		return deny()
	}
	return key, true
}

// serveDialectModels lists the models the key may use in dialect d: enabled
// synthetic models that have a usable target for d or are served in d by
// translation (the flag is on, a pair is released and a target of the other
// dialect is usable: what resolution accepts), then every catalogued model
// of the providers that speak d, as "<provider>/<model>".
func (g *Gateway) serveDialectModels(w http.ResponseWriter, r *http.Request, d *Dialect, key store.GatewayKey) {
	items := []modelItem{}
	if g.Catalog == nil {
		d.writeModels(w, items)
		return
	}
	ctx := r.Context()
	providers, err := g.Catalog.ListProviders(ctx)
	if err != nil {
		g.Log.Error("aigateway: list providers failed", "err", err)
		g.fail(w, r, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	speaks := make(map[string]bool, len(providers))    // providers of this dialect
	formats := make(map[string]string, len(providers)) // every provider's format
	for _, p := range providers {
		speaks[p.Slug] = p.APIFormat == d.Name
		formats[p.Slug] = p.APIFormat
	}
	models, err := g.Catalog.ListModels(ctx)
	if err != nil {
		g.Log.Error("aigateway: list models failed", "err", err)
		g.fail(w, r, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	for _, m := range models {
		if m.Enabled && (servedIn(m, d.Name, speaks) || translatedIn(m, d, formats)) && store.ModelAllowed(key.AllowedModels, m.Name) {
			items = append(items, modelItem{ID: m.Name, OwnedBy: "burrow", DisplayName: m.Name})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	var direct []modelItem
	for _, p := range providers {
		if !speaks[p.Slug] {
			continue
		}
		pm, err := g.Catalog.ListProviderModels(ctx, p.Slug)
		if err != nil {
			// One provider's catalog failing must not hide the others.
			g.Log.Warn("aigateway: model catalog not read", "provider", p.Slug, "err", err)
			continue
		}
		for _, m := range pm {
			id := p.Slug + "/" + m.ModelID
			if store.ModelAllowed(key.AllowedModels, id) {
				direct = append(direct, modelItem{ID: id, OwnedBy: p.Slug, DisplayName: m.DisplayName})
			}
		}
	}
	sort.Slice(direct, func(i, j int) bool { return direct[i].ID < direct[j].ID })
	d.writeModels(w, append(items, direct...))
}

// servedIn reports whether m has at least one target for dialect whose
// provider speaks it (the same rule resolution applies to a request).
func servedIn(m db.AIModel, dialect string, speaks map[string]bool) bool {
	for _, t := range m.Targets {
		if t.Dialect == dialect && speaks[t.ProviderSlug] {
			return true
		}
	}
	return false
}

// pathResponses is the one Responses API endpoint the gateway serves:
// creating a response. Burrow keeps no response state, and a request for a
// stored response names no model to route or to check a key's list by.
const pathResponses = "/v1/responses"

// endpointSupported reports whether provider p offers the inference endpoint
// at path. Every provider of a dialect offers that dialect's endpoints, except
// the Responses API, which an operator states per provider.
func endpointSupported(path string, p db.AIProvider) bool {
	return strings.TrimRight(path, "/") != pathResponses || p.SupportsResponses
}
