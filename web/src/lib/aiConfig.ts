import type { ServiceAIConfig } from "@/lib/contract";

// A fully-populated default ServiceAIConfig.
//
// GET /services/{id}/ai-config returns {} for services with no stored config,
// and may return a PARTIAL blob (different dashboard pages PUT different
// sub-sections — routing, cache.semantic, etc.). Every consumer must therefore
// merge the response over these defaults before reading nested fields, or it
// will crash on e.g. `cfg.routing.model_alias` / `cfg.cache.semantic` /
// `cfg.inspector.enabled` when that sub-object is absent.
export const DEFAULT_AI_CONFIG: ServiceAIConfig = {
  cache: {
    enabled: false,
    applies_per: "per_endpoint",
    ttl_seconds: 600,
    max_entries: 1000,
    max_per_entry_kb: 64,
    semantic: {
      enabled: false,
      min_similarity: 0.85,
      embedding_mode: "local",
      embedding_url: "http://localhost:11434/v1/embeddings",
      embedding_model: "nomic-embed-text",
      fallback_policy: "treat_as_miss",
      promote_on_miss: true,
      max_index_entries: 10000,
    },
  },
  redaction: { enabled: false, redact_for_logs_only: false, rule_ids: [], presidio_enabled: false },
  guardrails: { enabled: false, action: "log_only" },
  inspector: { enabled: true, max_requests: 100 },
  routing: {
    strategy: "single",
    model_alias: "",
    header_name: "X-Burrow-Model",
    paused: false,
    circuit_breaker: { failure_pct: 50, window_seconds: 30, cool_down_seconds: 60 },
    backends: [],
  },
  ip_geo: { enabled: false, allow_cidrs: [], block_cidrs: [], allow_countries: [], block_countries: [] },
  mtls: { enabled: false, ca_fingerprint_sha256: "" },
};

// Deep-merge a possibly-empty / partial ai-config over the defaults so callers
// can safely read every nested field. A fully-stored config is returned
// effectively unchanged (merge is idempotent over a complete blob).
export function withAIConfigDefaults(
  c: Partial<ServiceAIConfig> | null | undefined,
): ServiceAIConfig {
  const d = DEFAULT_AI_CONFIG;
  if (!c || typeof c !== "object") return d;
  // Spread the defaults first so every required field is present, then overlay
  // whatever the stored (possibly empty / partial) config actually contains.
  // The `as ServiceAIConfig` is sound: each sub-object starts from the complete
  // default, so the merged result always has every field at runtime — TS only
  // widens the spread of possibly-undefined sources to optional.
  return {
    cache: {
      ...d.cache,
      ...c.cache,
      semantic: { ...d.cache.semantic, ...c.cache?.semantic },
    },
    redaction: { ...d.redaction, ...c.redaction },
    guardrails: { ...d.guardrails, ...c.guardrails },
    inspector: { ...d.inspector, ...c.inspector },
    routing: {
      ...d.routing,
      ...c.routing,
      circuit_breaker: { ...d.routing.circuit_breaker, ...c.routing?.circuit_breaker },
      backends: c.routing?.backends ?? d.routing.backends,
    },
    ip_geo: { ...d.ip_geo, ...c.ip_geo },
    mtls: { ...d.mtls, ...c.mtls },
  } as ServiceAIConfig;
}
