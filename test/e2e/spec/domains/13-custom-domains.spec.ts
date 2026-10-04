// test-only — never deploy this shape.
//
// Custom domains need host routing, which is off. The service page must not
// offer the tab, and the old /domains link must land on the service.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({ storageState: AUTH_STORAGE_PATH });

test("13-custom-domains: no Custom domains tab, /domains redirects", async ({ page, request }) => {
  const list = await request.get("/api/v1/services");
  const services = (await list.json()) as { id: string; name: string }[];
  const ai = services.find((s) => s.name === "ai");
  if (!ai) throw new Error("ai service not found");
  await page.goto(`/services/${ai.id}`);
  await expect(page.getByRole("heading", { name: /Service.*\bai\b/ })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Access" })).toBeVisible();
  await expect(page.getByRole("tab", { name: /Custom domains/i })).toHaveCount(0);

  await page.goto(`/services/${ai.id}/domains`);
  await expect(page).toHaveURL(new RegExp(`/services/${ai.id}$`));
  await expect(page.getByRole("tab", { name: "Access" })).toBeVisible();
});
