// test-only — never deploy this shape.
//
// The OpenAPI viewer is server-side rendered under /api/v1/openapi/viewer/
// (not a SPA route). Verify it renders, asserts no external CDN scripts,
// and the embedded openapi.yaml endpoint responds.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { ensureReauth } from "../../fixtures/reauth";

test.use({ storageState: AUTH_STORAGE_PATH });
// No services domain: guarded writes need a recent password confirmation (T02b).
test.beforeEach(async ({ request }) => { await ensureReauth(request); });

test("17-openapi-viewer: viewer renders embedded spec; no external CDN scripts", async ({ page, request }) => {
  // openapi.yaml should be reachable for any authenticated admin.
  const yaml = await request.get("/api/v1/openapi.yaml");
  expect(yaml.status()).toBe(200);
  expect((await yaml.text()).startsWith("openapi:")).toBe(true);

  // Capture all script requests during navigation.
  const externalScripts: string[] = [];
  page.on("request", (r) => {
    if (r.resourceType() === "script" && !r.url().startsWith("http://localhost") && !r.url().startsWith("http://127.0.0.1")) {
      externalScripts.push(r.url());
    }
  });

  await page.goto("/api/v1/openapi/viewer/");
  await expect(page.getByText("Burrow API")).toBeVisible();
  await page.waitForLoadState("networkidle");

  expect(externalScripts, "no CDN-hosted scripts allowed").toHaveLength(0);
});
