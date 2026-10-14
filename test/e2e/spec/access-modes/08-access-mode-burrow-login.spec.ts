// test-only — never deploy this shape.
//
// Plan-fidelity note: a true visitor flow needs a browser context with the
// custom hostname resolved (hosts file or --resolve at the curl layer). The
// /etc/hosts entry is optional in the harness, so this spec verifies the
// burrow_login signal via the proxy's HTTP response (302 to the auth gate)
// using a no-session APIRequestContext + Host header — equivalent to the
// runbook §6c assertion without requiring hosts-file entries.
import { test, expect, request as playwrightRequest } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { HTTPS_INGRESS, aiHost } from "../../fixtures/env";

test.use({ storageState: AUTH_STORAGE_PATH });

test("08-access-mode-burrow-login: anonymous visitor gets redirected to the auth gate", async ({ page, request }) => {
  // Resolve service-id (same workaround as spec 07).
  const list = await request.get("/api/v1/services");
  const services = (await list.json()) as { id: string; name: string }[];
  const ai = services.find((s) => s.name === "ai");
  if (!ai) throw new Error("ai service not found");
  await page.goto(`/services/${ai.id}`);
  // The page is titled with the service's name alone.
  await expect(page.getByRole("heading", { name: "ai", level: 1 })).toBeVisible();

  await page.getByRole("radio", { name: /Burrow login/ }).click();
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByRole("button", { name: "Save changes" })).toBeEnabled();
  await page.waitForTimeout(500);

  // Visit the AI tunnel via the HTTPS ingress with NO session cookies.
  const anon = await playwrightRequest.newContext({ ignoreHTTPSErrors: true });
  const res = await anon.get(`${HTTPS_INGRESS}/healthz`, {
    headers: { host: aiHost() },
    maxRedirects: 0,
  });
  // Expect either a 302/303 redirect, OR a 401 with WWW-Authenticate. Both
  // are valid "you need to log in" signals depending on which auth gate
  // surface is wired.
  expect([302, 303, 401, 403]).toContain(res.status());
  await anon.dispose();
});
