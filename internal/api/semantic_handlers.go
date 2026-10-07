package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// SemanticEngine is the narrow interface the semantic cache handlers consume.
// The concrete semantic.Cache satisfies it via an adapter; tests provide a
// fake. Deliberately a subset — only the two operations the JSON surface
// needs (global clear + aggregate stats). Lookup/Promote live on the proxy
// hot path and are wired separately.
type SemanticEngine interface {
	// ClearAll removes every semantic index entry across all services.
	// The chromem-go in-memory collections are also dropped so stale state
	// does not survive until next process restart.
	ClearAll(ctx context.Context) error
	// AggregateStats sums occupancy/hit-rate across all services and
	// returns a global view. NoopEngine returns zeros; the chromem backend
	// sums across its in-memory per-service collections.
	AggregateStats(ctx context.Context) (SemanticStats, error)
}

// SemanticStats is the aggregate view returned by SemanticEngine.AggregateStats
// and mapped to the five semantic_* JSON fields of GET /cache/stats (spec A.4).
type SemanticStats struct {
	Entries            int
	OnDiskBytes        int64
	HitRate24h         float64
	SimilarReturned24h int
	Promotions24h      int
}

// ServiceAIConfigStore is the narrow read/write surface for the
// service_ai_config table used by PUT /services/{id}/ai-config.
type ServiceAIConfigStore interface {
	// GetServiceAIConfigRaw returns the raw JSON blob for the service, and
	// ok=false (nil error) when no row exists.
	GetServiceAIConfigRaw(ctx context.Context, serviceID string) (raw []byte, ok bool, err error)
	// UpsertServiceAIConfig writes (or replaces) the JSON blob for the
	// service. The config is expected to be valid JSON.
	UpsertServiceAIConfig(ctx context.Context, serviceID string, config []byte) error
}

// semanticDefaultSettings are the spec A.3 global defaults for the semantic
// cache. Returned when no per-service override is configured and also used
// as the fallback when the stored blob is missing/malformed.
//
// The wire type SemanticSettings is shared with cmd/server (see
// cache_settings_wire.go) so a PUT in / GET out round-trips byte-for-byte.
var semanticDefaultSettings = SemanticSettings{
	Enabled:         false,
	MinSimilarity:   0.85,
	EmbeddingMode:   "local",
	EmbeddingURL:    "http://localhost:11434/v1/embeddings",
	EmbeddingModel:  "nomic-embed-text",
	FallbackPolicy:  "treat_as_miss",
	PromoteOnMiss:   true,
	MaxIndexEntries: 10000,
}

// DeleteSemanticCacheEntries handles DELETE /api/v1/cache/semantic/entries.
// Admin or ai:configure:any only — gating is enforced by the router via
// requireAdminOrAIConfigureAny (same guard as DELETE /cache/entries). 204
// on success. This endpoint clears only the semantic tier; the exact-match
// tier is unaffected.
func (d Deps) DeleteSemanticCacheEntries(w http.ResponseWriter, r *http.Request) {
	if d.SemanticEngine == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := d.SemanticEngine.ClearAll(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, "clear semantic cache failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PutServiceAIConfig handles PUT /api/v1/services/{serviceID}/ai-config.
//
// Permission: service owner OR ai:configure:any (admin). The ownership check
// reuses the CacheServiceLookup.GetServiceOwner surface (same as the per-
// service cache-clear handler).
//
// Validation (spec A.3):
//   - cache.semantic.min_similarity must be in [0.0, 1.0] inclusive.
//   - cache.semantic.embedding_mode must be "local" or "none".
//
// All other fields in the blob are stored as-is (no schema enforcement).
// Returns 204 on success.
func (d Deps) PutServiceAIConfig(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if serviceID == "" {
		writeErr(w, http.StatusBadRequest, "service id is required")
		return
	}

	// Auth: owner OR ai:configure:any (same pattern as DeleteServiceCacheEntries).
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())

	// :any short-circuits ownership.
	if !authz.Can(role, authz.PermAIConfigureAny) {
		// Must be an :own caller and own the service.
		if !authz.Can(role, authz.PermAIConfigureOwn) {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		if d.CacheServices == nil {
			writeErr(w, http.StatusInternalServerError, "service lookup unavailable")
			return
		}
		owner, err := d.CacheServices.GetServiceOwner(r.Context(), serviceID)
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "service not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "service lookup failed")
			return
		}
		if owner != uid {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
	} else if d.CacheServices != nil {
		// Even for :any callers, surface 404 cleanly when the service does
		// not exist so the client gets a meaningful error.
		if _, err := d.CacheServices.GetServiceOwner(r.Context(), serviceID); errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "service not found")
			return
		}
	}

	// Decode the incoming blob.
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}

	// Validate JSON structure.
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate cache.semantic sub-block if present.
	if cacheRaw, ok := outer["cache"]; ok && len(cacheRaw) > 0 && string(cacheRaw) != "null" {
		if err := validateServiceAIConfigCacheBlock(cacheRaw); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if d.ServiceAIConfigs == nil {
		writeErr(w, http.StatusInternalServerError, "ai config store unavailable")
		return
	}
	if err := d.ServiceAIConfigs.UpsertServiceAIConfig(r.Context(), serviceID, raw); err != nil {
		writeErr(w, http.StatusInternalServerError, "save ai config failed")
		return
	}
	if d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceAIConfigUpdate,
			SubjectID: serviceID,
			Result:    "ok",
			SourceIP:  lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(aiConfigSections(outer)),
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// aiConfigSections says which sections of a service's AI configuration are
// switched on, for the audit entry of a write. It carries the flags only:
// what a section holds (patterns, addresses, limits) stays out of the log.
// A section that is missing, null or not an object counts as off, as it does
// for the chain.
func aiConfigSections(outer map[string]json.RawMessage) map[string]bool {
	enabled := func(raw json.RawMessage) bool {
		var s struct {
			Enabled bool `json:"enabled"`
		}
		return json.Unmarshal(raw, &s) == nil && s.Enabled
	}
	out := map[string]bool{"semantic": false}
	for _, name := range []string{"cache", "redaction", "guardrails", "inspector", "anthropic"} {
		out[name] = enabled(outer[name])
	}
	var cache struct {
		Semantic json.RawMessage `json:"semantic"`
	}
	if json.Unmarshal(outer["cache"], &cache) == nil {
		out["semantic"] = enabled(cache.Semantic)
	}
	return out
}

// GetServiceAIConfig handles GET /api/v1/services/{serviceID}/ai-config.
//
// Permission: service owner OR ai:configure:any (admin) — the same gate as
// PutServiceAIConfig. Returns 200 with the stored raw JSON blob, or an empty
// object ({}) when no config has been written for the service yet. This is the
// read side the dashboard's AI-endpoint detail page loads on mount.
func (d Deps) GetServiceAIConfig(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if serviceID == "" {
		writeErr(w, http.StatusBadRequest, "service id is required")
		return
	}

	// Auth: owner OR ai:configure:any (mirror PutServiceAIConfig exactly).
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())

	if !authz.Can(role, authz.PermAIConfigureAny) {
		if !authz.Can(role, authz.PermAIConfigureOwn) {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		if d.CacheServices == nil {
			writeErr(w, http.StatusInternalServerError, "service lookup unavailable")
			return
		}
		owner, err := d.CacheServices.GetServiceOwner(r.Context(), serviceID)
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "service not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "service lookup failed")
			return
		}
		if owner != uid {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
	} else if d.CacheServices != nil {
		if _, err := d.CacheServices.GetServiceOwner(r.Context(), serviceID); errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "service not found")
			return
		}
	}

	if d.ServiceAIConfigs == nil {
		writeErr(w, http.StatusInternalServerError, "ai config store unavailable")
		return
	}
	raw, ok, err := d.ServiceAIConfigs.GetServiceAIConfigRaw(r.Context(), serviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read ai config failed")
		return
	}
	if !ok || len(raw) == 0 {
		raw = []byte("{}")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// validateServiceAIConfigCacheBlock validates the cache sub-object of an
// ai-config blob. It checks only the semantic sub-block fields that have
// spec-mandated validation rules (A.3). All other fields pass through.
func validateServiceAIConfigCacheBlock(cacheRaw json.RawMessage) error {
	var cacheOuter map[string]json.RawMessage
	if err := json.Unmarshal(cacheRaw, &cacheOuter); err != nil {
		return fmt.Errorf("invalid cache block")
	}

	semRaw, ok := cacheOuter["semantic"]
	if !ok || len(semRaw) == 0 || string(semRaw) == "null" {
		return nil
	}

	var sem struct {
		MinSimilarity *float64 `json:"min_similarity"`
		EmbeddingMode *string  `json:"embedding_mode"`
	}
	if err := json.Unmarshal(semRaw, &sem); err != nil {
		return fmt.Errorf("invalid cache.semantic block")
	}

	if sem.MinSimilarity != nil {
		v := *sem.MinSimilarity
		if v < 0.0 || v > 1.0 {
			return fmt.Errorf("semantic.min_similarity out of range")
		}
	}

	if sem.EmbeddingMode != nil {
		switch *sem.EmbeddingMode {
		case "local", "none":
			// valid
		default:
			return fmt.Errorf("unknown embedding_mode %q", *sem.EmbeddingMode)
		}
	}

	return nil
}
