// test-only — never deploy this shape.
//
// Custom domains are not a tab of the service page, also on a relay with host
// routing on (the harness relay sets BURROW_HTTP_PROXY_LISTEN=:8443). They are
// the "Host names" card, with the Protocol card, of an http service (H13),
// and the old /domains link must land on the service.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { ensureReauth } from "../../fixtures/reauth";

test.use({ storageState: AUTH_STORAGE_PATH });
// No services domain: guarded writes need a recent password confirmation (T02b).
test.beforeEach(async ({ request }) => { await ensureReauth(request); });

test("13-custom-domains: no Custom domains tab, /domains redirects", async ({ page, request }) => {
  const list = await request.get("/api/v1/services");
  const services = (await list.json()) as { id: string; name: string }[];
  const ai = services.find((s) => s.name === "ai");
  if (!ai) throw new Error("ai service not found");
  await page.goto(`/services/${ai.id}`);
  await expect(page.getByRole("heading", { level: 1, name: "ai", exact: true })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Access" })).toBeVisible();
  await expect(page.getByRole("tab", { name: /Custom domains/i })).toHaveCount(0);
  // H13: the cards of an http service.
  await expect(page.getByRole("region", { name: "Host names", exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "Protocol", exact: true })).toBeVisible();

  await page.goto(`/services/${ai.id}/domains`);
  await expect(page).toHaveURL(new RegExp(`/services/${ai.id}$`));
  await expect(page.getByRole("tab", { name: "Access" })).toBeVisible();
});
