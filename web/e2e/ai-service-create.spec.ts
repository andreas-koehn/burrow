import { test, expect } from "@playwright/test";

// Phase 5: AI gateway actionable — the guided provider for a local model server.
//
// Flow: /gateway/providers → "New provider" → "A model server on one of my
// machines" → name it → "Continue" creates a service that only the gateway
// reaches and the provider on top of it → the dialog shows the command to run
// on the machine and waits → "Done" opens /gateway/providers/<slug>.
//
// This spec runs against the REAL built burrowd (no MSW). It opens the dialog
// through /gateway/providers?new=local; the button on the Providers page and
// every branch of the steps are covered by RTL.

test.use({ storageState: "playwright-auth.json" });

// Per-test sequence suffix: guarantees unique names across tests in the
// shared embedded-burrowd session even if two POSTs land in the same millisecond.
let svcSeq = 0;

test("P5: /services?new=ai, the old way in, leads to New provider on the guided steps", async ({ page }) => {
  await page.goto("/services?new=ai");

  await expect(page).toHaveURL(/\/gateway\/providers\?new=local$/);
  const dialog = page.getByRole("dialog", { name: "New provider" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("radio", { name: "A model server on one of my machines" })).toBeChecked();
  // Exact: the choice above it is named "A model server on one of my machines".
  await expect(dialog.getByLabel("Model server", { exact: true })).toBeVisible();

  // Nothing about a service's access is asked: the dialog sets it.
  await expect(dialog.getByRole("combobox", { name: /access mode/i })).toHaveCount(0);

  // Closing the dialog takes the parameter out of the address.
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).not.toBeVisible();
  await expect(page).toHaveURL(/\/gateway\/providers$/);
});

test("P5: the guided steps create the provider and show the command that serves it", async ({ page }) => {
  // A unique name: the service is named after it, and services.name carries a
  // UNIQUE(user_id, name) constraint.
  const name = `E2E Model ${Date.now()}-${++svcSeq}`;
  const slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-");

  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf" || c.name === "__Host-burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  await page.goto("/gateway/providers?new=local");

  const dialog = page.getByRole("dialog", { name: "New provider" });
  await expect(dialog).toBeVisible();

  // Step 1: the name. Continue says what is missing until there is one.
  const proceed = dialog.getByRole("button", { name: "Continue" });
  await expect(proceed).toBeDisabled();
  await expect(dialog.getByText("Enter a name.")).toBeVisible();
  await dialog.getByLabel("Name").fill(name);
  await expect(dialog.getByText(`/ai/${slug}/v1`)).toBeVisible();
  await proceed.click();

  // Step 2: the command for the machine, serving the service that was just made.
  await expect(dialog.getByRole("heading", { name: "Run this on the machine" })).toBeVisible({ timeout: 10_000 });
  await expect(dialog.getByRole("group", { name: "Run command" })).toHaveText(`burrow http 11434 --name ${slug}`);
  // No client serves it in this stack: the dialog waits, and says so.
  await expect(dialog.locator("[role=status]").filter({ hasText: "Waiting for the client…" })).toBeVisible();

  // The service exists under that name, reachable through the gateway only.
  const svc = await page.request.get(`/api/v1/services/${slug}`, { headers });
  expect(svc.status()).toBe(200);
  expect(await svc.json()).toMatchObject({ id: slug, name: slug, access_mode: "api_key", gateway_only: true });

  // Done closes the dialog and opens the new provider's page.
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).not.toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/gateway/providers/${slug}$`), { timeout: 10_000 });
  await expect(page.getByRole("heading", { name: `Provider · ${name}`, level: 1 })).toBeVisible({ timeout: 10_000 });

  // Clean up: the provider, then the service. Await both so the deletions
  // complete before the next test runs in the shared session.
  await page.request.delete(`/api/v1/ai/providers/${slug}`, { headers }).catch(() => {});
  await page.request.delete(`/api/v1/services/${slug}`, { headers }).catch(() => {});
});

test("P5: ServiceDetail #upstream-key hash makes Upstream-key tab active", async ({ page }) => {
  // Pre-provision a service first so ServiceDetail has something to load.
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf" || c.name === "__Host-burrow_csrf")?.value ?? "";
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
