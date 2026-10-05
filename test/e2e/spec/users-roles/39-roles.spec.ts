// test-only — never deploy this shape.
//
// Spec 39 — Roles view + custom-role CRUD.
//
// Real-DOM notes (verified against web/src/pages/Roles.tsx +
// web/src/components/CustomRoleEditor.tsx and the live stack):
//   - Roles table is <table className="data" aria-label="Roles">.
//   - Built-in roles (admin, user) render a "View" button → RoleDetailDialog
//     whose accessible name is the role name and whose body holds
//     <ul aria-label="Permissions" className="perm-list"> of <code className="perm-key">.
//   - "New role" opens the CustomRoleEditor Dialog (title "New role") with the
//     DS Tabs component (role="tab"). General tab → #role-name / #role-desc.
//     Permissions tab → DS Switch per permission (role="switch", name=p.key).
//   - Create button label is "Create"; on success CustomRoleEditor toasts
//     "Role created." (sonner) and the new row gets a "Custom" badge.
//
// UI quirk (handled honestly, not faked): CustomRoleEditor renders its
// <Toaster> *inside* its Dialog, and onSuccess closes (unmounts) that dialog.
// The "Role created." toast therefore lives in a Toaster that is torn down the
// instant creation succeeds, so it is racy/often-never visible to the test.
// We assert it best-effort (soft) and treat the new "Custom"-badged row as the
// authoritative proof of success.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

test.use({ storageState: AUTH_STORAGE_PATH });

test("39-roles: view built-in perms + create custom role", async ({ page, request }) => {
  const roleName = `uirole${Date.now()}`;

  try {
    await page.goto("/settings/roles");
    await expect(page.getByRole("heading", { name: "Roles", level: 1 })).toBeVisible();

    const table = page.locator('table[aria-label="Roles"]');

    // --- VIEW a built-in role (admin) ---
    const adminRow = table.locator("tbody tr").filter({ hasText: /admin/i }).first();
    await adminRow.getByRole("button", { name: "View", exact: true }).click();

    // RoleDetailDialog: accessible name == the role name ("admin").
    const viewDialog = page.getByRole("dialog", { name: "admin" });
    await expect(viewDialog).toBeVisible();
    const permList = viewDialog.locator('ul[aria-label="Permissions"]');
    await expect(permList).toBeVisible();
    await expect(permList.locator("code.perm-key").first()).toBeVisible();
    await viewDialog.getByRole("button", { name: "Close", exact: true }).click();
    await expect(viewDialog).not.toBeVisible();

    // --- CREATE a custom role ---
    await page.getByRole("button", { name: /new role/i }).click();
    const editor = page.getByRole("dialog", { name: "New role" });
    await expect(editor).toBeVisible();

    // General tab is the default. Fill name + description.
    await editor.locator("#role-name").fill(roleName);
    await editor.locator("#role-desc").fill("e2e");

    // Switch to the Permissions tab.
    await editor.getByRole("tab", { name: "Permissions", exact: true }).click();

    // Toggle ONE permission switch on. Prefer a stable, low-impact key if it
    // exists; otherwise grab any visible permission switch. (The switches use
    // aria-label === the permission key.)
    const preferred = editor.getByRole("switch", { name: "audit:read", exact: true });
    const toggle = (await preferred.count()) > 0
      ? preferred
      : editor.getByRole("switch").first();
    await expect(toggle).toBeVisible();
    await toggle.click();

    // Create. Soft-assert the (racy, dialog-scoped) success toast in parallel
    // with the click so we catch it if it flashes before the dialog unmounts.
    const toast = page.getByText("Role created.");
    await Promise.all([
      toast.waitFor({ state: "visible", timeout: 4_000 }).then(
        () => expect.soft(toast).toBeVisible(),
        () => undefined, // toast torn down with the dialog — fall back to the row
      ),
      editor.getByRole("button", { name: "Create", exact: true }).click(),
    ]);

    // Authoritative proof of success: the new role row appears with a
    // "Custom" badge in the Roles table.
    const newRow = table.locator("tbody tr").filter({ hasText: roleName });
    await expect(newRow).toBeVisible({ timeout: 10_000 });
    await expect(newRow).toContainText("Custom");
  } finally {
    // Best-effort cleanup: delete the custom role we created.
    await request
      .delete(`/api/v1/roles/${roleName}`, { headers: adminHeaders() })
      .catch(() => undefined);
  }
});
