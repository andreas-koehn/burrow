import { test, expect } from "@playwright/test";

// P6A.4 — ⌘K command palette end-to-end spec.
//
// Tests run against the real burrowd server (no MSW mocks). The seeded
// admin account is used via the shared playwright-auth.json storage state.
// A seeded service named "web" is created during the test setup (see
// global-setup.ts) so we can reliably search for it in the palette.
//
// These tests are --list-only verified; actual execution requires the
// compose stack or a running burrowd (not run in CI by default —
// see the `e2e:run` task in Taskfile.yml for the full suite).

test.use({ storageState: "playwright-auth.json" });

test("palette opens via Ctrl+K and closes via Escape", async ({ page }) => {
  await page.goto("/");

  // Dialog must not be present initially.
  await expect(page.getByRole("dialog", { name: "Jump to…" })).not.toBeVisible();

  // Open palette with Ctrl+K.
  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Jump to…" });
  await expect(dialog).toBeVisible();

  // Close with Escape.
  await page.keyboard.press("Escape");
  await expect(dialog).not.toBeVisible();
});

test("palette: typing a seeded service name shows a result, Enter navigates", async ({ page }) => {
  await page.goto("/");

  // Open palette.
  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Jump to…" });
  await expect(dialog).toBeVisible();

  // The seeded service is named "web" (svc_web01 in the dev seed; the e2e
  // seed creates a service via the API in global-setup.ts).
  const searchInput = dialog.getByRole("searchbox");
  await searchInput.fill("web");

  // At least one result item should appear.
  const item = dialog.getByRole("option", { name: /web.*service/i }).first();
  await expect(item).toBeVisible();

  // Press Enter → URL changes to the services page or the specific service.
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/(services|services\/)/);
});

test("palette: affordance button in sidebar opens the palette", async ({ page }) => {
  await page.goto("/");

  // The sidebar affordance button must be present.
  const affordanceBtn = page.getByRole("button", { name: /search/i }).first();
  await expect(affordanceBtn).toBeVisible();

  // Clicking it opens the palette.
  await affordanceBtn.click();
  await expect(page.getByRole("dialog", { name: "Jump to…" })).toBeVisible();
});

test("palette: typing 'zzz' shows no-matches state", async ({ page }) => {
  await page.goto("/");

  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Jump to…" });
  await expect(dialog).toBeVisible();

  await dialog.getByRole("searchbox").fill("zzz");
  await expect(dialog.getByText(/no matches/i)).toBeVisible();
});
