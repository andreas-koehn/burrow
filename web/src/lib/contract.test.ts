import { describe, it, expect, expectTypeOf } from "vitest";
import type {
  UserAdmin, UsersPage, RoleSummary, RoleDetail, Session,
  ClientView, ClientDetail, ServiceView, SettingsMap, AccessMode,
  Service, ServiceDetail, ServiceApiKey, AccessPolicy, CreatedApiKey,
  AiProvider, ServiceAIConfig, UsageEvent, RateLimit, Budget, PricingTable,
  PricingEntry, InspectorEntry, AuditEvent, AuditFingerprint, Webhook,
  CreatedWebhook, WebhookDelivery, AutomationToken, CreatedAutomationToken,
  BackupRow, MtlsConfig, IpGeoConfig,
  CustomRoleInput, PermissionDef, CacheSettings, RedactionRule,
  RedactionSettings, GuardrailSettings, InspectorSettings, RoutingPolicy,
  ProvisioningKey, ProvisioningPending, CostSummary,
  SemanticCacheSettings, CacheStatsV5, UpstreamSlot, UpstreamCredentialBinding,
  RoutingPolicyV5, CustomDomain, CreateCustomDomainInput,
  ConnectionLog, ConnectionLogRollup, RetentionSettings,
  WebhookV5, WebhookPreviewResponse, DatabaseStatus,
} from "@/lib/contract";
import { ACCESS_MODES, isAccessMode, WEBHOOK_EVENT_FIELDS } from "@/lib/contract";

describe("contract", () => {
  it("exposes the access-mode enum and guard", () => {
    expect(ACCESS_MODES).toEqual(["open", "api_key", "burrow_login", "mtls"]);
    expect(isAccessMode("open")).toBe(true);
    expect(isAccessMode("mtls")).toBe(true);
    expect(isAccessMode("nope")).toBe(false);
  });

  it("types compile against representative wire objects", () => {
    const u: UserAdmin = { id: "u1", email: "a@b.io", role: "admin", status: "active", last_login: null, created_at: "2026-01-01T00:00:00Z" };
    const page: UsersPage = { users: [u], total: 1 };
    const rs: RoleSummary = { name: "admin", description: "", created_at: "2026-01-01T00:00:00Z" };
    const rd: RoleDetail = { ...rs, permissions: ["tunnels:read:any"] };
    const s: Session = { id: "s1", ip: "1.2.3.4", user_agent: "UA", created_at: "x", expires_at: "y", current: true };
    const sv: ServiceView = { id: "t1", name: "web", type: "tcp", remote_port: 9000, local_addr: "127.0.0.1:3000", access_mode: "open", bytes_in: 0, bytes_out: 0, total_bytes_in: 0, total_bytes_out: 0 };
    const cv: ClientView = { session_id: "sess1", user_id: "u1", token_name: "tok", remote_addr: "1.2.3.4:5", os: "linux", arch: "amd64", client_version: "0.2.0", service_count: 1, total_bytes_in: 0, total_bytes_out: 0 };
    const cd: ClientDetail = { ...cv, services: [sv] };
    const sm: SettingsMap = { "smtp.host": "mx" };
    const am: AccessMode = "open";
    expect([u, page, rs, rd, s, cv, cd, sm, am]).toHaveLength(9);
  });

  it("Service shape matches v0.3.0 contract Part E", () => {
    const s: Service = {
      id: "s1", name: "web", type: "http", slug: "k7p2qx",
      url: "https://tunnels.example.com/svc/k7p2qx/", access_mode: "open",
      api_key_header: "Authorization", gateway_only: false, connected: true,
      remote_port: 0, local_addr: "127.0.0.1:3000",
    };
    expectTypeOf(s.access_mode).toEqualTypeOf<"open" | "api_key" | "burrow_login" | "mtls">();
    const d: ServiceDetail = { ...s, api_key_count: 2, access_policy: ["user"] };
    const k: ServiceApiKey = { id: "k1", name: "ci", last_used: null, created_at: "2026-05-19T00:00:00Z" };
    const p: AccessPolicy = { roles: ["user"] };
    const c: CreatedApiKey = { id: "k1", name: "ci", key: "buk_mock_abc" };
    expect([s, d, k, p, c]).toHaveLength(5);
  });

  it("v0.4.0 shapes match contract Parts B–M", () => {
    const ai: AiProvider = {
      slug: "web", name: "web", kind: "tunnel", api_format: "openai",
      service_id: "svc_web01", base_url: "https://b.example.com/ai/web/v1", model_alias: "fast",
      upstream_base_url: "", credential_slot: "", credential_present: false, credential_slots: [], breaker_open: false, billing: "metered", supports_responses: false, max_concurrent: 0, in_use: 0, model_count: 0,
      concrete_model: "llama3.1:8b", backend_type: "ollama",
      api_key_count: 2, requests_24h: 1024, cache_hits_24h: 200,
      latency_p95_ms: 1200, status: "Connected", client_session_id: "sess_abc",
    };
    const cache: CacheSettings = { enabled: false, applies_per: "per_endpoint", ttl_seconds: 600, max_entries: 1000, max_per_entry_kb: 64 };
    const red: RedactionSettings = { enabled: false, redact_for_logs_only: false, rule_ids: [], presidio_enabled: false };
    const gr: GuardrailSettings = { enabled: false, action: "log_only" };
    const insp: InspectorSettings = { enabled: false, max_requests: 100 };
    const routing: RoutingPolicy = {
      strategy: "single", model_alias: "", header_name: "X-Burrow-Model", paused: false,
      circuit_breaker: { failure_pct: 50, window_seconds: 30, cool_down_seconds: 60 },
      backends: [], translate_to: "none",
    };
    const ipgeo: IpGeoConfig = { enabled: false, allow_cidrs: [], block_cidrs: [], allow_countries: [], block_countries: [] };
    const mtls: MtlsConfig = { enabled: false, ca_fingerprint_sha256: "" };
    const cfg: ServiceAIConfig = { cache, redaction: red, guardrails: gr, inspector: insp, routing, ip_geo: ipgeo, mtls };

    const usage: UsageEvent = { id: "u1", service_id: "svc_ai001", api_key_id: "sak_ci01", ts: "2026-05-19T00:00:00Z", kind: "openai", tokens_in: 10, tokens_out: 20, bytes_in: 100, bytes_out: 200, streamed: false, cache_hit: false, upstream_status: 200 };
    const pe: PricingEntry = { provider: "openai", model: "gpt-4o", input_per_million: 5, output_per_million: 15 };
    const pt: PricingTable = { version: "v0.4.0", entries: [pe] };
    const cs: CostSummary = { window: "today", total_usd: 1.23, tokens_in: 1000, tokens_out: 500, top_consumers: [], pct_of_budget: 0.5 };
    const budget: Budget = { id: "b1", scope: "api_key", subject_id: "sak_ci01", daily_usd: 10, action_on_exceed: "alert_webhook", alert_webhook_id: null, current_usd: 5, exceeded: false };
    const rl: RateLimit = { id: "rl1", scope: "api_key", subject: "sak_ci01", dimension: "rpm", limit: 60, burst: 10, created_at: "2026-05-19T00:00:00Z" };
    const ie: InspectorEntry = {
      id: "ie1", service_id: "svc_ai001", api_key_id: "sak_ci01", ts: "2026-05-19T00:00:00Z",
      method: "POST", path: "/v1/chat/completions", status: 200, duration_ms: 120,
      bytes_in: 100, bytes_out: 200, req_headers: { authorization: "Bearer •••" },
      req_body: "{}", resp_headers: {}, resp_body: "{}", truncated: false, cache: "MISS",
      redactions: [], trace_id: "tr1", remote_ip: "1.2.3.4",
    };
    const audit: AuditEvent = { id: "ae1", ts: "2026-05-19T00:00:00Z", actor_id: "u1", actor_email: "a@b.io", action: "tokens.create", subject_id: "tok_1", subject_label: "tok_1", result: "ok", source_ip: "1.2.3.4", user_agent: "UA", request_id: "req1", payload: {}, prev_hash: "00", hash: "ff" };
    const fp: AuditFingerprint = { public_key: "MIIB...", fingerprint: "SHA256:abcd" };
    const wh: Webhook = { id: "w1", name: "hook", url: "https://example.com/hook", events: ["audit.tokens.create"], paused: false, consecutive_failures: 0, first_failure_at: null, created_at: "2026-05-19T00:00:00Z" };
    const cwh: CreatedWebhook = { webhook: wh, signing_secret: "whsec_•••" };
    const wd: WebhookDelivery = { id: "wd1", webhook_id: "w1", event: "audit.tokens.create", ts: "2026-05-19T00:00:00Z", url: wh.url, status_code: 200, attempt: 1, latency_ms: 50, request_body_preview: "", response_body_preview: "" };
    const pd: PermissionDef = { key: "tunnels:read:any", group: "tunnels", description: "Read all tunnels" };
    const cri: CustomRoleInput = { name: "analyst", description: "", permissions: ["tunnels:read:any"], default_for_new_users: false };
    const at: AutomationToken = { id: "at1", name: "ci", prefix: "bua_", user_id: "u1", role_at_mint: "admin", permissions: [], expires_at: null, last_used: null, created_at: "2026-05-19T00:00:00Z" };
    const cat: CreatedAutomationToken = { token: at, plaintext: "bua_mock_abc" };
    const br: BackupRow = { id: "bk1", taken_at: "2026-05-19T00:00:00Z", version: "v0.4.0", size_bytes: 1024, db_sha256: "deadbeef", path: "/var/burrow/backups/bk1.tar.gz" };
    const rr: RedactionRule = { id: "rr1", name: "email", pattern: "[a-z]+@[a-z]+", action: "mask", scope: "both" };
    const pk: ProvisioningKey = { id: "pk1", name: "fleet", prefix: "bup_", scope: "multi", expires_at: null, default_role: "user", last_used: null, created_at: "2026-05-19T00:00:00Z" };
    const pp: ProvisioningPending = { id: "pp1", hostname: "node-1", os: "linux", arch: "amd64", remote_ip: "1.2.3.4", provisioning_key_id: "pk1", first_seen: "2026-05-19T00:00:00Z" };

    expect([ai, cfg, usage, pt, cs, budget, rl, ie, audit, fp, wh, cwh, wd, pd, cri, at, cat, br, rr, pk, pp]).toHaveLength(21);
  });

  it("v0.5.0 shapes compile against representative wire objects", () => {
    const scs: SemanticCacheSettings = {
      enabled: false, min_similarity: 0.85,
      embedding_mode: "local",
      embedding_url: "http://localhost:11434/v1/embeddings",
      embedding_model: "nomic-embed-text",
      fallback_policy: "treat_as_miss",
      promote_on_miss: true, max_index_entries: 10000,
    };
    const stats: CacheStatsV5 = {
      entries: 47, on_disk_bytes: 3145728, hit_rate_24h: 0.31,
      semantic_entries: 12, semantic_disk_bytes: 524288,
      semantic_hit_rate_24h: 0.04,
      semantic_similar_returned_24h: 7,
      semantic_promotions_24h: 3,
    };
    const slot: UpstreamSlot = "OPENAI";
    const binding: UpstreamCredentialBinding = {
      service_id: "svc_ai001",
      slot: "OPENAI",
      header_name: "Authorization",
      header_format: "Bearer {key}",
      slot_present: true,
    };
    const routing5: RoutingPolicyV5 = {
      strategy: "multi_provider",
      model_alias: "fast", header_name: "X-Burrow-Model",
      paused: false,
      circuit_breaker: { failure_pct: 50, window_seconds: 30, cool_down_seconds: 60 },
      backends: [{ service_id: "svc_ai001", weight: 100, concrete_model: "llama3.1:8b" }],
      translate_to: "none",
    };
    const domain: CustomDomain = {
      id: "dom_001", service_id: "svc_ai001", hostname: "foo.example.com",
      cert_sha256: "deadbeef", not_before: "2026-05-01T00:00:00Z",
      not_after: "2027-05-01T00:00:00Z", created_at: "2026-05-19T00:00:00Z",
      updated_at: "2026-05-19T00:00:00Z", status: "active",
      status_updated_at: "2026-05-19T00:00:00Z",
    };
    const createDomain: CreateCustomDomainInput = {
      hostname: "foo.example.com",
      cert_pem: "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----",
      key_pem: "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----",
    };
    const log: ConnectionLog = {
      id: "cl_001", kind: "http_proxy", service_id: "svc_ai001",
      tunnel_id: "", user_id: "", client_session_id: "",
      source_ip: "203.0.113.7", user_agent: "Mozilla/5.0",
      started_at: "2026-05-19T12:00:00Z", ended_at: "2026-05-19T12:01:30Z",
      duration_ms: 90000, bytes_in: 1024, bytes_out: 4096,
      status: "closed_clean", reason: "",
    };
    const rollup: ConnectionLogRollup = {
      day: "2026-05-19",
      service_id: "svc_ai001",
      kind: "http_proxy",
      sessions: 100,
      bytes_in: 100000,
      bytes_out: 200000,
      avg_duration_ms: 115,
      p95_duration_ms: 300,
    };
    const retention: RetentionSettings = {
      audit_retention_days: 0, usage_retention_days: 90,
      redaction_retention_days: 30, connection_logs_retention_days: 30,
      connection_logs_rollups_retention_days: 0,
      webhook_deliveries_retention_days: 30,
      inspector_retention_count: 100,
      audit_retention_note: "Audit retention only deletes the six rate-limited leaf event types — see docs.",
    };
    const wh5: WebhookV5 = {
      id: "wh_ops", name: "ops-pager", url: "https://example.com/hook",
      events: ["audit.tokens.create", "ai.upstream_error"],
      paused: false, consecutive_failures: 0, first_failure_at: null,
      created_at: "2026-05-19T00:00:00Z",
      payload_template: "",
    };
    const preview: WebhookPreviewResponse = { rendered: "{}", size_bytes: 2 };
    const dbStatus: DatabaseStatus = {
      driver: "sqlite", postgres_alpha: false, url_redacted: "",
    };

    // Type-level assertion: UpstreamSlot is string
    expectTypeOf(slot).toEqualTypeOf<string>();

    expect([scs, stats, binding, routing5, domain, createDomain, log, rollup, retention, wh5, preview, dbStatus]).toHaveLength(12);

    // Verify WEBHOOK_EVENT_FIELDS constant shape
    expect(WEBHOOK_EVENT_FIELDS["ai.upstream_error"]).toContain("service_id");
    expect(WEBHOOK_EVENT_FIELDS["ai.cache_promotion"]).toContain("prompt_fingerprint");
    expect(WEBHOOK_EVENT_FIELDS["audit.policy_change"]).toContain("actor_email");
    expect(WEBHOOK_EVENT_FIELDS["service.created"]).toContain("access_mode");
    expect(WEBHOOK_EVENT_FIELDS["service.deleted"]).toContain("name");
    expect(WEBHOOK_EVENT_FIELDS["connection.session_summary"]).toContain("p95_duration_ms");
  });
});
