// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

test.use({ storageState: AUTH_STORAGE_PATH });

test("38-user-edit: change a user's role to Admin via the Edit dialog", async ({ page, request }) => {
  const email = `edituser-${Date.now()}@e2e.local`;

  const created = await request.post("/api/v1/users", {
    headers: adminHeaders(),
    data: { email, password: "init-pass-123", role: "user" },
  });
  expect(created.status()).toBe(201);
  const userId = (await created.json()).id as string;
  expect(userId).toBeTruthy();

  try {
    await page.goto("/settings/users");

    // Filter to just this user via the searchbox.
    await page.getByRole("searchbox").fill(email);
    const row = page.locator("tr").filter({ hasText: email });
    await expect(row).toBeVisible({ timeout: 5_000 });
    // Sanity: starts as a User.
    await expect(row).toContainText("User");

    // Open the Edit dialog (scope the button to this row). exact:true is
    // required — the Delete button's aria-label "Delete user edituser-…"
    // substring-matches a loose "Edit".
    await row.getByRole("button", { name: "Edit", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Edit user" });
    await expect(dialog).toBeVisible();

    // #eu-role is a DS Select — a custom listbox, not a native <select>. Open
    // it and pick "Admin" from the option list, which is portalled to <body>,
    // outside the dialog.
    await dialog.locator("#eu-role").click();
    await page.getByRole("option", { name: "Admin", exact: true }).click();
    await dialog.getByRole("button", { name: "Save changes" }).click();

    // Toast confirms the update.
    await expect(page.getByText("User updated")).toBeVisible({ timeout: 5_000 });

    // The row's Role badge now shows "Admin". Re-filter to force a fresh row.
    await page.getByRole("searchbox").fill(email);
    const updatedRow = page.locator("tr").filter({ hasText: email });
    await expect(updatedRow).toBeVisible({ timeout: 5_000 });
    await expect(updatedRow).toContainText(/admin/i);
  } finally {
    await request.delete(`/api/v1/users/${userId}`, { headers: adminHeaders() });
  }
});
