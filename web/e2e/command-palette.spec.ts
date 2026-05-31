import { test, expect } from "@playwright/test";

// P6A.4 — ⌘K command palette end-to-end spec.
//
// Tests run against the real burrowd server (no MSW mocks). The seeded
// admin account is used via the shared playwright-auth.json storage state.
// A service is created via the API in the test that needs it so we can
// reliably search for it in the palette on a fresh empty DB.

test.use({ storageState: "playwright-auth.json" });

test("palette opens via Ctrl+K and closes via Escape", async ({ page }) => {
  await page.goto("/");

  // Dialog must not be present initially.
  await expect(page.getByRole("dialog", { name: "Jump to…" })).not.toBeVisible();

  // Click body to ensure the page has keyboard focus before pressing Ctrl+K.
  await page.locator("body").click();
  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Jump to…" });
  await expect(dialog).toBeVisible();

  // Close with Escape.
  await page.keyboard.press("Escape");
  await expect(dialog).not.toBeVisible();
});

test("palette: typing a service title shows a result, Enter navigates", async ({ page }) => {
  // Create a service via the real API so it appears in palette entity results.
  await page.goto("/");
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };
  const svcId = `palette-e2e-${Date.now()}`;
  const created = await page.request.post("/api/v1/services", {
    headers,
    data: { service_id: svcId, title: svcId, access_mode: "open" },
  });
  expect([201, 409]).toContain(created.status());

  // Open palette via the sidebar Search affordance (more reliable than bare Ctrl+K).
  const affordanceBtn = page.getByRole("button", { name: "Search" }).first();
  await expect(affordanceBtn).toBeVisible();
  await affordanceBtn.click();
  const dialog = page.getByRole("dialog", { name: "Jump to…" });
  await expect(dialog).toBeVisible();

  // Type the unique service id — it matches the service name.
  const searchInput = dialog.getByRole("searchbox");
  await searchInput.fill(svcId);

  // At least one result item should appear containing the service id.
  const item = dialog.getByRole("option", { name: new RegExp(svcId + ".*service", "i") }).first();
  await expect(item).toBeVisible({ timeout: 10_000 });

  // Press Enter → URL changes to the specific service page.
  const beforeURL = page.url();
  await page.keyboard.press("Enter");
  await page.waitForURL((url) => url.toString() !== beforeURL, { timeout: 10_000 });
  expect(page.url()).toContain(`/services/${svcId}`);

  // Clean up: delete the created service.
  void page.request.delete(`/api/v1/services/${svcId}`, { headers }).catch(() => {});
});

test("palette: affordance button in sidebar opens the palette", async ({ page }) => {
  await page.goto("/");

  // The sidebar affordance button must be present.
  const affordanceBtn = page.getByRole("button", { name: "Search" }).first();
  await expect(affordanceBtn).toBeVisible();

  // Clicking it opens the palette.
  await affordanceBtn.click();
  await expect(page.getByRole("dialog", { name: "Jump to…" })).toBeVisible();
});

test("palette: typing 'zzz' shows no-matches state", async ({ page }) => {
  await page.goto("/");

  // Click body to ensure the page has keyboard focus before pressing Ctrl+K.
  await page.locator("body").click();
  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Jump to…" });
  await expect(dialog).toBeVisible();

  await dialog.getByRole("searchbox").fill("zzz");
  await expect(dialog.getByText(/no matches/i)).toBeVisible();
});
