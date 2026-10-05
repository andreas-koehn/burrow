// test-only — never deploy this shape.
//
// Plan-fidelity note: the plan-as-written invented selectors (#invite-email,
// New role button, viewer role) that don't match the v0.5.2 UI. Real UI:
//   - "New user" button opens CreateUserDialog with cu-email / cu-pw / cu-role
//   - Built-in roles are admin + user only; custom roles via /roles editor are
//     a v0.4 surface — not exercised here to keep the spec resilient.
//   - Deletion is via a confirm dialog after clicking the per-row Delete button.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({ storageState: AUTH_STORAGE_PATH });

test("05-users-roles: create user (built-in role) + delete", async ({ page }) => {
  const email = `bob-${Date.now()}@e2e.local`;

  await page.goto("/users");
  await expect(page.getByRole("heading", { name: "Users" })).toBeVisible();

  await page.getByRole("button", { name: "New user", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "New user" });
  await expect(dialog).toBeVisible();
  await dialog.locator("#cu-email").fill(email);
  await dialog.locator("#cu-pw").fill("e2e-pass-bob");
  // role left at default "user".
  await dialog.getByRole("button", { name: "Create user", exact: true }).click();

  const row = page.locator('table[aria-label="Users"] tr').filter({ hasText: email });
  await expect(row).toBeVisible();

  // Delete the user; confirm via the confirmation dialog.
  await row.getByRole("button", { name: `Delete user ${email}` }).click();
  // The page renders an inline Dialog for confirmation; the action button label
  // is "Delete" (a danger-styled primary). If a different label is used the
  // spec will surface that in the failure output.
  const confirm = page.getByRole("dialog").last();
  await confirm.getByRole("button", { name: /delete/i }).click();

  await expect(row).not.toBeVisible({ timeout: 10_000 });
});
