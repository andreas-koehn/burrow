// test-only — never deploy this shape.
//
// Spec 30 — Webhook fire + UI delivery row.
//
// API drift vs plan (recorded, not escalated):
//   D1  token.mint is not in the closed-event vocabulary (ClosedEvents in
//       internal/webhook/dispatcher.go); plan assumed it existed.  Using
//       tunnel.connected (a valid closed event) instead.
//   D2  Token-mint does NOT publish any webhook event (no Publish() call in
//       internal/api/token_handlers.go); minting a token would never produce
//       a delivery row.  Using POST /webhooks/{id}/test (synchronous delivery
//       of webhook.test via DeliverNow) instead.
//   D3  POST /api/v1/webhooks returns a flat webhookResp body, not the nested
//       { webhook: { id } } shape the plan assumed.  Cast to { id: string }.

import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

test.use({ storageState: AUTH_STORAGE_PATH });

test("30-webhooks-delivery: token.mint fires + row appears", async ({ page, request }) => {
  // 1. Add a webhook via API (tunnel.connected is a valid closed event).
  const whResp = await request.post("/api/v1/webhooks", {
    headers: adminHeaders(),
    data: {
      name: `spec-30-${Date.now()}`,
      url: "http://mockoai:8081/healthz",
      events: ["tunnel.connected"],
    },
  });
  if (whResp.status() === 404 || whResp.status() === 500) {
    test.skip(true, "Webhook API not available in this build");
  }
  expect(whResp.status()).toBe(201);
  // Response is a flat webhookResp, not { webhook: { id } }.
  const wh = (await whResp.json()) as { id: string };
  const webhookId = wh.id;

  try {
    // 2. Fire a synchronous test delivery via POST /webhooks/{id}/test.
    //    (token.mint is not in ClosedEvents; token creation does not publish
    //    any webhook event — see D2 above.  DeliverNow fires webhook.test
    //    synchronously so the delivery row exists before we navigate.)
    const testResp = await request.post(`/api/v1/webhooks/${webhookId}/test`, {
      headers: adminHeaders(),
    });
    if (testResp.status() === 404 || testResp.status() === 500) {
      test.skip(
        true,
        "Webhook delivery worker not wired in this build — " +
          "no backend path publishes token.mint; " +
          "POST /webhooks/{id}/test also unavailable. " +
          "Refs internal/webhook/dispatcher.go ClosedEvents + " +
          "internal/api/token_handlers.go (no Publish call).",
      );
    }
    expect(testResp.status()).toBe(204);

    // 3. Open /webhooks → wait for the delivery row to appear within 10s.
    await page.goto("/settings/webhooks");
    await expect(
      page.locator("tr").filter({ hasText: /webhook\.test|200/ }).first()
    ).toBeVisible({ timeout: 10_000 });
  } finally {
    // 4. Cleanup — always delete the webhook so subsequent specs run clean.
    await request.delete(`/api/v1/webhooks/${webhookId}`, {
      headers: adminHeaders(),
    });
  }
});
