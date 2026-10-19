// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { ensureReauth } from "../../fixtures/reauth";

test.use({ storageState: AUTH_STORAGE_PATH });
// No services domain: guarded writes need a recent password confirmation (T02b).
test.beforeEach(async ({ request }) => { await ensureReauth(request); });

test("18-retention: page renders + at least one knob present", async ({ page }) => {
  await page.goto("/settings/retention");
  await expect(page.getByRole("heading", { name: /Retention/i })).toBeVisible();

  // At least one retention input field should exist (id pattern: ret-<key>).
  const knob = page.locator('input[id^="ret-"]').first();
  await expect(knob).toBeVisible();
});
