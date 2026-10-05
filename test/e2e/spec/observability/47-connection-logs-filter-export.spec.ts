// test-only — never deploy this shape.
//
// Covers the Traffic page's (/traffic, was /connection-logs) filter controls and
// the Export action. See web/src/pages/Traffic.tsx:
//   - Protocol / Service filters are native <select> (aria-label=…); the time
//     range is a radiogroup.
//   - Rollups is a native checkbox (aria-label="Rollups").
//   - Export is a header button that issues GET /connection-logs/export
//     ?format=ndjson via fetch (apiFetch) — it does NOT trigger a browser
//     `download` event, so we assert via the network response (the brief's
//     documented fallback path). Read-only — no cleanup.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({ storageState: AUTH_STORAGE_PATH });

// The page renders either a logs table, a rollups table (both
// aria-label="Traffic"), or an EmptyState — depending on seeded data.
// "renders without error" == one of those is visible and no error banner shows.
async function assertViewSettled(page: import("@playwright/test").Page) {
  const table = page.locator('table[aria-label="Traffic"]');
  const empty = page.getByText(/No traffic in this period|No rollups in this period/);
  await expect.poll(
    async () => (await table.count()) > 0 || (await empty.count()) > 0,
    { timeout: 10_000, message: "neither logs table nor empty-state rendered" },
  ).toBe(true);
  // A thrown render error would surface React's fallback / a missing heading.
  // exact:true — the EmptyState renders an <h4>"No traffic in this period"</h4>
  // that substring-matches a loose "Traffic".
  await expect(page.getByRole("heading", { name: "Traffic", exact: true })).toBeVisible();
}

test("47-connection-logs-filter-export: rollups toggle + kind filter render; Export hits the API", async ({ page }) => {
  await page.goto("/traffic");
  await expect(page.getByRole("heading", { name: "Traffic", exact: true })).toBeVisible();
  await assertViewSettled(page);

  // --- ROLLUPS TOGGLE ------------------------------------------------------
  // Native checkbox aria-label="Rollups". Toggle to rollup view, assert it
  // still renders, then toggle back to detail view.
  const rollups = page.getByRole("checkbox", { name: "Rollups", exact: true });
  await rollups.check();
  await expect(rollups).toBeChecked();
  await assertViewSettled(page);
  await rollups.uncheck();
  await expect(rollups).not.toBeChecked();
  await assertViewSettled(page);

  // --- PROTOCOL FILTER -----------------------------------------------------
  // Native <select> aria-label="Protocol". Pick "TCP proxy" and assert the view
  // re-settles without error (don't over-assert rows — seeded data varies).
  await page.getByLabel("Protocol", { exact: true }).selectOption("tcp_proxy");
  await assertViewSettled(page);
  // Reset to All.
  await page.getByLabel("Protocol", { exact: true }).selectOption("");
  await assertViewSettled(page);

  // --- EXPORT --------------------------------------------------------------
  // handleExport() issues GET /api/v1/connection-logs/export?format=ndjson via
  // fetch (no browser download event). Assert the network response is 200.
  const [resp] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().includes("/connection-logs/export") && r.request().method() === "GET",
      { timeout: 10_000 },
    ),
    page.getByRole("button", { name: /^export$/i }).click(),
  ]);
  expect(resp.status()).toBe(200);
  expect(resp.url()).toContain("format=ndjson");
});
