import { test, expect } from "@playwright/test";

test.use({ storageState: "playwright-auth.json" });

test("settings: an admin lands on General, and the navigation replaces the card grid", async ({ page }) => {
  await page.goto("/settings");
  await expect(page).toHaveURL(/\/settings\/general$/);
  await expect(page.getByRole("heading", { name: "General", exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Privacy", exact: true })).toBeVisible();
  // The Configuration card grid is gone; its pages are entries of the Settings navigation.
  await expect(page.getByRole("heading", { name: /^configuration$/i })).toHaveCount(0);

  const sidebar = page.locator(".sidebar");
  await expect(sidebar.getByRole("link", { name: "General", exact: true })).toHaveAttribute("aria-current", "page");
  await sidebar.getByRole("link", { name: "Email", exact: true }).click();
  await expect(page).toHaveURL(/\/settings\/email$/);
  await expect(page.getByLabel("SMTP server")).toBeVisible();
});

test("settings: old bookmarks land on the page's new address", async ({ page }) => {
  await page.goto("/audit?actor=x");
  await expect(page).toHaveURL(/\/settings\/audit\?actor=x$/);
  await page.goto("/account");
  await expect(page).toHaveURL(/\/settings\/profile$/);
  await expect(page.getByRole("heading", { name: "Profile & password" })).toBeVisible();
});
