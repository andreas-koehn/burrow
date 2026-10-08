// v04_loader.go — Deferral D1 closure: the concrete aigw.ConfigLoader
// implementation that resolves per-service AI config from the
// service_ai_config + service_ip_geo + services tables.
//
// The Loader is consulted by aigw.Chain.Dispatch on every proxied
// request to a service whose proxy.Resolved indicated a service_id. It
// returns ok=false when no service_ai_config row exists, which the
// chain treats as v0.3.0 pass-through (the chain's IsAIPassThrough
// short-circuit) — preserving the byte-for-byte upstream invariant for
// services that have not opted into AI features.
//
// Fail-open: DB errors and JSON-decode errors are returned to the
// chain, which logs + treats them as ok=false. A malformed
// service_ai_config row therefore degrades the affected service to
// pass-through, NOT to 500.
//
// The Loader is safe for concurrent use (the underlying *db.DB is
// concurrent-safe: SQLite + WAL + serialised writes).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/cache/semantic"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/guardrails"
)

// chainConfigLoader implements aigw.ConfigLoader against *db.DB. It is
// constructed once in buildV04Stack and wired into aiChain.Loader after
// aigw.NewChain returns.
type chainConfigLoader struct {
	db  *db.DB
	log *slog.Logger
}

// LoadAIConfig resolves the per-service AI config blob into the typed
// aigw.Service the chain consumes. The returned Service carries only
// the fields decoded from the JSON blob (Cache/Redaction/Guardrails/
// Inspector/Anthropic); ID and APIKeyHeader are preserved from the
// chain's caller — see aigw.Chain.Dispatch for the merge rules.
//
// Returns (zero, false, nil) when no service_ai_config row exists →
// chain pass-through. This is the v0.3.0 invariant the test suite
// asserts (TestAIChain_PassThroughWhenNoServiceConfig).
func (l chainConfigLoader) LoadAIConfig(ctx context.Context, serviceID string) (aigw.Service, bool, error) {
	if l.db == nil {
		return aigw.Service{}, false, nil
	}
	raw, ok, err := l.db.GetServiceAIConfigRaw(ctx, serviceID)
	if err != nil {
		// Fail-open at the chain layer — but surface the err so the
		// chain's Dispatch logs it.
		return aigw.Service{}, false, err
	}
	if !ok {
		// No row → v0.3.0 pass-through invariant.
		return aigw.Service{}, false, nil
	}
	cfg, err := decodeServiceAIConfig(raw)
	if err != nil {
		return aigw.Service{}, false, err
	}
	return aigw.Service{ID: serviceID, AIConfig: cfg}, true, nil
}

// decodeServiceAIConfig parses the outer JSON object and pulls out each
// sub-section into the typed aigw.ServiceAIConfig the chain consumes.
// Missing or "null" sub-objects leave the corresponding pointer nil —
// the chain's "feature disabled" signal.
//
// Unknown keys are tolerated (forward-compat: a newer UI may write
// additional sections this server version ignores). The routing
// sub-object is intentionally NOT decoded here — v0.4.0 wires
// route.Policy via the API layer, not the per-service config blob; the
// chain's routing step is log-only in v0.4.0 (see aigw.Chain.run). A
// "translate_to" inside it, which older dashboards wrote, is therefore
// read by nothing: it named a per-service format adapter that never ran
// and no longer exists.
func decodeServiceAIConfig(blob []byte) (aigw.ServiceAIConfig, error) {
	if len(blob) == 0 {
		return aigw.ServiceAIConfig{}, nil
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(blob, &outer); err != nil {
		return aigw.ServiceAIConfig{}, fmt.Errorf("service_ai_config: invalid json: %w", err)
	}
	var out aigw.ServiceAIConfig

	// .cache → *exact.Settings (+ optional .cache.semantic → *semantic.Settings)
	if raw, ok := outer["cache"]; ok && len(raw) > 0 && string(raw) != "null" {
		s, err := exact.SettingsFromJSON(raw)
		if err != nil {
			return aigw.ServiceAIConfig{}, fmt.Errorf("cache: %w", err)
		}
		out.Cache = &s

		// .cache.semantic → *semantic.Settings. The semantic block lives INSIDE
		// the cache block per spec A.3 (the JSON shape the v0.5.0 API at
		// internal/api/semantic_handlers.go::PutServiceAIConfig validates and
		// accepts). Decoding it here is what wires the chain's semantic tier
		// to the per-service config — without this, cfg.Semantic stays nil
		// even when the operator has enabled it, and chain.go's
		// `cfg.Semantic != nil && cfg.Semantic.Enabled` guard at line 501
		// never fires.
		var inner struct {
			Semantic json.RawMessage `json:"semantic"`
		}
		if err := json.Unmarshal(raw, &inner); err == nil &&
			len(inner.Semantic) > 0 && string(inner.Semantic) != "null" {
			var sem api.SemanticSettings
			if err := json.Unmarshal(inner.Semantic, &sem); err != nil {
				return aigw.ServiceAIConfig{}, fmt.Errorf("cache.semantic: %w", err)
			}
			settings := semantic.Settings{
				Enabled:         sem.Enabled,
				MinSimilarity:   sem.MinSimilarity,
				EmbeddingMode:   sem.EmbeddingMode,
				EmbeddingURL:    sem.EmbeddingURL,
				EmbeddingModel:  sem.EmbeddingModel,
				FallbackPolicy:  sem.FallbackPolicy,
				PromoteOnMiss:   sem.PromoteOnMiss,
				MaxIndexEntries: sem.MaxIndexEntries,
			}
			out.Semantic = &settings
		}
	}

	// .redaction → *aigw.RedactionConfig
	if raw, ok := outer["redaction"]; ok && len(raw) > 0 && string(raw) != "null" {
		var r struct {
			Enabled     bool `json:"enabled"`
			ForLogsOnly bool `json:"for_logs_only"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return aigw.ServiceAIConfig{}, fmt.Errorf("redaction: %w", err)
		}
		out.Redaction = &aigw.RedactionConfig{Enabled: r.Enabled, ForLogsOnly: r.ForLogsOnly}
	}

	// .guardrails → *guardrails.Settings
	if raw, ok := outer["guardrails"]; ok && len(raw) > 0 && string(raw) != "null" {
		var g guardrails.Settings
		if err := json.Unmarshal(raw, &g); err != nil {
			return aigw.ServiceAIConfig{}, fmt.Errorf("guardrails: %w", err)
		}
		out.Guardrails = &g
	}

	// .inspector → *aigw.InspectorConfig
	if raw, ok := outer["inspector"]; ok && len(raw) > 0 && string(raw) != "null" {
		var i struct {
			Enabled     bool `json:"enabled"`
			MaxRequests int  `json:"max_requests"`
		}
		if err := json.Unmarshal(raw, &i); err != nil {
			return aigw.ServiceAIConfig{}, fmt.Errorf("inspector: %w", err)
		}
		out.Inspector = &aigw.InspectorConfig{Enabled: i.Enabled, MaxRequests: i.MaxRequests}
	}

	// .anthropic → *aigw.AnthropicConfig. No adapter stands behind it (see
	// aigw.AnthropicConfig): a service that has the section stays on the
	// chain and is metered, and its requests are not rewritten.
	if raw, ok := outer["anthropic"]; ok && len(raw) > 0 && string(raw) != "null" {
		var a struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return aigw.ServiceAIConfig{}, fmt.Errorf("anthropic: %w", err)
		}
		out.Anthropic = &aigw.AnthropicConfig{Enabled: a.Enabled}
	}

	// .routing is intentionally NOT decoded here — see method doc comment.

	return out, nil
}

// The cache.semantic sub-block wire shape lives in internal/api/cache_settings_wire.go
// as api.SemanticSettings — shared with the runtime handler so a PUT in via
// the API surface round-trips losslessly through this loader to semantic.Settings.
