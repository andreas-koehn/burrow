// test-only — never deploy this shape.
//
// Plan-fidelity note: the AccessModePanel radio labels are full strings
// like "Open — raw passthrough" (not just "Open") and the save button is
// "Save changes" (not "Save"). The plan's selectors didn't match.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { HTTPS_INGRESS, aiHost } from "../../fixtures/env";

test.use({ storageState: AUTH_STORAGE_PATH });

test("06-access-mode-open: ai service in Open mode accepts unauthenticated GET", async ({ page, request }) => {
  await page.goto("/services?live=1");
  const aiRow = page.locator('table[aria-label="Services"] tr').filter({ hasText: "ai" });
  await aiRow.getByRole("button", { name: "Configure" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("radio", { name: /^Open/ }).click();
  await dialog.getByRole("button", { name: "Save changes" }).click();
  // Wait for the toast/close to settle so the relay applies the mode.
  await page.waitForTimeout(500);

  const res = await request.get(`${HTTPS_INGRESS}/healthz`, {
    headers: { host: aiHost() },
    ignoreHTTPSErrors: true,
  });
  expect(res.status()).toBe(200);
});
