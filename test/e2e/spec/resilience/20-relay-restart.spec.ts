// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH, loginAsAdmin } from "../../fixtures/auth";
import { TUNNEL_NAMES, resetAiSubdomainCache } from "../../fixtures/env";
import { composeRestartRelay } from "../../fixtures/stack";

test.use({ storageState: AUTH_STORAGE_PATH });

test("20-relay-restart: all 4 clients reconnect within 60s", async ({ page }) => {
  test.setTimeout(180_000);

  await page.goto("/services?live=1");
  const table = page.locator('table[aria-label="Services"]');
  for (const name of TUNNEL_NAMES) {
    await expect(
      table.locator("tr").filter({ hasText: name }).getByText("connected", { exact: true }),
    ).toBeVisible({ timeout: 15_000 });
  }

  composeRestartRelay();
  // The relay's in-memory subdomain registry is wiped; clear cache.
  resetAiSubdomainCache();
  // Session cookie may be cleared by the restart; re-login if redirected.
  await page.waitForTimeout(5_000);
  if (page.url().includes("/login")) {
    await loginAsAdmin(page);
  } else {
    await page.reload();
  }

  for (const name of TUNNEL_NAMES) {
    await expect(
      page.locator('table[aria-label="Services"] tr').filter({ hasText: name }).getByText("connected", { exact: true }),
      `${name} reconnects`,
    ).toBeVisible({ timeout: 60_000 });
  }
});
