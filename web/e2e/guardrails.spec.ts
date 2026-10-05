import { test, expect } from "@playwright/test";

// v0.4.0: /gateway/guardrails (was /guardrails) — regex redaction, Presidio sidecar, prompt-injection
// (spec Part B.2/B.3). The dashboard page uses three Accordion sections.
// The "Save guardrails" UI flow is currently broken end-to-end because the
// GET response wraps settings in {global, per_service} but the page's
// TypeScript types expect a flat {enabled, action} — known UI/server gap.

test.use({ storageState: "playwright-auth.json" });

test("v0.4.0: guardrails page mounts with three accordion sections", async ({ page }) => {
  await page.goto("/gateway/guardrails");
  await expect(page.getByRole("heading", { name: "Guardrails & redaction" })).toBeVisible();

  await expect(page.getByRole("button", { name: "Regex redaction" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Presidio (Microsoft sidecar)" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Prompt-injection guardrails" })).toBeVisible();

  // Open Presidio section to surface its "Test connection" button.
  await page.getByRole("button", { name: "Presidio (Microsoft sidecar)" }).click();
  await expect(page.getByRole("button", { name: "Test connection" })).toBeVisible();
});

test("v0.4.0: guardrails API — PUT /guardrails/settings round-trips (flat body)", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  // Server PUT expects {enabled, action} — not the {global, per_service}
  // wrapper the GET returns.
  const put = await page.request.put("/api/v1/guardrails/settings", {
    headers,
    data: { enabled: true, action: "refuse_403" },
  });
  expect(put.status()).toBe(204);

  const after = await page.request.get("/api/v1/guardrails/settings");
  const body = (await after.json()) as Record<string, unknown>;
  const global = body.global as Record<string, unknown>;
  expect(global.enabled).toBe(true);
  expect(global.action).toBe("refuse_403");
});
