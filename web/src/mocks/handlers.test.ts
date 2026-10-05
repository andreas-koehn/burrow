import { describe, it, expect, beforeEach } from "vitest";
import { addDirectProvider, db, resetDb } from "@/mocks/db";
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
    expect(db.aiProviders).toHaveLength(1);
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
      [{ credential_slot: "open-router" }, "credential slot must be 1-32 characters: A-Z, 0-9, _"],
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
    expect((await fetch("/api/v1/ai/providers/ollama", authed("DELETE"))).status).toBe(204);
    expect(db.aiProviders).toHaveLength(0);
    expect(db.services.some((x) => x.id === "svc_ai001")).toBe(true);
  });
});
