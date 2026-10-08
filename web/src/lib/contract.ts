// Typed mirror of dev/superpowers/specs/2026-05-19-v0.2.0-api-contract.md.
// snake_case fields match the wire verbatim; do not rename.

export type UserRole = "admin" | "user";
export type UserStatus = "active" | "suspended";

export interface UserAdmin {
  id: string;
  email: string;
  role: UserRole;
  status: UserStatus;
  last_login: string | null;
  created_at: string;
}

export interface UsersPage {
  users: UserAdmin[];
  total: number;
}

export interface RoleSummary {
  name: string;
  description: string;
  created_at: string;
  // v0.4.0 additive — built-in roles are locked; custom roles are editable.
  // Older backends omit it; treat undefined as built-in for backwards safety.
  builtin?: boolean;
}

export interface RoleDetail extends RoleSummary {
  permissions: string[];
}

export interface Session {
  id: string;
  ip: string;
  user_agent: string;
  created_at: string;
  expires_at: string;
  current: boolean;
}

export type SettingsMap = Record<string, string>;
export type SmtpTls = "none" | "starttls" | "implicit";

export const ACCESS_MODES = ["open", "api_key", "burrow_login", "mtls"] as const;
export type AccessMode = (typeof ACCESS_MODES)[number];
export function isAccessMode(v: string): v is AccessMode {
  return (ACCESS_MODES as readonly string[]).includes(v);
}

export interface ServiceView {
  id: string;
  name: string;
  type: string;
  remote_port: number;
  local_addr: string;
  access_mode: AccessMode;
  /** Durable service id for http services; absent for tcp. Routable at /services/:id. */
  service_id?: string;
  bytes_in: number;
  bytes_out: number;
  total_bytes_in: number;
  total_bytes_out: number;
}

export interface ClientView {
  session_id: string;
  user_id: string;
  token_name: string;
  remote_addr: string;
  os: string;
  arch: string;
  client_version: string;
  service_count: number;
  total_bytes_in: number;
  total_bytes_out: number;
}

/** GET /client/discovery: what a client may know before it has a token. Public. */
export interface ClientDiscovery {
  control: string;
  version: string;
  min_client_version: string;
  protocol_version: number;
}

/**
 * GET /client/login/requests/{user_code}: a machine asking to be signed in
 * (`burrow login`), as the approval page shows it. Hostname, OS, architecture,
 * client version and the suggested token name were sent by that machine and
 * are untrusted display text; the source IP and the age are the relay's own.
 */
export interface ClientLoginRequest {
  user_code: string;
  hostname: string;
  os: string;
  arch: string;
  client_version: string;
  source_ip: string;
  status: "pending" | "approved" | "denied";
  suggested_token_name: string;
  age_seconds: number;
  created_at: string;
  expires_at: string;
}

export interface ClientDetail extends ClientView {
  services: ServiceView[];
}

export interface NewToken {
  name: string;
  token: string;
}

// ---- v0.3.0: durable services + HTTP access config ----
// Mirror of dev/superpowers/specs/2026-05-19-v0.3.0-api-contract.md Part C/D/E.

export interface Service {
  id: string;
  name: string;
  type: "tcp" | "http";
  slug: string;
  url: string;
  access_mode: AccessMode;
  api_key_header: string;
  /** Reachable through the AI gateway only; `url` is then empty. */
  gateway_only: boolean;
  connected: boolean;
  remote_port: number;
  local_addr: string;
}

export interface ServiceDetail extends Service {
  api_key_count: number;
  access_policy: string[];
}

export interface ServiceApiKey {
  id: string;
  name: string;
  last_used: string | null;
  created_at: string;
}

export interface AccessPolicy {
  roles: string[];
}

// create-key response (plaintext, shown once)
export interface CreatedApiKey {
  id: string;
  name: string;
  key: string;
}

// ---- v0.4.0: AI gateway + company-scale dashboard ----
// Mirror of dev/superpowers/specs/2026-05-19-v0.4.0-api-contract.md.

// AI provider — a slug under which a model backend is served at base_url.
// Identity fields come from the providers table; the rest is read live from
// the backing service.
export interface AiProvider {
  slug: string;
  name: string;
  kind: "tunnel" | "direct";
  api_format: "openai" | "anthropic";
  service_id: string;
  /** https://<auth_domain>/ai/<provider>/v1, or "" when the relay has no auth domain. */
  base_url: string;
  /** Direct providers only: where the relay sends requests; "" for a tunnel provider. */
  upstream_base_url: string;
  /** Direct providers only: name of the vault slot. The credential itself never passes the API. */
  credential_slot: string;
  /** Whether the vault slot holds a value; with several slots, whether every one does. */
  credential_present: boolean;
  /** The slots credential_slot names, in the order they are tried; empty for a tunnel provider. Never a value. */
  credential_slots: { slot: string; present: boolean }[];
  /** The gateway is skipping the provider for now because recent requests to it failed. */
  breaker_open: boolean;
  /** Direct providers, admin callers only. */
  auth_header?: string;
  /** Direct providers, admin callers only: header value with the literal {key} placeholder. */
  auth_format?: string;
  /** Direct providers, admin callers only: names of the static extra headers; values are write-only. */
  extra_header_names?: string[];
  billing: "metered" | "flat";
  /**
   * Whether the provider offers the OpenAI Responses API (POST /responses), as
   * its operator stated. While false, /openai/v1/responses refuses its models.
   */
  supports_responses: boolean;
  /** How many requests the provider serves at once; more wait for a free place. 0 = no limit. */
  max_concurrent: number;
  /** How many of those places are taken right now; 0 while there is no limit. */
  in_use: number;
  /** Number of entries in the provider's stored model list. */
  model_count: number;
  model_alias: string;
  concrete_model: string;
  backend_type: "ollama" | "vllm" | "openai-compat" | "other";
  api_key_count: number;
  requests_24h: number;
  cache_hits_24h: number;
  latency_p95_ms: number;
  status: "Connected" | "Degraded" | "Offline";
  client_session_id: string;
}

// One entry of a provider's stored model list.
export interface AiProviderModel {
  id: string;
  display_name: string;
  context_length: number;
  synced_at: string;
}

// Body of PUT /ai/providers/{slug}/upstream; a field left out keeps its stored
// value. There is no field for the credential.
export interface AiProviderUpstreamInput {
  api_format?: "openai" | "anthropic";
  base_url?: string;
  credential_slot?: string;
  auth_header?: string;
  auth_format?: string;
  /** Replaces all extra headers; {} removes them. */
  extra_headers?: Record<string, string>;
  billing?: "metered" | "flat";
  /** Only for api_format "openai". */
  supports_responses?: boolean;
  /** 0–1000; 0 = no limit. */
  max_concurrent?: number;
}

// Service AI config (spec Part B.7) — one row per service, default-filled.
export interface CacheSettings {
  enabled: boolean;
  applies_per: "global" | "per_endpoint" | "per_api_key";
  ttl_seconds: number;
  max_entries: number;
  max_per_entry_kb: number;
  // v0.5.0 additive — present only when the build tag is on.
  semantic?: SemanticCacheSettings;
}
export interface RedactionSettings {
  enabled: boolean;
  redact_for_logs_only: boolean;
  rule_ids: string[];
  presidio_enabled: boolean;
  presidio_url?: string;
}
export interface GuardrailSettings {
  enabled: boolean;
  action: "log_only" | "refuse_403" | "refuse_safe";
}
export interface GuardrailPattern {
  id: string;
  description: string;
}
export interface GuardrailPerServiceSetting {
  service_id: string;
  enabled: boolean | null;
  action: string | null;
}
// GET /guardrails/settings returns the global settings nested under `global`
// plus any per-service overrides. PUT accepts the flat GuardrailSettings body.
export interface GuardrailSettingsResponse {
  global: GuardrailSettings;
  per_service: GuardrailPerServiceSetting[];
}
export interface InspectorSettings {
  enabled: boolean;
  max_requests: number;
}
export interface RoutingPolicy {
  strategy: "single" | "failover" | "weighted" | "header_based" | "sticky" | "multi_provider";
  model_alias: string;
  header_name: string;
  paused: boolean;
  circuit_breaker: { failure_pct: number; window_seconds: number; cool_down_seconds: number };
  backends: { service_id: string; weight: number; concrete_model: string }[];
}
export interface IpGeoConfig {
  enabled: boolean;
  allow_cidrs: string[];
  block_cidrs: string[];
  allow_countries: string[];
  block_countries: string[];
}
export interface MtlsConfig {
  enabled: boolean;
  ca_fingerprint_sha256: string;
  mtls_ca_pem?: string;
}
export interface ServiceAIConfig {
  cache: CacheSettings;
  redaction: RedactionSettings;
  guardrails: GuardrailSettings;
  inspector: InspectorSettings;
  routing: RoutingPolicy;
  ip_geo: IpGeoConfig;
  mtls: MtlsConfig;
}

// Usage + cost (spec Part F).
export interface UsageEvent {
  id: string;
  service_id: string;
  api_key_id: string;
  ts: string;
  kind: "openai" | "anthropic" | "mcp" | "unknown";
  tokens_in: number;
  tokens_out: number;
  bytes_in: number;
  bytes_out: number;
  streamed: boolean;
  cache_hit: boolean;
  upstream_status: number;
}
export interface PricingEntry {
  provider: string;
  model: string;
  input_per_million: number;
  output_per_million: number;
}
export interface PricingTable {
  version: string;
  entries: PricingEntry[];
}
export interface CostSummary {
  window: "today" | "week" | "month" | "year";
  total_usd: number;
  tokens_in: number;
  tokens_out: number;
  top_consumers: { api_key_id: string; service_id: string; tokens_in: number; tokens_out: number; usd: number }[];
  pct_of_budget: number | null;
  /** Present with ?group_by=…; sorted by usd, the "" group last. */
  group_by?: CostGroupBy;
  groups?: CostGroupRow[];
}
export type CostGroupBy = "gateway_key" | "model" | "provider" | "target_model" | "dialect";
export interface CostGroupRow {
  /** gateway_key: the key id; model/provider/target_model/dialect: the name. "" is the remainder or "not attributed". */
  key: string;
  requests: number;
  tokens_in: number;
  tokens_out: number;
  usd: number;
}
export type BudgetScope = "api_key" | "service" | "user" | "global" | "gateway_key" | "model";
export interface Budget {
  id: string;
  scope: BudgetScope;
  subject_id: string;
  daily_usd: number;
  daily_tokens: number;
  action_on_exceed: "alert_webhook" | "throttle_zero" | "disable_key";
  alert_webhook_id: string | null;
  current_usd: number;
  current_tokens: number;
  exceeded: boolean;
}

// Rate limits / quotas (spec Part D).
export interface RateLimit {
  id: string;
  scope: "api_key" | "role" | "service" | "global";
  subject: string;
  dimension: "rpm" | "bpm";
  limit: number;
  burst: number;
  window?: "minute" | "day";
  created_at: string;
}

// Request inspector (spec Part E).
export interface InspectorEntry {
  id: string;
  service_id: string;
  api_key_id: string;
  ts: string;
  method: string;
  path: string;
  status: number;
  duration_ms: number;
  bytes_in: number;
  bytes_out: number;
  req_headers: Record<string, string>;
  req_body: string;
  resp_headers: Record<string, string>;
  resp_body: string;
  truncated: boolean;
  cache: "HIT" | "MISS" | "SKIP";
  redactions: { rule: string; count: number }[];
  trace_id: string;
  remote_ip: string;
  mcp?: { method: string; tool: string; params: unknown };
}

// Audit (spec Part G).
export interface AuditEvent {
  id: string;
  ts: string;
  actor_id: string;
  actor_email: string;
  action: string;
  subject_id: string;
  subject_label: string;
  result: "ok" | "denied" | "error";
  source_ip: string;
  user_agent: string;
  request_id: string;
  payload: Record<string, unknown>;
  prev_hash: string;
  hash: string;
}
export interface AuditFingerprint {
  public_key: string;
  fingerprint: string;
}

// Webhooks (spec Part H).
export interface Webhook {
  id: string;
  name: string;
  url: string;
  events: string[];
  paused: boolean;
  consecutive_failures: number;
  first_failure_at: string | null;
  created_at: string;
}
export interface CreatedWebhook {
  webhook: Webhook;
  signing_secret: string;
}
export interface WebhookDelivery {
  id: string;
  webhook_id: string;
  event: string;
  ts: string;
  url: string;
  status_code: number;
  attempt: 1 | 2 | 3;
  latency_ms: number;
  request_body_preview: string;
  response_body_preview: string;
}

// Custom roles / permissions (spec Part I).
export interface PermissionDef {
  key: string;
  group: string;
  description: string;
}
export interface CustomRoleInput {
  name: string;
  description: string;
  permissions: string[];
  default_for_new_users: boolean;
}

// Automation tokens (spec Part M).
export interface AutomationToken {
  id: string;
  name: string;
  prefix: string;
  user_id: string;
  role_at_mint: string;
  permissions: string[];
  expires_at: string | null;
  last_used: string | null;
  created_at: string;
}
export interface CreatedAutomationToken {
  token: AutomationToken;
  plaintext: string;
}

// Backups (spec Part L).
export interface BackupRow {
  id: string;
  taken_at: string;
  version: string;
  size_bytes: number;
  db_sha256: string;
  path: string;
}

// Synthetic models and gateway keys (GET/POST/PUT/DELETE /ai/models, /ai/keys; GET /ai/gateway).
export type Dialect = "openai" | "anthropic";
/** A target as a request carries it. */
export interface AiModelTarget {
  dialect: Dialect;
  provider: string;
  model: string;
}
/** A target in a response: also whether it can be tried right now. The relay refuses the field in a request. */
export interface AiModelTargetStatus extends AiModelTarget {
  available: boolean;
}
/** The target that answers a format right now. */
export interface AiServingTarget {
  provider: string;
  model: string;
}
export interface AiModel {
  name: string;
  description: string;
  enabled: boolean;
  fallback_on_rate_limit: boolean;
  attempt_timeout_s: number;
  total_timeout_s: number;
  /** By format, then in the order they are tried. */
  targets: AiModelTargetStatus[];
  /** The formats the model is served in, sorted; derived from the targets. */
  dialects: Dialect[];
  /**
   * Per format the model has a target in: the first available target, or null
   * when none is. A format without targets has no key.
   */
  serving: Partial<Record<Dialect, AiServingTarget | null>>;
  created_at: string;
  updated_at: string;
}
/** One attempt of a gateway request (GET /ai/requests/{id}/attempts, admins). */
export interface AiRequestAttempt {
  /** The order the attempts were made in, from 0; not a target's place in the model's list. */
  position: number;
  provider: string;
  model: string;
  /** Upstream HTTP status; 0 when none was received. */
  status: number;
  /** "" for the attempt that answered; else http_<status>, timeout, no_response, busy, breaker_open, … */
  error_code: string;
  duration_ms: number;
  ts: string;
}
export interface AiGatewayKey {
  id: string;
  name: string;
  key_prefix: string;
  user_id: string;
  /** Empty: every model. Else synthetic names, "<provider>/<model>" or "<provider>/*". */
  allowed_models: string[];
  last_used: string | null;
  created_at: string;
  revoked_at: string | null;
}
/** POST /ai/keys: the stored key plus its plaintext, returned this once. */
export interface CreatedAiGatewayKey extends AiGatewayKey {
  key: string;
}
export interface GatewayInfo {
  /** base_url is "" when the relay has no auth domain. */
  endpoints: { dialect: Dialect; base_url: string }[];
}

// Provisioning (§4.28; pulled forward from v0.3.1).
export interface ProvisioningKey {
  id: string;
  name: string;
  prefix: string;
  scope: "single" | "multi";
  expires_at: string | null;
  default_role: string;
  last_used: string | null;
  created_at: string;
}
export interface ProvisioningPending {
  id: string;
  hostname: string;
  os: string;
  arch: string;
  remote_ip: string;
  provisioning_key_id: string;
  first_seen: string;
}

// Redaction rules (spec Part B.7 / Guardrails).
export interface RedactionRule {
  id: string;
  name: string;
  pattern: string;
  action: "mask" | "drop" | "hash";
  scope: "request_body" | "response_body" | "both";
  builtin?: boolean;
}

// ---- v0.5.0: semantic cache, upstream credentials, multi-provider routing, ----
// ---- custom domains, connection logs, retention, webhooks v5, database status ----
// Mirror of dev/superpowers/specs/2026-05-20-v0.5.0-api-contract.md.

export interface SemanticCacheSettings {
  enabled: boolean;
  min_similarity: number;
  embedding_mode: "local" | "none";
  embedding_url: string;
  embedding_model: string;
  fallback_policy: "treat_as_miss" | "return_cached_marked";
  promote_on_miss: boolean;
  max_index_entries: number;
}

// CacheStatsV5 is a superset of v0.4.0 CacheStats.
export interface CacheStatsV5 {
  entries: number;
  on_disk_bytes: number;
  hit_rate_24h: number;
  semantic_entries: number;
  semantic_disk_bytes: number;
  semantic_hit_rate_24h: number;
  semantic_similar_returned_24h: number;
  semantic_promotions_24h: number;
}

// Upstream credential binding (spec Part B — upstream slot management).
export type UpstreamSlot = string;

export type Provider = "ollama" | "vllm" | "openai-compat" | "openai" | "anthropic" | "other";

export interface UpstreamCredentialBinding {
  service_id: string;
  slot: UpstreamSlot;
  header_name: string;
  header_format: string;
  slot_present: boolean;
}

// RoutingPolicyV5 adds "multi_provider" to the strategy union.
export interface RoutingPolicyV5 {
  strategy: "single" | "failover" | "weighted" | "header_based" | "sticky" | "multi_provider";
  model_alias: string;
  header_name: string;
  paused: boolean;
  circuit_breaker: { failure_pct: number; window_seconds: number; cool_down_seconds: number };
  backends: { service_id: string; weight: number; concrete_model: string }[];
}

// Custom domains (spec Part D).
//
// v0.5.2 Task 10: status is the closed enum from the
// internal/proxy/customdomain state machine. `pending` is reserved for
// future ACME / auto-renew flows; the cert-bearing rows transition
// between `active`, `cert_expiring`, and `cert_expired` via the daily tick.
export type CustomDomainStatus =
  | "pending"
  | "active"
  | "cert_expiring"
  | "cert_expired";

export interface CustomDomain {
  id: string;
  service_id: string;
  hostname: string;
  cert_sha256: string;
  not_before: string;
  not_after: string;
  created_at: string;
  updated_at: string;
  status: CustomDomainStatus;
  status_updated_at: string;
}

export interface CreateCustomDomainInput {
  hostname: string;
  cert_pem: string;
  key_pem: string;
}

export interface CustomDomainRejection {
  error: string;
  reason: "san_mismatch" | "chain_invalid" | "key_mismatch" | "expired";
}

// Connection logs (spec Part E).
export type ConnectionLogKind = "control" | "http_proxy" | "tcp_proxy";
export type ConnectionLogStatus = "closed_clean" | "closed_error" | "closed_idle" | "rejected";

export interface ConnectionLog {
  id: string;
  kind: ConnectionLogKind;
  service_id: string;
  tunnel_id: string;
  user_id: string;
  client_session_id: string;
  source_ip: string;
  user_agent: string;
  started_at: string;
  ended_at: string;
  duration_ms: number;
  bytes_in: number;
  bytes_out: number;
  status: ConnectionLogStatus;
  reason: string;
}

// ConnectionLogRollupTopIP — one (ip, sessions) entry from the per-rollup
// top-source-IPs array (v0.5.1 Q12 toggle). Sessions is the per-day
// per-(service_id, kind) hit count attributed to ip.
export interface ConnectionLogRollupTopIP {
  ip: string;
  sessions: number;
}

export interface ConnectionLogRollup {
  day: string;
  service_id: string;
  kind: ConnectionLogKind;
  sessions: number;
  bytes_in: number;
  bytes_out: number;
  avg_duration_ms: number;
  p95_duration_ms: number;
  // top_source_ips: present when the connection_logs.rollup_include_top_ips
  // setting is "true" or unset (default true). Absent when the operator has
  // explicitly disabled the toggle. An empty array means the toggle is ON
  // but no IPs have been aggregated yet for this group.
  top_source_ips?: ConnectionLogRollupTopIP[];
}

// Retention settings (spec Part F).
export interface RetentionSettings {
  audit_retention_days: number;
  usage_retention_days: number;
  redaction_retention_days: number;
  connection_logs_retention_days: number;
  connection_logs_rollups_retention_days: number;
  webhook_deliveries_retention_days: number;
  inspector_retention_count: number;
  audit_retention_note: string;
}

// WebhookV5 — superset of v0.4.0 Webhook + payload_template (spec Part H).
export interface WebhookV5 {
  id: string;
  name: string;
  url: string;
  events: string[];
  paused: boolean;
  consecutive_failures: number;
  first_failure_at: string | null;
  created_at: string;
  payload_template: string;
}

export interface WebhookPreviewResponse {
  rendered: string;
  size_bytes: number;
}

// Database status (spec Part G — Postgres alpha).
export interface DatabaseStatus {
  driver: "sqlite" | "postgres";
  postgres_alpha: boolean;
  url_redacted: string;
}

// WEBHOOK_EVENT_FIELDS: maps v0.5.0 event names to their available field names (spec H.2).
export const WEBHOOK_EVENT_FIELDS: Record<string, string[]> = {
  "ai.upstream_error": ["service_id", "backend_service_id", "status", "error", "retry_count"],
  "ai.cache_promotion": ["service_id", "exact_key_hash", "prompt_fingerprint", "similarity_to_first"],
  "audit.policy_change": ["actor_email", "action", "before", "after"],
  "service.created": ["service_id", "name", "type", "access_mode"],
  "service.deleted": ["service_id", "name"],
  "connection.session_summary": ["service_id", "kind", "window_start", "window_end", "sessions", "bytes_in", "bytes_out", "avg_duration_ms", "p95_duration_ms", "top_source_ips"],
};
