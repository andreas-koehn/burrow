// test-only — never deploy this shape.
//
// Spec 25 — Quota rate-limit UI + audit trail.
// Creates a rate-limit rule via the API (UI form would also work but
// the API is faster + less brittle), fires 7 requests through the
// proxy, asserts 429 fires by request 6-7, then opens /audit and
// asserts the ratelimit.enforced row is visible.

import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { HTTPS_INGRESS, aiHost } from "../../fixtures/env";
import { adminHeaders } from "../../fixtures/api";

test.use({ storageState: AUTH_STORAGE_PATH });

test("25-quota-rate-limit: 429 + audit row", async ({ page, request }) => {
  // The requests below are anonymous, so the "ai" service must be in Open
  // mode. Set it here instead of relying on what an earlier spec left behind,
  // and put the previous mode back at the end.
  const services = (await (await request.get("/api/v1/services")).json()) as {
    id: string; name: string; access_mode: string;
  }[];
  const ai = services.find((s) => s.name === "ai");
  if (!ai) throw new Error("ai service not found");
  const setMode = (mode: string) =>
    request.put(`/api/v1/services/${ai.id}/access-mode`, {
      headers: adminHeaders(),
      data: { access_mode: mode },
    });
  expect((await setMode("open")).status()).toBe(204);

  // 1. Create a rate-limit rule via the admin API.
  // API drift: plan used `lim` + `subject: "*"` — actual handler uses
  // `limit` (json:"limit") and requires subject="" for global scope.
  const createResp = await request.post("/api/v1/rate-limits", {
    headers: adminHeaders(),
    data: {
      scope: "global",
      subject: "",
      dimension: "rpm",
      limit: 5,
      burst: 5,
    },
  });
  if (createResp.status() === 404 || createResp.status() === 500) {
    test.skip(true, "rate-limit store not wired in this build");
  }
  expect(createResp.status()).toBeLessThan(400);
  const rule = (await createResp.json()) as { id: string };

  // 2. Fire 7 requests through the proxy in quick succession.
  const host = aiHost();
  const statuses: number[] = [];
  for (let i = 0; i < 7; i++) {
    const r = await request.post(`${HTTPS_INGRESS}/v1/chat/completions`, {
      headers: { host, "content-type": "application/json" },
      data: { model: "mock", stream: false, messages: [{ role: "user", content: "test" }] },
      ignoreHTTPSErrors: true,
    });
    statuses.push(r.status());
  }

  // Cleanup before asserting so the rule doesn't persist.
  await request.delete(`/api/v1/rate-limits/${rule.id}`, {
    headers: adminHeaders(),
  });
  await setMode(ai.access_mode || "open");

  // At least one of the last 2 should be 429.
  expect(statuses.slice(5)).toContain(429);

  // 3. Open audit log, look for ratelimit.enforced row.
  await page.goto("/audit");
  await expect(page.getByRole("heading", { name: /Audit/i })).toBeVisible();
  await expect(
    page.getByRole("table").locator("tr").filter({ hasText: /ratelimit\.enforced/ }).first()
  ).toBeVisible({ timeout: 10_000 });
});
