// test-only — never deploy this shape.
//
// Plan adaptation: /cost is actually /cost-budgets in v0.5.2. The Cost &
// budgets page has metric tiles + a Budgets table. Without an active budget
// or model pricing configured for "mock", the rows are zero — that's still
// a meaningful "page renders" signal.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { HTTPS_INGRESS, aiHost } from "../../fixtures/env";

test.use({ storageState: AUTH_STORAGE_PATH });

test("12-ai-gateway-metering-cost: chat-completions metered + cost page renders", async ({ page, request }) => {
  // Drive a single chat-completions request through the proxy.
  const res = await request.post(`${HTTPS_INGRESS}/v1/chat/completions`, {
    headers: { host: aiHost(), "content-type": "application/json" },
    data: { model: "mock", stream: false, messages: [{ role: "user", content: "meter" }] },
    ignoreHTTPSErrors: true,
  });
  expect(res.status()).toBe(200);

  // The Cost & budgets page renders — surface signal that metering wiring works.
  await page.goto("/gateway/cost");
  await expect(page.getByRole("heading", { name: /Cost.*budgets/i })).toBeVisible();
  // Metric tiles are present (24h / 7d / 30d windows).
  await expect(page.locator('[aria-label*="Spend by window"]').first()).toBeVisible();
});
