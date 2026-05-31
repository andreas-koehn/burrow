// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { loginAsAdmin, AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { TUNNEL_NAMES } from "../../fixtures/env";

test("01-bootstrap: login + all 4 seeded tunnels visible + connected", async ({ page, context }) => {
  await loginAsAdmin(page);

  // / now renders <Home /> (Overview); navigate explicitly to /tunnels.
  await page.goto("/tunnels");
  await expect(page.getByRole("heading", { name: "Tunnels" })).toBeVisible();

  const table = page.locator('table[aria-label="Tunnels"]');
  for (const name of TUNNEL_NAMES) {
    const row = table.locator("tr").filter({ hasText: name });
    await expect(row, `${name} row visible`).toBeVisible({ timeout: 15_000 });
    await expect(
      row.getByText("connected", { exact: true }),
      `${name} connected`,
    ).toBeVisible({ timeout: 15_000 });
  }

  await context.storageState({ path: AUTH_STORAGE_PATH });
});
