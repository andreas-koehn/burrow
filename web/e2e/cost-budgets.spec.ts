import { test, expect } from "@playwright/test";

// v0.4.0: /cost — cost summary tiles + budgets CRUD (spec Part F). Admin-
// only. The AI GATEWAY nav group hides this when there's no api_key
// service in the fixture; navigate by URL.

test.use({ storageState: "playwright-auth.json" });

test("v0.4.0: cost page renders zeroed tiles + add-budget flow lists the row", async ({ page }) => {
  await page.goto("/cost");
  await expect(page.getByRole("heading", { name: "Cost & budgets" })).toBeVisible();

  // Four spend tiles (today/week/month/year).
  const strip = page.getByRole("list", { name: "Spend by window" });
  await expect(strip).toBeVisible();
  await expect(strip.getByText("Today", { exact: true })).toBeVisible();
  await expect(strip.getByText("Week", { exact: true })).toBeVisible();
  await expect(strip.getByText("Month", { exact: true })).toBeVisible();
  await expect(strip.getByText("Year", { exact: true })).toBeVisible();
  // At least one "$0.00" appears on a fresh DB.
  await expect(strip.getByText("$0.00").first()).toBeVisible();

  // New budget dialog.
  await page.getByRole("button", { name: "New budget" }).click();
  const dlg = page.getByRole("dialog", { name: "New budget" });
  await expect(dlg).toBeVisible();
  const subject = `ak_e2e_${Date.now()}`;
  await dlg.getByLabel("Subject").fill(subject);
  await dlg.getByLabel("Daily USD").fill("5");
  await dlg.getByRole("button", { name: "Create" }).click();
  await expect(dlg).not.toBeVisible();

  // The new row appears in the Budgets table.
  const table = page.getByRole("table", { name: "Budgets" });
  await expect(table).toBeVisible();
  await expect(table.getByText(subject)).toBeVisible();
  await expect(table.getByText("$5.00").first()).toBeVisible();
});

test("v0.4.0: cost API — /budgets POST validates daily_usd > 0", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  const bad = await page.request.post("/api/v1/budgets", {
    headers,
    data: { scope: "api_key", subject_id: "ak_x", daily_usd: 0, action_on_exceed: "alert_webhook" },
  });
  expect(bad.status()).toBe(400);
});
