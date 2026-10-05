// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { HTTPS_INGRESS, aiHost } from "../../fixtures/env";

test.use({ storageState: AUTH_STORAGE_PATH });

test("07-access-mode-api-key: api_key required; correct key 200; missing 401", async ({ page, request }) => {
  // Plan-fidelity: /tunnels Configure dialog is the originally-planned UI
  // path. After defect D1 was fixed (server.TunnelView surfaces service_id,
  // Tunnels.tsx passes it instead of tunnel.id), this works for HTTP tunnels.
  await page.goto("/tunnels");
  const row = page.locator('table[aria-label="Tunnels"] tr').filter({ hasText: "ai" });
  await row.getByRole("button", { name: "Configure" }).click();

  const dialog = page.getByRole("dialog", { name: /Access/ });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("radio", { name: /API key/ }).click();
  await dialog.getByRole("button", { name: "Save changes" }).click();
  await expect(dialog.getByRole("button", { name: "Save changes" })).toBeEnabled();

  // Mint a key. With D2's useId() fix, nested dialogs resolve by name cleanly.
  await dialog.getByRole("button", { name: "New key" }).click();
  const createDialog = page.getByRole("dialog", { name: "Create API key" });
  await expect(createDialog).toBeVisible();
  await createDialog.locator("#api-key-name").fill(`spec07-${Date.now()}`);
  await createDialog.getByRole("button", { name: "Create", exact: true }).click();

  const revealDialog = page.getByRole("dialog", { name: "Copy your API key now" });
  await expect(revealDialog).toBeVisible();
  const key = (await revealDialog.locator(".reveal-once .key-row .v").innerText()).trim();
  expect(key.length).toBeGreaterThanOrEqual(16);
  await revealDialog.getByRole("button", { name: "Done" }).click();

  const host = aiHost();
  // With the correct API key (api_key_header defaults to "Authorization"; the
  // proxy strips a "Bearer " prefix for that header — see internal/proxy/access.go).
  const ok = await request.get(`${HTTPS_INGRESS}/healthz`, {
    headers: { host, authorization: `Bearer ${key}` },
    ignoreHTTPSErrors: true,
  });
  expect(ok.status()).toBe(200);

  // Without the API key.
  const denied = await request.get(`${HTTPS_INGRESS}/healthz`, {
    headers: { host },
    ignoreHTTPSErrors: true,
  });
  expect(denied.status()).toBe(401);
});
