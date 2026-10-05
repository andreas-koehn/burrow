package api

// ai_provider_handlers.go — /ai/providers: list, read, metrics, create,
// update, delete, upstream settings and the model catalog.
//
// A provider is a row of the ai_providers table: a slug under which a model
// backend is served at https://<auth_domain>/ai/<slug>/v1.  The read handlers
// join that row with live data of its backing service (model alias, key
// count, tunnel status, trailing-24h request counts).  latency_p95_ms is
// always 0: usage_events has no latency column.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// aiProviderResp is the JSON wire shape for one provider, in the list and in
// the single-provider responses.
type aiProviderResp struct {
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	APIFormat       string `json:"api_format"`
	ServiceID       string `json:"service_id"`
	BaseURL         string `json:"base_url"`
	ModelAlias      string `json:"model_alias"`
	ConcreteModel   string `json:"concrete_model"`
	BackendType     string `json:"backend_type"`
	APIKeyCount     int    `json:"api_key_count"`
	Requests24h     int    `json:"requests_24h"`
	CacheHits24h    int    `json:"cache_hits_24h"`
	LatencyP95ms    int    `json:"latency_p95_ms"`
	Status          string `json:"status"`
	ClientSessionID string `json:"client_session_id"`

	// Direct providers. The credential itself is never part of a response:
	// credential_slot is the name of a vault slot and credential_present
	// tells whether that slot holds a value.
	UpstreamBaseURL   string `json:"upstream_base_url"`
	CredentialSlot    string `json:"credential_slot"`
	CredentialPresent bool   `json:"credential_present"`
	Billing           string `json:"billing"`
	ModelCount        int    `json:"model_count"`

	// How the relay presents the credential upstream, for callers who may
	// change it (admins): PUT …/upstream keeps what a body leaves out, and
	// these fields show what that is. auth_format holds the literal {key}
	// placeholder, never the credential. Of the extra headers only the names
	// are shown, sorted: a value is free text and may be a secret. Left out
	// for other callers and for tunnel providers.
	AuthHeader       string   `json:"auth_header,omitempty"`
	AuthFormat       string   `json:"auth_format,omitempty"`
	ExtraHeaderNames []string `json:"extra_header_names,omitzero"`
}

// upstreamAuthView fills the fields of v that only a caller who may configure
// the provider is shown.
func upstreamAuthView(v *aiProviderResp, p db.AIProvider) {
	if p.Kind != "direct" {
		return
	}
	v.AuthHeader, v.AuthFormat = p.AuthHeader, p.AuthFormat
	v.ExtraHeaderNames = make([]string, 0, len(p.ExtraHeaders))
	for name := range p.ExtraHeaders {
		v.ExtraHeaderNames = append(v.ExtraHeaderNames, name)
	}
	slices.Sort(v.ExtraHeaderNames)
}

// credentialPresent reports whether the vault holds a non-empty value for
// slot. The value is looked at only to tell "set" from "empty".
func (d Deps) credentialPresent(slot string) bool {
	if d.CredentialVault == nil || slot == "" {
		return false
	}
	v, ok := d.CredentialVault.Get(slot)
	return ok && v != ""
}

// modelCount is the size of the provider's stored model list; 0 on error.
func (d Deps) modelCount(ctx context.Context, slug string) int {
	models, err := d.AIProviders.ListProviderModels(ctx, slug)
	if err != nil {
		return 0
	}
	return len(models)
}

// endpointMetricsResp is the JSON wire shape for the per-provider metrics
// endpoint.  Mirrors the TypeScript EndpointMetrics interface in
// AiEndpointDetail.tsx.
type endpointMetricsResp struct {
	Requests24h       int     `json:"requests_24h"`
	TokensIn24h       int     `json:"tokens_in_24h"`
	TokensOut24h      int     `json:"tokens_out_24h"`
	CostUSD24h        float64 `json:"cost_usd_24h"`
	CacheHitRatio24h  float64 `json:"cache_hit_ratio_24h"`
	RequestsPerMinute []int   `json:"requests_per_minute"`
}

// providerToBackendType converts a model-alias provider string to the
// backend_type enum the UI expects.
//
//	"openai"       → "openai-compat"
//	"openai-compat"→ "openai-compat"
//	"ollama"       → "ollama"
//	"vllm"         → "vllm"
//	anything else  → "other"
func providerToBackendType(provider string) string {
	switch provider {
	case "openai", "openai-compat":
		return "openai-compat"
	case "ollama":
		return "ollama"
	case "vllm":
		return "vllm"
	default:
		return "other"
	}
}

// composeProviderURL returns "https://<authDomain>/ai/<slug>/v1", or "" when
// the relay has no auth domain.
func composeProviderURL(slug, authDomain string) string {
	if slug == "" || authDomain == "" {
		return ""
	}
	return "https://" + authDomain + "/ai/" + slug + "/v1"
}

// providerViews returns the response item for every provider whose backing
// service the caller may see (same visibility as ListServices), ordered by
// slug.  Identity fields come from the provider row; model_alias,
// concrete_model, backend_type, api_key_count, status and the 24h counts are
// read live from the backing service.
func (d Deps) providerViews(r *http.Request) ([]aiProviderResp, error) {
	role, err := d.callerRole(r)
	if err != nil {
		return nil, err
	}
	uid := userID(r.Context())

	svcs, err := d.Services.ListServices(r.Context(), uid, role)
	if err != nil {
		return nil, err
	}
	visible := make(map[string]bool, len(svcs))
	for _, sv := range svcs {
		visible[sv.ID] = true
	}

	providers, err := d.AIProviders.ListProviders(r.Context())
	if err != nil {
		return nil, err
	}

	// Build a service_id → first alias mapping from the model alias registry.
	// We pick the alias with the highest priority (lowest priority number)
	// for each service.  If ModelAliases is nil or the list fails, we proceed
	// with an empty map — every provider will show empty alias fields.
	aliasForService := map[string]modelAliasResp{}
	if d.ModelAliases != nil {
		if aliases, err := d.ModelAliases.ListModelAliases(r.Context()); err == nil {
			for _, a := range aliases {
				if a.ServiceID == "" {
					continue
				}
				existing, seen := aliasForService[a.ServiceID]
				// Lower priority number = higher priority.  Pick the first
				// (lowest Priority) alias seen for each service.
				if !seen || a.Priority < existing.Priority {
					aliasForService[a.ServiceID] = toModelAliasResp(a)
				}
			}
		}
	}

	// Trailing-24h request + cache-hit counts per service. Degrades to zeros
	// when the metrics store isn't wired (early-wiring / handler tests) or
	// the query fails (logged).
	counts := map[string]db.AIEndpointCount{}
	if d.AIMetrics != nil {
		if c, err := d.AIMetrics.AIEndpointCounts24h(r.Context()); err != nil {
			d.warn("ai provider counts query failed", "err", err)
		} else {
			counts = c
		}
	}

	out := make([]aiProviderResp, 0, len(providers))
	for _, p := range providers {
		if !visible[p.ServiceID] {
			continue
		}

		// Count API keys for this service via ListAPIKeys.  On error, default
		// to 0 (non-fatal: the provider still renders).
		keyCount := 0
		if keys, err := d.Services.ListAPIKeys(r.Context(), uid, role, p.ServiceID); err == nil {
			keyCount = len(keys)
		}

		// Live tunnel snapshot: determines status.  LiveTunnels may be nil in
		// some wiring configurations; composeLive handles that gracefully
		// (returns zero-value snapshot).  client_session_id stays empty:
		// LiveTunnelSnapshot has no SessionID field today and the dashboard
		// handles "" gracefully.
		// A direct provider has no tunnel: it is usable when its credential
		// slot is set.
		status := "Offline"
		present := false
		if p.Kind == "direct" {
			if present = d.credentialPresent(p.CredentialSlot); present {
				status = "Connected"
			}
		} else if d.composeLive(p.ServiceID).Connected {
			status = "Connected"
		}

		alias := aliasForService[p.ServiceID]

		v := aiProviderResp{
			Slug:            p.Slug,
			Name:            p.Name,
			Kind:            p.Kind,
			APIFormat:       p.APIFormat,
			ServiceID:       p.ServiceID,
			BaseURL:         composeProviderURL(p.Slug, d.AuthDomain),
			ModelAlias:      alias.Alias,
			ConcreteModel:   alias.ConcreteModel,
			BackendType:     providerToBackendType(alias.Provider),
			APIKeyCount:     keyCount,
			Requests24h:     counts[p.ServiceID].Requests,
			CacheHits24h:    counts[p.ServiceID].CacheHits,
			LatencyP95ms:    0, // usage_events has no latency column — not derivable
			Status:          status,
			ClientSessionID: "",

			UpstreamBaseURL:   p.BaseURL,
			CredentialSlot:    p.CredentialSlot,
			CredentialPresent: present,
			Billing:           p.Billing,
			ModelCount:        d.modelCount(r.Context(), p.Slug),
		}
		// The upstream routes are admin only.
		if role == "admin" {
			upstreamAuthView(&v, p)
		}
		out = append(out, v)
	}
	return out, nil
}

// providerView returns the response item for one slug.  ok is false when no
// provider has that slug or the caller may not see its service.
func (d Deps) providerView(r *http.Request, slug string) (v aiProviderResp, ok bool, err error) {
	views, err := d.providerViews(r)
	if err != nil {
		return aiProviderResp{}, false, err
	}
	for _, v := range views {
		if v.Slug == slug {
			return v, true, nil
		}
	}
	return aiProviderResp{}, false, nil
}

// writeProviderView answers a successful write with the same body GET returns
// for that provider.
func (d Deps) writeProviderView(w http.ResponseWriter, r *http.Request, status int, p db.AIProvider) {
	v, ok, err := d.providerView(r, p.Slug)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		// The write went through but the live join has no row for it (the
		// backing service vanished in between): answer with the stored fields.
		v = aiProviderResp{
			Slug: p.Slug, Name: p.Name, Kind: p.Kind, APIFormat: p.APIFormat, ServiceID: p.ServiceID,
			BaseURL:     composeProviderURL(p.Slug, d.AuthDomain),
			BackendType: providerToBackendType(""), Status: "Offline",
			UpstreamBaseURL: p.BaseURL, CredentialSlot: p.CredentialSlot, Billing: p.Billing,
		}
		// Only the admin-only write handlers answer through here.
		upstreamAuthView(&v, p)
	}
	writeJSON(w, status, v)
}

// GetAIProviders handles GET /api/v1/ai/providers.
func (d Deps) GetAIProviders(w http.ResponseWriter, r *http.Request) {
	views, err := d.providerViews(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// GetAIProvider handles GET /api/v1/ai/providers/{slug}.  A provider the
// caller may not see answers 404, like one that does not exist.
func (d Deps) GetAIProvider(w http.ResponseWriter, r *http.Request) {
	v, ok, err := d.providerView(r, chi.URLParam(r, "slug"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// GetAIProviderMetrics handles GET /api/v1/ai/providers/{slug}/metrics.
// The aggregates are read from usage_events of the provider's backing
// service.  A provider the caller may not see answers 404, like one that does
// not exist.
func (d Deps) GetAIProviderMetrics(w http.ResponseWriter, r *http.Request) {
	p, err := d.AIProviders.ProviderBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "provider not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	serviceID := p.ServiceID

	// Validate the backing service is accessible to the caller.
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	_, err = d.Services.GetService(r.Context(), uid, role, serviceID)
	if err != nil {
		// Forbidden is reported as not found: a 403 would tell the caller
		// that the slug exists.
		if errors.Is(err, store.ErrForbidden) || errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "provider not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	// 60 buckets — one request count per minute over the trailing hour.
	rpm := make([]int, 60)
	resp := endpointMetricsResp{RequestsPerMinute: rpm}

	// Aggregate from usage_events when the metrics store is wired. Degrades to
	// zeros on any read error (logged; metrics are non-critical to the page
	// rendering).
	if d.AIMetrics != nil {
		if agg, err := d.AIMetrics.AIEndpointMetrics24h(r.Context(), serviceID); err != nil {
			d.warn("ai provider metrics query failed", "provider", p.Slug, "err", err)
		} else {
			resp.Requests24h = agg.Requests
			resp.TokensIn24h = int(agg.TokensIn)
			resp.TokensOut24h = int(agg.TokensOut)
			if agg.Requests > 0 {
				resp.CacheHitRatio24h = float64(agg.CacheHits) / float64(agg.Requests)
			}
			copy(rpm, agg.PerMinute[:])
			// Cost per kind, as the cost engine computes it: what upstreams
			// reported, plus the pricing table for the tokens of the rows
			// without a reported cost.
			var usd float64
			for _, k := range agg.ByKind {
				usd += k.ReportedUSD
				if d.CostEngine != nil {
					usd += d.CostEngine.UsdFor(k.Kind, int(k.PricedTokensIn), int(k.PricedTokensOut))
				}
			}
			resp.CostUSD24h = usd
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// warn logs at Warn level; Deps.Log may be nil in partial wirings.
func (d Deps) warn(msg string, args ...any) {
	if d.Log != nil {
		d.Log.Warn(msg, args...)
	}
}

// upstreamReq carries the upstream settings of a direct provider. There is
// no field for the credential: credential_slot names a vault slot, and the
// value of that slot is configured on the relay, not through the API.
type upstreamReq struct {
	APIFormat      string            `json:"api_format"`
	BaseURL        string            `json:"base_url"`
	CredentialSlot string            `json:"credential_slot"`
	AuthHeader     string            `json:"auth_header"`
	AuthFormat     string            `json:"auth_format"`
	ExtraHeaders   map[string]string `json:"extra_headers"`
	Billing        string            `json:"billing"`
}

func (u upstreamReq) input() store.DirectProviderInput {
	return store.DirectProviderInput{
		APIFormat: u.APIFormat, BaseURL: u.BaseURL, CredentialSlot: u.CredentialSlot,
		AuthHeader: u.AuthHeader, AuthFormat: u.AuthFormat, ExtraHeaders: u.ExtraHeaders, Billing: u.Billing,
	}
}

// plainFieldRe is what a JSON field name must look like to be repeated in an
// error message.
var plainFieldRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// credentialHint is appended to an unknown-field 400 of a direct provider.
const credentialHint = "; the credential is set on the relay, credential_slot names its slot"

// decodeStrictJSON decodes body into v and refuses a field v does not have.
// A key pasted into "api_key" or "credential" would otherwise be dropped
// without a word and the provider saved without it. The known fields of v are
// filled even when an unknown one is refused. The returned message is the 400
// to answer with ("" on success); it names the field, and the field's value
// is never repeated, logged or audited. unknownField tells the caller whether
// a hint about the field fits.
func decodeStrictJSON(body io.Reader, v any) (msg string, unknownField bool) {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		return "", false
	}
	if quoted, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		if name, uerr := strconv.Unquote(quoted); uerr == nil && plainFieldRe.MatchString(name) {
			return "unknown field " + strconv.Quote(name), true
		}
		return "unknown field in the request body", true
	}
	return "invalid JSON body", false
}

// set reports whether the body carried any upstream setting.
func (u upstreamReq) set() bool {
	return u.APIFormat != "" || u.BaseURL != "" || u.CredentialSlot != "" || u.AuthHeader != "" ||
		u.AuthFormat != "" || u.ExtraHeaders != nil || u.Billing != ""
}

// postProviderReq is the body of POST /api/v1/ai/providers. service_id is
// for kind "tunnel", the upstream settings for kind "direct".
type postProviderReq struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	ServiceID string `json:"service_id"`
	upstreamReq
}

// putProviderReq is the body of PUT /api/v1/ai/providers/{slug}.
type putProviderReq struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// providerSlugRule is the 400 message for a malformed or reserved slug.
const providerSlugRule = auth.SlugRule + `; "v1" is reserved`

// mapProviderErr writes the HTTP error for a provider store error and reports
// whether it handled it.
func mapProviderErr(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrInvalidProviderSlug):
		writeErr(w, http.StatusBadRequest, providerSlugRule)
	case errors.Is(err, store.ErrProviderNotFound):
		writeErr(w, http.StatusNotFound, "provider not found")
	case errors.Is(err, store.ErrProviderExists):
		writeErr(w, http.StatusConflict, "provider slug or service already in use")
	case errors.Is(err, store.ErrProviderBusy):
		writeErr(w, http.StatusConflict, "the provider was changed by someone else at the same time; try again")
	case errors.Is(err, store.ErrProviderService):
		writeErr(w, http.StatusConflict, "a tunnel provider needs an http service in API-key mode")
	case errors.Is(err, store.ErrInvalidProviderConfig):
		// The reason names the field; it never repeats a header value or the URL.
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), store.ErrInvalidProviderConfig.Error()+": "))
	default:
		return false
	}
	return true
}

// validProviderName trims name and writes the 400 when it is empty or too long.
func validProviderName(w http.ResponseWriter, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return "", false
	}
	if len(name) > 120 {
		writeErr(w, http.StatusBadRequest, "name must be at most 120 chars")
		return "", false
	}
	return name, true
}

// auditProvider appends one provider audit event (best-effort).
func (d Deps) auditProvider(r *http.Request, action string, p db.AIProvider, payload map[string]any) {
	if d.AuditAppender == nil {
		return
	}
	lc := audit.LogContextFrom(r.Context())
	_ = d.AuditAppender.Append(r.Context(), audit.Event{
		ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
		Action:    action,
		SubjectID: p.Slug, SubjectLabel: p.Name,
		Result:   "ok",
		SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
		Payload: audit.MustJSON(payload),
	})
}

// upstreamHostCheckTimeout bounds the DNS lookup done when a direct provider
// is saved. A variable so tests can shorten it.
var upstreamHostCheckTimeout = 5 * time.Second

const (
	msgBaseURLInvalid = "base URL must be an https URL without credentials, query or fragment"
	msgBaseURLPrivate = "base URL resolves to a private or loopback address"
	msgBaseURLNoDNS   = "base URL host could not be resolved"
)

// upstreamHostAllowed vets the host of a direct provider's base URL when the
// provider is saved, and writes the 400 itself. It gives the admin an early,
// readable answer; the dial guard of the upstream transport is what enforces
// the rule on every connection. With private upstreams allowed the check is
// skipped, like the guard.
func (d Deps) upstreamHostAllowed(w http.ResponseWriter, r *http.Request, baseURL string) bool {
	if d.AllowPrivateUpstreams || d.HostCheck == nil {
		return true
	}
	u, err := aiprovider.ValidateBaseURL(baseURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, msgBaseURLInvalid)
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), upstreamHostCheckTimeout)
	defer cancel()
	// The check runs in its own goroutine so that a resolver that ignores
	// the deadline cannot hold the request.
	done := make(chan error, 1)
	go func() { done <- d.HostCheck(ctx, u.Hostname()) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	switch {
	case err == nil:
		return true
	case errors.Is(err, aiprovider.ErrBlockedAddress):
		writeErr(w, http.StatusBadRequest, msgBaseURLPrivate)
	default:
		writeErr(w, http.StatusBadRequest, msgBaseURLNoDNS)
	}
	return false
}

// upstreamAudit is the audit payload of a direct provider's settings: names
// only, never a header value.
func upstreamAudit(p db.AIProvider) map[string]any {
	return map[string]any{
		"kind":            p.Kind,
		"base_url":        p.BaseURL,
		"credential_slot": p.CredentialSlot,
		"billing":         p.Billing,
	}
}

// PostAIProvider handles POST /api/v1/ai/providers (admin only).  Kind
// "tunnel" registers an existing http service in API-key mode as a provider;
// kind "direct" adds a provider the relay calls itself.  An empty slug is
// derived from the name.
func (d Deps) PostAIProvider(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	// The body is read strictly for both kinds: without a kind a body is a
	// tunnel create, and a key next to a base_url must not vanish in a 201.
	var in postProviderReq
	if msg, unknown := decodeStrictJSON(r.Body, &in); msg != "" {
		if unknown && in.Kind == "direct" {
			msg += credentialHint
		}
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	name, ok := validProviderName(w, in.Name)
	if !ok {
		return
	}
	if in.Kind == "" {
		in.Kind = "tunnel"
	}
	if in.Kind != "tunnel" && in.Kind != "direct" {
		writeErr(w, http.StatusBadRequest, "kind must be 'tunnel' or 'direct'")
		return
	}
	slug := in.Slug
	if slug == "" {
		slug = store.ProviderSlugFromName(name)
	}
	// The store checks it too; checking here keeps a malformed or reserved
	// slug out of every store implementation.
	if !store.ValidProviderSlug(slug) {
		writeErr(w, http.StatusBadRequest, providerSlugRule)
		return
	}

	if in.Kind == "direct" {
		d.postDirectProvider(w, r, slug, name, in.upstreamReq)
		return
	}
	// Upstream settings on a tunnel provider would be dropped; say so instead
	// of answering 201.
	if in.upstreamReq.set() {
		writeErr(w, http.StatusBadRequest, "api_format, base_url, credential_slot, auth_header, auth_format, extra_headers and billing are for kind 'direct'")
		return
	}

	p, err := d.AIProviders.CreateTunnelProvider(r.Context(), slug, name, in.ServiceID)
	if err != nil {
		if !mapProviderErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	d.auditProvider(r, audit.ActionAIProviderCreate, p, map[string]any{
		"kind":       p.Kind,
		"service_id": p.ServiceID,
	})
	d.writeProviderView(w, r, http.StatusCreated, p)
}

// postDirectProvider is the kind "direct" branch of PostAIProvider. The
// backing service is owned by the admin who creates the provider.
func (d Deps) postDirectProvider(w http.ResponseWriter, r *http.Request, slug, name string, up upstreamReq) {
	if !d.upstreamHostAllowed(w, r, up.BaseURL) {
		return
	}
	in := up.input()
	in.Slug, in.Name = slug, name
	p, err := d.AIProviders.CreateDirectProvider(r.Context(), userID(r.Context()), in)
	if err != nil {
		if !mapProviderErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditProvider(r, audit.ActionAIProviderCreate, p, upstreamAudit(p))
	d.writeProviderView(w, r, http.StatusCreated, p)
}

// PutAIProvider handles PUT /api/v1/ai/providers/{slug} (admin only).  Slug
// and name are both required.  After a slug change the old base URL stops
// working at once.
func (d Deps) PutAIProvider(w http.ResponseWriter, r *http.Request) {
	oldSlug := chi.URLParam(r, "slug")
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var in putProviderReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name, ok := validProviderName(w, in.Name)
	if !ok {
		return
	}
	if !store.ValidProviderSlug(in.Slug) {
		writeErr(w, http.StatusBadRequest, providerSlugRule)
		return
	}

	p, err := d.AIProviders.UpdateProvider(r.Context(), oldSlug, in.Slug, name)
	if err != nil {
		if !mapProviderErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	d.auditProvider(r, audit.ActionAIProviderUpdate, p, map[string]any{
		"old_slug": oldSlug,
		"new_slug": p.Slug,
	})
	d.writeProviderView(w, r, http.StatusOK, p)
}

// DeleteAIProvider handles DELETE /api/v1/ai/providers/{slug} (admin only).
// A tunnel provider's service is kept.  A direct provider's backing service
// is removed with it, and with that its API keys and model list; the vault
// slot is configured on the relay and is not touched.
func (d Deps) DeleteAIProvider(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	p, ok := d.providerForWrite(w, r)
	if !ok {
		return
	}
	if err := d.AIProviders.DeleteProvider(r.Context(), slug); err != nil {
		if !mapProviderErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditProvider(r, audit.ActionAIProviderDelete, p, map[string]any{"kind": p.Kind})
	w.WriteHeader(http.StatusNoContent)
}

// providerForWrite loads the provider named in the path for an admin-only
// handler and writes the 404 / 500 itself.
func (d Deps) providerForWrite(w http.ResponseWriter, r *http.Request) (db.AIProvider, bool) {
	p, err := d.AIProviders.ProviderBySlug(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, db.ErrNotFound) || errors.Is(err, store.ErrProviderNotFound) {
		writeErr(w, http.StatusNotFound, "provider not found")
		return db.AIProvider{}, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return db.AIProvider{}, false
	}
	return p, true
}

// PutAIProviderUpstream handles PUT /api/v1/ai/providers/{slug}/upstream
// (admin only).  It changes the upstream settings of a direct provider; a
// field that is left out keeps its stored value, so the credential slot stays
// unless the body names another one.  extra_headers {} removes all headers.
// The change applies to the next request.
func (d Deps) PutAIProviderUpstream(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var in upstreamReq
	if msg, unknown := decodeStrictJSON(r.Body, &in); msg != "" {
		if unknown {
			msg += credentialHint
		}
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	old, ok := d.providerForWrite(w, r)
	if !ok {
		return
	}
	if old.Kind != "direct" {
		writeErr(w, http.StatusConflict, "only direct providers have upstream settings")
		return
	}
	if in.BaseURL != "" && !d.upstreamHostAllowed(w, r, in.BaseURL) {
		return
	}
	p, err := d.AIProviders.UpdateProviderUpstream(r.Context(), old.Slug, in.input())
	if err != nil {
		if !mapProviderErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	payload := upstreamAudit(p)
	payload["upstream"] = true
	d.auditProvider(r, audit.ActionAIProviderUpdate, p, payload)
	d.writeProviderView(w, r, http.StatusOK, p)
}

// aiProviderModelResp is one entry of a provider's model list.
type aiProviderModelResp struct {
	ID            string    `json:"id"`
	DisplayName   string    `json:"display_name"`
	ContextLength int64     `json:"context_length"`
	SyncedAt      time.Time `json:"synced_at"`
}

// GetAIProviderModels handles GET /api/v1/ai/providers/{slug}/models.  A
// provider the caller may not see answers 404, like one that does not exist.
func (d Deps) GetAIProviderModels(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	_, ok, err := d.providerView(r, slug)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	models, err := d.AIProviders.ListProviderModels(r.Context(), slug)
	if err != nil {
		if !mapProviderErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	out := make([]aiProviderModelResp, len(models))
	for i, m := range models {
		out[i] = aiProviderModelResp{ID: m.ModelID, DisplayName: m.DisplayName, ContextLength: m.ContextLength, SyncedAt: m.SyncedAt}
	}
	writeJSON(w, http.StatusOK, out)
}

// modelSyncTimeout bounds one model sync, whatever the fetcher does. It is
// below the router's 30 s request timeout and the fetcher's own 30 s cap, so
// that this deadline is the one that fires and the admin gets the 502 with
// its message.
var modelSyncTimeout = 25 * time.Second

// modelSyncs holds the slugs with a sync in flight: one outbound call per
// provider at a time.
var modelSyncs sync.Map

// PostAIProviderModelsSync handles POST /api/v1/ai/providers/{slug}/models/sync
// (admin only).  It reads the model list from a direct provider's upstream
// and replaces the stored list with it.  The call leaves the relay through
// the guarded upstream transport; nothing the upstream sent is returned
// except the number of models.
func (d Deps) PostAIProviderModelsSync(w http.ResponseWriter, r *http.Request) {
	p, ok := d.providerForWrite(w, r)
	if !ok {
		return
	}
	if p.Kind != "direct" {
		writeErr(w, http.StatusConflict, "sync is available for direct providers")
		return
	}
	if d.FetchProviderModels == nil {
		writeErr(w, http.StatusServiceUnavailable, "model sync is not available on this relay")
		return
	}
	if _, running := modelSyncs.LoadOrStore(p.Slug, struct{}{}); running {
		writeErr(w, http.StatusConflict, "a sync for this provider is already running")
		return
	}
	defer modelSyncs.Delete(p.Slug)

	ctx, cancel := context.WithTimeout(r.Context(), modelSyncTimeout)
	defer cancel()
	fetched, err := d.FetchProviderModels(ctx, p)
	if errors.Is(err, aiprovider.ErrNotConfigured) {
		writeErr(w, http.StatusConflict, "the credential slot "+p.CredentialSlot+" is not set")
		return
	}
	if err != nil {
		// FetchModels words its errors for an admin: no transport error, no
		// upstream body, no credential.
		d.warn("ai provider model sync failed", "provider", p.Slug, "err", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	models := make([]db.AIProviderModel, len(fetched))
	for i, m := range fetched {
		models[i] = db.AIProviderModel{ProviderSlug: p.Slug, ModelID: m.ID, DisplayName: m.DisplayName, ContextLength: m.ContextLength}
	}
	if err := d.AIProviders.ReplaceProviderModels(r.Context(), p.Slug, models); err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidProviderConfig):
			writeErr(w, http.StatusBadGateway, "the provider's model list was refused")
		case !mapProviderErr(w, err):
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditProvider(r, audit.ActionAIProviderModelsSync, p, map[string]any{"count": len(models)})
	writeJSON(w, http.StatusOK, map[string]int{"count": len(models)})
}

// msgModelID is the 400 message for a model id that cannot be stored.
const msgModelID = "id must be 1-200 characters without control characters"

// PostAIProviderModel handles POST /api/v1/ai/providers/{slug}/models (admin
// only): adds one model id by hand, for either kind of provider.
func (d Deps) PostAIProviderModel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !aiprovider.ValidModelID(in.ID) {
		writeErr(w, http.StatusBadRequest, msgModelID)
		return
	}
	p, ok := d.providerForWrite(w, r)
	if !ok {
		return
	}
	if err := d.AIProviders.AddProviderModel(r.Context(), p.Slug, in.ID); err != nil {
		if !mapProviderErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditProvider(r, audit.ActionAIProviderModelAdd, p, map[string]any{"model_id": in.ID})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteAIProviderModel handles DELETE /api/v1/ai/providers/{slug}/models?id=…
// (admin only).  The id is a query parameter because model ids contain "/".
func (d Deps) DeleteAIProviderModel(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !aiprovider.ValidModelID(id) {
		writeErr(w, http.StatusBadRequest, msgModelID)
		return
	}
	p, ok := d.providerForWrite(w, r)
	if !ok {
		return
	}
	if err := d.AIProviders.RemoveProviderModel(r.Context(), p.Slug, id); err != nil {
		switch {
		case errors.Is(err, store.ErrProviderNotFound):
			writeErr(w, http.StatusNotFound, "model not found")
		case !mapProviderErr(w, err):
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditProvider(r, audit.ActionAIProviderModelRemove, p, map[string]any{"model_id": id})
	w.WriteHeader(http.StatusNoContent)
}
