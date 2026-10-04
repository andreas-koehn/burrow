// test-only — never deploy this shape.
//
// Plan-fidelity note: drives chat-completions through the HTTPS proxy
// (host-routed) instead of the plan's localhost:9001 (which doesn't bind —
// HTTP tunnels are host-routed on :8443 only).
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { HTTPS_INGRESS, aiHost } from "../../fixtures/env";

test.use({ storageState: AUTH_STORAGE_PATH });

test("10-ai-gateway-basic: chat-completions SSE works via Burrow → mockoai", async ({ page, request }) => {
  // Reset the ai service to Open mode so this spec doesn't depend on prior
  // test state (specs 06-08 leave it in burrow_login).
  const list = await request.get("/api/v1/services");
  const services = (await list.json()) as { id: string; name: string }[];
  const ai = services.find((s) => s.name === "ai");
  if (!ai) throw new Error("ai service not found");
  await page.goto(`/services/${ai.id}`);
  await page.getByRole("radio", { name: /^Open/ }).click();
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByRole("button", { name: "Save changes" })).toBeEnabled();
  await page.waitForTimeout(500);

  const res = await request.post(`${HTTPS_INGRESS}/v1/chat/completions`, {
    headers: { host: aiHost(), "content-type": "application/json" },
    data: { model: "mock", stream: true, messages: [{ role: "user", content: "hi" }] },
    ignoreHTTPSErrors: true,
  });
  expect(res.status()).toBe(200);
  const body = await res.text();
  expect(body).toContain("data: ");
  expect(body).toContain("[DONE]");
});
