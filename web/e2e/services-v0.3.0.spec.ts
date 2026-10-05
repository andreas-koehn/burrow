import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";

// v0.3.0 dashboard surfaces — Services page + the new service-scoped JSON API
// (api-keys, access-mode, access-policy) — against the REAL built burrowd. The
// Playwright fixture does not run a live burrow client, so there are no live
// services to configure; this spec covers:
//   - Services nav link routes to /services and the page mounts
//   - Empty-state copy renders ("burrow connect ... --type http")
//   - v0.3.0 routes are reachable through the real chi router with the
//     documented contract error bodies (Parts C/D/E) for unknown services
//   - Pre-validation: PUT /access-policy with an unknown role fires the
//     spec's 400 {"error":"unknown role \"<name>\""} body even when the
//     service id doesn't exist (handler order matches the spec)
//
// The full live-tunnel access-mode flow is covered by the Go e2e tests
// (cmd/server/e2e_access_modes_test.go) which spin up a real client +
// proxy ingress + wildcard TLS.

const ADMIN_EMAIL = "e2e@example.com";
const ADMIN_PASSWORD = "e2e-password-123";

async function login(page: Page) {
  await page.goto("/");
  await expect(page).toHaveURL(/\/login/);
  await page.getByLabel("Email").fill(ADMIN_EMAIL);
  await page.getByLabel("Password").fill(ADMIN_PASSWORD);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
}

test("v0.3.0: Services page renders + nav + empty-state copy", async ({ page }) => {
  await login(page);

  // Sidebar nav has a "Services" link added in v0.3.0. Scoped to the sidebar:
  // the breadcrumb names the Services workspace too.
  await page.locator(".sidebar").getByRole("link", { name: /^Services/ }).click();
  await expect(page).toHaveURL(/\/services$/);

  // Page heading + subhead. Use exact:true so the PageHeader <h1>Services</h1>
  // doesn't also match the empty-state <h4>No services yet</h4>.
  await expect(page.getByRole("heading", { name: "Services", exact: true })).toBeVisible();
  await expect(
    page.getByText("Durable services exposed through this relay", { exact: false }),
  ).toBeVisible();

  // The empty-state CTA is only rendered when the service list is empty. The
  // suite runs against a shared embedded burrowd and the real binary exposes
  // no DELETE /services route, so earlier specs may leave persisted services
  // behind. Branch on the live list so we assert the REAL surface either way:
  //   - empty list  → the documented `burrow connect … --type http` hint.
  //   - non-empty   → the Services data table with its filter toolbar.
  const services = await page.request.get("/api/v1/services");
  expect(services.status()).toBe(200);
  const rows = (await services.json()) as unknown[];
  if (rows.length === 0) {
    await expect(page.getByText("burrow connect", { exact: false })).toBeVisible();
    await expect(page.getByText("--type http", { exact: false })).toBeVisible();
  } else {
    await expect(page.getByRole("table", { name: "Services" })).toBeVisible();
    await expect(page.getByLabel("Filter services")).toBeVisible();
  }

  // Logout
  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page).toHaveURL(/\/login/);
});

test("v0.3.0 API: services + api-keys + access-mode + access-policy contract", async ({ page }) => {
  await login(page);

  // Bring the CSRF cookie out of the page context for direct API calls.
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  expect(csrf).not.toBe("");
  const csrfHeaders = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  // GET /api/v1/services → 200 + JSON array. (Earlier specs in this shared
  // session may have persisted services and the real binary has no
  // DELETE /services route, so we assert the shape — array of services none of
  // which is the unknown probe id below — rather than strict emptiness.)
  const list = await page.request.get("/api/v1/services");
  expect(list.status()).toBe(200);
  const listed = (await list.json()) as Array<{ id: string }>;
  expect(Array.isArray(listed)).toBe(true);
  expect(listed.some((s) => s.id === "nope-no-such")).toBe(false);

  // GET /api/v1/services/{unknown} → 404 {"error":"service not found"}.
  const detail = await page.request.get("/api/v1/services/nope-no-such");
  expect(detail.status()).toBe(404);
  expect(await detail.json()).toEqual({ error: "service not found" });

  // GET /api/v1/services/{unknown}/api-keys → 404 (admin path: gate passes).
  const keysList = await page.request.get("/api/v1/services/nope-no-such/api-keys");
  expect(keysList.status()).toBe(404);
  expect(await keysList.json()).toEqual({ error: "service not found" });

  // POST /api/v1/services/{unknown}/api-keys missing CSRF → 403.
  const keyPostNoCsrf = await page.request.post("/api/v1/services/nope-no-such/api-keys", {
    headers: { "Content-Type": "application/json" },
    data: { name: "ci" },
  });
  expect(keyPostNoCsrf.status()).toBe(403);
  expect(await keyPostNoCsrf.json()).toEqual({ error: "csrf token invalid" });

  // POST /api/v1/services/{unknown}/api-keys empty name → 400 (validated
  // before service lookup by the store; admin caller bypasses ownership).
  const keyPostEmpty = await page.request.post("/api/v1/services/nope-no-such/api-keys", {
    headers: csrfHeaders,
    data: { name: "" },
  });
  expect(keyPostEmpty.status()).toBe(400);
  expect(await keyPostEmpty.json()).toEqual({ error: "name is required" });

  // POST /api/v1/services/{unknown}/api-keys valid name → 404 (service missing).
  const keyPostUnknownSvc = await page.request.post("/api/v1/services/nope-no-such/api-keys", {
    headers: csrfHeaders,
    data: { name: "ci" },
  });
  expect(keyPostUnknownSvc.status()).toBe(404);
  expect(await keyPostUnknownSvc.json()).toEqual({ error: "service not found" });

  // PUT /api/v1/services/{unknown}/access-mode missing body → 400.
  const modeNoBody = await page.request.put("/api/v1/services/nope-no-such/access-mode", {
    headers: csrfHeaders,
    data: {},
  });
  expect(modeNoBody.status()).toBe(400);
  expect(await modeNoBody.json()).toEqual({ error: "access_mode is required" });

  // PUT /api/v1/services/{unknown}/access-mode bogus value on unknown service
  // → 404 (store validation order is gate → mode enum: the service-existence
  // gate fires before the mode-enum check, so an attacker probing endpoints
  // can't distinguish "service exists, mode bogus" from "service missing").
  const modeBad = await page.request.put("/api/v1/services/nope-no-such/access-mode", {
    headers: csrfHeaders,
    data: { access_mode: "bogus" },
  });
  expect(modeBad.status()).toBe(404);
  expect(await modeBad.json()).toEqual({ error: "service not found" });

  // PUT /api/v1/services/{unknown}/access-mode valid → 404 (service missing).
  const modeUnknownSvc = await page.request.put("/api/v1/services/nope-no-such/access-mode", {
    headers: csrfHeaders,
    data: { access_mode: "open" },
  });
  expect(modeUnknownSvc.status()).toBe(404);
  expect(await modeUnknownSvc.json()).toEqual({ error: "service not found" });

  // GET /api/v1/services/{unknown}/access-policy → 404.
  const polGet = await page.request.get("/api/v1/services/nope-no-such/access-policy");
  expect(polGet.status()).toBe(404);
  expect(await polGet.json()).toEqual({ error: "service not found" });

  // PUT /api/v1/services/{unknown}/access-policy with unknown role → 400 with
  // the offending role in the body (spec Part D; pre-validation fires before
  // any service lookup).
  const polUnknownRole = await page.request.put("/api/v1/services/nope-no-such/access-policy", {
    headers: csrfHeaders,
    data: { roles: ["superuser"] },
  });
  expect(polUnknownRole.status()).toBe(400);
  expect(await polUnknownRole.json()).toEqual({
    error: 'unknown role "superuser"',
  });

  // PUT /api/v1/services/{unknown}/access-policy with valid roles → 404.
  const polValid = await page.request.put("/api/v1/services/nope-no-such/access-policy", {
    headers: csrfHeaders,
    data: { roles: ["admin", "user"] },
  });
  expect(polValid.status()).toBe(404);
  expect(await polValid.json()).toEqual({ error: "service not found" });

  // Logout
  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page).toHaveURL(/\/login/);
});

test("v0.3.0 API: unauthenticated visitor hits 401 on every new route", async ({ page }) => {
  // No login. Each v0.3.0 route must 401 with the spec body.
  const routes = [
    { method: "GET", path: "/api/v1/services" },
    { method: "GET", path: "/api/v1/services/anything" },
    { method: "GET", path: "/api/v1/services/anything/api-keys" },
    { method: "GET", path: "/api/v1/services/anything/access-policy" },
  ];
  for (const r of routes) {
    const resp = await page.request.fetch(r.path, { method: r.method });
    expect(resp.status(), `${r.method} ${r.path} status`).toBe(401);
    expect(await resp.json()).toEqual({ error: "unauthorized" });
  }
});
