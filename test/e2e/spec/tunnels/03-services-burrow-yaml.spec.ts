// test-only — never deploy this shape.
//
// Plan-fidelity note: the plan-as-written expected svc-a + svc-b to appear on
// /services. In v0.5.2, only HTTP-type tunnels become persisted Service rows
// (the `ai` row); TCP-type tunnels live in the in-memory tunnel registry
// only. So this spec asserts the v0.3 burrow.yaml multi-service surface via
// Services on Live (where both svc-a and svc-b appear connected) rather than
// the saved services. Same end-to-end signal: one client process exposes two services.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { pingMultiTunnels } from "../../fixtures/traffic";

test.use({ storageState: AUTH_STORAGE_PATH });

test("03-services-burrow-yaml: client-multi exposes 2 services through one client", async ({ page, request }) => {
  await page.goto("/services?live=1");
  const table = page.locator('table[aria-label="Services"]');
  for (const name of ["svc-a", "svc-b"]) {
    const row = table.locator("tr").filter({ hasText: name });
    await expect(row, `${name} row visible`).toBeVisible();
    await expect(
      row.getByText("connected", { exact: true }),
      `${name} connected`,
    ).toBeVisible();
  }

  await pingMultiTunnels(request);

  // Both tunnels stay connected after traffic.
  for (const name of ["svc-a", "svc-b"]) {
    const row = table.locator("tr").filter({ hasText: name });
    await expect(row.getByText("connected", { exact: true })).toBeVisible();
  }
});
