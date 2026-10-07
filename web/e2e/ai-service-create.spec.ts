import { test, expect } from "@playwright/test";

// Phase 5: AI gateway actionable — guided create-service flow.
//
// Flow: /gateway/providers → click "New AI service" → Services page "New AI
// service" dialog opens (access mode fixed to API key, no picker) → fill
// service_id → "Create and continue" → the service is created and registered
// as a provider → navigate to /gateway/providers/<slug>.
//
// This spec runs against the REAL built burrowd (no MSW). It navigates
// directly to /services?new=ai to exercise the auto-open flow on its own; the
// "New AI service" button on the Providers page is covered by RTL.

test.use({ storageState: "playwright-auth.json" });

// Per-test sequence suffix: guarantees unique service ids across tests in the
// shared embedded-burrowd session even if two POSTs land in the same millisecond.
let svcSeq = 0;

test("P5: ?new=ai auto-opens the New AI service dialog without an access-mode picker", async ({ page }) => {
  await page.goto("/services?new=ai");

  // The create dialog should auto-open
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("heading", { name: "New AI service" })).toBeVisible();

  // Access mode is fixed to API key for AI services: no picker is offered
  await expect(dialog.getByRole("combobox", { name: /access mode/i })).toHaveCount(0);

  // URL param is cleared (dialog is open but the ?new=ai is gone)
  await expect(page).not.toHaveURL(/new=ai/);
});

test("P5: create AI service via dialog registers a provider and opens its page", async ({ page }) => {
  // Pre-create step: ensure a unique service id.
  const uniqueId = `svc-e2e-ai-${Date.now()}-${++svcSeq}`;

  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  await page.goto("/services?new=ai");

  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();

  // Fill in a unique service ID and a matching Title. The Title maps to the
  // services.name column, which carries a UNIQUE(user_id, name) constraint;
  // the real binary exposes no DELETE /services route, so leaving the name
  // blank would collide with any other blank-named service this suite creates
  // (POST → 409). A unique title keeps each created service independent.
  await dialog.getByLabel(/service id/i).fill(uniqueId);
  await dialog.getByLabel(/^title$/i).fill(uniqueId);

  // Click Create and continue — POST /services with {service_id, access_mode:"api_key"}
  await dialog.getByRole("button", { name: "Create and continue" }).click();

  // After success the dialog closes and we land on the new provider's page.
  // The server derives the provider slug from the title.
  await expect(dialog).not.toBeVisible({ timeout: 10_000 });
  await expect(page).toHaveURL(/\/gateway\/providers\/[a-z0-9-]+$/, { timeout: 10_000 });
  await expect(page.getByRole("heading", { name: `Provider · ${uniqueId}`, level: 1 })).toBeVisible({ timeout: 10_000 });
  await expect(page.getByRole("heading", { name: "Connect a client" })).toBeVisible();

  // Clean up: the provider, then the service. Await both so the deletions
  // complete before the next test runs in the shared session.
  const slug = new URL(page.url()).pathname.split("/").pop();
  await page.request.delete(`/api/v1/ai/providers/${slug}`, { headers }).catch(() => {});
  await page.request.delete(`/api/v1/services/${uniqueId}`, { headers }).catch(() => {});
});

test("P5: ServiceDetail #upstream-key hash makes Upstream-key tab active", async ({ page }) => {
  // Pre-provision a service first so ServiceDetail has something to load.
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  const svcId = `svc-e2e-hash-${Date.now()}-${++svcSeq}`;
  // A unique `title` (→ services.name) is required: the name column has a
  // UNIQUE(user_id, name) constraint and the real binary has no
  // DELETE /services route, so a blank name would collide (409) with any other
  // blank-named service created elsewhere in this shared session.
  const created = await page.request.post("/api/v1/services", {
    headers,
    data: { service_id: svcId, title: svcId, access_mode: "api_key" },
  });
  expect([201, 409]).toContain(created.status());

  // Navigate directly with the hash
  await page.goto(`/services/${svcId}#upstream-key`);

  // ServiceDetail should load and Upstream-key tab should be active
  const upstreamTab = page.getByRole("tab", { name: /upstream key/i });
  await expect(upstreamTab).toBeVisible({ timeout: 10_000 });
  await expect(upstreamTab).toHaveAttribute("aria-selected", "true");
});
