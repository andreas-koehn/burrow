// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({ storageState: AUTH_STORAGE_PATH });

test("15-audit-chain: UI mint → token.mint audit row → chain valid", async ({ page }) => {
  // Mint a token so the audit chain has an entry.
  await page.goto("/tokens");
  const name = `audit-${Date.now()}`;
  await page.fill("#token-name", name);
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await page.locator('[role="dialog"]', { has: page.getByRole("heading", { name: "Copy your token now" }) }).last().getByRole("button", { name: "Done" }).click();

  // Now check the audit log.
  await page.goto("/settings/audit");
  await expect(page.getByRole("heading", { name: "Audit log" })).toBeVisible();

  // Search for the token's name (audit event JSON contains it).
  await page.getByRole("searchbox", { name: "Search audit events" }).fill(name);
  await page.waitForTimeout(500);

  // At least one row should match. Audit row text includes "token" somewhere
  // (action name like "token.mint" or similar).
  const table = page.locator('table[aria-label="Audit events"]');
  await expect(table.locator("tbody tr").first()).toBeVisible({ timeout: 5_000 });

  // The filtered table must contain a row whose Action cell is exactly "token.mint".
  // Action string is locked: internal/audit/actions.go ActionTokenMint = "token.mint".
  await expect
    .poll(async () => table.locator("tbody tr").filter({ hasText: "token.mint" }).count(), {
      timeout: 5_000,
      message: "Expected at least one row with Action=token.mint after UI mint",
    })
    .toBeGreaterThanOrEqual(1);

  // Verify chain — expect a green success notice (POST /audit/verify).
  await page.getByRole("button", { name: "Verify chain" }).click();
  await expect(page.locator(".notice-inline.ok")).toContainText(/Chain valid from/, { timeout: 5_000 });
});
