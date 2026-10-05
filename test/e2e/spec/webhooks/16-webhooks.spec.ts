// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({ storageState: AUTH_STORAGE_PATH });

test("16-webhooks: page renders + New webhook flow opens", async ({ page }) => {
  await page.goto("/webhooks");
  // The page now renders both an <h1>Webhooks</h1> (PageHeader title) and an
  // <h2>Configured webhooks</h2> section heading; non-exact match resolved to
  // both. Pin to the exact page title.
  await expect(page.getByRole("heading", { name: "Webhooks", exact: true })).toBeVisible();
  await expect(page.locator('table[aria-label="Webhooks"]')).toBeVisible();

  // Open New webhook dialog (exercises the write path entrypoint).
  await page.getByRole("button", { name: "New webhook" }).click();
  await expect(page.locator('[role="dialog"]')).toBeVisible({ timeout: 5_000 });
});
