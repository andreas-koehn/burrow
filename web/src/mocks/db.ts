import type {
  UserAdmin, RoleSummary, Session, ClientDetail, SettingsMap,
  Service, ServiceApiKey, CostSummary, ServiceAIConfig,
  InspectorEntry, CacheSettings, RedactionRule, RedactionSettings,
  GuardrailPattern, GuardrailSettingsResponse, Budget, PricingTable, AuditEvent, Webhook,
  WebhookDelivery, ProvisioningKey, ProvisioningPending,
  AutomationToken, BackupRow, CacheStatsV5, SemanticCacheSettings,
  ModelAliasV5, UpstreamCredentialBinding, CustomDomain,
  RetentionSettings, DatabaseStatus,
  ConnectionLog, ConnectionLogRollup,
} from "@/lib/contract";

export interface CacheSettingsPayload {
  global: CacheSettings;
  per_service: Record<string, CacheSettings>;
  // v0.5.0 additive — top-level global semantic block.
  semantic?: SemanticCacheSettings;
}

// Internal service row: the wire Service plus the owning user_id (stripped
// before serialization — owner-scoping the v0.3.0 /services surface).
export interface ServiceRow extends Service {
  user_id: string;
}

// Per-service AI-endpoint metadata seeded for the v0.4.0 /ai/endpoints lens.
// Backend derives these from runtime metrics; the mock seeds plausible values.
export interface AiMetaRow {
  backend_type: "ollama" | "vllm" | "openai-compat" | "other";
  requests_24h: number;
  cache_hits_24h: number;
  latency_p95_ms: number;
  status: "Connected" | "Degraded" | "Offline";
  client_session_id: string;
}

export interface MockDb {
  me: { id: string; email: string; role: "admin" | "user" };
  csrf: string;
  connectServer: string;
  users: UserAdmin[];
  roles: RoleSummary[];
  rolePerms: Record<string, string[]>;
  sessions: Session[];
  settings: SettingsMap;
  smtpPasswordSet: boolean;
  clients: ClientDetail[];
  tokens: { id: string; name: string; last_used: string | null; created_at: string }[];
  services: ServiceRow[];
  serviceApiKeys: Record<string, ServiceApiKey[]>;
  serviceAccessPolicy: Record<string, string[]>;
  aiMeta: Record<string, AiMetaRow>;
  modelAliases: ModelAliasV5[];
  costSummary: Record<"today" | "week" | "month" | "year", CostSummary>;
  aiConfigs: Record<string, ServiceAIConfig>;
  inspectorEntries: Record<string, InspectorEntry[]>;
  cacheSettings: CacheSettingsPayload;
  cacheStats: CacheStatsV5;
  redactionRules: { built_in: RedactionRule[]; custom: RedactionRule[] };
  redactionSettings: RedactionSettings;
  guardrailSettings: GuardrailSettingsResponse;
  guardrailPatterns: GuardrailPattern[];
  budgets: Budget[];
  pricing: PricingTable;
  audit: AuditEvent[];
  webhooks: Webhook[];
  webhookDeliveries: WebhookDelivery[];
  provisioningKeys: ProvisioningKey[];
  provisioningPending: ProvisioningPending[];
  automationTokens: AutomationToken[];
  backups: BackupRow[];
  // v0.5.0 upstream credentials (spec Part B).
  upstreamSlots: string[];
  absentSlots: Set<string>;
  upstreamBindings: Record<string, UpstreamCredentialBinding>;
  // v0.5.0 custom domains (spec Part D).
  customDomains: CustomDomain[];
  // v0.5.0 retention settings (spec Part F).
  retentionSettings: RetentionSettings;
  // v0.5.0 database status (spec Part G).
  databaseStatus: DatabaseStatus;
  // v0.5.0 connection logs (spec Part E).
  connectionLogs: ConnectionLog[];
  connectionLogRollups: ConnectionLogRollup[];
}

function seed(): MockDb {
  const meId = "bur_usr_admin01";
  return {
    me: { id: meId, email: "alice@acme.io", role: "admin" },
    csrf: "test-csrf-token",
    connectServer: "relay.example.com:7000",
    users: [
      { id: meId, email: "alice@acme.io", role: "admin", status: "active", last_login: "2026-05-18T09:00:00Z", created_at: "2026-01-12T08:00:00Z" },
      { id: "bur_usr_bob0002", email: "bob@acme.io", role: "user", status: "active", last_login: null, created_at: "2026-02-01T08:00:00Z" },
      { id: "bur_usr_carol03", email: "carol@acme.io", role: "user", status: "suspended", last_login: "2026-04-10T12:00:00Z", created_at: "2026-03-01T08:00:00Z" },
    ],
    roles: [
      { name: "admin", description: "Full access — manage tunnels, tokens, users and roles.", created_at: "2026-01-01T00:00:00Z", builtin: true },
      { name: "user", description: "Use own tunnels and tokens; manage own account.", created_at: "2026-01-01T00:00:00Z", builtin: true },
      { name: "analyst", description: "Read-only access to traffic, cost, and audit.", created_at: "2026-03-01T00:00:00Z", builtin: false },
    ],
    rolePerms: {
      admin: ["tunnels:read:any", "tunnels:manage:any", "tokens:manage:any", "services:configure:any", "sessions:manage:any", "users:read", "users:manage", "roles:read", "settings:manage"],
      user: ["tunnels:read:own", "tunnels:manage:own", "tokens:manage:own", "services:configure:own", "sessions:manage:own"],
      analyst: ["tunnels:read:any", "audit:read", "cost:read"],
    },
    sessions: [
      { id: "sess_cur", ip: "203.0.113.7", user_agent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36", created_at: "2026-05-18T09:00:00Z", expires_at: "2026-05-25T09:00:00Z", current: true },
      { id: "sess_old", ip: "198.51.100.4", user_agent: "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0", created_at: "2026-05-10T09:00:00Z", expires_at: "2026-05-17T09:00:00Z", current: false },
    ],
    settings: {},
    smtpPasswordSet: false,
    clients: [
      {
        session_id: "sess_4f7a9c0b2e81", user_id: meId, token_name: "office-box-1",
        remote_addr: "203.0.113.7:51234", os: "linux", arch: "amd64", client_version: "0.2.0",
        service_count: 1, total_bytes_in: 10240, total_bytes_out: 4096,
        services: [
          { id: "tnl_web01", name: "web-staging", type: "tcp", remote_port: 9000, local_addr: "127.0.0.1:3000", access_mode: "open", bytes_in: 2048, bytes_out: 1024, total_bytes_in: 10240, total_bytes_out: 4096 },
        ],
      },
    ],
    tokens: [
      { id: "tok_1", name: "office-box-1", last_used: "2026-05-18T09:00:00Z", created_at: "2026-05-01T08:00:00Z" },
    ],
    services: [
      { id: "svc_web01", user_id: meId, name: "web", type: "http", subdomain: "k7p2qx", hostname: "k7p2qx.tunnels.example.com", access_mode: "open", api_key_header: "Authorization", connected: true, remote_port: 0, local_addr: "127.0.0.1:3000" },
      { id: "svc_ai001", user_id: meId, name: "ollama", type: "http", subdomain: "ai4m2q", hostname: "ai4m2q.tunnels.example.com", access_mode: "api_key", api_key_header: "Authorization", connected: true, remote_port: 0, local_addr: "127.0.0.1:11434" },
      { id: "svc_graf01", user_id: meId, name: "grafana", type: "http", subdomain: "gf7x1p", hostname: "gf7x1p.tunnels.example.com", access_mode: "burrow_login", api_key_header: "Authorization", connected: false, remote_port: 0, local_addr: "127.0.0.1:3001" },
      { id: "svc_pg001", user_id: meId, name: "postgres", type: "tcp", subdomain: "", hostname: "", access_mode: "open", api_key_header: "Authorization", connected: true, remote_port: 9000, local_addr: "127.0.0.1:5432" },
    ],
    serviceApiKeys: {
      svc_ai001: [
        { id: "sak_ci01", name: "ci", last_used: null, created_at: "2026-05-10T08:00:00Z" },
        { id: "sak_prod1", name: "prod", last_used: "2026-05-18T09:00:00Z", created_at: "2026-05-01T08:00:00Z" },
      ],
    },
    serviceAccessPolicy: {
      svc_graf01: ["user"],
    },
    aiMeta: {
      svc_ai001: {
        backend_type: "ollama",
        requests_24h: 1024,
        cache_hits_24h: 200,
        latency_p95_ms: 1200,
        status: "Connected",
        client_session_id: "sess_4f7a9c0b2e81",
      },
    },
    modelAliases: [
      { alias: "fast", concrete_model: "llama3.1:8b", service_id: "svc_ai001", provider: "ollama", priority: 100, created_at: "2026-05-19T00:00:00Z" },
    ],
    costSummary: {
      today: { window: "today", total_usd: 1.23, tokens_in: 12000, tokens_out: 8000, top_consumers: [], pct_of_budget: 0.5 },
      week:  { window: "week",  total_usd: 8.77, tokens_in: 80000, tokens_out: 55000, top_consumers: [], pct_of_budget: 0.4 },
      month: { window: "month", total_usd: 32.50, tokens_in: 320000, tokens_out: 215000, top_consumers: [], pct_of_budget: 0.7 },
      year:  { window: "year",  total_usd: 390.00, tokens_in: 3800000, tokens_out: 2550000, top_consumers: [], pct_of_budget: 0.6 },
    },
    aiConfigs: {
      svc_ai001: defaultAiConfig(),
    },
    inspectorEntries: {
      svc_ai001: seedInspector("svc_ai001"),
    },
    cacheSettings: {
      global: { enabled: true, applies_per: "per_endpoint", ttl_seconds: 600, max_entries: 1000, max_per_entry_kb: 64 },
      per_service: {},
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
    cacheStats: {
      entries: 47, on_disk_bytes: 3145728, hit_rate_24h: 0.31,
      semantic_entries: 12, semantic_disk_bytes: 524288,
      semantic_hit_rate_24h: 0.04,
      semantic_similar_returned_24h: 7, semantic_promotions_24h: 3,
    },
    redactionRules: {
      built_in: [
        { id: "email", name: "Email address", pattern: "[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\\.[A-Za-z]{2,}", action: "mask", scope: "both", builtin: true },
        { id: "phone", name: "E.164 phone", pattern: "\\+?[0-9 ()-]{7,}", action: "mask", scope: "both", builtin: true },
        { id: "ssn", name: "US SSN", pattern: "\\d{3}-\\d{2}-\\d{4}", action: "mask", scope: "both", builtin: true },
      ],
      custom: [],
    },
    redactionSettings: { enabled: true, redact_for_logs_only: false, rule_ids: ["email"], presidio_enabled: false, presidio_url: "http://localhost:5000" },
    guardrailSettings: { global: { enabled: false, action: "log_only" }, per_service: [] },
    guardrailPatterns: [
      { id: "ignore_prev", description: "ignore previous instructions" },
      { id: "you_are_now", description: "you are now" },
      { id: "system_prompt", description: "system prompt" },
      { id: "jailbreak", description: "jailbreak" },
      { id: "dev_mode", description: "developer mode" },
    ],
    budgets: [
      { id: "bdg_ci", scope: "api_key", subject_id: "sak_ci01", daily_usd: 10, action_on_exceed: "alert_webhook", alert_webhook_id: null, current_usd: 5, exceeded: false },
    ],
    pricing: {
      version: "v0.4.0",
      entries: [
        { provider: "openai", model: "gpt-4o", input_per_million: 5, output_per_million: 15 },
        { provider: "anthropic", model: "claude-sonnet-4-6", input_per_million: 3, output_per_million: 15 },
        { provider: "ollama", model: "llama3.1:8b", input_per_million: 0, output_per_million: 0 },
      ],
    },
    audit: [
      { id: "ae_001", ts: "2026-05-19T08:00:00Z", actor_id: meId, actor_email: "alice@acme.io", action: "services.create", subject_id: "svc_ai001", subject_label: "ollama", result: "ok", source_ip: "203.0.113.7", user_agent: "Mozilla/5.0", request_id: "req_001", payload: { name: "ollama" }, prev_hash: "0000000000000000000000000000000000000000000000000000000000000000", hash: "11111111111111111111111111111111111111111111111111111111111111111111" },
      { id: "ae_002", ts: "2026-05-19T08:01:00Z", actor_id: meId, actor_email: "alice@acme.io", action: "services.update", subject_id: "svc_ai001", subject_label: "ollama", result: "ok", source_ip: "203.0.113.7", user_agent: "Mozilla/5.0", request_id: "req_002", payload: { access_mode: "api_key" }, prev_hash: "11", hash: "22" },
      { id: "ae_003", ts: "2026-05-19T08:02:00Z", actor_id: meId, actor_email: "alice@acme.io", action: "tokens.create", subject_id: "tok_1", subject_label: "office-box-1", result: "ok", source_ip: "203.0.113.7", user_agent: "Mozilla/5.0", request_id: "req_003", payload: { name: "office-box-1" }, prev_hash: "22", hash: "33" },
    ],
    webhooks: [
      { id: "wh_ops", name: "ops-pager", url: "https://example.com/hook", events: ["audit.tokens.create"], paused: false, consecutive_failures: 3, first_failure_at: "2026-05-19T07:00:00Z", created_at: "2026-05-01T00:00:00Z", payload_template: "" } as unknown as Webhook,
    ],
    webhookDeliveries: [
      { id: "wd_001", webhook_id: "wh_ops", event: "audit.tokens.create", ts: "2026-05-19T07:00:00Z", url: "https://example.com/hook", status_code: 502, attempt: 1, latency_ms: 100, request_body_preview: "{...}", response_body_preview: "gateway" },
      { id: "wd_002", webhook_id: "wh_ops", event: "audit.tokens.create", ts: "2026-05-19T07:00:05Z", url: "https://example.com/hook", status_code: 502, attempt: 2, latency_ms: 110, request_body_preview: "{...}", response_body_preview: "gateway" },
    ],
    provisioningKeys: [
      { id: "pk_fleet", name: "fleet", prefix: "bup_w7s9z", scope: "multi", expires_at: null, default_role: "user", last_used: "2026-05-15T09:00:00Z", created_at: "2026-04-01T08:00:00Z" },
    ],
    provisioningPending: [
      { id: "pp_node1", hostname: "node-1.lan", os: "linux", arch: "amd64", remote_ip: "10.0.1.7", provisioning_key_id: "pk_fleet", first_seen: "2026-05-19T07:30:00Z" },
    ],
    automationTokens: [
      { id: "at_ci01", name: "ci-runner", prefix: "bua_x9k", user_id: meId, role_at_mint: "user", permissions: ["tunnels:read:any", "audit:read"], expires_at: null, last_used: "2026-05-18T09:00:00Z", created_at: "2026-04-01T00:00:00Z" },
    ],
    backups: [
      { id: "bk_20260515", taken_at: "2026-05-15T03:00:00Z", version: "v0.3.0", size_bytes: 5 * 1024 * 1024, db_sha256: "deadbeef1234567890abcdef1234567890abcdef1234567890abcdef1234beef", path: "/var/burrow/backups/bk_20260515.tar.gz" },
    ],
    // v0.5.0 upstream credentials (spec Part B).
    // Slots sorted alphabetically; ANTHROPIC_TEAM_A is "absent" by default in tests that need it.
    upstreamSlots: ["ANTHROPIC_TEAM_A", "OPENAI"],
    absentSlots: new Set<string>(),
    upstreamBindings: {
      svc_ai001: {
        service_id: "svc_ai001",
        slot: "OPENAI",
        header_name: "Authorization",
        header_format: "Bearer {key}",
        slot_present: true,
      },
    },
    // v0.5.0 custom domains (spec Part D). v0.5.2 Task 10 added
    // status_updated_at and the four-state enum.
    customDomains: [
      {
        id: "dom_001",
        service_id: "svc_ai001",
        hostname: "foo.example.com",
        cert_sha256: "deadbeef0123456789abcdef",
        not_before: "2026-05-01T00:00:00Z",
        not_after: new Date(Date.now() + 90 * 24 * 60 * 60 * 1000).toISOString(),
        created_at: "2026-05-19T00:00:00Z",
        updated_at: "2026-05-19T00:00:00Z",
        status: "active",
        status_updated_at: "2026-05-19T00:00:00Z",
      },
    ],
    // v0.5.0 retention settings (spec Part F).
    retentionSettings: {
      audit_retention_days: 0,
      usage_retention_days: 90,
      redaction_retention_days: 30,
      connection_logs_retention_days: 30,
      connection_logs_rollups_retention_days: 0,
      webhook_deliveries_retention_days: 30,
      inspector_retention_count: 100,
      audit_retention_note: "Audit retention only deletes the six rate-limited leaf event types — see docs.",
    },
    // v0.5.0 database status (spec Part G).
    databaseStatus: {
      driver: "sqlite",
      postgres_alpha: false,
      url_redacted: "",
    },
    // v0.5.0 connection logs (spec Part E).
    connectionLogs: seedConnectionLogs(),
    connectionLogRollups: seedConnectionLogRollups(),
  };
}

// Default-filled ServiceAIConfig — every section disabled, sensible defaults.
function defaultAiConfig(): ServiceAIConfig {
  return {
    cache: {
      enabled: false, applies_per: "per_endpoint", ttl_seconds: 600, max_entries: 1000, max_per_entry_kb: 64,
      // v0.5.0 additive — present only when the build tag is on.
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
      model_alias: "fast",
      header_name: "X-Burrow-Model",
      paused: false,
      circuit_breaker: { failure_pct: 50, window_seconds: 30, cool_down_seconds: 60 },
      backends: [],
      translate_to: "none",
    },
    ip_geo: { enabled: false, allow_cidrs: [], block_cidrs: [], allow_countries: [], block_countries: [] },
    mtls: { enabled: false, ca_fingerprint_sha256: "" },
  };
}

// Twelve inspector entries — enough to test the "10 newest" Recent Requests
// table on the AI endpoint detail page.
function seedInspector(serviceId: string): InspectorEntry[] {
  const out: InspectorEntry[] = [];
  for (let i = 0; i < 12; i++) {
    out.push({
      id: `ie_${serviceId}_${String(i).padStart(3, "0")}`,
      service_id: serviceId,
      api_key_id: i % 2 === 0 ? "sak_ci01" : "sak_prod1",
      ts: new Date(Date.parse("2026-05-19T12:00:00Z") - i * 60_000).toISOString(),
      method: "POST",
      path: "/v1/chat/completions",
      status: 200,
      duration_ms: 100 + i * 20,
      bytes_in: 256,
      bytes_out: 1024,
      req_headers: { "content-type": "application/json", authorization: "Bearer ••• [redacted]" },
      req_body: `{"model":"llama3.1:8b","messages":[{"role":"user","content":"hi #${i}"}]}`,
      resp_headers: { "content-type": "application/json" },
      resp_body: `{"id":"chatcmpl-${i}","choices":[{"message":{"content":"hello #${i}"}}]}`,
      truncated: false,
      cache: i % 5 === 0 ? "HIT" : "MISS",
      redactions: [],
      trace_id: `tr_${i}`,
      remote_ip: "203.0.113.7",
    });
  }
  return out;
}

// 25 connection log rows — 10 http_proxy, 10 control, 5 tcp_proxy.
// Anchor at "now" so the default 24h date-range preset always captures the
// full range of seeded kinds (otherwise rows drift out of the window over time).
function seedConnectionLogs(): ConnectionLog[] {
  const base = Date.now();
  const kinds: ConnectionLog["kind"][] = [
    "http_proxy", "http_proxy", "http_proxy", "http_proxy", "http_proxy",
    "http_proxy", "http_proxy", "http_proxy", "http_proxy", "http_proxy",
    "control", "control", "control", "control", "control",
    "control", "control", "control", "control", "control",
    "tcp_proxy", "tcp_proxy", "tcp_proxy", "tcp_proxy", "tcp_proxy",
  ];
  const statuses: ConnectionLog["status"][] = [
    "closed_clean", "closed_clean", "closed_clean", "closed_error", "closed_idle",
    "closed_clean", "closed_clean", "rejected", "closed_clean", "closed_error",
    "closed_clean", "closed_clean", "closed_clean", "closed_clean", "closed_clean",
    "closed_clean", "closed_clean", "closed_clean", "closed_error", "closed_idle",
    "closed_clean", "closed_clean", "closed_clean", "closed_error", "closed_clean",
  ];
  const ips = [
    "203.0.113.7", "198.51.100.4", "192.0.2.55", "10.0.0.1", "172.16.0.22",
    "203.0.113.42", "198.51.100.9", "192.0.2.100", "10.0.1.7", "172.16.1.5",
    "203.0.113.7", "198.51.100.4", "192.0.2.55", "10.0.0.1", "172.16.0.22",
    "203.0.113.42", "198.51.100.9", "192.0.2.100", "10.0.1.7", "172.16.1.5",
    "203.0.113.7", "198.51.100.4", "192.0.2.55", "10.0.0.1", "172.16.0.22",
  ];
  const svcIds = [
    "svc_web01", "svc_ai001", "svc_graf01", "svc_pg001", "svc_web01",
    "svc_ai001", "svc_graf01", "svc_pg001", "svc_web01", "svc_ai001",
    "svc_web01", "svc_ai001", "svc_graf01", "svc_pg001", "svc_web01",
    "svc_ai001", "svc_graf01", "svc_pg001", "svc_web01", "svc_ai001",
    "svc_pg001", "svc_web01", "svc_ai001", "svc_pg001", "svc_web01",
  ];
  const logs: ConnectionLog[] = [];
  for (let i = 0; i < 25; i++) {
    const startedMs = base - i * 3600_000; // spread over 24h
    const durMs = 500 + i * 200;
    const endedMs = startedMs + durMs;
    logs.push({
      id: `cl_${String(i + 1).padStart(3, "0")}`,
      kind: kinds[i]!,
      service_id: svcIds[i]!,
      tunnel_id: `tnl_${String(i + 1).padStart(3, "0")}`,
      user_id: "bur_usr_admin01",
      client_session_id: `sess_${String(i + 1).padStart(3, "0")}`,
      source_ip: ips[i]!,
      user_agent: "burrow-client/0.5.0",
      started_at: new Date(startedMs).toISOString(),
      ended_at: new Date(endedMs).toISOString(),
      duration_ms: durMs,
      bytes_in: 1024 * (i + 1),
      bytes_out: 512 * (i + 1),
      status: statuses[i]!,
      reason: "",
    });
  }
  return logs;
}

// 3 rollup rows — one per day for the last 3 days, each a different service+kind.
// Days computed relative to "today" so the default 24h preset always includes
// the most recent rollup row.
//
// v0.5.1 Q12: the first two rows carry a top_source_ips field (toggle ON),
// the third row leaves it undefined (toggle OFF for that group). This mirrors
// the real backend's per-(day, service_id, kind) emission policy and lets the
// component test assert both "field present" and "field omitted" branches.
function seedConnectionLogRollups(): ConnectionLogRollup[] {
  const dayOffset = (d: number): string =>
    new Date(Date.now() - d * 86_400_000).toISOString().slice(0, 10);
  const days = [dayOffset(2), dayOffset(1), dayOffset(0)];
  const kinds: ConnectionLogRollup["kind"][] = ["http_proxy", "control", "tcp_proxy"];
  const svcIds = ["svc_web01", "svc_ai001", "svc_pg001"];
  // Layout: index 0 = 2 days ago (toggle OFF, undefined); index 1 = 1 day ago
  // (toggle ON, has data); index 2 = today (toggle ON, has data). The test's
  // default 24h date filter selects rows from ~yesterday onward, so index 1
  // and index 2 are visible and both carry top_source_ips data.
  const topIPs: (ConnectionLogRollup["top_source_ips"])[] = [
    undefined,
    [
      { ip: "192.168.1.1", sessions: 9 },
    ],
    [
      { ip: "10.0.0.1", sessions: 42 },
      { ip: "10.0.0.2", sessions: 17 },
    ],
  ];
  return days.map((day, i) => ({
    day,
    service_id: svcIds[i]!,
    kind: kinds[i]!,
    sessions: 100 + i * 50,
    bytes_in: 1_000_000 * (i + 1),
    bytes_out: 500_000 * (i + 1),
    avg_duration_ms: 300 + i * 100,
    p95_duration_ms: 900 + i * 200,
    top_source_ips: topIPs[i],
  }));
}

export let db: MockDb = seed();

export function resetDb(): void {
  db = seed();
}
