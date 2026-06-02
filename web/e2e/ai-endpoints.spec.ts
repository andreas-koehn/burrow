import { test, expect } from "@playwright/test";

// v0.4.0: AI endpoints dashboard surface. The AI GATEWAY nav group is gated
// on ≥1 service with access_mode="api_key" (Layout.tsx).
//
// v0.5.2 P3.6: we now pre-provision an api_key service via the admin
// POST /api/v1/services endpoint so the AI GATEWAY nav group appears in the
// sidebar (previously the test had to navigate by URL because no live client
// existed in the e2e fixture).
//
// The backend route `/api/v1/ai/endpoints` is now wired by the stock relay
// binary and returns 200. Pre-provisioning an api_key http service (below)
// surfaces it as an AI endpoint, so the page mounts successfully and renders
// the "AI endpoints" data table (NOT an error and NOT the empty state). This
// test asserts that real surface: heading, subtitle, the metric-tile labels,
// and the populated endpoints table containing the pre-provisioned service.

// Use the globalSetup-cached admin session (see web/e2e/global-setup.ts).
test.use({ storageState: "playwright-auth.json" });

test("v0.4.0: AI endpoints page mounts with heading + metric strip", async ({ page }) => {
  // Pre-provision the service that makes the AI GATEWAY nav link render
  // (also acts as a self-contained smoke for the v0.5.2 POST /services route).
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };
  const created = await page.request.post("/api/v1/services", {
    headers,
    data: { service_id: "svc_e2e_ai", title: "Playwright AI gateway", access_mode: "api_key" },
  });
  expect([201, 409]).toContain(created.status());

  await page.goto("/ai/endpoints");
  await expect(page).toHaveURL(/\/ai\/endpoints$/);
  await expect(page.getByRole("heading", { name: "AI endpoints" })).toBeVisible();
  await expect(
    page.getByText("OpenAI-compatible API", { exact: false }),
  ).toBeVisible();

  // Metric strip — four tiles. The cost tile reads /cost/summary, which DOES
  // exist server-side, so the strip mounts even with no live endpoints.
  const strip = page.getByRole("list", { name: "AI endpoint metrics" });
  await expect(strip).toBeVisible();
  await expect(strip.getByText("Requests (24h)", { exact: true })).toBeVisible();
  await expect(strip.getByText("Tokens in/out (24h)", { exact: true })).toBeVisible();
  await expect(strip.getByText("Cost estimate (24h)", { exact: true })).toBeVisible();
  await expect(strip.getByText("Cache hit ratio (24h)", { exact: true })).toBeVisible();

  // /ai/endpoints is wired in the real binary; the pre-provisioned api_key
  // service surfaces as an AI endpoint, so the page mounts cleanly and renders
  // the "AI endpoints" data table (no error banner). The created service is a
  // row in that table.
  const endpointsTable = page.getByRole("table", { name: "AI endpoints" });
  await expect(endpointsTable).toBeVisible();
  await expect(
    endpointsTable.getByText("Playwright AI gateway", { exact: false }),
  ).toBeVisible();
});

test("v0.4.0: AI endpoints depends on /cost/summary contract", async ({ page }) => {
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
