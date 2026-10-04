import { test, expect } from "@playwright/test";

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
