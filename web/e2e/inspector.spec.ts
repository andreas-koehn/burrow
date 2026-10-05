import { test, expect } from "@playwright/test";

// v0.4.0: /gateway/requests/:serviceId (was /inspector/:serviceId) — request inspector (spec Part E). The UI is
// scoped to a specific service; the e2e fixture has no live AI services to
// populate. The full live-traffic flow is covered by the Go integration
// tests (e2e_inspector_test.go).

test.describe("v0.4.0: inspector — authenticated surfaces", () => {
  test.use({ storageState: "playwright-auth.json" });

  test("page mounts with heading for an arbitrary service id", async ({ page }) => {
    await page.goto("/gateway/requests/no-such-service");
    await expect(page).toHaveURL(/\/gateway\/requests\/no-such-service$/);
    await expect(page.getByRole("heading", { name: "Request inspector" })).toBeVisible();
  });

  test("inspector API — list returns 404 for unknown service", async ({ page }) => {
    const list = await page.request.get(
      "/api/v1/services/no-such/inspector/requests?limit=10",
    );
    expect(list.status()).toBe(404);
    const detail = await page.request.get(
      "/api/v1/services/no-such/inspector/requests/no-such-rid",
    );
    expect(detail.status()).toBe(404);
  });
});

test("v0.4.0: inspector API — unauthenticated 401 (no storageState)", async ({ page }) => {
  const list = await page.request.get("/api/v1/services/anything/inspector/requests");
  expect(list.status()).toBe(401);
  expect(await list.json()).toEqual({ error: "unauthorized" });
});
