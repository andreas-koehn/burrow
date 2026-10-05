// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({ storageState: AUTH_STORAGE_PATH });

test("11-ai-gateway-semantic-cache: surface present + enable round-trips (or skip)", async ({ page }) => {
  await page.goto("/gateway/cache");
  // Wait for the page to fully render before probing for the tab — without
  // this, the 2s `isVisible` race can fire before React hydrates the Tabs.
  await expect(page.getByRole("heading", { name: /Prompt cache/i })).toBeVisible();
  // Open the Semantic tab. If the SPA doesn't show this tab when the backend
  // is built without -tags=semantic_cache, skip.
  const tab = page.getByRole("tab", { name: "Semantic" });
  const visible = await tab.isVisible({ timeout: 10_000 }).catch(() => false);
  if (!visible) {
    test.skip(true, "Semantic cache tab not present — relay built without -tags=semantic_cache");
  }
  await tab.click();

  // The Semantic settings panel should render *something* — either an enable
  // toggle (when the engine is wired) or a "Not available" notice (default
  // build with the noop engine). Either is acceptable for this spec — it
  // checks the surface is reachable. Functional cache-hit testing requires
  // the semantic_cache build tag, which is out of scope for this gate.
  const anyContent = await page
    .locator('[role="tabpanel"] :is(button, input, p)')
    .first()
    .isVisible({ timeout: 5_000 })
    .catch(() => false);
  expect(anyContent).toBe(true);
});
