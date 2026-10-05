// test-only — never deploy this shape.
//
// Regression guard for dialog viewport overflow (B2).
// At 1440×900 the Services Configure dialog content (~1100 px) previously
// spilled above the viewport top with no scroll. This spec ensures the
// dialog box stays entirely within the viewport after the max-height /
// internal-scroll fix.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({
  storageState: AUTH_STORAGE_PATH,
  viewport: { width: 1440, height: 900 },
});

test("21-dialog-tall: Configure dialog fits within the viewport at 1440×900", async ({ page }) => {
  await page.goto("/services?live=1");

  const aiRow = page.locator('table[aria-label="Services"] tr').filter({ hasText: "ai" });
  await expect(aiRow).toBeVisible();
  await aiRow.getByRole("button", { name: "Configure" }).click();

  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();

  const box = await dialog.boundingBox();
  expect(box, "dialog bounding box should be defined").not.toBeNull();

  const viewportSize = page.viewportSize();
  expect(viewportSize, "viewport size should be defined").not.toBeNull();

  // eslint-disable-next-line @typescript-eslint/no-non-null-assertion
  const vp = viewportSize!;
  // eslint-disable-next-line @typescript-eslint/no-non-null-assertion
  const b = box!;

  expect(b.y, "dialog top edge must be within viewport").toBeGreaterThanOrEqual(0);
  expect(b.y + b.height, "dialog bottom edge must be within viewport").toBeLessThanOrEqual(vp.height + 1);
});
