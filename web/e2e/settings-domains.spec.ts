import { test, expect } from "@playwright/test";

// P7B.5: Settings hub/form consistency + Custom-domains de-duplication.
//
// Verifies:
//   1. /settings — General panel renders above Configuration cards.
//   2. "Custom domains (all services)" card links to /settings/custom-domains.
//   3. On /settings/custom-domains a service row's link goes to /services/:id/domains.
//   4. An InfoHint tooltip becomes visible on hover.

test.use({ storageState: "playwright-auth.json" });

test("settings: General panel renders above Configuration cards", async ({ page }) => {
  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();

  const generalHeading = page.getByRole("heading", { name: /general/i });
  const configHeading = page.getByRole("heading", { name: /^configuration$/i });

  await expect(generalHeading).toBeVisible();
  await expect(configHeading).toBeVisible();

  // Assert DOM order: General comes before Configuration.
  const generalPos = await generalHeading.evaluate((el) =>
    el.compareDocumentPosition(document.querySelector("[id='sec-configuration']") ?? el)
  );
  // compareDocumentPosition returns bitmask; Node.DOCUMENT_POSITION_FOLLOWING = 4
  // means the argument (Configuration) comes AFTER the calling element (General).
  expect(generalPos & 4).toBeTruthy();
});

test("settings: Custom domains card links to /settings/custom-domains", async ({ page }) => {
  await page.goto("/settings");
  const link = page.getByRole("link", { name: /custom domains \(all services\)/i });
  await expect(link).toBeVisible();
  await link.click();
  await expect(page).toHaveURL(/\/settings\/custom-domains/);
  await expect(page.getByRole("heading", { level: 1, name: "Custom domains (all services)" })).toBeVisible();
});

test("settings/custom-domains: service row links to /services/:id/domains", async ({ page }) => {
  await page.goto("/settings/custom-domains");
  await expect(page.getByRole("heading", { level: 1, name: "Custom domains (all services)" })).toBeVisible();

  // Wait for the table to either show rows or the empty state (EmptyState renders as .state-card).
  const tableOrEmpty = page.locator("table.data, .state-card");
  await expect(tableOrEmpty.first()).toBeVisible({ timeout: 10_000 });

  // If there are service rows, click the first one and verify navigation.
  const serviceLinks = page.locator("table.data tbody tr td:first-child a");
  const count = await serviceLinks.count();
  if (count > 0) {
    const href = await serviceLinks.first().getAttribute("href");
    expect(href).toMatch(/^\/services\/.+\/domains$/);
    await serviceLinks.first().click();
    await expect(page).toHaveURL(/\/services\/.+\/domains/);
  }
});

test("settings/custom-domains: InfoHint tooltip visible on hover", async ({ page }) => {
  await page.goto("/settings/custom-domains");
  await expect(page.getByRole("heading", { level: 1, name: "Custom domains (all services)" })).toBeVisible();

  const hint = page.getByRole("button", { name: /what is custom domains\?/i });
  await expect(hint).toBeVisible();
  await hint.hover();
  await expect(page.getByRole("tooltip")).toBeVisible();
});
