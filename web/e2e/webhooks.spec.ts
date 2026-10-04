import { test, expect } from "@playwright/test";

// v0.4.0: /webhooks — outbound HMAC webhook delivery (spec Part H.1).
//
// The v0.4.0 Webhooks.tsx dialog hardcodes events=["audit.tokens.create"]
// but the backend's closed event vocabulary (webhook.ClosedEvents in
// internal/webhook/dispatcher.go) does NOT include that string — so the
// dashboard-driven create always 400s. We document that as a known UI bug
// and instead exercise:
//   1. Page mounts + nav + empty state copy.
//   2. Client-side https:// guard rejects http URLs before any POST.
//   3. The backend contract by direct API call: a valid event (`webhook.test`)
//      produces a 201 with the plaintext signing_secret returned exactly once;
//      GET /webhooks redacts it.

test.use({ storageState: "playwright-auth.json" });

test("v0.4.0: webhooks page mounts + rejects non-https URL client-side", async ({ page }) => {
  await page.goto("/webhooks");
  await expect(page).toHaveURL(/\/webhooks$/);
  await expect(page.getByRole("heading", { name: "Webhooks", exact: true })).toBeVisible();

  // Open the add dialog.
  await page.getByRole("button", { name: "New webhook" }).click();
  const dlg = page.getByRole("dialog", { name: "New webhook" });
  await expect(dlg).toBeVisible();
  await dlg.getByLabel("Name").fill("e2e-bad");
  await dlg.getByLabel("URL").fill("http://example.com/hook");
  await dlg.getByRole("button", { name: "Create" }).click();

  // Client-side guard: inline alert "URL must use https://" and the dialog
  // stays open. No POST is issued.
  await expect(dlg.getByRole("alert")).toContainText(/https:/);
  await expect(dlg).toBeVisible();
  await expect(page.getByRole("dialog", { name: "Signing secret" })).not.toBeVisible();
});

test("v0.4.0: webhooks API — create returns signing_secret once + GET redacts it", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  expect(csrf).not.toBe("");
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  // Pick a name unique to this test to avoid cross-spec collisions on the
  // shared (reused) webserver — see playwright.config.ts reuseExistingServer.
  const name = `e2e-hook-${Date.now()}`;
  const created = await page.request.post("/api/v1/webhooks", {
    headers,
    data: { name, url: "https://example.com/hook", events: ["webhook.test"] },
  });
  expect(created.status()).toBe(201);
  const body = (await created.json()) as Record<string, unknown>;
  expect(body.id).toBeTruthy();
  expect(body.name).toBe(name);
  expect(body.signing_secret).toBeTruthy();
  expect(typeof body.signing_secret).toBe("string");
  const id = body.id as string;

  // GET /webhooks lists the new row WITHOUT the signing secret.
  const list = await page.request.get("/api/v1/webhooks");
  expect(list.status()).toBe(200);
  const rows = (await list.json()) as Array<Record<string, unknown>>;
  const row = rows.find((r) => r.id === id);
  expect(row).toBeTruthy();
  expect(row!.signing_secret).toBeUndefined();
  expect(row!.name).toBe(name);
});

test("v0.4.0: webhooks API — closed event vocabulary is enforced", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  const bad = await page.request.post("/api/v1/webhooks", {
    headers,
    data: {
      name: "e2e-bad-event",
      url: "https://example.com/hook",
      events: ["definitely.not.real"],
    },
  });
  expect(bad.status()).toBe(400);
  expect((await bad.json()).error).toContain("definitely.not.real");
});
