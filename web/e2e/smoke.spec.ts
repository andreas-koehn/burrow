import { test, expect } from "@playwright/test";

// Happy-path smoke test against the REAL built burrowd (no mocks).
// Exercises: login → tunnels dashboard → token create+revoke (one-time dialog)
//            → theme toggle (.dark class) → change-password + re-login → logout.

const E2E_EMAIL = "e2e@example.com";
const E2E_INITIAL_PASSWORD = "e2e-password-123";
const E2E_NEW_PASSWORD = "e2e-password-456";

test("smoke: full happy-path", async ({ page }) => {
  // ── 1. Login ──────────────────────────────────────────────────────────────
  // Navigate to root; RequireAuth redirects unauthenticated users to /login.
  await page.goto("/");
  await expect(page).toHaveURL(/\/login/);

  // Use the id-tied labels from Login.tsx: htmlFor="login-email" / "login-password"
  await page.getByLabel("Email").fill(E2E_EMAIL);
  await page.getByLabel("Password").fill(E2E_INITIAL_PASSWORD);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();

  // ── 2. Home dashboard ────────────────────────────────────────────────────
  // After login the SPA routes to / which now renders <Home /> (Overview).
  await expect(page).toHaveURL(/\//);
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();

  // An old /tunnels bookmark opens Services on Live; verify its empty-state.
  await page.goto("/tunnels");
  await expect(page).toHaveURL(/\/services\?live=1$/);
  await expect(page.getByRole("heading", { name: "Services", level: 1 })).toBeVisible();
  await expect(page.getByRole("radio", { name: "Live" })).toBeChecked();
  // Empty-state message (no burrow clients connected in CI)
  await expect(
    page.getByText("Nothing is live right now", { exact: false }),
  ).toBeVisible();

  // ── 3. Tokens: create → one-time dialog → revoke ─────────────────────────
  await page.locator(".sidebar").getByRole("link", { name: /^Clients(,|$)/ }).click();
  await page.getByRole("tab", { name: "Tokens" }).click();
  await expect(page).toHaveURL(/\/clients\?tab=tokens$/);

  // Fill token name — Label htmlFor="token-name" in ClientTokensPanel.tsx
  await page.getByLabel("Token name").fill("e2e-smoke");
  await page.getByRole("button", { name: "Create" }).click();

  // One-time dialog: DialogTitle="Copy your token now"; the revealed token
  // lives in the design-system reveal panel `.reveal-once .key-row .v`
  // (design/tokens.jsx) — the pre-re-skin `<pre>` no longer exists.
  await expect(
    page.getByRole("heading", { name: "Copy your token now" }),
  ).toBeVisible();
  const tokenText = await page.locator(".reveal-once .v").textContent();
  expect(tokenText).toMatch(/^bur_/);

  // Close the dialog
  await page.getByRole("button", { name: "Done" }).click();
  await expect(
    page.getByRole("heading", { name: "Copy your token now" }),
  ).not.toBeVisible();

  // Token name cell appears in the table (exact match to avoid the Revoke cell)
  await expect(page.getByRole("cell", { name: "e2e-smoke", exact: true })).toBeVisible();

  // Revoke — the per-row button (aria-label `Revoke token ${t.name}`) now opens
  // a confirmation dialog ("Revoke token?") with a confirming "Revoke" button.
  await page.getByRole("button", { name: "Revoke token e2e-smoke" }).click();
  const revokeDialog = page.getByRole("dialog", { name: "Revoke token?" });
  await expect(revokeDialog).toBeVisible();
  await revokeDialog.getByRole("button", { name: "Revoke", exact: true }).click();

  // Row disappears after revocation
  await expect(page.getByRole("cell", { name: "e2e-smoke", exact: true })).not.toBeVisible();

  // ── 4. Theme toggle: .dark class ─────────────────────────────────────────
  // Layout.tsx aria-label: "Switch to dark theme" (when currently light)
  const themeToggle = page.getByRole("button", { name: /Switch to (dark|light) theme/ });

  // Ensure we start from light mode
  const htmlHandle = page.locator("html");
  const isDark = async () => (await htmlHandle.getAttribute("class") ?? "").includes("dark");

  if (await isDark()) {
    // Already dark — toggle to light first to get a predictable baseline
    await themeToggle.click();
    await expect(htmlHandle).not.toHaveClass(/dark/);
  }

  // Toggle to dark
  await themeToggle.click();
  await expect(htmlHandle).toHaveClass(/dark/);

  // Toggle back to light
  await themeToggle.click();
  await expect(htmlHandle).not.toHaveClass(/dark/);

  // ── 5. Account: change password ──────────────────────────────────────────
  // The user chip at the foot of the sidebar leads to the profile page.
  await page.getByRole("link", { name: /^Your profile/ }).click();
  await expect(page.getByRole("heading", { name: "Change password" })).toBeVisible();

  // Labels from Account.tsx: htmlFor="current-password" / "new-password" / "confirm-password"
  await page.getByLabel("Current password", { exact: true }).fill(E2E_INITIAL_PASSWORD);
  await page.getByLabel("New password", { exact: true }).fill(E2E_NEW_PASSWORD);
  await page.getByLabel("Confirm new password", { exact: true }).fill(E2E_NEW_PASSWORD);
  await page.getByRole("button", { name: "Change password" }).click();

  // Success toast from Account.tsx: toast.success("Password changed successfully")
  await expect(
    page.getByText("Password changed successfully", { exact: false }),
  ).toBeVisible();

  // ── 6. Log out + re-login with new password ───────────────────────────────
  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page).toHaveURL(/\/login/);

  // Re-login with the NEW password to prove it persisted
  await page.getByLabel("Email").fill(E2E_EMAIL);
  await page.getByLabel("Password").fill(E2E_NEW_PASSWORD);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();

  // ── 7. Final logout ──────────────────────────────────────────────────────
  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page).toHaveURL(/\/login/);
});
