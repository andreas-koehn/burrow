// test-only — never deploy this shape.
//
// Plan adaptation: connection logs are the Traffic page (/traffic, was
// /connection-logs) — there isn't a per-service tab in ServiceDetail. We drive
// TCP traffic against the tcp-echo tunnel, then assert the global Traffic
// table has at least one row.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { pingTcpTunnel } from "../../fixtures/traffic";

test.use({ storageState: AUTH_STORAGE_PATH });

test("14-connection-logs: TCP sessions appear in /traffic", async ({ page, request }) => {
  await pingTcpTunnel(request, 5);

  await page.goto("/traffic");
  await expect(page.getByRole("heading", { name: "Traffic", exact: true })).toBeVisible();

  const table = page.locator('table[aria-label="Traffic"]').first();
  // At least one row should be present after the 5 fresh-session pings.
  await expect(table.locator("tbody tr").first()).toBeVisible({ timeout: 10_000 });
});
