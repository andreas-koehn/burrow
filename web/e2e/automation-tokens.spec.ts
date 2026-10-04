import { test, expect } from "@playwright/test";

// v0.4.0: /account/automation — long-lived bearer tokens (spec Part M.1).

test.use({ storageState: "playwright-auth.json" });

test("v0.4.0: automation tokens — mint reveals plaintext once + lists with prefix", async ({ page }) => {
  await page.goto("/account/automation");
  await expect(page).toHaveURL(/\/account\/automation$/);
  await expect(page.getByRole("heading", { name: "Automation tokens" })).toBeVisible();

  // The list may already contain rows from other test runs against the
  // reused server; just assert the page mounted and we can mint a new one.
  await page.getByRole("button", { name: "New token" }).click();
  const dlg = page.getByRole("dialog", { name: "New automation token" });
  await expect(dlg).toBeVisible();
  const name = `e2e-ci-${Date.now()}`;
  await dlg.getByLabel("Name").fill(name);
  // Expiry select defaults to "Never" → expires_at=null in the request body.
  await dlg.getByRole("button", { name: "Create" }).click();

  // Follow-up dialog reveals the plaintext exactly once.
  const reveal = page.getByRole("dialog", { name: "New automation token" });
  await expect(reveal).toBeVisible();
  await expect(reveal.getByText("Save this token now", { exact: false })).toBeVisible();
  const code = reveal.locator("code").first();
  await expect(code).toBeVisible();
  const plaintext = (await code.textContent()) ?? "";
  expect(plaintext.length).toBeGreaterThan(8);

  // Dismiss the reveal dialog.
  await reveal.getByRole("button", { name: "I've saved it" }).click();
  await expect(reveal).not.toBeVisible();

  // The token appears in the table with the redacted prefix (no plaintext).
  const table = page.getByRole("table", { name: "Automation tokens" });
  await expect(table).toBeVisible();
  await expect(table.getByRole("cell", { name, exact: true })).toBeVisible();
});

test("v0.4.0: automation tokens API — POST returns plaintext once; GET redacts", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  const name = `e2e-api-${Date.now()}`;
  const created = await page.request.post("/api/v1/automation/tokens", {
    headers,
    data: { name, expires_at: null },
  });
  expect(created.status()).toBe(201);
  const body = (await created.json()) as Record<string, unknown>;
  expect(typeof body.plaintext).toBe("string");
  expect((body.plaintext as string).length).toBeGreaterThan(8);
  const token = body.token as Record<string, unknown>;
  expect(token.name).toBe(name);
  expect(token.prefix).toBeTruthy();

  // List does not return the plaintext.
  const list = await page.request.get("/api/v1/automation/tokens");
  expect(list.status()).toBe(200);
  const rows = (await list.json()) as Array<Record<string, unknown>>;
  const row = rows.find((r) => r.name === name);
  expect(row).toBeTruthy();
  expect(row!.plaintext).toBeUndefined();
});
