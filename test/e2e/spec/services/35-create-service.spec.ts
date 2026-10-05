// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

interface Service {
  id: string;
  name: string;
}

test.use({ storageState: AUTH_STORAGE_PATH });

test("35-create-service: pre-provision a service through the UI dialog", async ({ page, request }) => {
  // The dialog pre-provisions a service by id. service_id must match
  // ^[a-z0-9_-]{3,64}$ — keep it lowercase letters/digits/hyphen. The backend
  // stores the optional Title as the service's display name.
  const serviceId = `uitest-${Date.now()}`;
  // Unique title too — the backend enforces a unique service name, so a
  // hardcoded title would collide with a leftover on a non-fresh stack.
  const title = `UI test ${serviceId}`;

  try {
    await page.goto("/services");

    // PageHeader CTA — label is "+ New service".
    await page.getByRole("button", { name: /new service/i }).click();

    const dialog = page.getByRole("dialog", { name: "New service" });
    await expect(dialog).toBeVisible();
    await dialog.locator("#ns-service-id").fill(serviceId);
    await dialog.locator("#ns-title").fill(title);
    await dialog.getByRole("button", { name: "Create", exact: true }).click();

    // Success toast: "Service {serviceId} created."
    await expect(page.getByText(new RegExp(`${serviceId} created`))).toBeVisible({ timeout: 5_000 });

    // The new row appears in the Services table (displayed name = the title).
    await expect(
      page.locator('table[aria-label="Services"]').locator("tr").filter({ hasText: title }),
    ).toBeVisible({ timeout: 5_000 });
  } finally {
    // Best-effort cleanup whether or not the create succeeded.
    const res = await request.get("/api/v1/services");
    if (res.ok()) {
      const services = (await res.json()) as Service[];
      // Backend stores Title as the service "name"; match by id or name.
      const mine = services.find((s) => s.id === serviceId || s.name === title);
      if (mine) {
        await request.delete(`/api/v1/services/${mine.id}`, { headers: adminHeaders() });
      }
    }
  }
});
