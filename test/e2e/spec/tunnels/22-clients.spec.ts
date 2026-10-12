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
      row.getByText("Online", { exact: true }),
      `${name} online`,
    ).toBeVisible({ timeout: 10_000 });
  }
});

test("22-clients: Connect a client shows the lines to run and leads to Tokens for a token", async ({ page }) => {
  await loginAsAdmin(page);
  await page.goto("/clients/connect");

  // The lines to run on the machine; none of them holds a token.
  await expect(page.locator(".install-line pre code")).toHaveText([
    /\/install\.(sh \| sh|ps1 \| iex)$/, /^burrow login \S+$/, "burrow http 3000",
  ]);

  // The page mints no token itself (spec 04 covers minting): it says where one is made.
  await page.getByRole("button", { name: "Other ways to connect" }).click();
  await expect(page.getByRole("button", { name: /generate token/i })).toHaveCount(0);
  await page.getByRole("link", { name: "Clients → Tokens" }).click();
  await expect(page.getByRole("tab", { name: "Tokens", selected: true })).toBeVisible();
  await expect(page.locator("#token-name")).toBeVisible();
});
