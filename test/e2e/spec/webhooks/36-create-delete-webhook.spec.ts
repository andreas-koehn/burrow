// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

interface Webhook {
  id: string;
  name: string;
}

test.use({ storageState: AUTH_STORAGE_PATH });

test("36-create-delete-webhook: create via UI (reveal secret) then delete via UI", async ({ page, request }) => {
  const name = `uihook-${Date.now()}`;

  try {
    await page.goto("/webhooks");

    // Open the "New webhook" dialog from the PageHeader CTA.
    await page.getByRole("button", { name: /add webhook|create webhook|new webhook/i }).click();
    const addDialog = page.getByRole("dialog", { name: "New webhook" });
    await expect(addDialog).toBeVisible();

    await addDialog.locator("#wh-name").fill(name);
    await addDialog.locator("#wh-url").fill("https://example.com/hook");
    // The dialog now defaults to a valid event (the first of WEBHOOK_EVENTS,
    // synced to the backend ClosedEvents vocabulary). Add one more valid event.
    // The DS Checkbox renders a <button role="checkbox" id="ev-…"> (NOT an
    // <input>); its accessible name is the event string. Target by role.
    await addDialog.getByRole("checkbox", { name: "service.created", exact: true }).check();
    await addDialog.getByRole("button", { name: "Create", exact: true }).click();

    // Signing-secret reveal dialog (title "Signing secret").
    const secretDialog = page.getByRole("dialog", { name: "Signing secret" });
    await expect(secretDialog).toBeVisible({ timeout: 5_000 });
    await expect(secretDialog.getByText(/won't see it again/i)).toBeVisible();
    const secret = (await secretDialog.locator("code").innerText()).trim();
    expect(secret.length).toBeGreaterThanOrEqual(16);
    await secretDialog.getByRole("button", { name: /I've saved it/ }).click();

    // The webhook row now appears in the Webhooks table.
    const row = page.locator('table[aria-label="Webhooks"] tr').filter({ hasText: name });
    await expect(row).toBeVisible({ timeout: 5_000 });

    // Delete via the row's actions menu. The DropdownMenu trigger is the "⋯"
    // icon button labelled `Actions for {name}`; the "Delete" menu item fires
    // the delete mutation directly (no confirm dialog in this UI).
    await row.getByRole("button", { name: `Actions for ${name}` }).click();
    await page.getByRole("menuitem", { name: "Delete" }).click();

    // Row disappears.
    await expect(row).toHaveCount(0, { timeout: 5_000 });
  } finally {
    // Best-effort cleanup if the UI delete didn't run.
    const res = await request.get("/api/v1/webhooks");
    if (res.ok()) {
      const hooks = (await res.json()) as Webhook[];
      const mine = hooks.find((w) => w.name === name);
      if (mine) {
        await request.delete(`/api/v1/webhooks/${mine.id}`, { headers: adminHeaders() });
      }
    }
  }
});
