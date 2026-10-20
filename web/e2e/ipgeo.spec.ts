import { test, expect } from "@playwright/test";

// v0.4.0: per-service IP/geo restrictions (spec Part J). The IPGeoPanel
// component renders only inside AccessModePanel for a LIVE service — the
// e2e fixture has no connected burrow client, so the panel itself is not
// reachable through the dashboard. The component is covered by
// IPGeoPanel.test.tsx (unit). Here we assert the server-side contract.

test.describe("v0.4.0: ip-geo — authenticated surfaces", () => {
  test.use({ storageState: "playwright-auth.json" });

  test("/geo/status returns boolean enabled flag", async ({ page }) => {
    const status = await page.request.get("/api/v1/geo/status");
    expect(status.status()).toBe(200);
    const body = (await status.json()) as Record<string, unknown>;
    expect(typeof body.enabled).toBe("boolean");
  });

  test("per-service GET/PUT returns 404 for unknown service", async ({ page }) => {
    const get = await page.request.get("/api/v1/services/no-such/ip-geo");
    expect(get.status()).toBe(404);

    const cookies = await page.context().cookies();
    const csrf = cookies.find((c) => c.name === "burrow_csrf" || c.name === "__Host-burrow_csrf")?.value ?? "";
    const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };
    const put = await page.request.put("/api/v1/services/no-such/ip-geo", {
      headers,
      data: {
        enabled: true,
        allow_cidrs: ["10.0.0.0/8"],
        block_cidrs: [],
        allow_countries: [],
        block_countries: [],
      },
    });
    expect(put.status()).toBe(404);
  });
});

test("v0.4.0: ip-geo API — unauthenticated 401 on every route", async ({ page }) => {
  for (const p of ["/api/v1/geo/status", "/api/v1/services/any/ip-geo"]) {
    const resp = await page.request.get(p);
    expect(resp.status(), `GET ${p}`).toBe(401);
  }
});
