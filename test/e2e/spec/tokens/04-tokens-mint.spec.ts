// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({ storageState: AUTH_STORAGE_PATH });

test("04-tokens-mint: UI form mints bur_*; dialog reveals; list updates", async ({ page }) => {
  await page.goto("/clients?tab=tokens");
  await expect(page.getByRole("tab", { name: "Tokens", selected: true })).toBeVisible();

  const name = `e2e-full-04-${Date.now()}`;
  await page.fill("#token-name", name);
  await page.getByRole("button", { name: "Create", exact: true }).click();

  const dialog = page.getByRole("dialog", { name: "Copy your token now" });
  await expect(dialog).toBeVisible();
  const tok = (await dialog.locator(".reveal-once .key-row .v").innerText()).trim();
  expect(tok).toMatch(/^bur_/);
  expect(tok.length).toBeGreaterThanOrEqual(20);

  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(page.locator('table[aria-label="Tokens"] tr').filter({ hasText: name })).toBeVisible();
});
