import { describe, it, expect, beforeEach } from "vitest";
import { db, resetDb } from "@/mocks/db";
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
    r = await postProvider({ name: "Local", kind: "direct", service_id: "svc_ai001" });
    expect([r.status, (await r.json()).error]).toEqual([400, "kind must be 'tunnel'"]);
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
