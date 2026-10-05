import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";

// v0.2.0 admin flows against the REAL built burrowd (no MSW). Exercises the
// new v0.2.0 surfaces end-to-end through the embedded SPA:
//   login → Users (list + create POST + edit PATCH + delete DELETE) → Roles
//   (detail + code permissions) → Settings (GET + PUT round-trip) → Clients
//   (empty state) → access-mode endpoint contract (PUT, CSRF, 404 shape).
//
// The seeded admin (BURROW_ADMIN_EMAIL/PASSWORD from playwright.config.ts) is
// the only account in the fresh e2e DB.

const ADMIN_EMAIL = "e2e@example.com";
const ADMIN_PASSWORD = "e2e-password-123";
const NEW_USER_EMAIL = "teammate@example.com";
const NEW_USER_PASSWORD = "teammate-password-1";

async function login(page: Page) {
  await page.goto("/");
  await expect(page).toHaveURL(/\/login/);
  await page.getByLabel("Email").fill(ADMIN_EMAIL);
  await page.getByLabel("Password").fill(ADMIN_PASSWORD);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
}

test("v0.2.0 admin: users CRUD + roles + settings + clients + access-mode", async ({ page }) => {
  await login(page);

  // ── Users: list (GET /users → {users,total}) ──────────────────────────────
  // The sidebar's admin shortcut; scoped to the sidebar because the breadcrumb repeats names.
  const sidebar = page.locator(".sidebar");
  await sidebar.getByRole("link", { name: "Users & roles" }).click();
  await expect(page.getByRole("heading", { name: "Users" })).toBeVisible();
  const adminRow = page.locator("tbody tr").filter({ hasText: ADMIN_EMAIL });
  await expect(adminRow).toHaveCount(1);

  // ── Users: create (POST /users → 201) ─────────────────────────────────────
  await page.getByRole("button", { name: "New user", exact: true }).click();
  const createDlg = page.getByRole("dialog");
  await expect(createDlg.getByRole("heading", { name: "New user" })).toBeVisible();
  await createDlg.getByLabel("Email").fill(NEW_USER_EMAIL);
  await createDlg.getByLabel("Password").fill(NEW_USER_PASSWORD);
  await createDlg.getByRole("button", { name: "Create user", exact: true }).click();
  await expect(page.getByText("User created", { exact: false })).toBeVisible();
  const newRow = page.locator("tbody tr").filter({ hasText: NEW_USER_EMAIL });
  await expect(newRow).toHaveCount(1);
  await expect(newRow.getByText("User", { exact: true })).toBeVisible();

  // ── Users: edit role user→admin (PATCH /users/{id} → 204) ──────────────────
  await newRow.getByRole("button", { name: "Edit" }).click();
  const editDlg = page.getByRole("dialog");
  await expect(editDlg.getByRole("heading", { name: "Edit user" })).toBeVisible();
  // ds Select is a custom listbox: click its trigger, then the option.
  await editDlg.locator(".select-trigger").click();
  await page.getByRole("option", { name: "Admin" }).click();
  await editDlg.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByText("User updated", { exact: false })).toBeVisible();
  await expect(
    page.locator("tbody tr").filter({ hasText: NEW_USER_EMAIL }).getByText("Admin", { exact: true }),
  ).toBeVisible();

  // ── Users: delete (DELETE /users/{id} → 204) ──────────────────────────────
  await page
    .locator("tbody tr")
    .filter({ hasText: NEW_USER_EMAIL })
    .getByRole("button", { name: `Delete user ${NEW_USER_EMAIL}` })
    .click();
  const delDlg = page.getByRole("dialog");
  await expect(delDlg.getByRole("heading", { name: "Delete user?" })).toBeVisible();
  await delDlg.getByRole("button", { name: "Delete user", exact: true }).click();
  await expect(page.getByText("User deleted", { exact: false })).toBeVisible();
  await expect(page.locator("tbody tr").filter({ hasText: NEW_USER_EMAIL })).toHaveCount(0);

  // ── Roles: list + detail w/ code-defined permissions ──────────────────────
  // Users is a Settings page: the sidebar already shows the Settings navigation
  // (and no footer shortcut), so Roles is one click away.
  await sidebar.getByRole("link", { name: "Roles", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Roles" })).toBeVisible();
  const adminRoleRow = page.locator("tbody tr").filter({ hasText: "admin" });
  await expect(adminRoleRow).toHaveCount(1);
  await adminRoleRow.getByRole("button", { name: "View" }).click();
  const roleDlg = page.getByRole("dialog");
  // Permissions come from internal/authz (admin → full set incl users:manage).
  await expect(roleDlg.getByText("users:manage", { exact: true })).toBeVisible();
  await roleDlg.getByRole("button", { name: "Close", exact: true }).click();

  // ── Settings: GET then PUT round-trip (whitelisted keys persist) ──────────
  // Still inside Settings (Roles): its navigation lists the Email page.
  await sidebar.getByRole("link", { name: /^Email(,|$)/ }).click();
  await expect(page.getByRole("heading", { name: "Email", exact: true })).toBeVisible();
  await page.getByLabel("SMTP server").fill("smtp.e2e.example.com");
  await page.getByLabel("Port").fill("587");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Email settings saved", { exact: false })).toBeVisible();
  // Round-trip: leave and return; GET /settings must return the persisted host.
  await sidebar.getByRole("link", { name: /^Back to / }).click();
  await sidebar.getByRole("link", { name: /^Clients/ }).click();
  await sidebar.getByRole("link", { name: /^Settings(,|$)/ }).click();
  await sidebar.getByRole("link", { name: /^Email(,|$)/ }).click();
  await expect(page.getByLabel("SMTP server")).toHaveValue("smtp.e2e.example.com");

  // ── Clients: empty state (GET /clients → []) ─────────────────────────────
  await sidebar.getByRole("link", { name: /^Back to / }).click();
  await sidebar.getByRole("link", { name: /^Clients/ }).click();
  await expect(page.getByRole("heading", { name: "Clients" })).toBeVisible();
  await expect(page.getByText("No clients connected", { exact: false })).toBeVisible();

  // ── Access-mode endpoint contract ─────────────────────────────────────────
  // The AccessModePanel UI renders in the ServiceDetail access tab and the
  // Tunnels/Services Configure dialogs; a pure web e2e has no connected
  // burrow data-plane, so the panel itself is covered by
  // AccessModePanel.test.tsx (unit). Here we assert the exact request the
  // panel issues — PUT
  // /api/v1/tunnels/{id}/access-mode with the double-submit X-CSRF-Token —
  // reaches the real handler and honours the contract: 404 {"error":"tunnel
  // not found"} for an unknown/unowned id, and 403 {"error":"csrf token
  // invalid"} when the header is omitted. Proves the seam end-to-end.
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  expect(csrf).not.toBe("");
  const res = await page.request.put("/api/v1/tunnels/no-such-tunnel/access-mode", {
    headers: { "X-CSRF-Token": csrf, "Content-Type": "application/json" },
    data: { access_mode: "open" },
  });
  expect(res.status()).toBe(404);
  expect(await res.json()).toEqual({ error: "tunnel not found" });

  const noCsrf = await page.request.put("/api/v1/tunnels/no-such-tunnel/access-mode", {
    headers: { "Content-Type": "application/json" },
    data: { access_mode: "open" },
  });
  expect(noCsrf.status()).toBe(403);
  expect(await noCsrf.json()).toEqual({ error: "csrf token invalid" });

  // ── Logout ────────────────────────────────────────────────────────────────
  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page).toHaveURL(/\/login/);
});
