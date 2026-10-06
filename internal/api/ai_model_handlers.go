package api

// ai_model_handlers.go — /ai/models: synthetic models, and /ai/gateway: the
// base URLs clients are given.
//
// A synthetic model is a name that resolves to an ordered list of provider
// targets per dialect.  Any signed-in caller may read them; writes go
// through requireAIModelWrite.  The store validates the data: its reasons are
// the 400 messages.  A response also says which targets can be tried right
// now and which one is serving each dialect.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// aiModelTarget is one target on the wire, in requests and responses. In a
// request dialect may be left out: the store fills in the provider's format.
type aiModelTarget struct {
	Dialect  string `json:"dialect"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// aiModelTargetResp is a target in a response: what was stored, and whether
// the target can be tried right now.
type aiModelTargetResp struct {
	aiModelTarget
	Available bool `json:"available"`
}

// aiServingTarget names the target that answers a dialect right now.
type aiServingTarget struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// aiModelResp is the JSON wire shape of one synthetic model.
type aiModelResp struct {
	Name                string `json:"name"`
	Description         string `json:"description"`
	Enabled             bool   `json:"enabled"`
	FallbackOnRateLimit bool   `json:"fallback_on_rate_limit"`
	AttemptTimeoutS     int    `json:"attempt_timeout_s"`
	TotalTimeoutS       int    `json:"total_timeout_s"`
	// Within one dialect the order is the order the targets are tried in.
	Targets []aiModelTargetResp `json:"targets"`
	// The formats the model is served in, sorted; derived from the targets.
	Dialects []string `json:"dialects"`
	// Per dialect the model has a target in: the first available target, or
	// null when none is available or the model is disabled.
	Serving   map[string]*aiServingTarget `json:"serving"`
	CreatedAt time.Time                   `json:"created_at"`
	UpdatedAt time.Time                   `json:"updated_at"`
}

// providersBySlug loads every provider once for a model view. Without a
// provider store the map is empty and every target reads as unavailable.
func (d Deps) providersBySlug(ctx context.Context) (map[string]db.AIProvider, error) {
	out := map[string]db.AIProvider{}
	if d.AIProviders == nil {
		return out, nil
	}
	providers, err := d.AIProviders.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		out[p.Slug] = p
	}
	return out, nil
}

// targetAvailable reports whether a target of dialect can be tried on p right
// now: the provider speaks the dialect, the breaker is not skipping it, and
// for a direct provider every credential slot it names is set, for a
// tunnelled one its client is connected. It only reads: the breaker is asked
// with Open, which starts no trial.
func (d Deps) targetAvailable(p db.AIProvider, dialect string) bool {
	if p.APIFormat != dialect {
		return false
	}
	if d.AIBreaker != nil && d.AIBreaker.Open(p.Slug) {
		return false
	}
	if p.Kind == "tunnel" {
		return d.composeLive(p.ServiceID).Connected
	}
	_, present := d.credentialSlots(p)
	return present
}

// modelView is the response for m. providers is the result of
// providersBySlug; a target whose provider is not in it is unavailable.
func (d Deps) modelView(m db.AIModel, providers map[string]db.AIProvider) aiModelResp {
	out := aiModelResp{
		Name: m.Name, Description: m.Description, Enabled: m.Enabled,
		FallbackOnRateLimit: m.FallbackOnRateLimit,
		AttemptTimeoutS:     m.AttemptTimeoutS, TotalTimeoutS: m.TotalTimeoutS,
		Targets:   make([]aiModelTargetResp, 0, len(m.Targets)),
		Dialects:  []string{},
		Serving:   map[string]*aiServingTarget{},
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
	for _, t := range m.Targets { // ordered by dialect, then position
		p, known := providers[t.ProviderSlug]
		available := known && d.targetAvailable(p, t.Dialect)
		out.Targets = append(out.Targets, aiModelTargetResp{
			aiModelTarget: aiModelTarget{Dialect: t.Dialect, Provider: t.ProviderSlug, Model: t.TargetModel},
			Available:     available,
		})
		if !slices.Contains(out.Dialects, t.Dialect) {
			out.Dialects = append(out.Dialects, t.Dialect)
		}
		if serving, seen := out.Serving[t.Dialect]; !seen || (serving == nil && available) {
			out.Serving[t.Dialect] = nil
			if available {
				out.Serving[t.Dialect] = &aiServingTarget{Provider: t.ProviderSlug, Model: t.TargetModel}
			}
		}
	}
	// Requests for a disabled model are refused whatever its targets can do.
	if !m.Enabled {
		for dialect := range out.Serving {
			out.Serving[dialect] = nil
		}
	}
	slices.Sort(out.Dialects)
	return out
}

// writeModelView answers with the view of m. It also answers the writes,
// which are stored and audited by then: when the providers cannot be read the
// answer keeps its status and reports every target as unavailable, so that a
// client does not repeat a create that went through.
func (d Deps) writeModelView(w http.ResponseWriter, r *http.Request, status int, m db.AIModel) {
	providers, err := d.providersBySlug(r.Context())
	if err != nil {
		d.warn("ai model view: provider list failed, targets reported as unavailable", "model", m.Name, "err", err)
		providers = map[string]db.AIProvider{}
	}
	writeJSON(w, status, d.modelView(m, providers))
}

// aiModelReq is the body of POST /ai/models and PUT /ai/models/{name}.
// Timeouts of 0 (or left out) become the store's defaults.
type aiModelReq struct {
	// PUT: left out keeps the name; another name renames the model.
	Name        string `json:"name"`
	Description string `json:"description"`
	// Left out: on on create, unchanged on update.
	Enabled             *bool           `json:"enabled"`
	FallbackOnRateLimit bool            `json:"fallback_on_rate_limit"`
	AttemptTimeoutS     int             `json:"attempt_timeout_s"`
	TotalTimeoutS       int             `json:"total_timeout_s"`
	Targets             []aiModelTarget `json:"targets"`
}

const (
	// maxModelBody bounds a model body: 16 targets of a 63-byte slug and a
	// 200-byte model id plus a 500-byte description fit several times.
	maxModelBody = 16 << 10
	// maxModelReqTargets is 8 per dialect for the two dialects; the store
	// counts per dialect.
	maxModelReqTargets = 16

	msgModelName     = "name must be 2-63 characters: lowercase letters, digits, dot, underscore, hyphen"
	msgModelTargets  = "a model needs at least one target and at most 8 per format"
	msgModelNotFound = "model not found"
	msgModelsOff     = "synthetic models are not available on this relay"
)

// model turns the body into the store's type. enabled is the value used when
// the body leaves the field out.
func (in aiModelReq) model(enabled bool) db.AIModel {
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	m := db.AIModel{
		Name: in.Name, Description: in.Description, Enabled: enabled,
		FallbackOnRateLimit: in.FallbackOnRateLimit,
		AttemptTimeoutS:     in.AttemptTimeoutS, TotalTimeoutS: in.TotalTimeoutS,
		Targets: make([]db.AIModelTarget, 0, len(in.Targets)),
	}
	for _, t := range in.Targets {
		m.Targets = append(m.Targets, db.AIModelTarget{Dialect: t.Dialect, ProviderSlug: t.Provider, TargetModel: t.Model})
	}
	return m
}

// validModelReq checks what needs no database, so that a malformed name or
// an unbounded target list reaches no store implementation; the store checks
// everything again, and the providers. It writes the 400 itself.
func validModelReq(w http.ResponseWriter, in aiModelReq) bool {
	if !store.ValidModelName(in.Name) {
		writeErr(w, http.StatusBadRequest, msgModelName)
		return false
	}
	if len(in.Targets) == 0 || len(in.Targets) > maxModelReqTargets {
		writeErr(w, http.StatusBadRequest, msgModelTargets)
		return false
	}
	return true
}

// mapModelErr writes the HTTP error for a model store error and reports
// whether it handled it.
func mapModelErr(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrInvalidModel):
		// The reason names a field or a provider slug, never the caller's text.
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), store.ErrInvalidModel.Error()+": "))
	case errors.Is(err, store.ErrModelNotFound):
		writeErr(w, http.StatusNotFound, msgModelNotFound)
	case errors.Is(err, store.ErrModelExists):
		writeErr(w, http.StatusConflict, "model name already in use")
	default:
		return false
	}
	return true
}

// modelAudit is the audit payload of a model: its switches and its targets
// as "dialect:provider/model". The description is free text and stays out.
func modelAudit(m db.AIModel) map[string]any {
	targets := make([]string, 0, len(m.Targets))
	for _, t := range m.Targets {
		targets = append(targets, t.Dialect+":"+t.ProviderSlug+"/"+t.TargetModel)
	}
	return map[string]any{
		"enabled":                m.Enabled,
		"fallback_on_rate_limit": m.FallbackOnRateLimit,
		"targets":                targets,
	}
}

// auditAI appends one audit event for a model or a gateway key (best-effort).
func (d Deps) auditAI(r *http.Request, action, subjectID, subjectLabel string, payload map[string]any) {
	if d.AuditAppender == nil {
		return
	}
	lc := audit.LogContextFrom(r.Context())
	_ = d.AuditAppender.Append(r.Context(), audit.Event{
		ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
		Action:    action,
		SubjectID: subjectID, SubjectLabel: subjectLabel,
		Result:   "ok",
		SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
		Payload: audit.MustJSON(payload),
	})
}

// requireAIModelWrite gates the three routes that change a synthetic model.
// It must run after RequireSession.
//
// A dashboard session passes for an admin and for a role that holds
// ai:configure:any. An automation token passes only when it declares
// ai:configure:any AND its user's role still grants it: the role of the
// token's user alone opens nothing, so an admin's token minted for something
// else cannot create, repoint or delete a model.
//
// Whether the caller is a token is read from the token id, not from the
// declared permissions: a token that declares none must not be taken for a
// session.
func (d Deps) requireAIModelWrite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, err := d.callerRoleForAuth(r)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		allowed := role == "admin" || authz.Can(role, authz.PermAIConfigureAny)
		if bearerTokenID(r.Context()) != "" {
			allowed = slices.Contains(bearerPerms(r.Context()), string(authz.PermAIConfigureAny)) &&
				authz.Can(role, authz.PermAIConfigureAny)
		}
		if !allowed {
			writeErr(w, http.StatusForbidden, "ai:configure:any required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GetAIModels handles GET /api/v1/ai/models: every synthetic model, by name.
func (d Deps) GetAIModels(w http.ResponseWriter, r *http.Request) {
	out := []aiModelResp{}
	if d.AIModels != nil {
		models, err := d.AIModels.ListModels(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		providers, err := d.providersBySlug(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, m := range models {
			out = append(out, d.modelView(m, providers))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// modelFromPath loads the model named in the path and writes the 404 / 500
// itself. A name that cannot be a model's is not looked up.
func (d Deps) modelFromPath(w http.ResponseWriter, r *http.Request) (db.AIModel, bool) {
	name := chi.URLParam(r, "name")
	if d.AIModels == nil || !store.ValidModelName(name) {
		writeErr(w, http.StatusNotFound, msgModelNotFound)
		return db.AIModel{}, false
	}
	m, err := d.AIModels.ModelByName(r.Context(), name)
	if err != nil {
		if !mapModelErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return db.AIModel{}, false
	}
	return m, true
}

// GetAIModel handles GET /api/v1/ai/models/{name}.
func (d Deps) GetAIModel(w http.ResponseWriter, r *http.Request) {
	if m, ok := d.modelFromPath(w, r); ok {
		d.writeModelView(w, r, http.StatusOK, m)
	}
}

// PostAIModel handles POST /api/v1/ai/models (admin or ai:configure:any).
func (d Deps) PostAIModel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxModelBody)
	var in aiModelReq
	if msg, _ := decodeStrictJSON(r.Body, &in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if !validModelReq(w, in) {
		return
	}
	if d.AIModels == nil {
		writeErr(w, http.StatusServiceUnavailable, msgModelsOff)
		return
	}
	m, err := d.AIModels.CreateModel(r.Context(), in.model(true))
	if err != nil {
		if !mapModelErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditAI(r, audit.ActionAIModelCreate, m.Name, "", modelAudit(m))
	d.writeModelView(w, r, http.StatusCreated, m)
}

// PutAIModel handles PUT /api/v1/ai/models/{name} (admin or
// ai:configure:any).  The body replaces the model: its targets are the new
// list.  Two fields keep their stored value when left out: name and enabled.
// A different name renames the model; gateway keys that name the old one in
// their allow-list are not rewritten and no longer reach it.
func (d Deps) PutAIModel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxModelBody)
	var in aiModelReq
	if msg, _ := decodeStrictJSON(r.Body, &in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	old, ok := d.modelFromPath(w, r)
	if !ok {
		return
	}
	if in.Name == "" {
		in.Name = old.Name
	}
	if !validModelReq(w, in) {
		return
	}
	m, err := d.AIModels.UpdateModel(r.Context(), old.Name, in.model(old.Enabled))
	if err != nil {
		if !mapModelErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	payload := modelAudit(m)
	if m.Name != old.Name {
		payload["old_name"] = old.Name
	}
	d.auditAI(r, audit.ActionAIModelUpdate, m.Name, "", payload)
	d.writeModelView(w, r, http.StatusOK, m)
}

// DeleteAIModel handles DELETE /api/v1/ai/models/{name} (admin or
// ai:configure:any).  Requests for the name are refused from then on.
// Gateway keys keep the name in their allow-list; it matches nothing until a
// model of that name exists again.
func (d Deps) DeleteAIModel(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if d.AIModels == nil || !store.ValidModelName(name) {
		writeErr(w, http.StatusNotFound, msgModelNotFound)
		return
	}
	if err := d.AIModels.DeleteModel(r.Context(), name); err != nil {
		if !mapModelErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditAI(r, audit.ActionAIModelDelete, name, "", map[string]any{})
	w.WriteHeader(http.StatusNoContent)
}

// aiGatewayEndpoint is one dialect endpoint of the gateway.
type aiGatewayEndpoint struct {
	Dialect string `json:"dialect"`
	BaseURL string `json:"base_url"`
}

// GetAIGatewayInfo handles GET /api/v1/ai/gateway: the base URL a client is
// given per dialect.  base_url is "" when the relay has no auth domain; the
// dashboard then builds it from its own origin.
func (d Deps) GetAIGatewayInfo(w http.ResponseWriter, _ *http.Request) {
	endpoints := []aiGatewayEndpoint{{Dialect: "openai"}, {Dialect: "anthropic"}}
	if d.AuthDomain != "" {
		endpoints[0].BaseURL = "https://" + d.AuthDomain + "/openai/v1"
		endpoints[1].BaseURL = "https://" + d.AuthDomain + "/anthropic"
	}
	writeJSON(w, http.StatusOK, map[string][]aiGatewayEndpoint{"endpoints": endpoints})
}
