import { test, expect } from "@playwright/test";

// v0.4.0: /settings/backups — backup & restore (spec Part L.3).

test.use({ storageState: "playwright-auth.json" });

test("v0.4.0: take a backup → row appears with sha256", async ({ page }) => {
  await page.goto("/settings/backups");
  await expect(page).toHaveURL(/\/settings\/backups$/);
  await expect(page.getByRole("heading", { name: "Backup & restore" })).toBeVisible();

  await page.getByRole("button", { name: "Create backup" }).click();

  // A row appears with a Copy-sha icon button whose aria-label encodes the
  // backup id ("Copy sha for <id>"); the table also renders Verify buttons.
  const copyShaBtn = page.getByRole("button", { name: /Copy sha for/ }).first();
  await expect(copyShaBtn).toBeVisible({ timeout: 15_000 });

  const table = page.getByRole("table", { name: "Backup history" });
  await expect(table.getByRole("button", { name: "Verify" }).first()).toBeVisible();
});

test("v0.4.0: backups API — POST /backups returns 202 with id/started_at", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf" || c.name === "__Host-burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  const queued = await page.request.post("/api/v1/backups", { headers, data: {} });
  expect(queued.status()).toBe(202);
  const body = (await queued.json()) as Record<string, unknown>;
  expect(typeof body.id).toBe("string");
  expect(typeof body.started_at).toBe("string");
});
