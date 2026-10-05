import { test, expect } from "@playwright/test";

// v0.4.0: /gateway/cache (was /cache) — exact-match prompt cache settings (spec Part B.1).

test.use({ storageState: "playwright-auth.json" });

test("v0.4.0: prompt cache — toggle enabled + save shows success toast", async ({ page }) => {
  await page.goto("/gateway/cache");
  await expect(page.getByRole("heading", { name: "Prompt cache" })).toBeVisible();

  // Exact-match tab is selected by default; its inputs are present.
  const enabledSwitch = page.getByRole("switch", { name: "Enable exact cache" });
  await expect(enabledSwitch).toBeVisible();
  await expect(page.getByLabel("TTL (seconds)")).toBeVisible();
  await expect(page.getByLabel("Max entries")).toBeVisible();

  await enabledSwitch.click();
  await page.getByRole("button", { name: "Save cache settings" }).click();
  await expect(page.getByText("Cache settings saved", { exact: false })).toBeVisible();
});

test("v0.4.0: prompt cache API — PUT /cache/settings round-trips a flat body", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  // GET wraps the persisted blob in {global, per_service}.
  const before = await page.request.get("/api/v1/cache/settings");
  expect(before.status()).toBe(200);
  const cur = (await before.json()) as { global: Record<string, unknown> };
  expect(typeof cur.global.enabled).toBe("boolean");

  // Server PUT (exact.SettingsFromJSON) expects a FLAT body — the page's
  // {global, per_service} wrapper is a known UI/server contract gap; we
  // exercise the server's documented contract here.
  const flat = { ...(cur.global as Record<string, unknown>), enabled: !cur.global.enabled };
  const put = await page.request.put("/api/v1/cache/settings", { headers, data: flat });
  expect(put.status()).toBe(204);

  const after = await page.request.get("/api/v1/cache/settings");
  const got = (await after.json()) as { global: Record<string, unknown> };
  expect(got.global.enabled).toBe(flat.enabled);
});
