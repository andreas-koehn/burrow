// test-only — never deploy this shape.
//
// Spec 24 — Request inspector page + replay.
// Drives one chat-completion request, sees a new row in the inspector,
// expands it for headers/body, triggers Replay → expects a second row.

import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";
import { HTTPS_INGRESS, aiHost } from "../../fixtures/env";

test.use({ storageState: AUTH_STORAGE_PATH });

test("24-inspector: row appears + replay creates a second row", async ({ page, request }) => {
  // Find the ai service.
  const list = await request.get("/api/v1/services");
  const services = (await list.json()) as { id: string; name: string }[];
  const ai = services.find((s) => s.name === "ai");
  if (!ai) throw new Error("ai service not found");

  // Ensure inspector.enabled=true so the chain captures requests. The
  // PUT /ai-config endpoint tolerates missing sections — only validates
  // cache.semantic.{min_similarity,embedding_mode} if present.
  const aiCfgResp = await request.put(`/api/v1/services/${ai.id}/ai-config`, {
    headers: adminHeaders(),
    data: JSON.stringify({ inspector: { enabled: true, max_requests: 50 } }),
  });
  expect(aiCfgResp.ok()).toBeTruthy();

  // Ensure access_mode=open so the unauthenticated proxy POST returns 200
  // and gets captured by the inspector (no auth rejection before capture).
  const amResp = await request.put(`/api/v1/services/${ai.id}/access-mode`, {
    headers: adminHeaders(),
    data: JSON.stringify({ access_mode: "open" }),
  });
  expect(amResp.ok()).toBeTruthy();

  // Fire the request BEFORE navigating to the inspector page. The inspector
  // list query runs on page mount and will pick up captured rows from the DB
  // directly, avoiding an SSE timing race under full-suite load where the
  // EventSource connection may not be established before the request fires.
  const host = aiHost();
  const msgBody = JSON.stringify({ model: "x", stream: false, messages: [{ role: "user", content: "hi from spec-24" }] });
  const r = await request.post(`${HTTPS_INGRESS}/v1/chat/completions`, {
    headers: { host, "content-type": "application/json" },
    data: msgBody,
    ignoreHTTPSErrors: true,
  });
  expect(r.status()).toBe(200);

  // Now navigate to the inspector page. The initial list query fetches
  // /services/{id}/inspector/requests from DB — the captured row will be
  // present without relying on SSE delivery timing.
  await page.goto(`/gateway/requests/${ai.id}`);
  await expect(page.getByRole("heading", { name: /Request inspector/i })).toBeVisible();

  // Wait for the captured row to appear in the Requests table.
  // Scope to the Requests table (not the Headers table inside the detail pane)
  // and use the "clickable" class to exclude the empty-state row.
  const requestRows = page.locator('table[aria-label="Requests"] tbody tr.clickable');
  await expect(requestRows.first()).toBeVisible({ timeout: 10_000 });

  // Click the first (newest) row. The row's onClick calls nav(/gateway/requests/{svcId}/{id})
  // which changes the URL — this proves we clicked a real data row and
  // `selected` state was set.
  await requestRows.first().click();
  await page.waitForURL(`/gateway/requests/${ai.id}/**`, { timeout: 10_000 });

  // Hard-wait for the detail-toolbar to be visible. The detail pane renders
  // only after the detail API fetch completes (detail.data becomes non-null).
  // Under full-suite load this can take several seconds.
  const detailToolbar = page.locator(".detail-toolbar");
  await expect(detailToolbar).toBeVisible({ timeout: 15_000 });

  // Capture the request-list row count before replay so we can assert it grew.
  // Scope to the Requests table aria-label to exclude the Headers table rows.
  const rowsBefore = await requestRows.count();

  // Trigger Replay. The "Replay" button in the detail-toolbar carries
  // aria-label="Open replay dialog" (the button text "Replay" is shared with
  // "Replay & compare", so match by aria-label to avoid strict-mode violation).
  const replayOpenBtn = detailToolbar.getByRole("button", { name: "Open replay dialog" });
  await expect(replayOpenBtn).toBeVisible({ timeout: 10_000 });
  await replayOpenBtn.click();

  // Confirm inside the "Replay request" dialog.
  const replayDialog = page.getByRole("dialog", { name: /Replay request/i });
  await expect(replayDialog).toBeVisible({ timeout: 5_000 });
  await replayDialog.getByRole("button", { name: /^Replay$/i }).click();

  // After replay, expect at least one more row than before. Use a poll loop
  // because SSE delivery is async and the exact final count may exceed
  // rowsBefore+1 when prior entries are flushed concurrently.
  await expect(async () => {
    const after = await requestRows.count();
    expect(after).toBeGreaterThanOrEqual(rowsBefore + 1);
  }).toPass({ timeout: 10_000 });
});
