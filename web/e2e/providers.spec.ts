import { test, expect } from "@playwright/test";

// Providers dashboard surface (/gateway/providers). The AI GATEWAY nav group
// is always shown to an admin (Layout.tsx).
//
// The test pre-provisions an api_key http service through the admin
// POST /api/v1/services endpoint and registers it as a provider through
// POST /api/v1/ai/providers: a provider is never created implicitly. The page
// must then mount and render the "Providers" data table (NOT an error and NOT
// the empty state). This test asserts that real surface: heading, subtitle,
// the metric-tile labels, and the table row of the provider created here.

// Use the globalSetup-cached admin session (see web/e2e/global-setup.ts).
test.use({ storageState: "playwright-auth.json" });

test("Providers page mounts with heading + metric strip", async ({ page }) => {
  // Pre-provision the backing service (also a self-contained smoke for the
  // v0.5.2 POST /services route), then register it as a provider.
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };
  const created = await page.request.post("/api/v1/services", {
    headers,
    data: { service_id: "svc_e2e_ai", title: "Playwright AI gateway", access_mode: "api_key" },
  });
  expect([201, 409]).toContain(created.status());
  // 409 = a previous run in this session already registered it.
  const provider = await page.request.post("/api/v1/ai/providers", {
    headers,
    data: { slug: "e2e-playwright-ai", name: "Playwright AI gateway", kind: "tunnel", service_id: "svc_e2e_ai" },
  });
  expect([201, 409]).toContain(provider.status());

  await page.goto("/gateway/providers");
  await expect(page).toHaveURL(/\/gateway\/providers$/);
  await expect(page.getByRole("heading", { name: "Providers", level: 1 })).toBeVisible();
  await expect(
    page.getByText("each under its own base URL", { exact: false }),
  ).toBeVisible();

  // Metric strip — four tiles. The cost tile reads /cost/summary, which DOES
  // exist server-side, so the strip mounts even with no live provider.
  const strip = page.getByRole("list", { name: "Provider metrics" });
  await expect(strip).toBeVisible();
  await expect(strip.getByText("Requests (24h)", { exact: true })).toBeVisible();
  await expect(strip.getByText("Tokens in/out (24h)", { exact: true })).toBeVisible();
  await expect(strip.getByText("Cost estimate (24h)", { exact: true })).toBeVisible();
  await expect(strip.getByText("Cache hit ratio (24h)", { exact: true })).toBeVisible();

  // The provider registered above is a row of the "Providers" table (no error
  // banner), with its base URL path and a button that copies the full URL.
  const providersTable = page.getByRole("table", { name: "Providers" });
  await expect(providersTable).toBeVisible();
  const row = providersTable.getByRole("row").filter({ hasText: "Playwright AI gateway" });
  await expect(row).toBeVisible();
  await expect(row.getByText("/ai/e2e-playwright-ai/v1", { exact: true })).toBeVisible();
  await expect(row.getByRole("button", { name: /^Copy base URL .*\/ai\/e2e-playwright-ai\/v1$/ })).toBeVisible();

  // The detail page tells a client how to connect.
  await row.getByRole("link", { name: "Playwright AI gateway" }).click();
  await expect(page).toHaveURL(/\/gateway\/providers\/e2e-playwright-ai$/);
  await expect(page.getByRole("heading", { name: "Connect a client" })).toBeVisible();
});

test("Providers page depends on /cost/summary contract", async ({ page }) => {
  // The page consumes /api/v1/cost/summary?window=today for its cost tile —
  // this route IS wired in the real server (router.go: GetCostSummary).
  const summary = await page.request.get("/api/v1/cost/summary?window=today");
  expect(summary.status()).toBe(200);
  const body = (await summary.json()) as Record<string, unknown>;
  expect(body).toMatchObject({ window: "today" });
  expect(typeof body.total_usd).toBe("number");
  expect(typeof body.tokens_in).toBe("number");
  expect(typeof body.tokens_out).toBe("number");
});
