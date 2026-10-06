package aigateway

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/db"
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
// asked for, model resolution, the target's service policy (access mode,
// IP/geo), and only then the upstream credential. Nothing of the request but
// its key is looked at before the key is accepted.
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
	res, ok := g.resolveOrFail(w, r, requested, d)
	if !ok {
		return
	}

	target := res.Targets[0]
	upstream, host, ok := g.targetUpstream(w, r, target.Provider)
	if !ok {
		return
	}
	// The forwarded body differs from the client's in the bytes of the
	// "model" value only. It has exactly one top-level "model" key in any
	// letter case, so the model the chain reads from it is the target's.
	setBody(r, body.WithModel(target.Model))

	route := aigw.NewRoute(key.ID, d.Name, requested, w.Header().Get(headerRequestID))
	route.SetTarget(target.Provider.Slug, target.Model)
	r = r.WithContext(aigw.WithRoute(r.Context(), route))
	w.Header().Set(headerProvider, target.Provider.Slug)
	w.Header().Set(headerModel, target.Model)

	// The gateway key and the dashboard's cookies stop here.
	stripCredentials(r)
	if g.Chain == nil {
		upstream.ServeHTTP(w, r)
		return
	}
	if !d.metered(path) {
		// No usage row, and nothing else is left out: limits, redaction and
		// guardrails apply to a prompt that is only counted, too.
		r = r.WithContext(aigw.WithoutUsage(r.Context()))
	}
	// Reported cost is believed only from an upstream the relay calls itself.
	g.Chain.DispatchMetered(w, r, target.Provider.ServiceID, host, "Authorization", "", target.Provider.Kind == "direct", upstream)
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

// resolveOrFail resolves the requested model for d and writes the error itself.
func (g *Gateway) resolveOrFail(w http.ResponseWriter, r *http.Request, requested string, d *Dialect) (Resolution, bool) {
	res, err := g.resolve(r.Context(), requested, d.Name)
	var mismatch *formatMismatchError
	switch {
	case err == nil && len(res.Targets) > 0:
		return res, true
	case err == nil || errors.Is(err, errModelNotFound):
		g.fail(w, r, http.StatusNotFound, "model_not_found", "unknown model "+shownName(requested))
	case errors.As(err, &mismatch):
		g.fail(w, r, http.StatusBadRequest, "format_mismatch",
			"model "+shownName(requested)+" is not served in the "+d.Name+" format; use "+g.dialectBaseURL(mismatch.ServedBy))
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
// synthetic models that have a usable target for d, then every catalogued
// model of the providers that speak d, as "<provider>/<model>".
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
	speaks := make(map[string]bool, len(providers)) // providers of this dialect
	for _, p := range providers {
		speaks[p.Slug] = p.APIFormat == d.Name
	}
	models, err := g.Catalog.ListModels(ctx)
	if err != nil {
		g.Log.Error("aigateway: list models failed", "err", err)
		g.fail(w, r, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	for _, m := range models {
		if m.Enabled && servedIn(m, d.Name, speaks) && store.ModelAllowed(key.AllowedModels, m.Name) {
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
