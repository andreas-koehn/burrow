import { test, expect } from "@playwright/test";

// Phase 5: AI gateway actionable — guided create-service flow.
//
// Flow: /ai/endpoints → click "+ Create AI service" → Services page dialog
// opens with Access mode pre-set to "API key" → fill service_id → Create
// → server returns {id, created_at} → navigate to /services/<id>#upstream-key
// → Upstream-key tab is active.
//
// This spec runs against the REAL built burrowd (no MSW). The POST /services
// route is wired in the real binary (v0.5.2). The /ai/endpoints route returns
// a non-200 in the stock binary (feature-gated), so the page renders in its
// feature-absent or error state — the "+ Create AI service" button is on the
// PageHeader (visible in both states for admin, except featureAbsent).
//
// Because the real /ai/endpoints returns 404 (featureAbsent branch), the
// empty-state CTA is NOT rendered; the PageHeader CTA is rendered only in the
// non-featureAbsent path. Therefore this spec navigates directly to
// /services?new=ai to exercise the auto-open flow independently.

test.use({ storageState: "playwright-auth.json" });

test("P5: ?new=ai auto-opens Services dialog pre-filled with API-key access mode", async ({ page }) => {
  await page.goto("/services?new=ai");

  // The create dialog should auto-open
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("heading", { name: /create service/i })).toBeVisible();

  // The Access mode Select trigger should show "API key"
  const accessModeBtn = dialog.getByRole("button", { name: /access mode/i });
  await expect(accessModeBtn).toContainText(/api key/i);

  // URL param is cleared (dialog is open but the ?new=ai is gone)
  await expect(page).not.toHaveURL(/new=ai/);
});

test("P5: create AI service via dialog routes to upstream-key tab", async ({ page }) => {
  // Pre-create step: ensure a unique service id.
  const uniqueId = `svc-e2e-ai-${Date.now()}`;

  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  await page.goto("/services?new=ai");

  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();

  // Fill in a unique service ID
  await dialog.getByLabel(/service id/i).fill(uniqueId);

  // The Access mode should already be set to API key (auto-filled by ?new=ai)
  const accessModeBtn = dialog.getByRole("button", { name: /access mode/i });
  await expect(accessModeBtn).toContainText(/api key/i);

  // Click Create — POST /services with {service_id, access_mode:"api_key"}
  await dialog.getByRole("button", { name: /^create$/i }).click();

  // After success the dialog closes and we navigate to /services/<id>#upstream-key
  await expect(dialog).not.toBeVisible({ timeout: 10_000 });

  // URL should be /services/<id> (with or without the hash in the URL bar)
  await expect(page).toHaveURL(new RegExp(`/services/${uniqueId}`), { timeout: 10_000 });

  // The Upstream-key tab should be active
  const upstreamTab = page.getByRole("tab", { name: /upstream key/i });
  await expect(upstreamTab).toBeVisible();
  await expect(upstreamTab).toHaveAttribute("aria-selected", "true");

  // Clean up: the created service — best-effort, not critical
  void page.request.delete(`/api/v1/services/${uniqueId}`, { headers }).catch(() => {});
});

test("P5: ServiceDetail #upstream-key hash makes Upstream-key tab active", async ({ page }) => {
  // Pre-provision a service first so ServiceDetail has something to load.
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  const svcId = `svc-e2e-hash-${Date.now()}`;
  const created = await page.request.post("/api/v1/services", {
    headers,
    data: { service_id: svcId, access_mode: "api_key" },
  });
  expect([201, 409]).toContain(created.status());

  // Navigate directly with the hash
  await page.goto(`/services/${svcId}#upstream-key`);

  // ServiceDetail should load and Upstream-key tab should be active
  const upstreamTab = page.getByRole("tab", { name: /upstream key/i });
  await expect(upstreamTab).toBeVisible({ timeout: 10_000 });
  await expect(upstreamTab).toHaveAttribute("aria-selected", "true");
});
