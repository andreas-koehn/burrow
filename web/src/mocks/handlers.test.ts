import { describe, it, expect, beforeEach } from "vitest";
import { addDirectProvider, db, resetDb } from "@/mocks/db";
import type { AiModel } from "@/lib/contract";
import "@/mocks/server"; // installed via test setup; import asserts module loads

const CSRF = "test-csrf-token";
function authed(method: string): RequestInit {
  const h: Record<string, string> = { "Content-Type": "application/json" };
  if (!["GET", "HEAD", "OPTIONS"].includes(method)) h["X-CSRF-Token"] = CSRF;
  return { method, headers: h, credentials: "include" };
}

describe("MSW handlers (contract fidelity)", () => {
  beforeEach(() => { resetDb(); document.cookie = `burrow_csrf=${CSRF}; path=/`; });

  it("GET /api/v1/me returns the seeded admin", async () => {
    const r = await fetch("/api/v1/me", authed("GET"));
    expect(r.status).toBe(200);
    const b = await r.json();
    expect(b).toEqual({ id: expect.any(String), email: "alice@acme.io", role: "admin" });
  });

  it("GET /api/v1/users is paginated and enveloped", async () => {
    const r = await fetch("/api/v1/users?limit=20&offset=0", authed("GET"));
    const b = await r.json();
    expect(Array.isArray(b.users)).toBe(true);
    expect(typeof b.total).toBe("number");
    expect(b.users[0]).toHaveProperty("status");
    expect(b.users[0]).toHaveProperty("last_login");
  });

  it("POST /api/v1/users without CSRF header is 403", async () => {
    const r = await fetch("/api/v1/users", {
      method: "POST", credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ email: "x@y.io", password: "password123", role: "user" }),
    });
    expect(r.status).toBe(403);
    expect(await r.json()).toEqual({ error: "csrf token invalid" });
  });

  it("POST /api/v1/users duplicate email is 409", async () => {
    const r = await fetch("/api/v1/users", { ...authed("POST"), body: JSON.stringify({ email: "bob@acme.io", password: "password123", role: "user" }) });
    expect(r.status).toBe(409);
    expect(await r.json()).toEqual({ error: "email already in use" });
  });

  it("PATCH /api/v1/users/{me} status is 400 (self-status guard)", async () => {
    const me = await (await fetch("/api/v1/me", authed("GET"))).json();
    const r = await fetch(`/api/v1/users/${me.id}`, { ...authed("PATCH"), body: JSON.stringify({ status: "suspended" }) });
    expect(r.status).toBe(400);
    expect(await r.json()).toEqual({ error: "cannot change your own status" });
  });

  it("GET /api/v1/roles is a bare array; detail has permissions", async () => {
    const list = await (await fetch("/api/v1/roles", authed("GET"))).json();
    expect(Array.isArray(list)).toBe(true);
    const detail = await (await fetch("/api/v1/roles/user", authed("GET"))).json();
    expect(detail.permissions).toContain("tunnels:read:own");
  });

  it("GET /api/v1/settings never returns smtp.password", async () => {
    await fetch("/api/v1/settings", { ...authed("PUT"), body: JSON.stringify({ "smtp.host": "mx", "smtp.password": "leak" }) });
    const b = await (await fetch("/api/v1/settings", authed("GET"))).json();
    expect(b["smtp.host"]).toBe("mx");
    expect(b).not.toHaveProperty("smtp.password");
  });

  it("POST /api/v1/settings/test-email is 409 when unconfigured", async () => {
    const r = await fetch("/api/v1/settings/test-email", { ...authed("POST"), body: JSON.stringify({ to: "ops@acme.io" }) });
    expect(r.status).toBe(409);
  });

  it("GET /api/v1/clients/{id} flattens ClientView + services", async () => {
    const list = await (await fetch("/api/v1/clients", authed("GET"))).json();
    const id = list[0].session_id;
    const d = await (await fetch(`/api/v1/clients/${id}`, authed("GET"))).json();
    expect(d.session_id).toBe(id);
    expect(d).not.toHaveProperty("client");
    expect(d.services[0].access_mode).toBe("open");
  });

  it("PUT /api/v1/tunnels/{id}/access-mode rejects bad enum and accepts open", async () => {
    const bad = await fetch("/api/v1/tunnels/tnl_web01/access-mode", { ...authed("PUT"), body: JSON.stringify({ access_mode: "nope" }) });
    expect(bad.status).toBe(400);
    const ok = await fetch("/api/v1/tunnels/tnl_web01/access-mode", { ...authed("PUT"), body: JSON.stringify({ access_mode: "open" }) });
    expect(ok.status).toBe(204);
  });

  it("unauthenticated request is 401 (no session cookie scenario)", async () => {
    const r = await fetch("/api/v1/users", { method: "GET", headers: { "x-mock-unauth": "1" } });
    expect(r.status).toBe(401);
    expect(await r.json()).toEqual({ error: "unauthorized" });
  });
});

describe("MSW redaction rule handlers mirror the API's errors", () => {
  beforeEach(() => { resetDb(); document.cookie = `burrow_csrf=${CSRF}; path=/`; });
  const post = (b: object) =>
    fetch("/api/v1/redaction/rules", { ...authed("POST"), body: JSON.stringify(b) });

  it("POST rejects a missing or unknown action / scope with 400", async () => {
    for (const [b, msg] of [
      [{ name: "n", pattern: "a", scope: "both" }, "invalid action"],
      [{ name: "n", pattern: "a", action: "nope", scope: "both" }, "invalid action"],
      [{ name: "n", pattern: "a", action: "mask" }, "invalid scope"],
      [{ name: "n", pattern: "a", action: "mask", scope: "nope" }, "invalid scope"],
    ] as const) {
      const r = await post(b);
      expect(r.status).toBe(400);
      expect(await r.json()).toEqual({ error: msg });
    }
  });

  it("DELETE is 404 for an unknown id, 409 for a built-in, 204 for a custom rule", async () => {
    const unknown = await fetch("/api/v1/redaction/rules/nope", authed("DELETE"));
    expect(unknown.status).toBe(404);
    expect(await unknown.json()).toEqual({ error: "rule not found" });
    const builtIn = await fetch("/api/v1/redaction/rules/email", authed("DELETE"));
    expect(builtIn.status).toBe(409);
    expect(await builtIn.json()).toEqual({ error: "built-in rules cannot be deleted" });
    const created = await (await post({ name: "n", pattern: "a", action: "mask", scope: "both" })).json();
    const ok = await fetch(`/api/v1/redaction/rules/${created.id}`, authed("DELETE"));
    expect(ok.status).toBe(204);
  });
});

describe("MSW provider handlers mirror the API", () => {
  beforeEach(() => { resetDb(); document.cookie = `burrow_csrf=${CSRF}; path=/`; });

  const postProvider = (body: unknown) =>
    fetch("/api/v1/ai/providers", { ...authed("POST"), body: JSON.stringify(body) });

  it("POST /ai/providers: an unknown service is a 409, like a service in the wrong mode", async () => {
    for (const service_id of ["nope", "svc_web01", "svc_pg001"]) {
      const r = await postProvider({ name: "Local", service_id });
      expect(r.status).toBe(409);
      expect((await r.json()).error).toBe("a tunnel provider needs an http service in API-key mode");
    }
  });

  it("POST /ai/providers validates name and kind before anything else", async () => {
    let r = await postProvider({ name: "   ", service_id: "svc_ai001" });
    expect([r.status, (await r.json()).error]).toEqual([400, "name is required"]);
    r = await postProvider({ name: "n".repeat(121), service_id: "svc_ai001" });
    expect([r.status, (await r.json()).error]).toEqual([400, "name must be at most 120 chars"]);
    // The limit is counted in bytes, as Go's len does: 61 two-byte letters are 122.
    r = await postProvider({ name: "ä".repeat(61), service_id: "svc_ai001" });
    expect([r.status, (await r.json()).error]).toEqual([400, "name must be at most 120 chars"]);
    r = await postProvider({ name: "Local", kind: "other", service_id: "svc_ai001" });
    expect([r.status, (await r.json()).error]).toEqual([400, "kind must be 'tunnel' or 'direct'"]);
  });

  const direct = { name: "OpenRouter", slug: "openrouter", kind: "direct", base_url: "https://openrouter.ai/api/v1", credential_slot: "OPENROUTER" };

  it("POST /ai/providers refuses a field it does not know, for either kind, naming it", async () => {
    for (const b of [{ ...direct, api_key: "sk-secret" }, { name: "Local", service_id: "svc_ai001", api_key: "sk-secret" }]) {
      const r = await postProvider(b);
      const msg = (await r.json()).error as string;
      expect(r.status).toBe(400);
      expect(msg).toBe('unknown field "api_key"; the credential is set on the relay, credential_slot names its slot');
      expect(msg).not.toContain("sk-secret");
    }
    expect(db.aiProviders.map((p) => p.slug)).toEqual(["ollama", "zai", "zai-anthropic"]);
  });

  it("POST /ai/providers kind direct: creates the provider with a hidden backing service", async () => {
    const r = await postProvider({ ...direct, extra_headers: { "X-Title": "Burrow" } });
    expect(r.status).toBe(201);
    const v = await r.json();
    expect(v).toMatchObject({
      slug: "openrouter", kind: "direct", upstream_base_url: "https://openrouter.ai/api/v1", credential_slot: "OPENROUTER",
      credential_present: true, billing: "metered", model_count: 0, status: "Connected",
      auth_header: "Authorization", auth_format: "Bearer {key}", extra_header_names: ["X-Title"],
    });
    expect(JSON.stringify(v)).not.toContain('"Burrow"'); // header values are write-only
    const list = await (await fetch("/api/v1/services", authed("GET"))).json();
    expect(list.some((x: { id: string }) => x.id === v.service_id)).toBe(false);
    expect((await postProvider(direct)).status).toBe(409);
  });

  it("POST /ai/providers kind direct: validates like the server", async () => {
    const cases: [Record<string, unknown>, string][] = [
      [{ base_url: "http://openrouter.ai/api/v1" }, "base URL must be an https URL without credentials, query or fragment"],
      [{ base_url: "https://u:p@openrouter.ai/v1" }, "base URL must be an https URL without credentials, query or fragment"],
      [{ base_url: "https://openrouter.ai/v1?x=1" }, "base URL must be an https URL without credentials, query or fragment"],
      [{ base_url: "https://10.0.0.5/v1" }, "base URL resolves to a private or loopback address"],
      [{ credential_slot: "open-router" }, "credential slots must be 1-4 distinct names of 1-32 characters: A-Z, 0-9, _"],
      [{ auth_format: "Bearer" }, 'auth format must contain "{key}" once, at most 128 characters, no control characters'],
      [{ auth_format: "{key}{key}" }, 'auth format must contain "{key}" once, at most 128 characters, no control characters'],
      [{ billing: "free" }, "billing must be 'metered' or 'flat'"],
      [{ extra_headers: { Authorization: "x" } }, 'extra header "Authorization" is not allowed'],
      [{ extra_headers: { "X-Forwarded-For": "x" } }, 'extra header "X-Forwarded-For" is not allowed'],
    ];
    for (const [patch, message] of cases) {
      const r = await postProvider({ ...direct, ...patch });
      expect([r.status, (await r.json()).error]).toEqual([400, message]);
    }
  });

  it("a direct provider's credential is present only when its slot is set and non-empty", async () => {
    addDirectProvider("zai", { credential_slot: "ZAI" });
    addDirectProvider("openrouter", { credential_slot: "OPENROUTER" });
    const get = async (slug: string) => (await fetch(`/api/v1/ai/providers/${slug}`, authed("GET"))).json();
    expect(await get("zai")).toMatchObject({ credential_present: false, status: "Offline" });
    expect(await get("openrouter")).toMatchObject({ credential_present: true, status: "Connected" });
    db.absentSlots.add("OPENROUTER");
    expect(await get("openrouter")).toMatchObject({ credential_present: false });
    // The upstream auth settings are for admins only.
    db.me = { ...db.me, role: "user" };
    const v = await get("openrouter");
    expect(v).not.toHaveProperty("auth_header");
    expect(v).not.toHaveProperty("auth_format");
    expect(v).not.toHaveProperty("extra_header_names");
  });

  const putUpstream = (slug: string, body: unknown) =>
    fetch(`/api/v1/ai/providers/${slug}/upstream`, { ...authed("PUT"), body: JSON.stringify(body) });

  it("PUT …/upstream: omitted fields keep, extra_headers replaces all, {} clears, unknown fields are refused", async () => {
    addDirectProvider("openrouter", { credential_slot: "OPENROUTER", extra_headers: { "X-Title": "Burrow", "X-Team": "a" } });
    let r = await putUpstream("openrouter", { billing: "flat" });
    expect(r.status).toBe(200);
    expect(await r.json()).toMatchObject({ billing: "flat", credential_slot: "OPENROUTER", extra_header_names: ["X-Team", "X-Title"] });
    r = await putUpstream("openrouter", { extra_headers: { "X-Other": "1" } });
    expect((await r.json()).extra_header_names).toEqual(["X-Other"]);
    r = await putUpstream("openrouter", { extra_headers: {} });
    expect((await r.json()).extra_header_names).toEqual([]);
    r = await putUpstream("openrouter", { api_key: "sk-secret" });
    expect([r.status, (await r.json()).error]).toEqual([400, 'unknown field "api_key"; the credential is set on the relay, credential_slot names its slot']);
    r = await putUpstream("openrouter", { base_url: "http://x.example/v1" });
    expect(r.status).toBe(400);
    expect((await putUpstream("ollama", { billing: "flat" })).status).toBe(409);
    expect((await putUpstream("nope", { billing: "flat" })).status).toBe(404);
    db.me = { ...db.me, role: "user" };
    expect((await putUpstream("openrouter", { billing: "flat" })).status).toBe(403);
  });

  it("model catalog routes: statuses as the server answers them", async () => {
    addDirectProvider("zai", { credential_slot: "ZAI" });
    addDirectProvider("openrouter", { credential_slot: "OPENROUTER" });
    const models = (slug: string, method: string, body?: unknown, q = "") =>
      fetch(`/api/v1/ai/providers/${slug}/models${q}`, { ...authed(method), ...(body ? { body: JSON.stringify(body) } : {}) });
    expect((await models("nope", "GET")).status).toBe(404);
    expect(await (await models("openrouter", "GET")).json()).toEqual([]);
    expect((await models("openrouter", "POST", { id: "" })).status).toBe(400);
    expect((await models("openrouter", "POST", { id: " padded " })).status).toBe(400);
    expect((await models("openrouter", "POST", { id: "x".repeat(201) })).status).toBe(400);
    expect((await models("openrouter", "POST", { id: "z/model" })).status).toBe(204);
    expect((await models("openrouter", "POST", { id: "z/model" })).status).toBe(204); // idempotent
    expect((await models("openrouter", "POST", { id: "a/model" })).status).toBe(204);
    const list = await (await models("openrouter", "GET")).json();
    expect(list.map((m: { id: string }) => m.id)).toEqual(["a/model", "z/model"]); // ordered by id
    expect((await models("openrouter", "DELETE", undefined, "")).status).toBe(400);
    expect((await models("openrouter", "DELETE", undefined, "?id=nope")).status).toBe(404);
    expect((await models("openrouter", "DELETE", undefined, `?id=${encodeURIComponent("z/model")}`)).status).toBe(204);

    const sync = (slug: string) => fetch(`/api/v1/ai/providers/${slug}/models/sync`, authed("POST"));
    let r = await sync("zai");
    expect([r.status, (await r.json()).error]).toEqual([409, "the credential slot ZAI is not set"]);
    r = await sync("ollama");
    expect([r.status, (await r.json()).error]).toEqual([409, "sync is available for direct providers"]);
    r = await sync("openrouter");
    expect([r.status, await r.json()]).toEqual([200, { count: 2 }]);
    expect(db.aiProviderModels["openrouter"]).toHaveLength(2); // the answer replaces the list

    db.me = { ...db.me, role: "user" };
    expect((await sync("openrouter")).status).toBe(403);
    expect((await models("openrouter", "POST", { id: "b" })).status).toBe(403);
    expect((await models("openrouter", "DELETE", undefined, "?id=b")).status).toBe(403);
  });

  it("DELETE of a direct provider takes its backing service, keys and model list with it", async () => {
    const p = addDirectProvider("openrouter", { credential_slot: "OPENROUTER" });
    db.serviceApiKeys[p.service_id] = [{ id: "sak_x", name: "x", last_used: null, created_at: "2026-10-05T00:00:00Z" }];
    db.aiProviderModels["openrouter"] = [{ id: "m", display_name: "", context_length: 0, synced_at: "2026-10-05T00:00:00Z" }];
    expect((await fetch("/api/v1/ai/providers/openrouter", authed("DELETE"))).status).toBe(204);
    expect(db.services.some((x) => x.id === p.service_id)).toBe(false);
    expect(db.serviceApiKeys[p.service_id]).toBeUndefined();
    expect(db.aiProviderModels["openrouter"]).toBeUndefined();
  });

  it("POST /ai/providers derives the slug like the server: collapsed, trimmed, cut at 40", async () => {
    db.aiProviders = [];
    let r = await postProvider({ name: "  My  Local -- LLM! ", service_id: "svc_ai001" });
    expect(r.status).toBe(201);
    expect((await r.json()).slug).toBe("my-local-llm");

    db.aiProviders = [];
    // 39 letters, a separator, more letters: the cut at 40 leaves a trailing hyphen to trim.
    r = await postProvider({ name: `${"a".repeat(39)} tail`, service_id: "svc_ai001" });
    expect((await r.json()).slug).toBe("a".repeat(39));

    db.aiProviders = [];
    for (const name of ["AI", "v1", "!!!"]) {
      r = await postProvider({ name, service_id: "svc_ai001" });
      expect(r.status).toBe(400);
      expect((await r.json()).error).toMatch(/^slug must be 3-40 characters.*"v1" is reserved$/);
    }
  });

  it("DELETE /ai/providers/:slug: 403 for a non-admin, 404 when unknown, 204 and the service stays", async () => {
    db.me = { ...db.me, role: "user" };
    expect((await fetch("/api/v1/ai/providers/ollama", authed("DELETE"))).status).toBe(403);
    db.me = { ...db.me, role: "admin" };
    expect((await fetch("/api/v1/ai/providers/nope", authed("DELETE"))).status).toBe(404);
    // A provider a model targets is refused, and the models are named.
    const used = await fetch("/api/v1/ai/providers/ollama", authed("DELETE"));
    expect([used.status, (await used.json()).error]).toEqual([409, "provider is used by model(s): burrow-simple"]);
    db.aiModels = db.aiModels.filter((m) => m.name !== "burrow-simple");
    expect((await fetch("/api/v1/ai/providers/ollama", authed("DELETE"))).status).toBe(204);
    expect(db.aiProviders.map((p) => p.slug)).toEqual(["zai", "zai-anthropic"]);
    expect(db.services.some((x) => x.id === "svc_ai001")).toBe(true);
  });
});

describe("MSW gateway handlers mirror the API", () => {
  beforeEach(() => { resetDb(); document.cookie = `burrow_csrf=${CSRF}; path=/`; });

  const send = (method: string, path: string, body?: unknown) =>
    fetch(`/api/v1${path}`, { ...authed(method), ...(body === undefined ? {} : { body: JSON.stringify(body) }) });
  const refusal = async (r: Response) => [r.status, (await r.json()).error];
  const target = { provider: "zai", model: "glm-5.1" };

  it("GET /ai/gateway names both endpoints, OpenAI first", async () => {
    expect(await (await send("GET", "/ai/gateway")).json()).toEqual({
      endpoints: [
        { dialect: "openai", base_url: "https://tunnels.example.com/openai/v1" },
        { dialect: "anthropic", base_url: "https://tunnels.example.com/anthropic" },
      ],
    });
  });

  it("POST /ai/models fills in the dialect, derives the formats and refuses what the store refuses", async () => {
    const r = await send("POST", "/ai/models", { name: "burrow-medium", targets: [target, { provider: "zai-anthropic", model: "glm-5.1" }] });
    expect(r.status).toBe(201);
    expect(await r.json()).toMatchObject({
      name: "burrow-medium", enabled: true, dialects: ["anthropic", "openai"],
      targets: [{ dialect: "anthropic", provider: "zai-anthropic", model: "glm-5.1" }, { dialect: "openai", ...target }],
    });
    expect(await refusal(await send("POST", "/ai/models", { name: "x1", targets: [{ provider: "nope", model: "m" }] })))
      .toEqual([400, "unknown provider nope"]);
    expect(await refusal(await send("POST", "/ai/models", { name: "x1", targets: [{ dialect: "anthropic", ...target }] })))
      .toEqual([400, "provider zai speaks openai, not anthropic"]);
    expect(await refusal(await send("POST", "/ai/models", { name: "x1", targets: [target, target] })))
      .toEqual([400, "a target is listed twice"]);
    expect(await refusal(await send("POST", "/ai/models", { name: "x1", dialects: ["openai"], targets: [target] })))
      .toEqual([400, 'unknown field "dialects"']);
    expect(await refusal(await send("POST", "/ai/models", { name: "burrow-simple", targets: [target] })))
      .toEqual([409, "model name already in use"]);
  });

  it("POST /ai/models answers with the relay's own reasons", async () => {
    const post = async (b: Record<string, unknown>) => refusal(await send("POST", "/ai/models", { name: "x1", targets: [target], ...b }));
    const NAME = "name must be 2-63 characters: lowercase letters, digits, dot, underscore, hyphen";
    expect(await post({ name: "a/b" })).toEqual([400, NAME]);
    expect(await post({ name: "" })).toEqual([400, NAME]);
    expect(await post({ name: "zai" })).toEqual([400, "name is already a provider slug"]);
    expect(await post({ description: "d".repeat(501) })).toEqual([400, "description must be at most 500 characters without control characters"]);
    const COUNT = "a model needs at least one target and at most 8 per format";
    expect(await post({ targets: [] })).toEqual([400, COUNT]);
    expect(await post({ targets: Array.from({ length: 9 }, (_, i) => ({ provider: "zai", model: `m${i}` })) })).toEqual([400, COUNT]);
    expect(await post({ targets: [{ provider: "zai", model: "" }] })).toEqual([400, "a target's model must be 1-200 characters without control characters"]);
    expect(await post({ targets: [{ dialect: "gemini", provider: "zai", model: "m" }] })).toEqual([400, "a target's format must be 'openai' or 'anthropic'"]);
    expect(await post({ attempt_timeout_s: 601 })).toEqual([400, "timeouts must be between 1 and 600 seconds"]);
    expect(await post({ attempt_timeout_s: 90, total_timeout_s: 30 })).toEqual([400, "total timeout must not be shorter than the attempt timeout"]);
    // Left out or 0, the timeouts are the defaults.
    const made = await (await send("POST", "/ai/models", { name: "x1", attempt_timeout_s: 0, targets: [target] })).json();
    expect([made.attempt_timeout_s, made.total_timeout_s]).toEqual([60, 120]);
  });

  it("targets come back by format, then in the order given, as the relay stores them", async () => {
    const r = await send("POST", "/ai/models", { name: "x2", targets: [
      { provider: "zai", model: "b" }, { provider: "zai-anthropic", model: "glm-5.1" }, { provider: "zai", model: "a" },
    ] });
    // Slot ZAI is not set on this relay: no target can be tried, nothing serves.
    const created = await r.json();
    expect(created.targets).toEqual([
      { dialect: "anthropic", provider: "zai-anthropic", model: "glm-5.1", available: false },
      { dialect: "openai", provider: "zai", model: "b", available: false },
      { dialect: "openai", provider: "zai", model: "a", available: false },
    ]);
    expect(created.serving).toEqual({ anthropic: null, openai: null });
    expect(db.aiModels.find((m) => m.name === "burrow-intelligence")!.targets.map((t) => t.dialect)).toEqual(["anthropic", "openai"]);
  });

  it("a model view says which targets can be tried and which one serves each format", async () => {
    const view = async () => (await (await send("GET", "/ai/models/burrow-simple")).json()) as AiModel;
    // The tunnel provider's client is connected; the model has no Anthropic target.
    expect(await view()).toMatchObject({
      targets: [{ provider: "ollama", model: "mistral", available: true }],
      serving: { openai: { provider: "ollama", model: "mistral" } },
    });
    expect((await view()).serving).not.toHaveProperty("anthropic");
    // Skipped by the breaker: unavailable, and the format has no serving target.
    db.aiBreakerOpen.add("ollama");
    expect(await view()).toMatchObject({ targets: [{ available: false }], serving: { openai: null } });
    db.aiBreakerOpen.clear();
    // The first available target serves; every slot of a provider must be set.
    db.upstreamSlots.push("ZAI");
    db.aiProviders.find((p) => p.slug === "zai")!.credential_slot = "ZAI,ZAI2";
    const list = (await (await send("GET", "/ai/models")).json()) as AiModel[];
    const smart = list.find((m) => m.name === "burrow-intelligence")!;
    expect(smart.serving).toEqual({ anthropic: { provider: "zai-anthropic", model: "glm-5.1" }, openai: null });
  });

  it("a disabled model serves nothing; its targets keep their availability", async () => {
    db.aiModels[0]!.enabled = false;
    expect(await (await send("GET", "/ai/models/burrow-simple")).json()).toMatchObject({
      enabled: false, targets: [{ provider: "ollama", available: true }], serving: { openai: null },
    });
  });

  it("translate: off on create, kept on PUT when left out or null, as the relay does", async () => {
    const created = (await (await send("POST", "/ai/models", { name: "burrow-new", targets: [target] })).json()) as AiModel;
    expect(created.translate).toBe(false);
    const put = async (body: Record<string, unknown>) => (await (await send("PUT", "/ai/models/burrow-new", { targets: [target], ...body })).json()) as AiModel;
    expect((await put({ translate: true })).translate).toBe(true);
    expect((await put({})).translate).toBe(true);
    expect((await put({ translate: null })).translate).toBe(true);
    expect((await put({ translate: false })).translate).toBe(false);
  });

  it("a model view says per format how the model is served, by the gateway's rule", async () => {
    const view = async (name: string) => (await (await send("GET", `/ai/models/${name}`)).json()) as AiModel;
    const modes = (m: AiModel) => [m.dialect_modes, m.responses_mode, m.translation_pairs];
    // An OpenAI target on a provider without the Responses API.
    expect(modes(await view("burrow-simple"))).toEqual([{ openai: "native", anthropic: "not_served" }, "not_served", {}]);
    db.aiModels[0]!.translate = true;
    expect(modes(await view("burrow-simple"))).toEqual([
      { openai: "native", anthropic: "translated" }, "translated", { anthropic: "messages-chat", responses: "responses-chat" },
    ]);
    // A provider that offers the Responses API serves it natively, whatever the flag says.
    db.aiProviders.find((p) => p.slug === "ollama")!.supports_responses = true;
    expect(modes(await view("burrow-simple"))).toEqual([{ openai: "native", anthropic: "translated" }, "native", { anthropic: "messages-chat" }]);
    // Targets in both formats: native in both; Responses only through translation.
    const smart = db.aiModels.find((m) => m.name === "burrow-intelligence")!;
    expect(modes(await view("burrow-intelligence"))).toEqual([{ openai: "native", anthropic: "native" }, "not_served", {}]);
    smart.translate = true;
    expect(modes(await view("burrow-intelligence"))).toEqual([{ openai: "native", anthropic: "native" }, "translated", { responses: "responses-chat" }]);
    // An Anthropic target only.
    smart.targets = smart.targets.filter((t) => t.dialect === "anthropic");
    expect(modes(await view("burrow-intelligence"))).toEqual([
      { openai: "translated", anthropic: "native" }, "translated", { openai: "chat-messages", responses: "responses-messages" },
    ]);
    // A disabled model is served nowhere.
    smart.enabled = false;
    expect(modes(await view("burrow-intelligence"))).toEqual([{ openai: "not_served", anthropic: "not_served" }, "not_served", {}]);
  });

  it("refuses the computed mode fields in a model body", async () => {
    for (const field of ["dialect_modes", "responses_mode", "translation_pairs"]) {
      expect(await refusal(await send("PUT", "/ai/models/burrow-simple", { [field]: {}, targets: [target] }))).toEqual([400, `unknown field "${field}"`]);
    }
  });

  it("a captured request carries translated and dropped", async () => {
    const rows = (await (await send("GET", "/services/svc_ai001/inspector/requests")).json()) as { translated: string; dropped: string[] }[];
    expect(rows.every((r) => r.translated === "" && Array.isArray(r.dropped) && r.dropped.length === 0)).toBe(true);
  });

  it("refuses the status fields in a model body, like the relay's strict decoder", async () => {
    expect(await refusal(await send("PUT", "/ai/models/burrow-simple", { serving: {}, targets: [target] }))).toEqual([400, 'unknown field "serving"']);
    expect(await refusal(await send("PUT", "/ai/models/burrow-simple", { targets: [{ ...target, available: true }] })))
      .toEqual([400, 'unknown field "available"']);
  });

  it("a provider view lists its slots and says whether the gateway is skipping it", async () => {
    db.upstreamSlots.push("ZAI");
    db.aiProviders.find((p) => p.slug === "zai")!.credential_slot = "ZAI,ZAI2";
    db.aiBreakerOpen.add("zai");
    expect(await (await send("GET", "/ai/providers/zai")).json()).toMatchObject({
      credential_slots: [{ slot: "ZAI", present: true }, { slot: "ZAI2", present: false }],
      credential_present: false, breaker_open: true, status: "Offline",
    });
    expect(await (await send("GET", "/ai/providers/ollama")).json()).toMatchObject({ credential_slots: [], breaker_open: false });
  });

  it("several credential slots: stored joined, at most four, none twice; a sync needs one that is set", async () => {
    const put = (slot: string) => send("PUT", "/ai/providers/zai/upstream", { credential_slot: slot });
    expect((await (await put("ZAI, ZAI2")).json()).credential_slot).toBe("ZAI,ZAI2");
    const msg = "credential slots must be 1-4 distinct names of 1-32 characters: A-Z, 0-9, _";
    expect(await refusal(await put("A,B,C,D,E"))).toEqual([400, msg]);
    expect(await refusal(await put("A,A"))).toEqual([400, msg]);
    expect(await refusal(await send("POST", "/ai/providers/zai/models/sync"))).toEqual([409, "none of the credential slots ZAI, ZAI2 is set"]);
  });

  it("GET /ai/requests/:id/attempts: an admin reads a log by position; an unknown id is an empty list", async () => {
    const log = await (await send("GET", "/ai/requests/req-1/attempts")).json();
    expect(log.map((a: { position: number; provider: string }) => [a.position, a.provider])).toEqual([[0, "zai"], [1, "openrouter"]]);
    expect(Object.keys(log[0]).sort()).toEqual(["duration_ms", "error_code", "model", "position", "provider", "status", "ts"]);
    expect(await (await send("GET", "/ai/requests/nope/attempts")).json()).toEqual([]);
    expect(await refusal(await send("GET", `/ai/requests/${"x".repeat(129)}/attempts`)))
      .toEqual([400, "request id must be 1-128 characters without control characters"]);
    // The id is decoded once, as the relay does: %2F is the "/" of the relay's ids, %25 a "%".
    db.aiAttempts["relay-1/AbC-000042"] = [{ position: 0, provider: "zai", model: "m", status: 0, error_code: "timeout", duration_ms: 1, ts: "2026-10-06T09:30:00Z" }];
    expect(await (await send("GET", "/ai/requests/relay-1%2FAbC-000042/attempts")).json()).toHaveLength(1);
    expect(await (await send("GET", "/ai/requests/relay-1%252FAbC-000042/attempts")).json()).toEqual([]);
    const percent = await send("GET", "/ai/requests/100%25/attempts");
    expect([percent.status, await percent.json()]).toEqual([200, []]);
    db.aiAttempts["100%"] = db.aiAttempts["req-1"]!;
    expect(await (await send("GET", "/ai/requests/100%25/attempts")).json()).toHaveLength(2);
    db.me = { ...db.me, role: "user" };
    expect(await refusal(await send("GET", "/ai/requests/req-1/attempts"))).toEqual([403, "admin required"]);
  });

  it("PUT /ai/models/:name keeps the name and enabled when they are left out, and replaces the rest", async () => {
    db.aiModels[0]!.enabled = false;
    const put = await send("PUT", "/ai/models/burrow-simple", { targets: [target] });
    expect(await put.json()).toMatchObject({ name: "burrow-simple", enabled: false, description: "", targets: [{ dialect: "openai", ...target }] });
  });

  it("GET, PUT and DELETE /ai/models/:name; writes are for admins", async () => {
    expect((await send("GET", "/ai/models/burrow-simple")).status).toBe(200);
    expect(await refusal(await send("GET", "/ai/models/nope"))).toEqual([404, "model not found"]);
    const put = await send("PUT", "/ai/models/burrow-simple", { name: "burrow-small", enabled: false, targets: [target] });
    expect(await put.json()).toMatchObject({ name: "burrow-small", enabled: false, dialects: ["openai"], created_at: "2026-05-19T00:00:00Z" });
    expect(await refusal(await send("PUT", "/ai/models/burrow-small", { name: "burrow-intelligence", targets: [target] })))
      .toEqual([409, "model name already in use"]);
    db.me = { ...db.me, role: "user" };
    expect((await send("GET", "/ai/models")).status).toBe(200);
    for (const [method, path] of [["POST", "/ai/models"], ["PUT", "/ai/models/burrow-small"], ["DELETE", "/ai/models/burrow-small"]]) {
      expect((await send(method!, path!, { name: "x1", targets: [target] })).status).toBe(403);
    }
    db.me = { ...db.me, role: "admin" };
    expect((await send("DELETE", "/ai/models/burrow-small")).status).toBe(204);
    expect(db.aiModels.map((m) => m.name)).toEqual(["burrow-intelligence"]);
  });

  it("POST /ai/keys returns the key once, uncached; the list never carries it", async () => {
    const r = await send("POST", "/ai/keys", { name: "ci", allowed_models: ["burrow-simple", "ollama/*"] });
    expect(r.status).toBe(201);
    expect(r.headers.get("Cache-Control")).toBe("no-store");
    const made = await r.json();
    expect(made.key).toMatch(/^bgw_.{43}$/);
    expect(made).toMatchObject({ name: "ci", allowed_models: ["burrow-simple", "ollama/*"], revoked_at: null, last_used: null });
    const list = await (await send("GET", "/ai/keys")).json();
    expect(list.map((k: { name: string }) => k.name)).toEqual(["laptop", "ci"]);
    expect(JSON.stringify(list)).not.toContain(made.key);
    expect(await refusal(await send("POST", "/ai/keys", { name: " " }))).toEqual([400, "name must be 1-120 characters without control characters"]);
    expect(await refusal(await send("POST", "/ai/keys", { name: "x", allowed_models: Array.from({ length: 65 }, (_, i) => `m${i}`) })))
      .toEqual([400, "at most 64 allowed models"]);
    for (const entry of ["zai/**", "*", "Zai/*", "zai/", ""]) {
      expect(await refusal(await send("POST", "/ai/keys", { name: "x", allowed_models: [entry] })))
        .toEqual([400, 'an allowed model must be a model name, "<provider>/<model>" or "<provider>/*"']);
    }
    expect(await refusal(await send("POST", "/ai/keys", { name: "x", key: "bgw_mine" }))).toEqual([400, 'unknown field "key"']);
  });

  it("DELETE /ai/keys/:id revokes and keeps the row; another user's key answers like a missing one", async () => {
    db.me = { ...db.me, id: "bur_usr_bob0002", role: "user" };
    expect(await (await send("GET", "/ai/keys")).json()).toEqual([]);
    expect(await refusal(await send("DELETE", "/ai/keys/gk_laptop1"))).toEqual([404, "key not found"]);
    db.me = { ...db.me, id: "bur_usr_admin01", role: "admin" };
    expect((await send("DELETE", "/ai/keys/gk_laptop1")).status).toBe(204);
    expect(db.aiGatewayKeys[0]!.revoked_at).not.toBeNull();
    expect(await refusal(await send("DELETE", "/ai/keys/nope"))).toEqual([404, "key not found"]);
  });
});
