import { test, expect } from "@playwright/test";

// v0.4.0: /roles — editable custom roles + permission matrix (spec Part I).

test.use({ storageState: "playwright-auth.json" });

test("v0.4.0: create a custom role with one permission grant", async ({ page }) => {
  await page.goto("/settings/roles");
  await expect(page.getByRole("heading", { name: "Roles" })).toBeVisible();

  await page.getByRole("button", { name: "New role" }).click();
  const dlg = page.getByRole("dialog", { name: "New role" });
  await expect(dlg).toBeVisible();

  const roleName = `e2e-curator-${Date.now()}`;
  await dlg.getByLabel("Name").fill(roleName);
  await dlg.getByLabel("Description").fill("E2E custom role with one grant");

  // Permissions tab — each permission row exposes a Switch whose aria-label
  // is the permission key (CustomRoleEditor.tsx).
  await dlg.getByRole("tab", { name: "Permissions" }).click();
  const auditSwitch = dlg.getByRole("switch", { name: "audit:read" });
  await expect(auditSwitch).toBeVisible();
  await auditSwitch.click();

  await dlg.getByRole("button", { name: "Create" }).click();
  await expect(dlg).not.toBeVisible();

  const row = page.locator("tbody tr").filter({ hasText: roleName });
  await expect(row).toHaveCount(1);
  await expect(row.getByText("Custom", { exact: true })).toBeVisible();
});

test("v0.4.0: roles API — POST /roles persists with the granted permission", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  const name = `e2e-readonly-${Date.now()}`;
  const created = await page.request.post("/api/v1/roles", {
    headers,
    data: {
      name,
      description: "Audit-only access",
      permissions: ["audit:read"],
      default_for_new_users: false,
    },
  });
  expect(created.status()).toBe(201);

  const detail = await page.request.get(`/api/v1/roles/${name}`);
  expect(detail.status()).toBe(200);
  const body = (await detail.json()) as Record<string, unknown>;
  expect(Array.isArray(body.permissions)).toBe(true);
  expect((body.permissions as string[]).includes("audit:read")).toBe(true);
});
