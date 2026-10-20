import { http, HttpResponse } from "msw";
import { ALL_PERMISSIONS } from "@/auth/permissions";
import { modelNameError } from "@/lib/modelNames";
import { db, NO_MODES, mockToolHash, MOCK_TOOL_PATTERN, type MockDb, type CacheSettingsPayload, type AiProviderRow, type ServiceRow } from "@/mocks/db";
import type { OAuthApprove, OAuthClient, AccessMode, AiGatewayKey, AiModel, AiModelTarget, AiProvider, AiRequestAttempt, ClientLoginRequest, ClientView, CostGroupBy, CostSummary, Dialect, DialectMode, KnownClient, Service, ServiceAIConfig, ToolSource, ToolSourceCreate, ToolSourceUpdate, ToolSourceTool, ToolSourceToolUpdate, ToolSyncResult, CustomDomain, CreateCustomDomainInput, RetentionSettings, GuardrailSettings, RedactionRule } from "@/lib/contract";

const json = (body: unknown, status = 200) => HttpResponse.json(body as object, { status });
const err = (status: number, message: string) => HttpResponse.json({ error: message }, { status });
const noContent = () => new HttpResponse(null, { status: 204 });

const SAFE = new Set(["GET", "HEAD", "OPTIONS"]);
const WHITELIST = [
  "smtp.host",
  "smtp.port",
  "smtp.username",
  "smtp.from",
  "smtp.tls",
  // v0.5.1 Q12 (UI toggle landed in v0.5.2): connection-log privacy toggle.
  "connection_logs.rollup_include_top_ips",
  // Read-only on the relay: BURROW_SERVICES_DOMAIN ("" = unset). Absent here until a test sets it.
  "services_domain",
];

// As the relay sorts them: usd descending, the "" group (remainder / not attributed) last.
function sortGroups<T extends { key: string; usd: number }>(rows: T[]): T[] {
  return [...rows].sort((x, y) => (x.key === "" ? 1 : 0) - (y.key === "" ? 1 : 0) || y.usd - x.usd);
}

// Gate: replicate 401 -> 403(csrf) -> 403(admin) ordering.
// `needs` names the permissions the real route accepts (internal/api/access.go); the admin role holds them all.
function gate(req: Request, opts: { admin?: boolean; needs?: string[] } = {}): Response | null {
  if (req.headers.get("x-mock-unauth") === "1") return err(401, "unauthorized");
  const method = req.method.toUpperCase();
  if (!SAFE.has(method)) {
    if (req.headers.get("X-CSRF-Token") !== db.csrf) return err(403, "csrf token invalid");
  }
  if (opts.admin && db.me.role !== "admin" && !(opts.needs ?? []).some((p) => db.me.permissions?.includes(p))) {
    return err(403, "admin required");
  }
  return null;
}

// ---- Tool sources: what the relay works out on every answer ----
function sourceWire(s: ToolSource): ToolSource {
  const tools = (db.toolSourceTools[s.slug] ?? []).filter((t) => t.status !== "removed");
  return { ...s, tool_count: tools.length, pending_count: tools.filter((t) => t.status === "pending").length };
}
function refreshCounts(slug: string) {
  const s = db.toolSources.find((x) => x.slug === slug);
  if (s) s.updated_at = new Date().toISOString();
}
function approveTool(t: ToolSourceTool) {
  t.approved_definition = t.definition;
  t.approved_hash = t.current_hash;
  t.status = "approved";
}
// Another approved tool is served under the name this one would be.
function servedNameTaken(tools: ToolSourceTool[], t: ToolSourceTool): boolean {
  const name = t.rename || t.name;
  return tools.some((x) => x !== t && x.status === "approved" && !x.hidden && (x.rename || x.name) === name);
}
function toolWire(slug: string, tools: ToolSourceTool[], t: ToolSourceTool): ToolSourceTool {
  const served_name = t.rename || t.name;
  const upstream_read_only = (t.definition.annotations as { readOnlyHint?: boolean } | undefined)?.readOnlyHint === true;
  const visible = t.status === "approved" && !t.hidden;
  const clash = visible && tools.some((x) => x !== t && x.status === "approved" && !x.hidden && (x.rename || x.name) === served_name);
  // D11: <slug>__<name> within 64 characters of [A-Za-z0-9_-], else the tool is on /mcp/<slug> only.
  const aggregated = `${slug}__${served_name}`;
  const issue: ToolSourceTool["aggregated_issue"] = clash ? "name_conflict"
    : !visible ? ""
    : !/^[A-Za-z0-9_-]+$/.test(aggregated) ? "name_invalid"
    : aggregated.length > 64 ? "name_too_long" : "";
  return {
    ...t, served_name, upstream_read_only, effective_read_only: t.read_only ?? upstream_read_only,
    served: visible && !clash, serve_issue: clash ? "name_conflict" : "",
    aggregated_name: visible && !clash && issue === "" ? aggregated : null, aggregated_issue: issue,
  };
}

// Whether the caller holds one of the permissions: the admin role holds them all, any other role what /me lists.
function holds(...perms: string[]): boolean {
  return db.me.role === "admin" || perms.some((p) => db.me.permissions?.includes(p));
}

// services:configure — admin holds :any; the owner holds :own (spec Part C).
function canConfigure(svc: { user_id: string }): boolean {
  if (holds("services:configure:any")) return true;
  return svc.user_id === db.me.id;
}

// Parse via text()+JSON.parse rather than req.json(): under jsdom/undici the
// Request#json() stream read is intermittently flaky, whereas text() is stable.
async function body<T>(req: Request): Promise<T | null> {
  try {
    const t = await req.text();
    if (!t) return null;
    return JSON.parse(t) as T;
  } catch { return null; }
}

// ---- client sign-in requests (internal/api/client_login_handlers.go) ----
const USER_CODE_ALPHABET = "ABCDEFGHJKMNPQRSTUVWXYZ23456789";
const CLIENT_LOGIN_GUESS_LIMIT = 20;

// The store's normalizeUserCode: upper case, no dashes or blanks, eight characters of the alphabet.
function normalizeUserCode(raw: string): string | null {
  if (raw.length > 64) return null;
  const code = raw.replace(/[- \t]/g, "").toUpperCase();
  if (code.length !== 8 || [...code].some((c) => !USER_CODE_ALPHABET.includes(c))) return null;
  return code;
}

// The store's cleanTokenName: trimmed, 1 to 120 characters, no control or format characters.
function cleanTokenName(raw: unknown): string | null {
  if (typeof raw !== "string") return null;
  const name = raw.trim();
  if (name === "" || [...name].length > 120 || /[\p{Cc}\p{Cf}]/u.test(name)) return null;
  return name;
}

// requireDashboardSession, then the lookup every request endpoint starts with.
// An unknown, malformed and an expired code are the same 404 and count as a wrong code.
function clientLoginFor(req: Request, rawCode: string): { row: ClientLoginRequest } | { res: Response } {
  const g = gate(req);
  if (g) return { res: g };
  if (db.clientLoginWrongCodes >= CLIENT_LOGIN_GUESS_LIMIT) {
    return { res: err(429, "too many wrong codes; try again in a minute") };
  }
  const code = normalizeUserCode(rawCode);
  const row = code === null ? undefined
    : db.clientLogins.find((r) => r.user_code.replace("-", "") === code && Date.parse(r.expires_at) > Date.now());
  if (!row) {
    db.clientLoginWrongCodes++;
    return { res: err(404, "sign-in request not found or expired") };
  }
  return { row };
}

// requireClientTokensManage: admin, or a role holding tokens:manage:own or :any.
function mayManageClientTokens(): boolean {
  if (db.me.role === "admin") return true;
  const perms = db.rolePerms[db.me.role] ?? [];
  return perms.includes("tokens:manage:own") || perms.includes("tokens:manage:any");
}

const PROVIDER_SLUG_RE = /^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$/;
const PROVIDER_SLUG_RULE =
  'slug must be 3-40 characters: lowercase letters, digits and hyphens, not starting or ending with a hyphen; "v1" is reserved';
const PROVIDER_SERVICE = "a tunnel provider needs an http service in API-key mode";
const providerSlugOk = (slug: string) => PROVIDER_SLUG_RE.test(slug) && slug !== "v1";

// Same rules as the server's validProviderName: trimmed, 1–120 bytes (Go's len).
function providerNameError(name: string): string | null {
  if (name === "") return "name is required";
  if (new TextEncoder().encode(name).length > 120) return "name must be at most 120 chars";
  return null;
}

// ---- direct providers: same checks, order and messages as the server ----
const MSG_BASE_URL = "base URL must be an https URL without credentials, query or fragment";
const MSG_BASE_URL_PRIVATE = "base URL resolves to a private or loopback address";
const MSG_MODEL_ID = "id must be 1-200 characters without control characters";
const UPSTREAM_FIELDS = ["api_format", "base_url", "credential_slot", "auth_header", "auth_format", "extra_headers", "billing", "supports_responses", "max_concurrent"];
const RESPONSES_FORMAT = "the Responses API belongs to the OpenAI format";
const MSG_MAX_CONCURRENT = "max concurrent requests must be between 0 and 1000";

// Same checks as the server for max_concurrent: a value that is not a whole
// number does not decode ("invalid JSON body"), one outside 0-1000 is refused
// with the store's reason. null and a field left out are the same.
function maxConcurrentError(v: unknown): string | null {
  if (v === undefined || v === null) return null;
  if (typeof v !== "number" || !Number.isInteger(v)) return "invalid JSON body";
  return v < 0 || v > 1000 ? MSG_MAX_CONCURRENT : null;
}
const CREATE_FIELDS = ["slug", "name", "kind", "service_id", "gateway_only", ...UPSTREAM_FIELDS];
const HEADER_NAME_RE = /^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$/;
const RELAY_HEADERS = new Set([
  "host", "content-length", "transfer-encoding", "connection", "keep-alive", "te", "trailer", "upgrade", "cookie", "forwarded",
]);
const isRelayHeader = (lower: string) =>
  RELAY_HEADERS.has(lower) || lower.startsWith("proxy-") || lower.startsWith("x-forwarded-");
// eslint-disable-next-line no-control-regex
const hasControl = (v: string) => /[\u0000-\u001f\u007f-\u009f]/.test(v);
const byteLen = (v: string) => new TextEncoder().encode(v).length;

interface UpstreamBody {
  api_format?: string;
  base_url?: string;
  credential_slot?: string;
  auth_header?: string;
  auth_format?: string;
  extra_headers?: Record<string, string>;
  billing?: string;
  supports_responses?: boolean;
  max_concurrent?: number | null;
}

// The server decodes these bodies strictly: a field it does not know, such as
// api_key, is a 400 that names the field and never repeats its value.
function unknownField(b: object, allowed: string[]): string | null {
  const extra = Object.keys(b).find((k) => !allowed.includes(k));
  if (extra === undefined) return null;
  const hint = "; the credential is set on the relay, credential_slot names its slot";
  return /^[A-Za-z0-9_.-]{1,64}$/.test(extra)
    ? `unknown field "${extra}"${hint}`
    : `unknown field in the request body${hint}`;
}

// Shape only (no DNS in the mock): https, no userinfo, query or fragment, and
// no host that is itself a private or loopback address.
function baseUrlError(raw: string): string | null {
  let u: URL;
  try { u = new URL(raw); } catch { return MSG_BASE_URL; }
  if (u.protocol !== "https:" || u.username || u.password || raw.includes("?") || raw.includes("#") || raw.length > 2048) {
    return MSG_BASE_URL;
  }
  const h = u.hostname;
  if (h === "localhost" || h.endsWith(".localhost") || h === "[::1]"
    || /^(10\.|127\.|169\.254\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)/.test(h)) {
    return MSG_BASE_URL_PRIVATE;
  }
  return null;
}

// Mirror of the store's normalizeDirect: validates the merged settings and
// fills the defaults.
function normalizeDirect(p: AiProviderRow): string | null {
  // One slot, or up to four separated by commas; stored joined without spaces.
  const slots = splitSlots(p.credential_slot);
  if (slots.length < 1 || slots.length > 4 || slots.some((slot, i) => !/^[A-Z0-9_]{1,32}$/.test(slot) || slots.indexOf(slot) !== i)) {
    return "credential slots must be 1-4 distinct names of 1-32 characters: A-Z, 0-9, _";
  }
  p.credential_slot = slots.join(",");
  p.auth_header ||= "Authorization";
  if (!HEADER_NAME_RE.test(p.auth_header) || isRelayHeader(p.auth_header.toLowerCase())) {
    return "auth header is not a valid header name";
  }
  p.auth_format ||= "Bearer {key}";
  if (p.auth_format.split("{key}").length !== 2 || hasControl(p.auth_format) || byteLen(p.auth_format) > 128) {
    return 'auth format must contain "{key}" once, at most 128 characters, no control characters';
  }
  p.billing ||= "metered";
  if (p.billing !== "metered" && p.billing !== "flat") return "billing must be 'metered' or 'flat'";
  p.api_format ||= "openai";
  if (p.api_format !== "openai" && p.api_format !== "anthropic") return "api_format must be 'openai' or 'anthropic'";
  if (p.supports_responses && p.api_format !== "openai") return RESPONSES_FORMAT;
  const extra = Object.entries(p.extra_headers ?? {});
  if (extra.length > 16) return "at most 16 extra headers";
  let total = 0;
  for (const [k, v] of extra) {
    if (!HEADER_NAME_RE.test(k)) return "an extra header has an invalid name";
    const lower = k.toLowerCase();
    if (isRelayHeader(lower) || lower === "authorization" || lower === "x-api-key" || lower === p.auth_header.toLowerCase()) {
      return `extra header "${k}" is not allowed`;
    }
    if (typeof v !== "string" || hasControl(v) || byteLen(v) > 512) return `extra header "${k}" has an invalid value`;
    total += byteLen(k) + byteLen(v);
  }
  if (total > 4096) return "extra headers are larger than 4096 bytes in total";
  return null;
}

// True only when the slot exists on the relay and is non-empty.
const credentialPresent = (slot: string | undefined) =>
  !!slot && db.upstreamSlots.includes(slot) && !db.absentSlots.has(slot);

// The store's SplitSlots: the slot names of a credential_slot value.
const splitSlots = (value: string | undefined) =>
  (value ?? "").split(",").map((s) => s.trim()).filter((s) => s !== "");

// The slots a direct provider names, each with whether it is set; none for a tunnel provider.
const credentialSlots = (p: AiProviderRow) =>
  p.kind === "direct" ? splitSlots(p.credential_slot).map((slot) => ({ slot, present: credentialPresent(slot) })) : [];

// Same rule as the server's ValidModelID.
const modelIdOk = (id: unknown): id is string =>
  typeof id === "string" && id !== "" && byteLen(id) <= 200 && !hasControl(id) && id.trim() === id;

// Same rules as the server's ProviderSlugFromName: lower-cased, runs of other
// characters collapsed to one hyphen, trimmed, cut at 40. An unusable result
// is left for the slug check to reject.
function providerSlugFromName(name: string): string {
  let slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (slug.length > 40) slug = slug.slice(0, 40).replace(/-+$/, "");
  return slug;
}

// A provider is visible when its backing service is (same rule as GET /services).
function providerVisible(p: AiProviderRow): boolean {
  const svc = db.services.find((s) => s.id === p.service_id);
  return !!svc && (holds("ai:read:any", "services:configure:any") || svc.user_id === db.me.id);
}

// A service as the relay returns it: the stored row without its owner, with
// the status the relay works out. Online while a client serves it, offline
// when one served it before, never connected otherwise.
function serviceWire(svc: ServiceRow): Service {
  const { user_id: _u, last_seen, last_client, ...row } = svc;
  // The session that holds the tunnel of the same name, as the relay matches them.
  const holder = svc.connected
    ? db.clients.find((c) => c.services.some((t) => t.name === svc.name && t.type === svc.type))
    : undefined;
  return {
    ...row,
    url: row.gateway_only ? "" : row.url,
    status: svc.connected ? "online" : last_seen ? "offline" : "never_connected",
    last_seen: last_seen ?? null,
    last_client: holder?.token_name ?? last_client ?? "",
    client_session_id: holder?.session_id ?? "",
    provider_slug: db.aiProviders.find((p) => p.kind === "tunnel" && p.service_id === svc.id)?.slug ?? "",
  };
}

function providerView(p: AiProviderRow): AiProvider {
  const meta = db.aiMeta[p.service_id];
  const backing = db.services.find((s) => s.id === p.service_id);
  // The first synthetic model that targets the provider, as the server reports it.
  const model = db.aiModels.find((m) => m.targets.some((t) => t.provider === p.slug));
  const target = model?.targets.find((t) => t.provider === p.slug);
  const direct = p.kind === "direct";
  // Set means every slot the provider names is set.
  const slots = credentialSlots(p);
  const present = slots.length > 0 && slots.every((x) => x.present);
  return {
    slug: p.slug,
    name: p.name,
    kind: p.kind,
    api_format: p.api_format,
    service_id: p.service_id,
    upstream_base_url: p.upstream_base_url ?? "",
    credential_slot: p.credential_slot ?? "",
    credential_present: present,
    credential_slots: slots,
    breaker_open: db.aiBreakerOpen.has(p.slug),
    billing: p.billing ?? "metered",
    supports_responses: p.supports_responses ?? false,
    max_concurrent: p.max_concurrent ?? 0,
    in_use: p.max_concurrent ? db.aiProviderInUse[p.slug] ?? 0 : 0,
    model_count: (db.aiProviderModels[p.slug] ?? []).length,
    // The upstream routes are admin only; header values are never returned.
    ...(direct && holds("ai:providers:manage")
      ? {
          auth_header: p.auth_header ?? "Authorization",
          auth_format: p.auth_format ?? "Bearer {key}",
          extra_header_names: Object.keys(p.extra_headers ?? {}).sort(),
        }
      : {}),
    base_url: `https://tunnels.example.com/ai/${p.slug}/v1`,
    model_alias: model?.name ?? "",
    concrete_model: target?.model ?? "",
    backend_type: meta?.backend_type ?? "other",
    api_key_count: (db.serviceApiKeys[p.service_id] ?? []).length,
    requests_24h: meta?.requests_24h ?? 0,
    cache_hits_24h: meta?.cache_hits_24h ?? 0,
    latency_p95_ms: meta?.latency_p95_ms ?? 0,
    // A direct provider has no tunnel: it is usable when its slots are set.
    status: direct ? (present ? "Connected" : "Offline") : meta?.status ?? "Offline",
    client_session_id: meta?.client_session_id ?? "",
    // A tunnel provider is online with its client (aiMeta, as `status`);
    // without one, whether a client served its backing service before decides.
    state: direct
      ? (present ? "ready" : "not_configured")
      : (meta?.status ?? "Offline") !== "Offline" ? "online" : backing?.last_seen ? "offline" : "never_connected",
    last_seen: direct ? null : backing?.last_seen ?? null,
    last_client: direct ? "" : backing?.last_client ?? "",
  };
}

// The relay's targetAvailable: the provider exists and speaks the format, the
// gateway is not skipping it, and a direct provider has every slot set, a
// tunnel provider a connected client.
function targetAvailable(t: AiModelTarget): boolean {
  const p = db.aiProviders.find((x) => x.slug === t.provider);
  if (!p || p.api_format !== t.dialect || db.aiBreakerOpen.has(p.slug)) return false;
  if (p.kind === "tunnel") return db.aiMeta[p.service_id]?.status === "Connected";
  const slots = credentialSlots(p);
  return slots.length > 0 && slots.every((x) => x.present);
}

// A model as the relay returns it: availability per target and, per format,
// the first available target (null when there is none).
function modelView(m: AiModel): AiModel {
  const targets = m.targets.map((t) => ({ dialect: t.dialect, provider: t.provider, model: t.model, available: targetAvailable(t) }));
  const serving: AiModel["serving"] = {};
  for (const t of targets) {
    if (serving[t.dialect]) continue;
    // Requests for a disabled model are refused whatever its targets can do.
    serving[t.dialect] = m.enabled && t.available ? { provider: t.provider, model: t.model } : null;
  }
  return { ...m, targets, serving, ...servedModes(m) };
}

// The relay's aigateway.ServedModes (internal/aigateway/translate.go
// chooseTargets): targets of the format that offer the endpoint serve it
// natively; only when there is none, and translate is on, the format is
// translated — first through its own targets that lack the endpoint (an
// OpenAI-format provider without the Responses API), then through the other
// format's. A target counts when its provider exists and speaks its format;
// a disabled model is served nowhere. All four pairs are released.
function servedModes(m: AiModel): Pick<AiModel, "dialect_modes" | "responses_mode" | "translation_pairs"> {
  const usable = (d: Dialect) => m.targets.flatMap((t) => {
    const p = db.aiProviders.find((x) => x.slug === t.provider);
    return t.dialect === d && p && p.api_format === d ? [p] : [];
  });
  const pairs: AiModel["translation_pairs"] = {};
  const mode = (key: Dialect | "responses"): DialectMode => {
    if (!m.enabled) return "not_served";
    const dialect: Dialect = key === "anthropic" ? "anthropic" : "openai";
    const own = usable(dialect);
    const offers = (p: (typeof own)[number]) => key !== "responses" || p.supports_responses === true;
    if (own.some(offers)) return "native";
    if (!m.translate) return "not_served";
    const through = [...own.filter((p) => !offers(p)), ...usable(dialect === "openai" ? "anthropic" : "openai")];
    if (through.length === 0) return "not_served";
    const caller = key === "anthropic" ? "messages" : key === "openai" ? "chat" : "responses";
    // Every pair once, in the order the targets are tried.
    pairs[key] = [...new Set(through.map((p) => `${caller}-${p.api_format === "openai" ? "chat" : "messages"}`))];
    return "translated";
  };
  return { dialect_modes: { openai: mode("openai"), anthropic: mode("anthropic") }, responses_mode: mode("responses"), translation_pairs: pairs };
}

// ---- synthetic models (internal/store/ai_models.go) ----
interface ModelBody {
  name?: unknown;
  description?: unknown;
  enabled?: unknown;
  fallback_on_rate_limit?: unknown;
  translate?: unknown;
  system_text?: unknown;
  routing?: unknown;
  attempt_timeout_s?: unknown;
  total_timeout_s?: unknown;
  targets?: unknown;
}
const TARGET_FIELDS = ["dialect", "provider", "model"];
const MODEL_FIELDS = ["name", "description", "enabled", "fallback_on_rate_limit", "translate", "system_text", "routing", "attempt_timeout_s", "total_timeout_s", "targets"];

const MSG_MODEL_NAME = "name must be 2-63 characters: lowercase letters, digits, dot, underscore, hyphen";
const MSG_MODEL_TARGETS = "a model needs at least one target and at most 8 per format";
const MODEL_DIALECTS: Dialect[] = ["anthropic", "openai"];

/**
 * The stored model for a request body, or the reason the relay refuses it, in
 * the relay's words (internal/store/ai_models.go normalizeModel). `old` is the
 * model a PUT replaces: a name, enabled or translate left out keeps its value.
 */
function modelFromBody(b: ModelBody | null, old?: AiModel): Omit<AiModel, "created_at" | "updated_at"> | string {
  if (!b || typeof b !== "object") return "invalid JSON body";
  // Strict decoding: "dialects", "serving" and the mode fields are derived
  // and may not be sent, and neither may a target's "available".
  const extra = Object.keys(b).find((k) => !MODEL_FIELDS.includes(k))
    ?? (Array.isArray(b.targets) ? b.targets : []).flatMap((t) => (t && typeof t === "object" ? Object.keys(t as object) : []))
      .find((k) => !TARGET_FIELDS.includes(k));
  if (extra) return `unknown field "${extra}"`;
  const name = (typeof b.name === "string" && b.name !== "" ? b.name : old?.name) ?? "";
  if (name === "" || modelNameError(name) !== null) return MSG_MODEL_NAME;
  if (db.aiProviders.some((p) => p.slug === name)) return "name is already a provider slug";
  const description = typeof b.description === "string" ? b.description.trim() : "";
  if (byteLen(description) > 500 || hasControl(description)) return "description must be at most 500 characters without control characters";
  const raw = Array.isArray(b.targets) ? (b.targets as Partial<Record<keyof AiModelTarget, unknown>>[]) : [];
  if (raw.length === 0 || raw.length > 8 * MODEL_DIALECTS.length) return MSG_MODEL_TARGETS;
  const targets: AiModelTarget[] = [];
  for (const t of raw) {
    const slug = typeof t.provider === "string" ? t.provider : "";
    if (!providerSlugOk(slug)) return "a target names an invalid provider slug";
    const p = db.aiProviders.find((x) => x.slug === slug);
    if (!p) return `unknown provider ${slug}`;
    // Left out, the format is the provider's.
    const dialect = t.dialect === undefined || t.dialect === "" ? p.api_format : t.dialect;
    if (dialect !== "openai" && dialect !== "anthropic") return "a target's format must be 'openai' or 'anthropic'";
    if (p.api_format !== dialect) return `provider ${slug} speaks ${p.api_format}, not ${dialect}`;
    const model = typeof t.model === "string" ? t.model.trim() : "";
    if (model === "" || byteLen(model) > 200 || hasControl(model)) return "a target's model must be 1-200 characters without control characters";
    if (targets.some((x) => x.dialect === dialect && x.provider === slug && x.model === model)) return "a target is listed twice";
    targets.push({ dialect, provider: slug, model });
    if (targets.filter((x) => x.dialect === dialect).length > 8) return MSG_MODEL_TARGETS;
  }
  // Left out: the stored value on update. An empty system_text clears it; an empty routing is refused.
  if (b.system_text !== undefined && typeof b.system_text !== "string") return "invalid JSON body";
  const systemText = typeof b.system_text === "string" ? b.system_text : old?.system_text ?? "";
  if (byteLen(systemText) > 8192) return "system_text is longer than 8192 bytes";
  if (b.routing !== undefined && b.routing !== "ordered" && b.routing !== "fastest" && b.routing !== "cheapest") {
    return "routing must be ordered, fastest or cheapest";
  }
  const routing = b.routing ?? old?.routing ?? "ordered";
  const attempt = typeof b.attempt_timeout_s === "number" && b.attempt_timeout_s !== 0 ? b.attempt_timeout_s : 60;
  const total = typeof b.total_timeout_s === "number" && b.total_timeout_s !== 0 ? b.total_timeout_s : 120;
  if (attempt < 1 || attempt > 600 || total < 1 || total > 600) return "timeouts must be between 1 and 600 seconds";
  if (total < attempt) return "total timeout must not be shorter than the attempt timeout";
  return {
    name,
    description,
    system_text: systemText,
    routing,
    enabled: typeof b.enabled === "boolean" ? b.enabled : old?.enabled ?? true,
    fallback_on_rate_limit: b.fallback_on_rate_limit === true,
    // Left out or null: off on create, the stored value on update.
    translate: typeof b.translate === "boolean" ? b.translate : old?.translate ?? false,
    attempt_timeout_s: attempt,
    total_timeout_s: total,
    // As the relay returns them: by format, then in the order given.
    // available, serving and the modes are worked out by modelView on every response.
    targets: MODEL_DIALECTS.flatMap((d) => targets.filter((t) => t.dialect === d)).map((t) => ({ ...t, available: false })),
    dialects: MODEL_DIALECTS.filter((d) => targets.some((t) => t.dialect === d)),
    serving: {},
    ...NO_MODES,
  };
}

// The store's ValidAllowEntry: a synthetic model name, "<provider>/<model>" or "<provider>/*".
function allowEntryOk(entry: string): boolean {
  const i = entry.indexOf("/");
  if (i < 0) return entry !== "" && modelNameError(entry) === null;
  const model = entry.slice(i + 1);
  if (!providerSlugOk(entry.slice(0, i)) || model === "") return false;
  return model === "*" || (!model.includes("*") && !/\s/.test(model) && !hasControl(model));
}

// The store's tool allow-list rule: "<source>__<tool>" or "<source>__*", at most 64 entries. Absent = none.
function toolListOf(v: unknown): string[] | string {
  if (v === undefined) return [];
  if (!Array.isArray(v) || v.length > 64) return "at most 64 allowed tools";
  if (v.some((e) => typeof e !== "string" || !/^[a-z0-9-]{1,24}__\S+$/.test(e) || e.length > 160)) {
    return 'an allowed tool must be "<source>__<tool>" or "<source>__*"';
  }
  return v as string[];
}

export const handlers = [
  // ---- auth / identity ----
  http.get("/api/v1/me", ({ request }) => gate(request) ?? json(db.me)),
  http.post("/api/v1/auth/logout", ({ request }) => gate(request) ?? noContent()),
  // Confirms the password and opens the window of the guarded routes ("password123").
  http.post("/api/v1/auth/reauth", async ({ request }) => {
    const g = gate(request); if (g) return g;
    const b = await body<{ password?: unknown }>(request);
    if (!b || typeof b.password !== "string" || b.password === "") return err(400, "password required");
    if (b.password !== "password123") return err(403, "password is incorrect");
    db.reauthConfirmed = true;
    return json({ expires_at: new Date(Date.now() + 600_000).toISOString() });
  }),
  http.post("/api/v1/auth/change-password", async ({ request }) => {
    const g = gate(request); if (g) return g;
    const b = await body<{ current_password?: string; new_password?: string }>(request);
    if (!b?.current_password || !b?.new_password) return err(400, "current_password and new_password are required");
    if (b.current_password !== "password123") return err(401, "current password is incorrect");
    if (b.new_password.length < 8) return err(400, "new password must be at least 8 characters");
    return noContent();
  }),

  // ---- users ----
  http.get("/api/v1/users", ({ request }) => {
    const g = gate(request, { admin: true, needs: ["users:read", "users:manage"] }); if (g) return g;
    const url = new URL(request.url);
    const q = (url.searchParams.get("q") ?? "").toLowerCase();
    const limit = Number(url.searchParams.get("limit")) || 50;
    const offset = Number(url.searchParams.get("offset")) || 0;
    const filtered = db.users.filter((u) => u.email.toLowerCase().includes(q));
    return json({ users: filtered.slice(offset, offset + limit), total: filtered.length });
  }),
  http.post("/api/v1/users", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ email?: string; password?: string; role?: string }>(request);
    if (!b?.email || !b?.password || !b?.role) return err(400, "email, password, and role are required");
    if (db.users.some((u) => u.email === b.email)) return err(409, "email already in use");
    if (b.password.length < 8) return err(400, "password must be at least 8 characters");
    if (b.role !== "admin" && b.role !== "user") return err(400, "role must be 'admin' or 'user'");
    const u: MockDb["users"][number] = {
      id: `bur_usr_${Math.random().toString(36).slice(2, 9)}`,
      email: b.email, role: b.role, status: "active", last_login: null,
      created_at: new Date().toISOString(),
    };
    db.users.push(u);
    return json(u, 201);
  }),
  http.patch("/api/v1/users/:id", async ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ role?: string; status?: string }>(request);
    if (!b || (b.role == null && b.status == null)) return err(400, "role and/or status required");
    const u = db.users.find((x) => x.id === params.id);
    if (!u) return err(404, "user not found");
    if (b.status != null) {
      if (b.status !== "active" && b.status !== "suspended") return err(400, "status must be 'active' or 'suspended'");
      if (u.id === db.me.id) return err(400, "cannot change your own status");
      u.status = b.status;
    }
    if (b.role != null) {
      if (b.role !== "admin" && b.role !== "user") return err(400, "role must be 'admin' or 'user'");
      u.role = b.role;
    }
    return noContent();
  }),
  http.delete("/api/v1/users/:id", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    if (params.id === db.me.id) return err(400, "cannot delete yourself");
    const i = db.users.findIndex((x) => x.id === params.id);
    if (i < 0) return err(404, "user not found");
    db.users.splice(i, 1);
    return noContent();
  }),

  // ---- roles ----
  http.get("/api/v1/roles", ({ request }) => gate(request, { admin: true, needs: ["roles:read", "roles:manage"] }) ?? json(db.roles)),
  http.get("/api/v1/roles/permissions", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    return json([
      { key: "tunnels:read:any",     group: "tunnels",  description: "Read all tunnels"   },
      { key: "tunnels:read:own",     group: "tunnels",  description: "Read own tunnels"   },
      { key: "tunnels:manage:any",   group: "tunnels",  description: "Manage all tunnels" },
      { key: "services:configure:any", group: "services", description: "Configure any service" },
      { key: "tokens:manage:any",    group: "tokens",   description: "Manage all tokens"  },
      { key: "audit:read",           group: "audit",    description: "Read audit log"     },
      { key: "cost:read",            group: "cost",     description: "Read cost data"     },
      { key: "webhooks:manage",      group: "webhooks", description: "Manage webhooks"    },
      { key: "users:manage",         group: "users",    description: "Manage users"       },
      { key: "settings:manage",      group: "settings", description: "Manage settings"    },
    ]);
  }),
  http.get("/api/v1/roles/:name", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const r = db.roles.find((x) => x.name === params.name);
    if (!r) return err(404, "role not found");
    return json({ ...r, permissions: db.rolePerms[r.name] ?? [] });
  }),
  http.post("/api/v1/roles", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ name?: string; description?: string; permissions?: string[]; default_for_new_users?: boolean }>(request);
    if (!b?.name) return err(400, "name is required");
    if (db.roles.some((r) => r.name === b.name)) return err(409, "role already exists");
    db.roles.push({ name: b.name, description: b.description ?? "", created_at: new Date().toISOString(), builtin: false });
    db.rolePerms[b.name] = Array.isArray(b.permissions) ? [...b.permissions] : [];
    return json({ name: b.name, description: b.description ?? "", created_at: new Date().toISOString(), builtin: false, permissions: db.rolePerms[b.name]! }, 201);
  }),
  http.put("/api/v1/roles/:name", async ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const r = db.roles.find((x) => x.name === params.name);
    if (!r) return err(404, "role not found");
    if (r.builtin) return err(409, "built-in roles cannot be edited");
    const b = await body<{ description?: string; permissions?: string[] }>(request);
    if (b?.description != null) r.description = b.description;
    if (b?.permissions != null) db.rolePerms[r.name] = [...b.permissions];
    return noContent();
  }),
  http.delete("/api/v1/roles/:name", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const i = db.roles.findIndex((x) => x.name === params.name);
    if (i < 0) return err(404, "role not found");
    if (db.roles[i]!.builtin) return err(409, "built-in roles cannot be deleted");
    db.roles.splice(i, 1);
    delete db.rolePerms[String(params.name)];
    return noContent();
  }),

  // ---- sessions ----
  http.get("/api/v1/sessions", ({ request }) => gate(request) ?? json(db.sessions)),
  http.delete("/api/v1/sessions/:id", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const i = db.sessions.findIndex((s) => s.id === params.id);
    if (i < 0) return err(404, "session not found");
    db.sessions.splice(i, 1);
    return noContent();
  }),
  http.post("/api/v1/sessions/revoke-all", ({ request }) => {
    const g = gate(request); if (g) return g;
    const before = db.sessions.length;
    db.sessions = db.sessions.filter((s) => s.current);
    return json({ revoked: before - db.sessions.length });
  }),

  // ---- settings ----
  http.get("/api/v1/settings", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const out: Record<string, string> = {};
    for (const k of WHITELIST) if (db.settings[k] != null) out[k] = db.settings[k];
    return json(out);
  }),
  http.put("/api/v1/settings", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<Record<string, string>>(request);
    if (!b) return err(400, "invalid request body");
    if (b["smtp.tls"] != null && !["none", "starttls", "implicit"].includes(b["smtp.tls"]))
      return err(400, "smtp.tls must be none, starttls, or implicit");
    for (const k of WHITELIST) if (b[k] != null) db.settings[k] = b[k];
    return noContent();
  }),
  http.post("/api/v1/settings/test-email", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ to?: string }>(request);
    if (!b?.to) return err(400, "to is required");
    if (!db.settings["smtp.host"] || !db.smtpPasswordSet)
      return err(409, "smtp is not configured — set host/port and BURROW_SMTP_PASSWORD");
    return noContent();
  }),

  // ---- clients ----
  http.get("/api/v1/clients", ({ request }) => {
    const g = gate(request, { admin: true, needs: ["tunnels:read:any"] }); if (g) return g;
    return json(db.clients.map(({ services: _services, ...v }) => v));
  }),
  http.get("/api/v1/clients/connect-info", ({ request }) => gate(request) ?? json({ server: db.connectServer })),
  // Registered before /clients/:id so the literal segment wins.
  // One row per client token: online with its live sessions, offline with the
  // last session the relay remembers, or never connected.
  http.get("/api/v1/clients/known", ({ request }) => {
    const g = gate(request, { admin: true, needs: ["tunnels:read:any"] }); if (g) return g;
    const live = (name: string) => db.clients.filter((c) => c.token_name === name);
    const row = (tokenId: string, name: string): KnownClient => {
      const sessions = live(name);
      const last = db.clientLastSessions[tokenId];
      const newest = sessions[0];
      const views: ClientView[] = sessions.map(({ services: _services, ...v }) => v);
      if (newest) {
        return {
          token_id: tokenId, token_name: name, user_id: newest.user_id, status: "online",
          last_seen: last?.last_seen ?? new Date().toISOString(),
          os: newest.os, arch: newest.arch, client_version: newest.client_version, remote_addr: newest.remote_addr,
          served: [...new Set(sessions.flatMap((c) => c.services.map((s) => s.name)))].sort(),
          sessions: views,
        };
      }
      return {
        token_id: tokenId, token_name: name, user_id: db.me.id,
        status: last ? "offline" : "never_connected",
        last_seen: last?.last_seen ?? null,
        os: last?.os ?? "", arch: last?.arch ?? "", client_version: last?.client_version ?? "", remote_addr: last?.remote_addr ?? "",
        served: [...(last?.served ?? [])].sort(),
        sessions: [],
      };
    };
    const rows = db.tokens.map((t) => row(t.id, t.name));
    // A live session whose token was revoked meanwhile keeps its row, without a token id.
    const revoked = [...new Set(db.clients.map((c) => c.token_name))].filter((n) => !db.tokens.some((t) => t.name === n));
    rows.push(...revoked.map((n) => row("", n)));
    const rank = { online: 0, offline: 1, never_connected: 2 } as const;
    rows.sort((a, b) =>
      rank[a.status] - rank[b.status]
      || (a.status === "offline" ? (b.last_seen ?? "").localeCompare(a.last_seen ?? "") : a.token_name.localeCompare(b.token_name)));
    return json(rows);
  }),
  // Public: a client asks before it has a token.
  http.get("/api/v1/client/discovery", () => json(db.discovery)),

  // ---- client sign-in requests: lookup, approve, deny (session + CSRF) ----
  http.get("/api/v1/client/login/requests/:code", ({ request, params }) => {
    const found = clientLoginFor(request, String(params.code));
    return "res" in found ? found.res : json({ ...found.row, password_required: db.clientLoginPasswordRequired });
  }),
  http.post("/api/v1/client/login/requests/:code/approve", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    if (!mayManageClientTokens()) return err(403, "tokens:manage required");
    const b = await body<{ token_name?: unknown; password?: unknown }>(request);
    if (!b) return err(400, "token_name is required");
    // As on the relay: the password (inline, or confirmed with POST /auth/reauth)
    // is checked before the request or the name.
    if (db.clientLoginPasswordRequired) {
      if (typeof b.password === "string" && b.password !== "") {
        if (b.password !== "password123") return err(403, "password is incorrect");
      } else if (!db.reauthConfirmed) {
        return err(403, "reauth_required");
      }
    }
    const name = cleanTokenName(b.token_name);
    if (name === null) return err(400, "token_name must be 1 to 120 characters without control characters");
    const found = clientLoginFor(request, String(params.code));
    if ("res" in found) return found.res;
    if (found.row.status !== "pending") return err(409, "sign-in request was already decided");
    found.row.status = "approved";
    return json({ ...found.row, password_required: db.clientLoginPasswordRequired });
  }),
  http.post("/api/v1/client/login/requests/:code/deny", ({ request, params }) => {
    const found = clientLoginFor(request, String(params.code));
    if ("res" in found) return found.res;
    if (found.row.status !== "pending") return err(409, "sign-in request was already decided");
    found.row.status = "denied";
    return json({ ...found.row, password_required: db.clientLoginPasswordRequired });
  }),
  http.get("/api/v1/clients/:id", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const c = db.clients.find((x) => x.session_id === params.id);
    if (!c) return err(404, "client not found");
    // http services carry the id of their durable service row (matched by
    // name, as the server does); tcp tunnels have none.
    const services = c.services.map((s) => {
      const svc = s.type === "http" ? db.services.find((x) => x.name === s.name && x.type === "http") : undefined;
      return svc ? { ...s, service_id: svc.id } : s;
    });
    return json({ ...c, services });
  }),

  // ---- per-service access mode ----
  http.put("/api/v1/tunnels/:id/access-mode", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const b = await body<{ access_mode?: string }>(request);
    if (!b?.access_mode) return err(400, "access_mode is required");
    let svc: MockDb["clients"][number]["services"][number] | undefined;
    for (const c of db.clients) { const s = c.services.find((x) => x.id === params.id); if (s) { svc = s; break; } }
    if (!svc) return err(404, "tunnel not found");
    if (!["open", "api_key", "burrow_login"].includes(b.access_mode))
      return err(400, "access_mode must be 'open', 'api_key', or 'burrow_login'");
    svc.access_mode = b.access_mode as typeof svc.access_mode;
    return noContent();
  }),

  // ---- v0.3.0 durable services (spec Part E) ----
  http.get("/api/v1/services", ({ request }) => {
    const g = gate(request); if (g) return g;
    // Owner-scoped; admin (tunnels:read:any) sees all.
    // The backing row of a direct AI provider is not listed.
    const rows = (holds("services:configure:any")
      ? db.services
      : db.services.filter((s) => s.user_id === db.me.id)).filter((s) => s.type !== "direct");
    return json(rows.map(serviceWire));
  }),
  // Registered before /services/:id so the literal segment wins.
  http.get("/api/v1/services/slug-suggestion", ({ request }) =>
    gate(request) ?? json({ slug: "q4m7kx" })),
  http.get("/api/v1/services/:id", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    return json({
      ...serviceWire(svc),
      api_key_count: (db.serviceApiKeys[svc.id] ?? []).length,
      access_policy: db.serviceAccessPolicy[svc.id] ?? [],
    });
  }),
  // The owner, or a holder of services:configure:any, deletes; anyone else gets the 404 of an unknown id.
  // A service a client serves, or an AI provider points at, stays (409), as for the relay.
  http.delete("/api/v1/services/:id", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc || !canConfigure(svc)) return err(404, "service not found");
    if (svc.connected) return err(409, "the service is connected; stop the client or remove the tunnel from its config, then delete");
    const provider = db.aiProviders.find((p) => p.service_id === svc.id);
    if (provider) {
      return err(409, `an AI provider still uses this service (${provider.slug}); delete the provider, and the models that use it, first`);
    }
    db.services = db.services.filter((s) => s.id !== svc.id);
    delete db.serviceApiKeys[svc.id];
    delete db.serviceAccessPolicy[svc.id];
    delete db.aiConfigs[svc.id];
    return new HttpResponse(null, { status: 204 });
  }),
  http.post("/api/v1/services", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ service_id?: string; title?: string; access_mode?: string; slug?: string }>(request);
    const id = (b?.service_id ?? "").trim();
    if (!/^[a-z0-9_-]{3,64}$/.test(id)) return err(400, "service_id must match ^[a-z0-9_-]{3,64}$");
    const modeMap: Record<string, string> = { "": "open", public: "open", open: "open", api_key: "api_key", burrow_login: "burrow_login" };
    const stored = modeMap[b?.access_mode ?? ""];
    if (stored === undefined) return err(400, `unknown access mode "${b?.access_mode}"`);
    // The relay keeps ids unique, and names per owner: a client finds its service by name.
    const title = b?.title ?? "";
    if (db.services.some((s) => s.id === id || (title !== "" && s.user_id === db.me.id && s.name === title))) {
      return err(409, "service id or slug already in use");
    }
    const slug = b?.slug ?? "";
    db.services.push({ id, user_id: db.me.id, name: title, type: "http", slug, url: slug ? `https://tunnels.example.com/svc/${slug}/` : "", access_mode: stored as AccessMode, api_key_header: "Authorization", gateway_only: false, connected: false, remote_port: 0, local_addr: "" });
    return json({ id, created_at: new Date().toISOString() }, 201);
  }),

  // ---- v0.3.0 per-service API keys (spec Part C; services:configure) ----
  http.get("/api/v1/services/:id/api-keys", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    return json(db.serviceApiKeys[svc.id] ?? []);
  }),
  http.post("/api/v1/services/:id/api-keys", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    const b = await body<{ name?: string }>(request);
    if (!b?.name) return err(400, "name is required");
    const id = `sak_${Math.random().toString(36).slice(2, 8)}`;
    (db.serviceApiKeys[svc.id] ||= []).push({ id, name: b.name, last_used: null, created_at: new Date().toISOString() });
    return json({ id, name: b.name, key: `buk_mock_${Math.random().toString(36).slice(2, 18)}` }, 201);
  }),
  http.delete("/api/v1/services/:id/api-keys/:keyId", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    const list = db.serviceApiKeys[svc.id] ?? [];
    const i = list.findIndex((k) => k.id === params.keyId);
    if (i < 0) return err(404, "api key not found");
    list.splice(i, 1);
    return noContent();
  }),

  // ---- v0.3.0 per-service access mode (v0.4.0 adds mtls + ca_pem) ----
  http.put("/api/v1/services/:id/access-mode", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    const b = await body<{ access_mode?: string; api_key_header?: string; mtls_ca_pem?: string }>(request);
    if (!b?.access_mode) return err(400, "access_mode is required");
    if (!["open", "api_key", "burrow_login", "mtls"].includes(b.access_mode))
      return err(400, "access_mode must be 'open', 'api_key', 'burrow_login', or 'mtls'");
    if (b.access_mode !== "open" && svc.type === "tcp")
      return err(409, "api_key, burrow_login, and mtls require an http service");
    svc.access_mode = b.access_mode as typeof svc.access_mode;
    if (b.access_mode === "api_key" && b.api_key_header) svc.api_key_header = b.api_key_header;
    return noContent();
  }),

  // ---- per-service slug (the /svc/<slug>/ path segment; services:configure) ----
  http.put("/api/v1/services/:id/slug", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    if (svc.type === "tcp") return err(409, "slug requires an http service");
    const b = await body<{ slug?: string }>(request);
    const slug = b?.slug ?? "";
    if (!/^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$/.test(slug))
      return err(400, "slug must be 3-40 characters: lowercase letters, digits and hyphens, not starting or ending with a hyphen");
    if (db.services.some((s) => s.slug === slug && s.id !== svc.id)) return err(409, "slug already in use");
    svc.slug = slug;
    svc.url = `https://tunnels.example.com/svc/${slug}/`;
    return json({ slug, url: svc.url });
  }),

  // ---- gateway-only: no direct address, reachable through the AI gateway only ----
  http.put("/api/v1/services/:id/gateway-only", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    if (svc.type !== "http") return err(409, "gateway-only requires an http service");
    const b = await body<{ gateway_only?: unknown }>(request);
    if (typeof b?.gateway_only !== "boolean") return err(400, "gateway_only (boolean) is required");
    svc.gateway_only = b.gateway_only;
    return json({ gateway_only: svc.gateway_only });
  }),

  // ---- v0.3.0 per-service access policy (spec Part D; services:configure) ----
  http.get("/api/v1/services/:id/access-policy", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    return json({ roles: db.serviceAccessPolicy[svc.id] ?? [] });
  }),
  http.put("/api/v1/services/:id/access-policy", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    const b = await body<{ roles?: string[] }>(request);
    if (!b || !Array.isArray(b.roles)) return err(400, "roles is required");
    const known = new Set(db.roles.map((r) => r.name));
    for (const role of b.roles) if (!known.has(role)) return err(400, `unknown role "${role}"`);
    db.serviceAccessPolicy[svc.id] = [...b.roles];
    return noContent();
  }),

  // ---- tokens (Connect-a-client) ----
  http.get("/api/v1/tokens", ({ request }) => gate(request) ?? json(db.tokens)),
  http.post("/api/v1/tokens", async ({ request }) => {
    const g = gate(request); if (g) return g;
    const b = await body<{ name?: string; expires_at?: string }>(request);
    if (!b?.name) return err(400, "name is required");
    let expires: string | null = null;
    if (b.expires_at !== undefined) {
      const t = Date.parse(b.expires_at);
      if (isNaN(t) || t <= Date.now()) return err(400, "expires_at must be in the future");
      if (t > Date.now() + 365 * 86_400_000) return err(400, "expires_at must be at most 365 days ahead");
      expires = new Date(t).toISOString();
    }
    db.tokens.push({ id: `tok_${Math.random().toString(36).slice(2, 7)}`, name: b.name, last_used: null, created_at: new Date().toISOString(), expires_at: expires });
    return json({ name: b.name, token: `bur_${Math.random().toString(36).slice(2, 18)}`, expires_at: expires }, 201);
  }),

  // ---- events (inert SSE so Clients/Connect don't error) ----
  http.get("/api/v1/events", ({ request }) =>
    gate(request) ?? new HttpResponse("retry: 10000\n\n", { status: 200, headers: { "Content-Type": "text/event-stream" } })),

  // ---- tunnels: every service a connected client holds, as GET /clients/:id lists them ----
  http.get("/api/v1/tunnels", ({ request }) => gate(request) ?? json(db.clients.flatMap((c) => c.services.map((s) => {
    const svc = s.type === "http" ? db.services.find((x) => x.name === s.name && x.type === "http") : undefined;
    return {
      id: s.id, name: s.name, type: s.type, remote_port: s.remote_port, local_addr: s.local_addr,
      bytes_in: s.bytes_in, bytes_out: s.bytes_out, connected: true, access_mode: s.access_mode,
      ...(svc ? { service_id: svc.id, url: svc.url } : {}),
    };
  })))),

  // ---- AI providers ----
  // Identity comes from db.aiProviders; the mock joins seeded aiMeta +
  // aiModels + serviceApiKeys of the backing service.
  http.get("/api/v1/ai/providers", ({ request }) => {
    const g = gate(request); if (g) return g;
    return json(db.aiProviders.filter(providerVisible).map(providerView));
  }),
  http.post("/api/v1/ai/providers", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ slug?: string; name?: string; kind?: string; service_id?: string; gateway_only?: boolean } & UpstreamBody>(request);
    if (!b || typeof b !== "object") return err(400, "invalid JSON body");
    const unknown = unknownField(b, CREATE_FIELDS);
    if (unknown) return err(400, unknown);
    const name = (b.name ?? "").trim();
    const nameErr = providerNameError(name);
    if (nameErr) return err(400, nameErr);
    const kind = b.kind || "tunnel";
    if (kind !== "tunnel" && kind !== "direct") return err(400, "kind must be 'tunnel' or 'direct'");
    const limitErr = maxConcurrentError(b.max_concurrent);
    if (limitErr) return err(400, limitErr);
    const slug = b.slug || providerSlugFromName(name);
    if (!providerSlugOk(slug)) return err(400, PROVIDER_SLUG_RULE);
    if (kind === "direct") {
      const urlErr = baseUrlError(b.base_url ?? "");
      if (urlErr) return err(400, urlErr);
      const row: AiProviderRow = {
        slug, name, kind: "direct", service_id: `prov-${slug}`,
        api_format: (b.api_format ?? "") as AiProviderRow["api_format"],
        upstream_base_url: b.base_url,
        credential_slot: b.credential_slot ?? "",
        auth_header: b.auth_header,
        auth_format: b.auth_format,
        extra_headers: b.extra_headers ?? {},
        billing: b.billing as AiProviderRow["billing"],
        supports_responses: b.supports_responses ?? false,
        max_concurrent: b.max_concurrent ?? 0,
      };
      const reason = normalizeDirect(row);
      if (reason) return err(400, reason);
      if (db.aiProviders.some((p) => p.slug === slug)) return err(409, "provider slug or service already in use");
      // The backing service is created with the provider and owned by the caller.
      db.services.push({
        id: row.service_id, user_id: db.me.id, name, type: "direct", slug: "", url: "",
        access_mode: "api_key", api_key_header: "Authorization", gateway_only: false, connected: false, remote_port: 0, local_addr: "",
      });
      db.aiProviders.push(row);
      return json(providerView(row), 201);
    }
    if (b.supports_responses !== undefined) {
      return err(400, "supports_responses of a tunnel provider is set with PUT /api/v1/ai/providers/{slug}");
    }
    if (b.max_concurrent != null) {
      return err(400, "max_concurrent of a tunnel provider is set with PUT /api/v1/ai/providers/{slug}");
    }
    // The format the model server behind the client speaks; OpenAI when left out.
    const format = b.api_format || "openai";
    if (format !== "openai" && format !== "anthropic") return err(400, "api_format must be 'openai' or 'anthropic'");
    // An unknown service is the same conflict as one in the wrong mode.
    const svc = db.services.find((s) => s.id === b.service_id);
    if (!svc || svc.type !== "http" || svc.access_mode !== "api_key") {
      return err(409, PROVIDER_SERVICE);
    }
    if (db.aiProviders.some((p) => p.slug === slug || p.service_id === svc.id)) {
      return err(409, "provider slug or service already in use");
    }
    const row: AiProviderRow = { slug, name, kind: "tunnel", api_format: format, service_id: svc.id };
    db.aiProviders.push(row);
    if (b.gateway_only === true) svc.gateway_only = true;
    return json(providerView(row), 201);
  }),
  // Registered before /ai/providers/:slug so the longer paths win.
  http.put("/api/v1/ai/providers/:slug/upstream", async ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<UpstreamBody>(request);
    if (!b || typeof b !== "object") return err(400, "invalid JSON body");
    const unknown = unknownField(b, UPSTREAM_FIELDS);
    if (unknown) return err(400, unknown);
    const limitErr = maxConcurrentError(b.max_concurrent);
    if (limitErr) return err(400, limitErr);
    const p = db.aiProviders.find((x) => x.slug === params.slug);
    if (!p) return err(404, "provider not found");
    if (p.kind !== "direct") return err(409, "only direct providers have upstream settings");
    if (b.base_url !== undefined) {
      const urlErr = baseUrlError(b.base_url);
      if (urlErr) return err(400, urlErr);
    }
    // A field that is left out keeps its stored value; extra_headers replaces all.
    const next: AiProviderRow = {
      ...p,
      api_format: (b.api_format ?? p.api_format) as AiProviderRow["api_format"],
      upstream_base_url: b.base_url ?? p.upstream_base_url,
      credential_slot: b.credential_slot ?? p.credential_slot,
      auth_header: b.auth_header ?? p.auth_header,
      auth_format: b.auth_format ?? p.auth_format,
      extra_headers: b.extra_headers ?? p.extra_headers,
      billing: (b.billing ?? p.billing) as AiProviderRow["billing"],
      supports_responses: b.supports_responses ?? p.supports_responses,
      max_concurrent: b.max_concurrent ?? p.max_concurrent,
    };
    const reason = normalizeDirect(next);
    if (reason) return err(400, reason);
    Object.assign(p, next);
    return json(providerView(p));
  }),
  http.post("/api/v1/ai/providers/:slug/models/sync", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const p = db.aiProviders.find((x) => x.slug === params.slug);
    if (!p) return err(404, "provider not found");
    if (p.kind !== "direct") return err(409, "sync is available for direct providers");
    // The list is read with the first slot that is set.
    const slots = credentialSlots(p);
    if (slots.length > 1 && !slots.some((x) => x.present)) {
      return err(409, `none of the credential slots ${slots.map((x) => x.slot).join(", ")} is set`);
    }
    if (!slots.some((x) => x.present)) return err(409, `the credential slot ${p.credential_slot} is not set`);
    const synced_at = new Date().toISOString();
    // The answer replaces the stored list, hand-added ids included.
    db.aiProviderModels[p.slug] = [
      { id: "acme/large-1", display_name: "Acme Large 1", context_length: 200000, synced_at },
      { id: "acme/small-1", display_name: "Acme Small 1", context_length: 32000, synced_at },
    ];
    return json({ count: 2 });
  }),
  http.get("/api/v1/ai/providers/:slug/models", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const p = db.aiProviders.find((x) => x.slug === params.slug);
    if (!p || !providerVisible(p)) return err(404, "provider not found");
    return json((db.aiProviderModels[p.slug] ?? []).slice().sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)));
  }),
  http.post("/api/v1/ai/providers/:slug/models", async ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ id?: unknown }>(request);
    if (!b || typeof b !== "object") return err(400, "invalid JSON body");
    if (!modelIdOk(b.id)) return err(400, MSG_MODEL_ID);
    const p = db.aiProviders.find((x) => x.slug === params.slug);
    if (!p) return err(404, "provider not found");
    const list = (db.aiProviderModels[p.slug] ||= []);
    // Adding an id that is already listed changes nothing.
    if (!list.some((m) => m.id === b.id)) {
      list.push({ id: b.id, display_name: "", context_length: 0, synced_at: new Date().toISOString() });
    }
    return noContent();
  }),
  http.delete("/api/v1/ai/providers/:slug/models", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const id = new URL(request.url).searchParams.get("id") ?? "";
    if (!modelIdOk(id)) return err(400, MSG_MODEL_ID);
    const p = db.aiProviders.find((x) => x.slug === params.slug);
    if (!p) return err(404, "provider not found");
    const list = db.aiProviderModels[p.slug] ?? [];
    const i = list.findIndex((m) => m.id === id);
    if (i < 0) return err(404, "model not found");
    list.splice(i, 1);
    return noContent();
  }),
  http.get("/api/v1/ai/providers/:slug/metrics", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const p = db.aiProviders.find((x) => x.slug === params.slug);
    if (!p || !providerVisible(p)) return err(404, "provider not found");
    const meta = db.aiMeta[p.service_id];
    const requests = meta?.requests_24h ?? 0;
    const summary = db.costSummary.today;
    // Deterministic sinusoidal sparkline — enough to render a stable curve.
    const rpm: number[] = [];
    for (let i = 0; i < 60; i++) {
      rpm.push(Math.round(20 + 15 * Math.sin(i / 4)));
    }
    return json({
      requests_24h: requests,
      tokens_in_24h: summary.tokens_in,
      tokens_out_24h: summary.tokens_out,
      cost_usd_24h: summary.total_usd,
      cache_hit_ratio_24h: requests > 0 ? (meta?.cache_hits_24h ?? 0) / requests : 0,
      requests_per_minute: rpm,
    });
  }),
  http.get("/api/v1/ai/providers/:slug", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const p = db.aiProviders.find((x) => x.slug === params.slug);
    if (!p || !providerVisible(p)) return err(404, "provider not found");
    return json(providerView(p));
  }),
  http.put("/api/v1/ai/providers/:slug", async ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const p = db.aiProviders.find((x) => x.slug === params.slug);
    if (!p) return err(404, "provider not found");
    const b = await body<{ slug?: string; name?: string; supports_responses?: boolean; max_concurrent?: number | null }>(request);
    const name = (b?.name ?? "").trim();
    const nameErr = providerNameError(name);
    if (nameErr) return err(400, nameErr);
    const slug = b?.slug ?? "";
    if (!providerSlugOk(slug)) return err(400, PROVIDER_SLUG_RULE);
    const limitErr = maxConcurrentError(b?.max_concurrent);
    if (limitErr) return err(400, limitErr);
    // Optional; left out, the stored value stays.
    if (b?.supports_responses && p.api_format !== "openai") return err(400, RESPONSES_FORMAT);
    if (db.aiProviders.some((x) => x !== p && x.slug === slug)) {
      return err(409, "provider slug or service already in use");
    }
    if (slug !== p.slug && db.aiProviderModels[p.slug]) {
      db.aiProviderModels[slug] = db.aiProviderModels[p.slug]!;
      delete db.aiProviderModels[p.slug];
    }
    // Model targets follow a renamed provider.
    for (const m of db.aiModels) for (const t of m.targets) if (t.provider === p.slug) t.provider = slug;
    p.slug = slug;
    p.name = name;
    if (b?.supports_responses !== undefined) p.supports_responses = b.supports_responses;
    if (b?.max_concurrent != null) p.max_concurrent = b.max_concurrent;
    return json(providerView(p));
  }),
  http.delete("/api/v1/ai/providers/:slug", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const i = db.aiProviders.findIndex((x) => x.slug === params.slug);
    if (i < 0) return err(404, "provider not found");
    // A provider a synthetic model targets cannot go: the models are named.
    const users = db.aiModels.filter((m) => m.targets.some((t) => t.provider === params.slug)).map((m) => m.name);
    if (users.length > 0) return err(409, `provider is used by model(s): ${users.join(", ")}`);
    const [gone] = db.aiProviders.splice(i, 1);
    delete db.aiProviderModels[gone!.slug];
    // A direct provider takes its backing service, API keys and AI
    // configuration with it; a tunnel provider's service stays.
    if (gone!.kind === "direct") {
      db.services = db.services.filter((x) => x.id !== gone!.service_id);
      delete db.serviceApiKeys[gone!.service_id];
      delete db.aiConfigs[gone!.service_id];
    }
    return noContent();
  }),

  // ---- v0.4.0 cost summary (spec Part F) ----
  http.get("/api/v1/cost/summary", ({ request }) => {
    const g = gate(request); if (g) return g;
    const url = new URL(request.url);
    const w = (url.searchParams.get("window") ?? "today") as CostSummary["window"];
    const summary = db.costSummary[w] ?? db.costSummary.today;
    const groupBy = url.searchParams.get("group_by");
    if (groupBy === null || groupBy === "") return json(summary);
    if (!(groupBy in db.costGroups)) return err(400, "group_by must be one of gateway_key|model|provider|target_model|dialect");
    return json({ ...summary, group_by: groupBy, groups: sortGroups(db.costGroups[groupBy as CostGroupBy]) });
  }),

  // ---- v0.4.0 service AI config (spec Part B.7) ----
  http.get("/api/v1/services/:id/ai-config", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    return json(db.aiConfigs[svc.id] ?? null);
  }),
  http.put("/api/v1/services/:id/ai-config", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    const b = await body<ServiceAIConfig>(request);
    if (!b) return err(400, "invalid ai-config body");
    db.aiConfigs[svc.id] = b;
    return noContent();
  }),

  // ---- v0.4.0 inspector requests (spec Part E) ----
  http.get("/api/v1/services/:id/inspector/requests", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    const url = new URL(request.url);
    const limit = Math.max(1, Math.min(500, Number(url.searchParams.get("limit")) || 100));
    // Mirrors the relay: since (RFC3339) and q (substring) narrow before the limit applies.
    const since = url.searchParams.get("since") ?? "";
    const q = (url.searchParams.get("q") ?? "").toLowerCase();
    const rows = (db.inspectorEntries[svc.id] ?? [])
      .filter((r) => !since || r.ts >= since)
      .filter((r) => !q || `${r.path} ${r.method} ${r.req_body}`.toLowerCase().includes(q))
      .sort((a, b) => (a.ts < b.ts ? 1 : a.ts > b.ts ? -1 : 0))
      .slice(0, limit);
    return json(rows);
  }),
  http.post("/api/v1/services/:id/inspector/requests/:rid/replay", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const list = db.inspectorEntries[String(params.id)] ?? [];
    const orig = list.find((e) => e.id === params.rid);
    if (!orig) return err(404, "request not found");
    const replayed = { ...orig, id: `${orig.id}_replay_${Date.now()}`, ts: new Date().toISOString() };
    list.unshift(replayed);
    return json({ new_entry: replayed }, 201);
  }),
  http.post("/api/v1/services/:id/inspector/requests/:rid/replay-compare", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const list = db.inspectorEntries[String(params.id)] ?? [];
    const orig = list.find((e) => e.id === params.rid);
    if (!orig) return err(404, "request not found");
    // Wire shape must match internal/api/inspector_handlers.go: diff is the
    // OBJECT {headers:string[], body:string}, NOT a string. A string here let
    // a real React #31 crash (rendering the object in <pre>) ship undetected.
    return json({
      original: orig,
      replayed: orig,
      diff: { headers: [], body: "" },
    });
  }),
  http.get("/api/v1/services/:id/inspector/requests/:rid", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const list = db.inspectorEntries[String(params.id)] ?? [];
    const entry = list.find((e) => e.id === params.rid);
    if (!entry) return err(404, "request not found");
    return json(entry);
  }),

  // ---- v0.4.0 per-service cache controls (spec Part B.7) ----
  http.delete("/api/v1/services/:id/cache/entries", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    return noContent();
  }),

  // ---- v0.4.0 global cache settings (spec §4.21) ----
  http.get("/api/v1/cache/settings", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.cacheSettings)),
  http.put("/api/v1/cache/settings", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<CacheSettingsPayload>(request);
    if (!b) return err(400, "invalid cache settings body");
    db.cacheSettings = b;
    return noContent();
  }),
  http.get("/api/v1/cache/stats", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.cacheStats)),
  http.delete("/api/v1/cache/entries", ({ request }) =>
    gate(request, { admin: true }) ?? noContent()),
  // v0.5.0: admin-only wipe of semantic index
  http.delete("/api/v1/cache/semantic/entries", ({ request }) =>
    gate(request, { admin: true }) ?? noContent()),

  // ---- v0.4.0 per-service IP/geo (spec Part J) ----
  http.get("/api/v1/services/:id/ip-geo", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    const cfg = db.aiConfigs[svc.id];
    return json(
      cfg?.ip_geo ?? { enabled: false, allow_cidrs: [], block_cidrs: [], allow_countries: [], block_countries: [] },
    );
  }),
  http.put("/api/v1/services/:id/ip-geo", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    if (!canConfigure(svc)) return err(403, "forbidden");
    const b = await body<MockDb["aiConfigs"][string]["ip_geo"]>(request);
    if (!b) return err(400, "invalid ipgeo body");
    const existing = db.aiConfigs[svc.id];
    if (existing) existing.ip_geo = b;
    return noContent();
  }),
  http.get("/api/v1/geo/status", ({ request }) =>
    gate(request) ?? json({ enabled: true, db_path: "/var/burrow/geoip.mmdb", db_age_seconds: 3600 })),

  // ---- v0.4.0 guardrails & redaction (spec §4.22) ----
  http.get("/api/v1/redaction/rules", ({ request }) =>
    gate(request, { admin: true, needs: ["ai:read:own", "ai:read:any", "ai:configure:own", "ai:configure:any"] }) ?? json(db.redactionRules)),
  http.post("/api/v1/redaction/rules", async ({ request }) => {
    const g = gate(request, { admin: true, needs: ["ai:configure:any"] }); if (g) return g;
    const b = await body<{ name?: string; pattern?: string; action?: RedactionRule["action"]; scope?: RedactionRule["scope"] }>(request);
    if (!b?.name?.trim()) return err(400, "name is required");
    if (!b.pattern) return err(400, "pattern is required");
    try { new RegExp(b.pattern); } catch { return err(400, "invalid regex"); }
    if (!b.action || !["mask", "drop", "hash"].includes(b.action)) return err(400, "invalid action");
    if (!b.scope || !["request_body", "response_body", "both", "tool_arguments", "tool_result"].includes(b.scope)) return err(400, "invalid scope");
    const rule: RedactionRule = {
      id: crypto.randomUUID(), name: b.name.trim(), pattern: b.pattern,
      action: b.action, scope: b.scope,
    };
    db.redactionRules.custom.push(rule);
    return json(rule, 201);
  }),
  http.delete("/api/v1/redaction/rules/:id", ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["ai:configure:any"] }); if (g) return g;
    if (db.redactionRules.built_in.some((r) => r.id === params.id))
      return err(409, "built-in rules cannot be deleted");
    const i = db.redactionRules.custom.findIndex((r) => r.id === params.id);
    if (i < 0) return err(404, "rule not found");
    db.redactionRules.custom.splice(i, 1);
    return noContent();
  }),
  http.get("/api/v1/redaction/settings", ({ request }) =>
    gate(request, { admin: true, needs: ["ai:read:own", "ai:read:any", "ai:configure:own", "ai:configure:any"] }) ?? json(db.redactionSettings)),
  http.put("/api/v1/redaction/settings", async ({ request }) => {
    const g = gate(request, { admin: true, needs: ["ai:configure:any"] }); if (g) return g;
    const b = await body<MockDb["redactionSettings"]>(request);
    if (!b) return err(400, "invalid redaction settings");
    db.redactionSettings = b;
    return noContent();
  }),
  http.post("/api/v1/redaction/preview", async ({ request }) => {
    const g = gate(request, { admin: true, needs: ["ai:configure:any"] }); if (g) return g;
    const b = await body<{ sample?: string }>(request);
    return json({ matches: [{ rule: "email", count: b?.sample?.includes("@") ? 1 : 0 }] });
  }),
  http.get("/api/v1/guardrails/settings", ({ request }) =>
    gate(request, { admin: true, needs: ["ai:read:own", "ai:read:any", "ai:configure:own", "ai:configure:any"] }) ?? json(db.guardrailSettings)),
  http.put("/api/v1/guardrails/settings", async ({ request }) => {
    const g = gate(request, { admin: true, needs: ["ai:configure:any"] }); if (g) return g;
    // PUT accepts the FLAT global settings; GET returns them nested under `global`.
    const b = await body<Partial<GuardrailSettings>>(request);
    if (!b) return err(400, "invalid guardrail settings");
    // Like the relay: a field the body leaves out keeps its stored value.
    db.guardrailSettings = { global: { ...db.guardrailSettings.global, ...b }, per_service: db.guardrailSettings.per_service };
    return noContent();
  }),
  http.post("/api/v1/guardrails/preview", async ({ request }) => {
    const g = gate(request, { admin: true, needs: ["ai:configure:any"] }); if (g) return g;
    const b = await body<{ pattern?: string; text?: string }>(request);
    if (!b?.pattern) return err(400, 'custom pattern "preview": the pattern is empty');
    let re: RegExp;
    try { re = new RegExp(b.pattern, "g"); } catch { return err(400, 'custom pattern "preview": not a valid regular expression (Go RE2 syntax)'); }
    const all = [...(b.text ?? "").matchAll(re)].filter((m) => m[0] !== "");
    return json({
      matches: all.slice(0, 100).map((m) => ({ start: m.index ?? 0, end: (m.index ?? 0) + m[0].length, text: m[0] })),
      truncated: all.length > 100,
    });
  }),
  http.get("/api/v1/guardrails/patterns", ({ request }) =>
    gate(request, { admin: true, needs: ["ai:read:own", "ai:read:any", "ai:configure:own", "ai:configure:any"] }) ?? json(db.guardrailPatterns)),

  // ---- v0.4.0 cost/pricing (spec §4.24) ----
  http.get("/api/v1/cost/pricing", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.pricing)),
  http.put("/api/v1/cost/pricing", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<MockDb["pricing"]>(request);
    if (!b) return err(400, "invalid pricing table");
    db.pricing = b;
    return noContent();
  }),
  http.get("/api/v1/cost/export", ({ request }) => {
    const g = gate(request); if (g) return g;
    return new HttpResponse("# burrow cost export (stub)\n", {
      status: 200,
      headers: { "Content-Type": "text/plain" },
    });
  }),
  http.get("/api/v1/budgets", ({ request }) =>
    gate(request, { admin: true, needs: ["budgets:manage"] }) ?? json(db.budgets)),
  http.post("/api/v1/budgets", async ({ request }) => {
    const g = gate(request, { admin: true, needs: ["budgets:manage"] }); if (g) return g;
    const b = await body<Partial<MockDb["budgets"][number]>>(request);
    if (!b) return err(400, "invalid json");
    // Mirrors validateBudget and the unknown-gateway-key check in internal/api/cost_handlers.go.
    const scopes = ["api_key", "service", "user", "global", "gateway_key", "model"];
    const actions = ["alert_webhook", "throttle_zero", "disable_key"];
    const usd = b.daily_usd ?? 0, tokens = b.daily_tokens ?? 0, subject = b.subject_id ?? "", scope = b.scope ?? "api_key";
    const action = b.action_on_exceed ?? "alert_webhook";
    if (!scopes.includes(scope)) return err(400, "scope must be one of api_key|service|user|global|gateway_key|model");
    if (!actions.includes(action)) return err(400, "action_on_exceed must be one of alert_webhook|throttle_zero|disable_key");
    if (typeof tokens !== "number" || !Number.isInteger(tokens)) return err(400, "invalid json");
    if (tokens < 0) return err(400, "daily_tokens must not be negative");
    if (typeof usd !== "number" || usd < 0 || Number.isNaN(usd)) return err(400, "daily_usd must not be negative");
    if (usd === 0 && tokens === 0) return err(400, "daily_usd or daily_tokens must be greater than zero");
    if (scope !== "global" && subject.trim() === "") return err(400, "subject_id is required for non-global scopes");
    if (scope === "global" && subject !== "") return err(400, "global scope must not specify a subject_id");
    if (subject !== subject.trim()) return err(400, "subject_id must not have leading or trailing whitespace");
    // eslint-disable-next-line no-control-regex
    if (/[\u0000-\u001f\u007f-\u009f]/.test(subject)) return err(400, "subject_id must not contain control characters");
    if (new TextEncoder().encode(subject).length > 256) return err(400, "subject_id too long (max 256 chars)");
    if (scope === "gateway_key" && !db.aiGatewayKeys.some((k) => k.id === subject)) return err(400, "unknown gateway key");
    const rec: MockDb["budgets"][number] = {
      id: `bdg_${Math.random().toString(36).slice(2, 8)}`,
      scope,
      subject_id: subject,
      daily_usd: usd,
      daily_tokens: tokens,
      action_on_exceed: action,
      alert_webhook_id: b.alert_webhook_id ?? null,
      current_usd: 0,
      current_tokens: 0,
      exceeded: false,
    };
    db.budgets.push(rec);
    return json(rec, 201);
  }),
  http.delete("/api/v1/budgets/:id", ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["budgets:manage"] }); if (g) return g;
    const i = db.budgets.findIndex((b) => b.id === params.id);
    if (i < 0) return err(404, "budget not found");
    db.budgets.splice(i, 1);
    return noContent();
  }),

  // ---- v0.4.0 audit (spec §4.25) ----
  http.get("/api/v1/audit/events", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const url = new URL(request.url);
    const limit = Math.max(1, Math.min(500, Number(url.searchParams.get("limit")) || 100));
    const beforeId = url.searchParams.get("before_id");
    const q = (url.searchParams.get("q") ?? "").toLowerCase();
    const action = url.searchParams.get("action");
    let rows = db.audit
      .slice()
      .sort((a, b) => (a.ts < b.ts ? 1 : a.ts > b.ts ? -1 : 0));
    if (beforeId) {
      const i = rows.findIndex((e) => e.id === beforeId);
      if (i >= 0) rows = rows.slice(i + 1);
    }
    if (action) rows = rows.filter((e) => e.action === action);
    if (q) rows = rows.filter((e) => `${e.action} ${e.subject_label} ${e.actor_email}`.toLowerCase().includes(q));
    return json(rows.slice(0, limit));
  }),
  http.get("/api/v1/mcp/status", ({ request }) => gate(request, { admin: true, needs: ["mcp:tools:read"] }) ?? json(db.mcpStatus)),
  http.get("/api/v1/mcp/tools", ({ request }) => gate(request, { admin: true, needs: ["mcp:tools:read"] }) ?? json(db.mcpTools)),
  http.get("/api/v1/audit/fingerprint", ({ request }) =>
    gate(request, { admin: true }) ?? json({ public_key: "MIIBIjANBgkqhkiG…", fingerprint: "SHA256:deadbeef" })),
  http.get("/api/v1/audit/export", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    return new HttpResponse(db.audit.map((e) => JSON.stringify(e)).join("\n"), {
      status: 200,
      headers: { "Content-Type": "application/x-ndjson" },
    });
  }),
  http.post("/api/v1/audit/verify", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    if (db.audit.length === 0) return json({ ok: true, first_id: "", last_id: "" });
    const sorted = db.audit.slice().sort((a, b) => (a.ts < b.ts ? -1 : 1));
    return json({ ok: true, first_id: sorted[0]!.id, last_id: sorted.at(-1)!.id });
  }),

  // ---- v0.4.0 webhooks (spec §4.26) ----
  http.get("/api/v1/webhooks", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.webhooks)),
  http.post("/api/v1/webhooks", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<Partial<MockDb["webhooks"][number]>>(request);
    if (!b?.name || !b?.url) return err(400, "name and url are required");
    if (!b.url.startsWith("https://")) return err(400, "url must be https://");
    const wh: MockDb["webhooks"][number] = {
      id: `wh_${Math.random().toString(36).slice(2, 8)}`,
      name: b.name,
      url: b.url,
      events: b.events ?? [],
      paused: false,
      consecutive_failures: 0,
      first_failure_at: null,
      created_at: new Date().toISOString(),
    };
    db.webhooks.push(wh);
    return json({ webhook: wh, signing_secret: `whsec_${Math.random().toString(36).slice(2, 18)}` }, 201);
  }),
  http.delete("/api/v1/webhooks/:id", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const i = db.webhooks.findIndex((w) => w.id === params.id);
    if (i < 0) return err(404, "webhook not found");
    db.webhooks.splice(i, 1);
    return noContent();
  }),
  http.post("/api/v1/webhooks/:id/pause", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const wh = db.webhooks.find((w) => w.id === params.id);
    if (!wh) return err(404, "webhook not found");
    wh.paused = true;
    return noContent();
  }),
  http.post("/api/v1/webhooks/:id/resume", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const wh = db.webhooks.find((w) => w.id === params.id);
    if (!wh) return err(404, "webhook not found");
    wh.paused = false;
    return noContent();
  }),
  http.post("/api/v1/webhooks/:id/test", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const wh = db.webhooks.find((w) => w.id === params.id);
    if (!wh) return err(404, "webhook not found");
    return noContent();
  }),
  http.get("/api/v1/webhooks/deliveries", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const url = new URL(request.url);
    const limit = Math.max(1, Math.min(500, Number(url.searchParams.get("limit")) || 50));
    const webhookId = url.searchParams.get("webhook_id");
    let rows = db.webhookDeliveries.slice().sort((a, b) => (a.ts < b.ts ? 1 : -1));
    if (webhookId) rows = rows.filter((d) => d.webhook_id === webhookId);
    return json(rows.slice(0, limit));
  }),

  // ---- v0.5.0 webhook PUT + preview (spec Part H) ----
  http.put("/api/v1/webhooks/:id", async ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const wh = db.webhooks.find((w) => w.id === params.id);
    if (!wh) return err(404, "webhook not found");
    const b = await body<{ url?: string; events?: string[]; payload_template?: string }>(request);
    if (!b) return err(400, "invalid request body");
    // Validate template if provided
    if (typeof b.payload_template === "string" && b.payload_template !== "") {
      if (b.payload_template.includes('{{template "')) {
        return err(400, "template: nested template includes are forbidden");
      }
      const openCount = (b.payload_template.match(/\{\{/g) ?? []).length;
      const closeCount = (b.payload_template.match(/\}\}/g) ?? []).length;
      if (openCount !== closeCount) {
        return err(400, "template: unbalanced delimiters at line 1");
      }
    }
    if (b.url != null) wh.url = b.url;
    if (Array.isArray(b.events)) wh.events = b.events;
    if (typeof b.payload_template === "string") (wh as unknown as Record<string, unknown>)["payload_template"] = b.payload_template;
    return noContent();
  }),

  http.post("/api/v1/webhooks/:id/preview", async ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const wh = db.webhooks.find((w) => w.id === params.id);
    if (!wh) return err(404, "webhook not found");
    const b = await body<{ event?: string; fields?: Record<string, unknown>; payload_template?: string }>(request);
    if (!b) return err(400, "invalid request body");

    // Use the draft template from the request body if provided; fall back to stored template.
    const tpl = typeof b.payload_template === "string"
      ? b.payload_template
      : (wh as unknown as Record<string, unknown>)["payload_template"] as string | undefined ?? "";

    // Validate template
    if (tpl.includes('{{template "')) {
      return err(400, "template: nested template includes are forbidden");
    }
    const openCount = (tpl.match(/\{\{/g) ?? []).length;
    const closeCount = (tpl.match(/\}\}/g) ?? []).length;
    if (openCount !== closeCount) {
      return err(400, "template: unbalanced delimiters at line 1");
    }

    // Render: replace {{.FieldName}} with field values; empty string for unknown
    function renderTemplate(template: string, fields: Record<string, unknown>): string {
      if (!template) {
        // Default JSON body when no template is set
        return JSON.stringify({ event: b?.event ?? "", fields: fields ?? {} }, null, 2);
      }
      return template.replace(
        /\{\{\s*\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g,
        (_, name: string) => {
          if (name in fields) {
            const val = fields[name];
            if (typeof val === "string") return val;
            return JSON.stringify(val);
          }
          return "";
        },
      );
    }

    const rendered = renderTemplate(tpl, b.fields ?? {});
    const size_bytes = new TextEncoder().encode(rendered).length;
    return json({ rendered, size_bytes });
  }),

  // ---- v0.4.0 provisioning keys (§4.28 — pulled forward) ----
  http.get("/api/v1/provisioning/keys", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.provisioningKeys)),
  http.post("/api/v1/provisioning/keys", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ name?: string; scope?: string; expires_at?: string | null; default_role?: string }>(request);
    if (!b?.name) return err(400, "name is required");
    const pk: MockDb["provisioningKeys"][number] = {
      id: `pk_${Math.random().toString(36).slice(2, 8)}`,
      name: b.name,
      prefix: `bup_${Math.random().toString(36).slice(2, 7)}`,
      scope: (b.scope ?? "multi") as MockDb["provisioningKeys"][number]["scope"],
      expires_at: b.expires_at ?? null,
      default_role: b.default_role ?? "user",
      last_used: null,
      created_at: new Date().toISOString(),
    };
    db.provisioningKeys.push(pk);
    return json({ key: pk, plaintext: `${pk.prefix}_${Math.random().toString(36).slice(2, 22)}` }, 201);
  }),
  http.delete("/api/v1/provisioning/keys/:id", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const i = db.provisioningKeys.findIndex((k) => k.id === params.id);
    if (i < 0) return err(404, "provisioning key not found");
    db.provisioningKeys.splice(i, 1);
    return noContent();
  }),
  http.get("/api/v1/provisioning/pending", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.provisioningPending)),
  http.post("/api/v1/provisioning/pending/:id/approve", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const i = db.provisioningPending.findIndex((p) => p.id === params.id);
    if (i < 0) return err(404, "pending request not found");
    db.provisioningPending.splice(i, 1);
    return noContent();
  }),
  http.post("/api/v1/provisioning/pending/:id/reject", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const i = db.provisioningPending.findIndex((p) => p.id === params.id);
    if (i < 0) return err(404, "pending request not found");
    db.provisioningPending.splice(i, 1);
    return noContent();
  }),

  // ---- v0.4.0 automation tokens (spec Part M) ----
  http.get("/api/v1/automation/tokens", ({ request }) =>
    gate(request) ?? json(db.automationTokens.filter((t) => db.me.role === "admin" || t.user_id === db.me.id))),
  // The three presets of internal/automation, each cut to what the caller holds (the relay does the same).
  http.get("/api/v1/automation/presets", ({ request }) => {
    const g = gate(request); if (g) return g;
    const held = new Set(db.me.permissions ?? []);
    const cut = (list: string[]) => list.filter((p) => held.has(p));
    const notOperator = ["users:manage", "roles:manage", "tokens:manage:any", "automation:tokens:manage:any", "settings:manage", "backup:run"];
    return json([
      { name: "read-only", description: "Reads everything that has a read permission (not services or budgets), changes nothing.", permissions: cut(ALL_PERMISSIONS.filter((p) => p.includes(":read"))) },
      { name: "operator", description: "Everything except managing users, roles, settings, backups and other people's tokens.", permissions: cut(ALL_PERMISSIONS.filter((p) => !notOperator.includes(p))) },
      { name: "full", description: "Every permission you hold.", permissions: cut([...ALL_PERMISSIONS]) },
    ]);
  }),
  http.post("/api/v1/automation/tokens", async ({ request }) => {
    const g = gate(request); if (g) return g;
    const b = await body<{ name?: string; expires_at?: string | null; permissions?: string[] }>(request);
    if (!b?.name) return err(400, "name is required");
    const t: MockDb["automationTokens"][number] = {
      id: `at_${Math.random().toString(36).slice(2, 8)}`,
      name: b.name,
      prefix: `bua_${Math.random().toString(36).slice(2, 7)}`,
      user_id: db.me.id,
      role_at_mint: db.me.role,
      permissions: b.permissions ?? [],
      expires_at: b.expires_at ?? null,
      last_used: null,
      created_at: new Date().toISOString(),
    };
    db.automationTokens.push(t);
    return json({ token: t, plaintext: `${t.prefix}_${Math.random().toString(36).slice(2, 22)}` }, 201);
  }),
  http.delete("/api/v1/automation/tokens/:id", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const i = db.automationTokens.findIndex((t) => t.id === params.id);
    if (i < 0) return err(404, "token not found");
    db.automationTokens.splice(i, 1);
    return noContent();
  }),

  // ---- v0.4.0 backups (spec Part L) ----
  http.get("/api/v1/backups", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.backups)),
  http.post("/api/v1/backups", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const id = `bk_${Math.random().toString(36).slice(2, 8)}`;
    const ts = new Date().toISOString();
    db.backups.push({
      id, taken_at: ts, version: "v0.4.0", size_bytes: 1024 * 1024,
      db_sha256: "0".repeat(64), path: `/var/burrow/backups/${id}.tar.gz`,
    });
    return json({ id, started_at: ts }, 202);
  }),
  http.get("/api/v1/backups/:id/download", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const bk = db.backups.find((b) => b.id === params.id);
    if (!bk) return err(404, "backup not found");
    return new HttpResponse("(backup blob stub)", {
      status: 200,
      headers: { "Content-Type": "application/gzip" },
    });
  }),
  http.post("/api/v1/backups/:id/verify", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const bk = db.backups.find((b) => b.id === params.id);
    if (!bk) return err(404, "backup not found");
    return json({ ok: true, sha256_match: true });
  }),
  http.delete("/api/v1/backups/:id", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const i = db.backups.findIndex((b) => b.id === params.id);
    if (i < 0) return err(404, "backup not found");
    db.backups.splice(i, 1);
    return noContent();
  }),
  http.post("/api/v1/backups/restore", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    return json({ restore_id: `rs_${Math.random().toString(36).slice(2, 8)}`, started_at: new Date().toISOString() }, 202);
  }),

  // ---- AI gateway: endpoints, synthetic models, gateway keys ----
  http.get("/api/v1/ai/gateway", ({ request }) => gate(request) ?? json({
    endpoints: [
      { dialect: "openai", base_url: "https://tunnels.example.com/openai/v1" },
      { dialect: "anthropic", base_url: "https://tunnels.example.com/anthropic" },
    ],
  })),
  http.get("/api/v1/ai/models", ({ request }) => gate(request) ?? json(db.aiModels.map(modelView))),
  http.post("/api/v1/ai/models", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const next = modelFromBody(await body<ModelBody>(request));
    if (typeof next === "string") return err(400, next);
    if (db.aiModels.some((m) => m.name === next.name)) return err(409, "model name already in use");
    const now = new Date().toISOString();
    const row: AiModel = { ...next, created_at: now, updated_at: now };
    db.aiModels.push(row);
    return json(modelView(row), 201);
  }),
  http.get("/api/v1/ai/models/:name", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const m = db.aiModels.find((x) => x.name === params.name);
    return m ? json(modelView(m)) : err(404, "model not found");
  }),
  http.put("/api/v1/ai/models/:name", async ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const i = db.aiModels.findIndex((x) => x.name === params.name);
    if (i < 0) return err(404, "model not found");
    const next = modelFromBody(await body<ModelBody>(request), db.aiModels[i]);
    if (typeof next === "string") return err(400, next);
    if (db.aiModels.some((m, at) => at !== i && m.name === next.name)) return err(409, "model name already in use");
    const row: AiModel = { ...next, created_at: db.aiModels[i]!.created_at, updated_at: new Date().toISOString() };
    db.aiModels[i] = row;
    return json(modelView(row));
  }),
  http.delete("/api/v1/ai/models/:name", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const i = db.aiModels.findIndex((x) => x.name === params.name);
    if (i < 0) return err(404, "model not found");
    db.aiModels.splice(i, 1);
    return noContent();
  }),
  // The captured requests of every provider the caller sees, as one list:
  // newest first, without headers and bodies. An entry that names no provider
  // is given the one whose buffer holds it.
  http.get("/api/v1/ai/requests", ({ request }) => {
    const g = gate(request); if (g) return g;
    const url = new URL(request.url);
    const param = (name: string) => url.searchParams.get(name) ?? "";
    let limit = 200;
    if (url.searchParams.has("limit")) {
      limit = Number(param("limit"));
      if (!Number.isInteger(limit) || limit < 1) return err(400, "limit must be a positive integer");
      limit = Math.min(limit, 1000);
    }
    const [provider, model, key, q, status, since, until] = ["provider", "model", "gateway_key", "q", "status", "since", "until"].map(param);
    const rows = db.aiProviders.filter(providerVisible)
      .flatMap((p) => (db.inspectorEntries[p.service_id] ?? []).map((e) => {
        const { req_headers: _qh, req_body: _qb, resp_headers: _ph, resp_body: _pb, ...row } = e;
        return { ...row, provider: e.provider || p.slug };
      }))
      .filter((r) => !provider || r.provider === provider)
      .filter((r) => !model || r.model === model)
      .filter((r) => !key || r.gateway_key_id === key)
      .filter((r) => !status || r.status === Number(status))
      .filter((r) => !since || r.ts >= since)
      .filter((r) => !until || r.ts <= until)
      // Case-sensitive, as on the relay.
      .filter((r) => !q || [r.method, r.path, r.provider, r.model].some((text) => text.includes(q)))
      .sort((a, b) => (a.ts < b.ts ? 1 : a.ts > b.ts ? -1 : 0))
      .slice(0, limit);
    return json(rows);
  }),
  // What one gateway request tried: an admin's dashboard session. An id
  // without a log is an empty list.
  http.get("/api/v1/ai/requests/:requestID/attempts", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    // MSW hands the path parameter over decoded; decoding it again would
    // read "a%252Fb" as "a/b" and throw on "100%25".
    const id = String(params.requestID);
    if (id === "" || byteLen(id) > 128 || hasControl(id)) return err(400, "request id must be 1-128 characters without control characters");
    const rows: AiRequestAttempt[] = db.aiAttempts[id] ?? [];
    return json([...rows].sort((a, b) => a.position - b.position));
  }),
  // A caller sees their own keys; ai:read:any or ai:configure:any sees all. Revoked keys stay listed.
  http.get("/api/v1/ai/keys", ({ request }) =>
    gate(request) ?? json(db.aiGatewayKeys.filter((k) => holds("ai:read:any", "ai:configure:any") || k.user_id === db.me.id))),
  http.post("/api/v1/ai/keys", async ({ request }) => {
    const g = gate(request); if (g) return g;
    const b = await body<{ name?: unknown; allowed_models?: unknown; allowed_tools?: unknown; tools_read_only?: unknown; system_text?: unknown }>(request);
    if (!b || typeof b !== "object") return err(400, "invalid JSON body");
    const extra = Object.keys(b).find((k) => !["name", "allowed_models", "allowed_tools", "tools_read_only", "system_text"].includes(k));
    if (extra) return err(400, `unknown field "${extra}"`);
    const name = typeof b.name === "string" ? b.name.trim() : "";
    if (name === "" || byteLen(name) > 120 || hasControl(name)) return err(400, "name must be 1-120 characters without control characters");
    if (b.system_text !== undefined && typeof b.system_text !== "string") return err(400, "invalid JSON body");
    const systemText = b.system_text ?? "";
    if (byteLen(systemText) > 8192) return err(400, "system_text is longer than 8192 bytes");
    const allowed = b.allowed_models ?? [];
    if (!Array.isArray(allowed)) return err(400, "invalid JSON body");
    if (allowed.length > 64) return err(400, "at most 64 allowed models");
    if (allowed.some((e) => typeof e !== "string" || !allowEntryOk(e))) {
      return err(400, 'an allowed model must be a model name, "<provider>/<model>" or "<provider>/*"');
    }
    const tools = toolListOf(b.allowed_tools);
    if (typeof tools === "string") return err(400, tools);
    if (b.tools_read_only !== undefined && typeof b.tools_read_only !== "boolean") return err(400, "invalid JSON body");
    const row: AiGatewayKey = {
      id: `gk_${Math.random().toString(36).slice(2, 10)}`,
      name,
      key_prefix: "bgw_xxxx",
      user_id: db.me.id,
      allowed_models: allowed as string[],
      allowed_tools: tools,
      tools_read_only: b.tools_read_only === true,
      system_text: systemText,
      last_used: null,
      created_at: new Date().toISOString(),
      revoked_at: null,
    };
    db.aiGatewayKeys.push(row);
    // The plaintext leaves the server this once and must not be stored on the way.
    return HttpResponse.json({ ...row, key: "bgw_" + "x".repeat(43) }, { status: 201, headers: { "Cache-Control": "no-store" } });
  }),
  // Name and system text only; a field left out stays. Another user's key answers like a missing one.
  http.put("/api/v1/ai/keys/:id", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const k = db.aiGatewayKeys.find((x) => x.id === params.id);
    if (!k || (!db.me.permissions?.includes("ai:configure:any") && k.user_id !== db.me.id)) return err(404, "key not found");
    const b = await body<{ name?: unknown; system_text?: unknown; allowed_models?: unknown; allowed_tools?: unknown; tools_read_only?: unknown }>(request);
    if (!b || typeof b !== "object") return err(400, "invalid JSON body");
    const extra = Object.keys(b).find((f) => !["name", "system_text", "allowed_models", "allowed_tools", "tools_read_only"].includes(f));
    if (extra) return err(400, `unknown field "${extra}"`);
    // Absent = keep; a list that is given replaces the stored one.
    if (b.allowed_models !== undefined) {
      if (!Array.isArray(b.allowed_models) || b.allowed_models.length > 64) return err(400, "at most 64 allowed models");
      if (b.allowed_models.some((e) => typeof e !== "string" || !allowEntryOk(e))) {
        return err(400, 'an allowed model must be a model name, "<provider>/<model>" or "<provider>/*"');
      }
      k.allowed_models = b.allowed_models as string[];
    }
    if (b.allowed_tools !== undefined) {
      const tools = toolListOf(b.allowed_tools);
      if (typeof tools === "string") return err(400, tools);
      k.allowed_tools = tools;
    }
    if (b.tools_read_only !== undefined) {
      if (typeof b.tools_read_only !== "boolean") return err(400, "invalid JSON body");
      k.tools_read_only = b.tools_read_only;
    }
    if (b.name !== undefined) {
      const name = typeof b.name === "string" ? b.name.trim() : "";
      if (name === "" || byteLen(name) > 120 || hasControl(name)) return err(400, "name must be 1-120 characters without control characters");
      k.name = name;
    }
    if (b.system_text !== undefined) {
      if (typeof b.system_text !== "string") return err(400, "invalid JSON body");
      if (byteLen(b.system_text) > 8192) return err(400, "system_text is longer than 8192 bytes");
      k.system_text = b.system_text;
    }
    return json(k);
  }),
  http.delete("/api/v1/ai/keys/:id", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const k = db.aiGatewayKeys.find((x) => x.id === params.id);
    // Another user's key answers like a missing one.
    if (!k || (!db.me.permissions?.includes("ai:configure:any") && k.user_id !== db.me.id)) return err(404, "key not found");
    k.revoked_at ??= new Date().toISOString();
    return noContent();
  }),

  // ---- v0.5.0 custom domains (spec Part D) ----
  http.get("/api/v1/services/:id/domains", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    const rows = db.customDomains
      .filter((d) => d.service_id === params.id)
      .slice()
      .sort((a, b) => (a.created_at < b.created_at ? 1 : a.created_at > b.created_at ? -1 : 0));
    return json(rows);
  }),
  http.post("/api/v1/services/:id/domains", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    const b = await body<CreateCustomDomainInput>(request);
    if (!b) return err(400, "invalid body");

    // Mock-validator (plan §Task 6 Step 1).
    if (b.hostname === "san-mismatch.example.com")
      return json({ error: "san_mismatch", reason: "san_mismatch" }, 400);
    if (b.cert_pem === "")
      return json({ error: "chain invalid", reason: "chain_invalid" }, 400);
    if (b.hostname === "expired.example.com")
      return json({ error: "expired", reason: "expired" }, 400);
    if (b.hostname === "key-mismatch.example.com")
      return json({ error: "key mismatch", reason: "key_mismatch" }, 400);

    const now = Date.now();
    const notAfter = new Date(now + 90 * 24 * 60 * 60 * 1000).toISOString();
    const notBefore = new Date(now).toISOString();
    const status: CustomDomain["status"] = "active";
    const row: CustomDomain = {
      id: `dom_${Math.random().toString(36).slice(2, 9)}`,
      service_id: String(params.id),
      hostname: b.hostname,
      cert_sha256: `mock_${b.hostname.replace(/\W/g, "_")}`,
      not_before: notBefore,
      not_after: notAfter,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
      status,
      status_updated_at: new Date().toISOString(),
    };
    db.customDomains.push(row);
    return json(row, 201);
  }),
  http.get("/api/v1/services/:id/domains/:did", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const d = db.customDomains.find((x) => x.id === params.did && x.service_id === params.id);
    if (!d) return err(404, "domain not found");
    return json(d);
  }),
  http.put("/api/v1/services/:id/domains/:did", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const d = db.customDomains.find((x) => x.id === params.did && x.service_id === params.id);
    if (!d) return err(404, "domain not found");
    return noContent();
  }),
  http.delete("/api/v1/services/:id/domains/:did", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const i = db.customDomains.findIndex((x) => x.id === params.did && x.service_id === params.id);
    if (i < 0) return err(404, "domain not found");
    db.customDomains.splice(i, 1);
    return noContent();
  }),

  // ---- v0.5.0 upstream credentials (spec Part B) ----
  http.get("/api/v1/upstream-credentials/slots", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    return json({ slots: db.upstreamSlots });
  }),
  http.get("/api/v1/services/:id/upstream-credential", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const binding = db.upstreamBindings[String(params.id)];
    if (!binding) return json({ slot_present: false });
    return json({
      ...binding,
      slot_present: !db.absentSlots.has(binding.slot),
    });
  }),
  http.put("/api/v1/services/:id/upstream-credential", async ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    const b = await body<{ slot?: string; header_name?: string; header_format?: string }>(request);
    if (!b?.slot) return err(400, "slot is required");
    if (!db.upstreamSlots.includes(b.slot)) return err(400, "unknown slot");
    const fmt = b.header_format ?? "Bearer {key}";
    if (!fmt.includes("{key}")) return err(400, "invalid header_format");
    db.upstreamBindings[String(params.id)] = {
      service_id: String(params.id),
      slot: b.slot,
      header_name: b.header_name ?? "Authorization",
      header_format: fmt,
      slot_present: !db.absentSlots.has(b.slot),
    };
    return noContent();
  }),
  http.delete("/api/v1/services/:id/upstream-credential", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    delete db.upstreamBindings[String(params.id)];
    return noContent();
  }),

  // ---- v0.5.0 retention settings (spec Part F) ----
  http.get("/api/v1/settings/retention", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.retentionSettings)),
  http.put("/api/v1/settings/retention", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<Partial<RetentionSettings>>(request);
    if (!b) return err(400, "invalid body");
    // Range validation per spec Part F.
    if (b.audit_retention_days != null && (b.audit_retention_days < 0 || b.audit_retention_days > 3650))
      return err(400, "audit_retention_days out of range");
    if (b.usage_retention_days != null && (b.usage_retention_days < 1 || b.usage_retention_days > 3650))
      return err(400, "usage_retention_days out of range");
    if (b.redaction_retention_days != null && (b.redaction_retention_days < 1 || b.redaction_retention_days > 3650))
      return err(400, "redaction_retention_days out of range");
    if (b.connection_logs_retention_days != null && (b.connection_logs_retention_days < 1 || b.connection_logs_retention_days > 3650))
      return err(400, "connection_logs_retention_days out of range");
    if (b.connection_logs_rollups_retention_days != null && (b.connection_logs_rollups_retention_days < 0 || b.connection_logs_rollups_retention_days > 3650))
      return err(400, "connection_logs_rollups_retention_days out of range");
    if (b.webhook_deliveries_retention_days != null && (b.webhook_deliveries_retention_days < 1 || b.webhook_deliveries_retention_days > 365))
      return err(400, "webhook_deliveries_retention_days out of range");
    if (b.inspector_retention_count != null && (b.inspector_retention_count < 1 || b.inspector_retention_count > 1000))
      return err(400, "inspector_retention_count out of range");
    // Store (keep note field immutable from API side).
    const note = db.retentionSettings.audit_retention_note;
    db.retentionSettings = { ...db.retentionSettings, ...b, audit_retention_note: note };
    return noContent();
  }),

  // ---- v0.5.0 database status (spec Part G) ----
  http.get("/api/v1/database", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.databaseStatus)),

  // ---- v0.5.0 connection logs (spec Part E) ----
  http.get("/api/v1/connection-logs", ({ request }) => {
    const g = gate(request, { admin: true, needs: ["audit:read"] }); if (g) return g;
    const url = new URL(request.url);
    const kindFilter = url.searchParams.get("kind") ?? "";
    const serviceId = url.searchParams.get("service_id") ?? "";
    const since = url.searchParams.get("since") ?? "";
    const until = url.searchParams.get("until") ?? "";
    const q = (url.searchParams.get("q") ?? "").toLowerCase();
    const limit = Math.max(1, Math.min(500, Number(url.searchParams.get("limit")) || 50));
    const beforeId = url.searchParams.get("before_id") ?? "";
    let rows = db.connectionLogs
      .slice()
      .sort((a, b) => (a.started_at < b.started_at ? 1 : a.started_at > b.started_at ? -1 : 0));
    if (kindFilter) rows = rows.filter((r) => r.kind === kindFilter);
    if (serviceId) rows = rows.filter((r) => r.service_id === serviceId);
    if (since) rows = rows.filter((r) => r.started_at >= since);
    if (until) rows = rows.filter((r) => r.started_at <= until);
    if (q) rows = rows.filter((r) =>
      `${r.source_ip} ${r.kind} ${r.service_id} ${r.status}`.toLowerCase().includes(q),
    );
    if (beforeId) {
      const i = rows.findIndex((r) => r.id === beforeId);
      if (i >= 0) rows = rows.slice(i + 1);
    }
    return json(rows.slice(0, limit));
  }),

  http.get("/api/v1/connection-logs/rollups", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const url = new URL(request.url);
    const serviceId = url.searchParams.get("service_id") ?? "";
    const kindFilter = url.searchParams.get("kind") ?? "";
    const since = url.searchParams.get("since") ?? "";
    const until = url.searchParams.get("until") ?? "";
    let rows = db.connectionLogRollups.slice();
    if (serviceId) rows = rows.filter((r) => r.service_id === serviceId);
    if (kindFilter) rows = rows.filter((r) => r.kind === kindFilter);
    if (since) rows = rows.filter((r) => r.day >= since.slice(0, 10));
    if (until) rows = rows.filter((r) => r.day <= until.slice(0, 10));
    return json(rows);
  }),

  http.get("/api/v1/connection-logs/export", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const lines = db.connectionLogs.map((r) => JSON.stringify(r)).join("\n");
    return new HttpResponse(lines || '{"stub":true}', {
      status: 200,
      headers: { "Content-Type": "text/plain" },
    });
  }),

  // ---- OAuth: consent, connected apps, pre-registered clients ----
  http.get("/api/v1/oauth/requests/:id", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const r = db.oauthRequests.find((x) => x.id === params.id);
    return r ? json(r) : err(404, "not found");
  }),
  http.post("/api/v1/oauth/requests/:id/approve", async ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const r = db.oauthRequests.find((x) => x.id === params.id);
    if (!r) return err(404, "not found");
    const b = (await body<OAuthApprove>(request)) ?? {};
    if (r.password_required) {
      if (typeof b.password === "string" && b.password !== "") {
        if (b.password !== "password123") return err(403, "password is incorrect");
      } else if (!db.reauthConfirmed) {
        return err(403, "reauth_required");
      }
    }
    if (r.resource_kind !== "burrow" && !r.can_grant_tools) return err(403, "ai:configure required to grant tools");
    db.oauthRequests = db.oauthRequests.filter((x) => x !== r);
    db.oauthGrants.push({
      id: `og_${db.oauthGrants.length + 1}`, client_id: r.client_id, client_name: r.client_name, user_id: db.me.id, user_email: db.me.email,
      resource: r.resource, scopes: b.scopes ?? r.scopes, allowed_tools: b.allowed_tools ?? [], tools_read_only: b.tools_read_only ?? true,
      created_at: new Date().toISOString(), last_used: null,
    });
    return json({ redirect_url: `${r.redirect_uri}?code=abc&state=xyz&iss=https%3A%2F%2Frelay.example.com` });
  }),
  http.post("/api/v1/oauth/requests/:id/deny", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const r = db.oauthRequests.find((x) => x.id === params.id);
    if (!r) return err(404, "not found");
    db.oauthRequests = db.oauthRequests.filter((x) => x !== r);
    return json({ redirect_url: `${r.redirect_uri}?error=access_denied&state=xyz&iss=https%3A%2F%2Frelay.example.com` });
  }),
  http.get("/api/v1/oauth/grants", ({ request }) => {
    const g = gate(request); if (g) return g;
    const all = ["1", "true"].includes(new URL(request.url).searchParams.get("all") ?? "");
    if (all && !db.me.permissions?.includes("oauth:clients:manage")) return err(403, "forbidden");
    return json(db.oauthGrants.filter((x) => all || x.user_id === db.me.id));
  }),
  http.delete("/api/v1/oauth/grants/:id", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const x = db.oauthGrants.find((v) => v.id === params.id);
    if (!x || (x.user_id !== db.me.id && !db.me.permissions?.includes("oauth:clients:manage"))) return err(404, "not found");
    db.oauthGrants = db.oauthGrants.filter((v) => v !== x);
    return noContent();
  }),
  http.get("/api/v1/oauth/clients", ({ request }) => {
    const g = gate(request, { admin: true, needs: ["oauth:clients:manage"] }); if (g) return g;
    return json(db.oauthClients);
  }),
  http.post("/api/v1/oauth/clients", async ({ request }) => {
    const g = gate(request, { admin: true, needs: ["oauth:clients:manage"] }); if (g) return g;
    const b = await body<{ name?: string; redirect_uris?: string[] }>(request);
    if (!b?.name?.trim()) return err(400, "name is required");
    if (!Array.isArray(b.redirect_uris) || b.redirect_uris.length === 0) return err(400, "redirect_uris is required");
    const bad = b.redirect_uris.find((u) => !/^(https:\/\/|http:\/\/(127\.0\.0\.1|localhost|\[::1\])|[a-z][a-z0-9+-]*\.[a-z0-9.+-]*:\/)/i.test(u));
    if (bad) return err(400, `redirect URI ${bad} must be https, http on a loopback address or an app scheme`);
    const row: OAuthClient = {
      client_id: `bcl_${(db.oauthClients.length + 1).toString(16).padStart(32, "0")}`, name: b.name.trim(), redirect_uris: b.redirect_uris,
      created_by: db.me.email, created_at: new Date().toISOString(),
    };
    db.oauthClients.push(row);
    return json(row, 201);
  }),
  http.delete("/api/v1/oauth/clients/:id", ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["oauth:clients:manage"] }); if (g) return g;
    const c = db.oauthClients.find((x) => x.client_id === params.id);
    if (!c) return err(404, "not found");
    db.oauthClients = db.oauthClients.filter((x) => x !== c);
    db.oauthGrants = db.oauthGrants.filter((x) => x.client_id !== c.client_id);
    return noContent();
  }),

  // ---- Tool sources (MCP gateway) ----
  // Reads need mcp:sources:read or :manage; every write needs :manage (the admin role holds both).
  http.get("/api/v1/mcp/sources", ({ request }) => {
    const g = gate(request, { admin: true, needs: ["mcp:sources:read", "mcp:sources:manage"] }); if (g) return g;
    return json([...db.toolSources].sort((a, b) => a.slug.localeCompare(b.slug)).map(sourceWire));
  }),
  http.post("/api/v1/mcp/sources", async ({ request }) => {
    const g = gate(request, { admin: true, needs: ["mcp:sources:manage"] }); if (g) return g;
    const b = await body<ToolSourceCreate & Record<string, unknown>>(request);
    if (!b || typeof b !== "object") return err(400, "invalid JSON body");
    const known = ["slug", "name", "kind", "service_id", "url", "credential_slot", "auth_header", "auth_format", "protocol_version", "rpm", "max_concurrent", "timeout_s", "enabled"];
    const extra = Object.keys(b).find((k) => !known.includes(k));
    if (extra) return err(400, `unknown field "${extra}"`);
    const slug = b.slug ?? "";
    if (["burrow", "v1"].includes(slug)) return err(400, `slug "${slug}" is reserved`);
    if (!/^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$/.test(slug)) return err(400, "slug must be 1-24 characters of a-z, 0-9 and hyphens, starting and ending with a letter or digit");
    if (b.kind !== "remote" && b.kind !== "tunnel") return err(400, "kind must be 'remote' or 'tunnel'");
    if (db.toolSources.some((s) => s.slug === slug)) return err(409, "a tool source with this slug already exists");
    let serviceId = "";
    if (b.kind === "remote") {
      if (!/^https:\/\/[^/\s?#@]+/.test(b.url ?? "")) return err(400, "url must be an https URL");
      serviceId = `mcp-${slug}`;
    } else {
      if (b.url || b.credential_slot) return err(400, "url and credential fields belong to a remote source");
      const svc = db.services.find((s) => s.id === b.service_id);
      if (!svc || svc.type !== "http") return err(409, "service_id must be an existing http service");
      if (db.toolSources.some((s) => s.service_id === svc.id)) return err(409, "the service is already used by a tool source");
      svc.gateway_only = true;
      serviceId = svc.id;
    }
    const now = new Date().toISOString();
    const row: ToolSource = {
      slug, name: (b.name ?? "").trim() || slug, kind: b.kind, service_id: serviceId,
      endpoint_url: `https://relay.example.com/mcp/${slug}`, url: b.kind === "remote" ? (b.url ?? "") : "",
      credential_slot: b.credential_slot ?? "", credential_present: false,
      auth_header: b.kind === "remote" ? (b.auth_header || "Authorization") : "",
      auth_format: b.kind === "remote" ? (b.auth_format || "Bearer {key}") : "",
      protocol_version: b.protocol_version ?? "", rpm: b.rpm ?? 0, max_concurrent: b.max_concurrent ?? 0, timeout_s: b.timeout_s ?? 60,
      enabled: b.enabled ?? true, last_sync_at: null, last_sync_error: "", tool_count: 0, pending_count: 0, created_at: now, updated_at: now,
    };
    db.toolSources.push(row);
    db.toolSourceTools[slug] = [];
    return json(sourceWire(row), 201);
  }),
  http.get("/api/v1/mcp/sources/:slug", ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["mcp:sources:read", "mcp:sources:manage"] }); if (g) return g;
    const s = db.toolSources.find((x) => x.slug === params.slug);
    return s ? json(sourceWire(s)) : err(404, "tool source not found");
  }),
  http.put("/api/v1/mcp/sources/:slug", async ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["mcp:sources:manage"] }); if (g) return g;
    const s = db.toolSources.find((x) => x.slug === params.slug);
    if (!s) return err(404, "tool source not found");
    const b = await body<ToolSourceUpdate & Record<string, unknown>>(request);
    if (!b || typeof b !== "object") return err(400, "invalid JSON body");
    const known = ["name", "url", "credential_slot", "auth_header", "auth_format", "protocol_version", "rpm", "max_concurrent", "timeout_s", "enabled"];
    const extra = Object.keys(b).find((k) => !known.includes(k));
    if (extra) return err(400, `unknown field "${extra}"`);
    if (b.timeout_s !== undefined && (b.timeout_s < 1 || b.timeout_s > 300)) return err(400, "timeout_s must be between 1 and 300");
    if (b.rpm !== undefined && (b.rpm < 0 || b.rpm > 100000)) return err(400, "rpm must be between 0 and 100000");
    if (b.max_concurrent !== undefined && (b.max_concurrent < 0 || b.max_concurrent > 1000)) return err(400, "max_concurrent must be between 0 and 1000");
    Object.assign(s, b, { updated_at: new Date().toISOString() });
    return json(sourceWire(s));
  }),
  http.delete("/api/v1/mcp/sources/:slug", ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["mcp:sources:manage"] }); if (g) return g;
    const s = db.toolSources.find((x) => x.slug === params.slug);
    if (!s) return err(404, "tool source not found");
    db.toolSources = db.toolSources.filter((x) => x !== s);
    delete db.toolSourceTools[s.slug];
    return noContent();
  }),
  http.post("/api/v1/mcp/sources/:slug/sync", ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["mcp:sources:manage"] }); if (g) return g;
    const s = db.toolSources.find((x) => x.slug === params.slug);
    if (!s) return err(404, "tool source not found");
    const svc = db.services.find((x) => x.id === s.service_id);
    if (s.kind === "tunnel" && !svc?.connected) {
      s.last_sync_error = "the client that serves this source is not connected";
      return err(502, s.last_sync_error);
    }
    const tools = db.toolSourceTools[s.slug] ?? (db.toolSourceTools[s.slug] = []);
    const offered = db.toolUpstream[s.slug] ?? tools.filter((t) => t.status !== "removed").map((t) => t.definition);
    const result: ToolSyncResult = { added: 0, changed: 0, removed: 0, tool_count: 0, pending_count: 0, skipped: [] };
    const seen = new Set<string>();
    for (const def of offered) {
      const name = def.name ?? "";
      seen.add(name);
      const hash = mockToolHash(def);
      const hit = (def.description ?? "").toLowerCase().includes(MOCK_TOOL_PATTERN)
        ? [{ field: "description" as const, pattern: MOCK_TOOL_PATTERN }] : [];
      const have = tools.find((t) => t.name === name);
      if (!have) {
        tools.push({
          name, status: "pending", current_hash: hash, approved_hash: "", definition: def, approved_definition: null, hidden: false,
          rename: "", description_override: "", read_only: null, upstream_read_only: false, effective_read_only: false,
          served_name: name, served: false, serve_issue: "", aggregated_name: null, aggregated_issue: "", guardrail_hits: hit, updated_at: new Date().toISOString(),
        });
        result.added++;
      } else if (have.current_hash !== hash || have.status === "removed") {
        if (have.status === "removed") result.added++; else result.changed++;
        have.definition = def; have.current_hash = hash; have.guardrail_hits = hit;
        have.status = have.approved_hash === hash ? "approved" : "pending";
      }
    }
    for (const t of tools) {
      if (!seen.has(t.name) && t.status !== "removed") { t.status = "removed"; result.removed++; }
    }
    s.last_sync_at = new Date().toISOString();
    s.last_sync_error = "";
    result.tool_count = tools.filter((t) => t.status !== "removed").length;
    result.pending_count = tools.filter((t) => t.status === "pending").length;
    return json(result);
  }),
  http.get("/api/v1/mcp/sources/:slug/tools", ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["mcp:sources:read", "mcp:sources:manage"] }); if (g) return g;
    const s = db.toolSources.find((x) => x.slug === params.slug);
    if (!s) return err(404, "tool source not found");
    const tools = db.toolSourceTools[s.slug] ?? [];
    return json([...tools].sort((a, b) => a.name.localeCompare(b.name)).map((t) => toolWire(String(params.slug), tools, t)));
  }),
  http.put("/api/v1/mcp/sources/:slug/tools/:name", async ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["mcp:sources:manage"] }); if (g) return g;
    const tools = db.toolSourceTools[String(params.slug)];
    const t = tools?.find((x) => x.name === params.name);
    if (!tools || !t) return err(404, "tool not found");
    const b = await body<ToolSourceToolUpdate & Record<string, unknown>>(request);
    if (!b || typeof b !== "object") return err(400, "invalid JSON body");
    const extra = Object.keys(b).find((k) => !["hidden", "rename", "description_override", "read_only"].includes(k));
    if (extra) return err(400, `unknown field "${extra}"`);
    if (b.rename !== undefined) {
      if (b.rename !== "" && !/^[A-Za-z0-9_-]+$/.test(b.rename)) return err(400, "rename may only contain letters, digits, _ and -");
      if (b.rename !== "" && tools.some((x) => x !== t && (x.name === b.rename || x.rename === b.rename))) {
        return err(409, `the name ${b.rename} is already used by another tool of this source`);
      }
    }
    if (b.hidden !== undefined) t.hidden = b.hidden;
    if (b.rename !== undefined) t.rename = b.rename === t.name ? "" : b.rename;
    if (b.description_override !== undefined) t.description_override = b.description_override;
    if (b.read_only !== undefined) t.read_only = b.read_only;
    t.updated_at = new Date().toISOString();
    const keys = b.rename ? db.toolRenameAffected[b.rename] : undefined;
    return json({ ...toolWire(String(params.slug), tools, t), ...(keys ? { keys_matching_rename: keys } : {}) });
  }),
  http.post("/api/v1/mcp/sources/:slug/tools/:name/approve", async ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["mcp:sources:manage"] }); if (g) return g;
    const tools = db.toolSourceTools[String(params.slug)];
    const t = tools?.find((x) => x.name === params.name);
    if (!tools || !t) return err(404, "tool not found");
    const b = await body<{ hash?: string }>(request);
    if (!b?.hash) return err(400, "hash is required");
    if (t.status !== "pending") return err(409, "the tool is not pending");
    if (b.hash !== t.current_hash) return err(409, "the definition changed since it was reviewed");
    if (servedNameTaken(tools, t)) return err(409, `another tool is served under the name ${t.rename || t.name}`);
    approveTool(t);
    refreshCounts(String(params.slug));
    return json(toolWire(String(params.slug), tools, t));
  }),
  http.post("/api/v1/mcp/sources/:slug/approve-all", async ({ request, params }) => {
    const g = gate(request, { admin: true, needs: ["mcp:sources:manage"] }); if (g) return g;
    const slug = String(params.slug);
    const tools = db.toolSourceTools[slug];
    if (!tools) return err(404, "tool source not found");
    const b = await body<{ hashes?: Record<string, string> }>(request);
    if (!b?.hashes || typeof b.hashes !== "object") return err(400, "hashes is required");
    const approved: string[] = [];
    const conflicts: string[] = [];
    for (const [name, hash] of Object.entries(b.hashes)) {
      const t = tools.find((x) => x.name === name);
      if (!t || t.status !== "pending" || t.current_hash !== hash) continue;
      if (servedNameTaken(tools, t)) { conflicts.push(name); continue; }
      approveTool(t);
      approved.push(name);
    }
    refreshCounts(slug);
    return json({ approved, conflicts });
  }),

  // ---- v0.5.0 OpenAPI viewer stubs ----
  http.get("/api/v1/openapi/viewer", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    return new HttpResponse("<!doctype html><h1>OpenAPI</h1>", {
      status: 200,
      headers: { "Content-Type": "text/html" },
    });
  }),
  http.get("/api/v1/openapi/viewer/static/viewer.js", () =>
    new HttpResponse("// openapi viewer stub", {
      status: 200,
      headers: { "Content-Type": "application/javascript" },
    }),
  ),
  http.get("/api/v1/openapi/viewer/static/viewer.css", () =>
    new HttpResponse("/* openapi viewer stub */", {
      status: 200,
      headers: { "Content-Type": "text/css" },
    }),
  ),
];
