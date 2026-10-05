// test-only — never deploy this shape.
//
// Spec 40 — Client token revoke flow.
//
// Real-DOM notes (verified against web/src/pages/Tokens.tsx + the live stack):
//   - The tokens are the Tokens tab of Clients (/clients?tab=tokens); the h1 is "Clients".
//   - Mint form: #token-name + a primary "Create" button.
//   - On mint a Dialog titled "Copy your token now" reveals the secret; its
//     footer has a "Done" button.
//   - Tokens table is <table className="data" aria-label="Tokens">. Each row's
//     revoke button carries aria-label `Revoke token {name}`.
//   - Revoke opens a confirm Dialog titled "Revoke token?" whose footer has a
//     destructive "Revoke" button (Cancel + Revoke). Confirming DELETEs the
//     token and the row disappears. No cleanup needed (the token is gone).
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({ storageState: AUTH_STORAGE_PATH });

test("40-token-revoke: mint then revoke via confirm dialog", async ({ page }) => {
  await page.goto("/clients?tab=tokens");
  await expect(page.getByRole("tab", { name: "Tokens", selected: true })).toBeVisible();

  const name = `revoke${Date.now()}`;

  // --- Mint ---
  await page.fill("#token-name", name);
  await page.getByRole("button", { name: "Create", exact: true }).click();

  // "Copy your token now" dialog → Done.
  const minted = page.getByRole("dialog", { name: "Copy your token now" });
  await expect(minted).toBeVisible();
  await minted.getByRole("button", { name: "Done", exact: true }).click();
  await expect(minted).not.toBeVisible();

  // The token row is present in the table.
  const table = page.locator('table[aria-label="Tokens"]');
  const row = table.locator("tbody tr").filter({ hasText: name });
  await expect(row).toBeVisible();

  // --- Revoke ---
  await row.getByRole("button", { name: `Revoke token ${name}` }).click();

  // Confirm dialog "Revoke token?" → click the footer's destructive "Revoke".
  const confirm = page.getByRole("dialog", { name: "Revoke token?" });
  await expect(confirm).toBeVisible();
  await confirm.getByRole("button", { name: "Revoke", exact: true }).click();

  // The row disappears from the table.
  await expect(row).not.toBeVisible({ timeout: 10_000 });
});
