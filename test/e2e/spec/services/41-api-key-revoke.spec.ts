// test-only — never deploy this shape.
//
// Spec 41 — Service API-key create + revoke flow.
//
// Real-DOM notes (verified against web/src/pages/ServiceDetail.tsx +
// web/src/components/ApiKeysPanel.tsx and the live stack):
//   - Service detail uses the DS Tabs component (role="tab"); the keys tab is
//     labelled "API keys" → content is <ApiKeysPanel>.
//   - API keys belong to http services. The harness's tcp tunnels (tcp-echo,
//     svc-a, svc-b) are tcp services since v0.13, whose page has no tabs, so
//     the spec takes the first http service. Keys can be minted regardless of
//     its access mode — we do NOT change the mode.
//   - Create: a primary "New key" button opens a Dialog "Create API key"
//     with #api-key-name + a "Create" button.
//   - On success a Dialog "Copy your API key now" reveals the key; footer "Done".
//   - Keys table is <table className="data" aria-label="API keys">. Each row's
//     revoke button carries aria-label `Revoke key {name}`.
//   - Revoke opens a confirm Dialog titled "Revoke API key" whose footer has a
//     "Revoke" button (Cancel + Revoke). Confirming DELETEs the key; row drops.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { ensureReauth } from "../../fixtures/reauth";
import { adminHeaders } from "../../fixtures/api";

interface ServiceRow { id: string; name: string; type: string }
interface ApiKeyRow { id: string; name: string }

test.use({ storageState: AUTH_STORAGE_PATH });
// No services domain: guarded writes need a recent password confirmation (T02b).
test.beforeEach(async ({ request }) => { await ensureReauth(request); });

test("41-api-key-revoke: create + revoke a service API key", async ({ page, request }) => {
  // Resolve an http service id: a tcp service has no API keys.
  const svcResp = await request.get("/api/v1/services");
  expect(svcResp.ok()).toBeTruthy();
  const services = (await svcResp.json()) as ServiceRow[];
  const svc = services.find((s) => s.type === "http");
  expect(svc, "an http service in the harness").toBeTruthy();
  if (!svc) return;
  const id = svc.id;

  const keyName = `uikey${Date.now()}`;

  try {
    await page.goto(`/services/${id}`);

    // Open the "API keys" tab.
    await page.getByRole("tab", { name: /api keys/i }).click();

    // --- Create key ---
    await page.getByRole("button", { name: "New key", exact: true }).click();
    const createDialog = page.getByRole("dialog", { name: "Create API key" });
    await expect(createDialog).toBeVisible();
    await createDialog.locator("#api-key-name").fill(keyName);
    await createDialog.getByRole("button", { name: "Create", exact: true }).click();

    // "Copy your API key now" dialog → Done.
    const revealed = page.getByRole("dialog", { name: "Copy your API key now" });
    await expect(revealed).toBeVisible();
    await revealed.getByRole("button", { name: "Done", exact: true }).click();
    await expect(revealed).not.toBeVisible();

    // The key row appears in the table.
    const table = page.locator('table[aria-label="API keys"]');
    const row = table.locator("tbody tr").filter({ hasText: keyName });
    await expect(row).toBeVisible();

    // --- Revoke ---
    await row.getByRole("button", { name: `Revoke key ${keyName}` }).click();
    const confirm = page.getByRole("dialog", { name: "Revoke API key" });
    await expect(confirm).toBeVisible();
    await confirm.getByRole("button", { name: "Revoke", exact: true }).click();

    // The key row disappears.
    await expect(row).not.toBeVisible({ timeout: 10_000 });
  } finally {
    // Best-effort: delete any leftover key with our name. Do NOT touch the
    // service's access mode.
    try {
      const listResp = await request.get(`/api/v1/services/${id}/api-keys`);
      if (listResp.ok()) {
        const keys = (await listResp.json()) as ApiKeyRow[];
        for (const k of keys.filter((k) => k.name === keyName)) {
          await request
            .delete(`/api/v1/services/${id}/api-keys/${k.id}`, { headers: adminHeaders() })
            .catch(() => undefined);
        }
      }
    } catch {
      // best-effort only
    }
  }
});
