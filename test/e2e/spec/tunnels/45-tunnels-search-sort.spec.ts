// test-only — never deploy this shape.
//
// Covers the list controls of Services on Live (/services?live=1, formerly
// /tunnels): the search/filter box and the sortable column headers. The stack
// seeds four tunnels (ai, tcp-echo, svc-a, svc-b); see web/src/pages/Services.tsx
// for the search input (aria-label="Filter services") and the sort buttons
// (aria-label="Sort by name (asc|desc)" etc.). Read-only — no cleanup.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({ storageState: AUTH_STORAGE_PATH });

test("45-tunnels-search-sort: filter box narrows rows; name header toggles asc/desc", async ({ page }) => {
  await page.goto("/services?live=1");

  const table = page.locator('table[aria-label="Services"]');
  await expect(table).toBeVisible();
  // The stack seeds four tunnels.
  await expect.poll(async () => await table.locator("tbody tr").count(), {
    timeout: 10_000,
    message: "expected >=4 seeded tunnel rows",
  }).toBeGreaterThanOrEqual(4);

  // --- SEARCH --------------------------------------------------------------
  // The search box is a DS Input rendered as type="search" with
  // aria-label="Filter services".
  const search = page.getByRole("searchbox", { name: "Filter services" });
  await search.fill("tcp-echo");

  // Only the tcp-echo row should remain; the "ai" tunnel must be filtered out.
  await expect(table.locator("tbody tr")).toHaveCount(1);
  await expect(table.locator("tbody tr").filter({ hasText: "tcp-echo" })).toBeVisible();
  await expect(table.locator("tbody tr").filter({ hasText: /^ai$/ })).toHaveCount(0);

  // Clearing restores all rows.
  await search.fill("");
  await expect.poll(async () => await table.locator("tbody tr").count(), {
    timeout: 5_000,
  }).toBeGreaterThanOrEqual(4);

  // --- SORT ----------------------------------------------------------------
  // Read the first data row's Name cell (.col-name) before and after toggling
  // the name sort. First click sets name-asc; second click flips to desc, so
  // the first visible row's name must change between the two directions.
  const firstName = () =>
    table.locator("tbody tr").first().locator("td.col-name").innerText();

  const nameHeader = page.getByRole("button", { name: /^Sort by name/ });

  // Click once → name ascending.
  await nameHeader.click();
  await expect(nameHeader).toHaveAccessibleName(/Sort by name \(asc\)/);
  const ascFirst = (await firstName()).trim();

  // Click again → name descending.
  await nameHeader.click();
  await expect(nameHeader).toHaveAccessibleName(/Sort by name \(desc\)/);
  await expect.poll(async () => (await firstName()).trim(), {
    timeout: 5_000,
    message: "first row name should change when sort flips asc->desc",
  }).not.toBe(ascFirst);

  const descFirst = (await firstName()).trim();
  // Sanity: ascending first name sorts before descending first name.
  expect(ascFirst.localeCompare(descFirst)).toBeLessThan(0);
});
