// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { loginAsAdmin } from "../../fixtures/auth";

test("22-clients: all 3 seeded clients visible + connected", async ({ page }) => {
  await loginAsAdmin(page);
  await page.goto("/clients");
  await expect(page.getByRole("heading", { name: "Clients" })).toBeVisible();

  const table = page.locator('table[aria-label="Clients"]');
  for (const name of ["e2e-ai", "e2e-multi", "e2e-tcp"]) {
    const row = table.locator("tr").filter({ hasText: name });
    await expect(row, `${name} row visible`).toBeVisible({ timeout: 10_000 });
    await expect(
      row.getByText("connected", { exact: true }),
      `${name} connected`,
    ).toBeVisible({ timeout: 10_000 });
  }
});

test("22-clients: Connect-a-client wizard mints a token", async ({ page }) => {
  await loginAsAdmin(page);
  await page.goto("/clients/connect");
  // The form lives behind "Other ways to connect" since the page leads with the three lines.
  await page.getByRole("button", { name: "Other ways to connect" }).click();

  await page.getByLabel("Client name").fill(`spec-22-${Date.now()}`);
  await page.getByRole("button", { name: /generate token/i }).click();

  // Click Reveal to unmask the token (default is masked as bur_••••••••).
  await page.getByRole("button", { name: /reveal token/i }).click();

  // Reveal dialog should expose a bur_ prefixed plaintext at least once.
  // Find the code element with the token (not the command block).
  const tokenCode = page.locator('code.mono', { hasText: /^bur_[A-Za-z0-9_-]{20,}$/ });
  await expect(tokenCode).toBeVisible({ timeout: 10_000 });
});
