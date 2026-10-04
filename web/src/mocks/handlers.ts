import { http, HttpResponse } from "msw";
import { db, type MockDb, type CacheSettingsPayload } from "@/mocks/db";
import type { AccessMode, AiEndpoint, CostSummary, ModelAliasV5, Provider, ServiceAIConfig, CustomDomain, CreateCustomDomainInput, RetentionSettings, GuardrailSettings, RedactionRule } from "@/lib/contract";

const VALID_PROVIDERS = new Set<string>(["ollama", "vllm", "openai-compat", "openai", "anthropic", "other"]);

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
];

// Gate: replicate 401 -> 403(csrf) -> 403(admin) ordering.
function gate(req: Request, opts: { admin?: boolean } = {}): Response | null {
  if (req.headers.get("x-mock-unauth") === "1") return err(401, "unauthorized");
  const method = req.method.toUpperCase();
  if (!SAFE.has(method)) {
    if (req.headers.get("X-CSRF-Token") !== db.csrf) return err(403, "csrf token invalid");
  }
  if (opts.admin && db.me.role !== "admin") return err(403, "admin required");
  return null;
}

// services:configure — admin holds :any; the owner holds :own (spec Part C).
function canConfigure(svc: { user_id: string }): boolean {
  if (db.me.role === "admin") return true;
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

export const handlers = [
  // ---- auth / identity ----
  http.get("/api/v1/me", ({ request }) => gate(request) ?? json(db.me)),
  http.post("/api/v1/auth/logout", ({ request }) => gate(request) ?? noContent()),
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
    const g = gate(request, { admin: true }); if (g) return g;
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
  http.get("/api/v1/roles", ({ request }) => gate(request, { admin: true }) ?? json(db.roles)),
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
    const g = gate(request, { admin: true }); if (g) return g;
    return json(db.clients.map(({ services: _services, ...v }) => v));
  }),
  http.get("/api/v1/clients/connect-info", ({ request }) => gate(request) ?? json({ server: db.connectServer })),
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
    const rows = db.me.role === "admin"
      ? db.services
      : db.services.filter((s) => s.user_id === db.me.id);
    return json(rows.map(({ user_id: _u, ...s }) => s));
  }),
  http.get("/api/v1/services/:id", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const svc = db.services.find((s) => s.id === params.id);
    if (!svc) return err(404, "service not found");
    const { user_id: _u, ...wire } = svc;
    return json({
      ...wire,
      api_key_count: (db.serviceApiKeys[svc.id] ?? []).length,
      access_policy: db.serviceAccessPolicy[svc.id] ?? [],
    });
  }),
  http.post("/api/v1/services", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ service_id?: string; title?: string; access_mode?: string }>(request);
    const id = (b?.service_id ?? "").trim();
    if (!/^[a-z0-9_-]{3,64}$/.test(id)) return err(400, "service_id must match ^[a-z0-9_-]{3,64}$");
    const modeMap: Record<string, string> = { "": "open", public: "open", open: "open", api_key: "api_key", burrow_login: "burrow_login" };
    const stored = modeMap[b?.access_mode ?? ""];
    if (stored === undefined) return err(400, `unknown access mode "${b?.access_mode}"`);
    if (db.services.some((s) => s.id === id)) return err(409, "service already exists");
    db.services.push({ id, user_id: db.me.id, name: b?.title ?? "", type: "http", subdomain: "", hostname: "", access_mode: stored as AccessMode, api_key_header: "Authorization", connected: false, remote_port: 0, local_addr: "" });
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
    const b = await body<{ name?: string }>(request);
    if (!b?.name) return err(400, "name is required");
    db.tokens.push({ id: `tok_${Math.random().toString(36).slice(2, 7)}`, name: b.name, last_used: null, created_at: new Date().toISOString() });
    return json({ name: b.name, token: `bur_${Math.random().toString(36).slice(2, 18)}` }, 201);
  }),

  // ---- events (inert SSE so Clients/Connect don't error) ----
  http.get("/api/v1/events", ({ request }) =>
    gate(request) ?? new HttpResponse("retry: 10000\n\n", { status: 200, headers: { "Content-Type": "text/event-stream" } })),

  // ---- tunnels (reference) ----
  http.get("/api/v1/tunnels", ({ request }) => gate(request) ?? json([])),

  // ---- v0.4.0 AI endpoints lens (spec §4.19) ----
  // Derived view over db.services where access_mode=api_key. Backend owns the
  // real metrics; the mock joins seeded aiMeta + modelAliases + serviceApiKeys.
  http.get("/api/v1/ai/endpoints", ({ request }) => {
    const g = gate(request); if (g) return g;
    const rows = db.me.role === "admin"
      ? db.services
      : db.services.filter((s) => s.user_id === db.me.id);
    const endpoints: AiEndpoint[] = rows
      .filter((s) => s.access_mode === "api_key")
      .map((s) => {
        const meta = db.aiMeta[s.id];
        const alias = db.modelAliases.find((a) => a.service_id === s.id);
        return {
          service_id: s.id,
          name: s.name,
          model_alias: alias?.alias ?? "",
          concrete_model: alias?.concrete_model ?? "",
          backend_type: meta?.backend_type ?? "other",
          api_key_count: (db.serviceApiKeys[s.id] ?? []).length,
          requests_24h: meta?.requests_24h ?? 0,
          cache_hits_24h: meta?.cache_hits_24h ?? 0,
          latency_p95_ms: meta?.latency_p95_ms ?? 0,
          status: meta?.status ?? "Offline",
          client_session_id: meta?.client_session_id ?? "",
        };
      });
    return json(endpoints);
  }),

  // ---- v0.4.0 cost summary (spec Part F) ----
  http.get("/api/v1/cost/summary", ({ request }) => {
    const g = gate(request); if (g) return g;
    const url = new URL(request.url);
    const w = (url.searchParams.get("window") ?? "today") as CostSummary["window"];
    const summary = db.costSummary[w] ?? db.costSummary.today;
    return json(summary);
  }),

  // ---- v0.4.0 per-endpoint metrics (spec §4.20) ----
  http.get("/api/v1/ai/endpoints/:service_id/metrics", ({ request, params }) => {
    const g = gate(request); if (g) return g;
    const meta = db.aiMeta[String(params.service_id)];
    if (!meta) return err(404, "service not found");
    const summary = db.costSummary.today;
    // Deterministic sinusoidal sparkline — enough to render a stable curve.
    const rpm: number[] = [];
    for (let i = 0; i < 60; i++) {
      rpm.push(Math.round(20 + 15 * Math.sin(i / 4)));
    }
    return json({
      requests_24h: meta.requests_24h,
      tokens_in_24h: summary.tokens_in,
      tokens_out_24h: summary.tokens_out,
      cost_usd_24h: summary.total_usd,
      cache_hit_ratio_24h: meta.requests_24h > 0 ? meta.cache_hits_24h / meta.requests_24h : 0,
      requests_per_minute: rpm,
    });
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
    const rows = (db.inspectorEntries[svc.id] ?? [])
      .slice()
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
    gate(request, { admin: true }) ?? json(db.redactionRules)),
  http.post("/api/v1/redaction/rules", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ name?: string; pattern?: string; action?: RedactionRule["action"]; scope?: RedactionRule["scope"] }>(request);
    if (!b?.name?.trim()) return err(400, "name is required");
    if (!b.pattern) return err(400, "pattern is required");
    try { new RegExp(b.pattern); } catch { return err(400, "invalid regex"); }
    if (!b.action || !["mask", "drop", "hash"].includes(b.action)) return err(400, "invalid action");
    if (!b.scope || !["request_body", "response_body", "both"].includes(b.scope)) return err(400, "invalid scope");
    const rule: RedactionRule = {
      id: crypto.randomUUID(), name: b.name.trim(), pattern: b.pattern,
      action: b.action, scope: b.scope,
    };
    db.redactionRules.custom.push(rule);
    return json(rule, 201);
  }),
  http.delete("/api/v1/redaction/rules/:id", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    if (db.redactionRules.built_in.some((r) => r.id === params.id))
      return err(409, "built-in rules cannot be deleted");
    const i = db.redactionRules.custom.findIndex((r) => r.id === params.id);
    if (i < 0) return err(404, "rule not found");
    db.redactionRules.custom.splice(i, 1);
    return noContent();
  }),
  http.get("/api/v1/redaction/settings", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.redactionSettings)),
  http.put("/api/v1/redaction/settings", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<MockDb["redactionSettings"]>(request);
    if (!b) return err(400, "invalid redaction settings");
    db.redactionSettings = b;
    return noContent();
  }),
  http.post("/api/v1/redaction/preview", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{ sample?: string }>(request);
    return json({ matches: [{ rule: "email", count: b?.sample?.includes("@") ? 1 : 0 }] });
  }),
  http.get("/api/v1/guardrails/settings", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.guardrailSettings)),
  http.put("/api/v1/guardrails/settings", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    // PUT accepts the FLAT global settings; GET returns them nested under `global`.
    const b = await body<GuardrailSettings>(request);
    if (!b) return err(400, "invalid guardrail settings");
    db.guardrailSettings = { global: b, per_service: db.guardrailSettings.per_service };
    return noContent();
  }),
  http.get("/api/v1/guardrails/patterns", ({ request }) =>
    gate(request, { admin: true }) ?? json(db.guardrailPatterns)),

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
    gate(request, { admin: true }) ?? json(db.budgets)),
  http.post("/api/v1/budgets", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<Partial<MockDb["budgets"][number]>>(request);
    if (!b || typeof b.daily_usd !== "number" || b.daily_usd <= 0)
      return err(400, "daily_usd must be greater than zero");
    const rec: MockDb["budgets"][number] = {
      id: `bdg_${Math.random().toString(36).slice(2, 8)}`,
      scope: b.scope ?? "api_key",
      subject_id: b.subject_id ?? "",
      daily_usd: b.daily_usd,
      action_on_exceed: b.action_on_exceed ?? "alert_webhook",
      alert_webhook_id: b.alert_webhook_id ?? null,
      current_usd: 0,
      exceeded: false,
    };
    db.budgets.push(rec);
    return json(rec, 201);
  }),
  http.delete("/api/v1/budgets/:id", ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
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
    let rows = db.audit
      .slice()
      .sort((a, b) => (a.ts < b.ts ? 1 : a.ts > b.ts ? -1 : 0));
    if (beforeId) {
      const i = rows.findIndex((e) => e.id === beforeId);
      if (i >= 0) rows = rows.slice(i + 1);
    }
    if (q) rows = rows.filter((e) => `${e.action} ${e.subject_label} ${e.actor_email}`.toLowerCase().includes(q));
    return json(rows.slice(0, limit));
  }),
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

  // ---- v0.5.0 model aliases (spec Part C) ----
  http.get("/api/v1/models/aliases", ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    return json(db.modelAliases);
  }),
  http.post("/api/v1/models/aliases", async ({ request }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const b = await body<{
      alias?: string;
      concrete_model?: string;
      service_id?: string;
      provider?: string;
      priority?: number;
    }>(request);
    if (!b?.alias) return err(400, "alias is required");
    if (!b?.concrete_model) return err(400, "concrete_model is required");
    if (!b?.service_id) return err(400, "service_id is required");
    const svc = db.services.find((s) => s.id === b.service_id);
    if (!svc) return err(400, "service_id does not exist");
    if (b.provider && !VALID_PROVIDERS.has(b.provider))
      return err(400, "provider must be one of: ollama, vllm, openai-compat, openai, anthropic, other");
    const alias: ModelAliasV5 = {
      alias: b.alias,
      concrete_model: b.concrete_model,
      service_id: b.service_id,
      provider: (b.provider as Provider) ?? "other",
      priority: b.priority ?? 100,
      created_at: new Date().toISOString(),
    };
    db.modelAliases.push(alias);
    return json(alias, 201);
  }),
  http.put("/api/v1/models/aliases/:alias", async ({ request, params }) => {
    const g = gate(request, { admin: true }); if (g) return g;
    const entries = db.modelAliases.filter((a) => a.alias === params.alias);
    if (entries.length === 0) return err(404, "alias not found");
    const b = await body<{ provider?: string; priority?: number }>(request);
    if (!b) return err(400, "invalid body");
    for (const entry of entries) {
      if (b.provider != null && VALID_PROVIDERS.has(b.provider)) entry.provider = b.provider as Provider;
      if (b.priority != null) entry.priority = b.priority;
    }
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
    const g = gate(request, { admin: true }); if (g) return g;
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
