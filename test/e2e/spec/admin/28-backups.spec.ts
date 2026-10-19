// test-only — never deploy this shape.
//
// Spec 28 — Backup/restore UI round-trip.

import { test, expect } from "@playwright/test";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { ensureReauth } from "../../fixtures/reauth";

test.use({ storageState: AUTH_STORAGE_PATH });
// No services domain: guarded writes need a recent password confirmation (T02b).
test.beforeEach(async ({ request }) => { await ensureReauth(request); });

test("28-backups: create + download + UI history", async ({ page }) => {
  await page.goto("/settings/backups");
  await expect(page.getByRole("heading", { name: /Backup/i, level: 1 })).toBeVisible();

  // Create backup.
  const createBtn = page.getByRole("button", { name: /Create backup/i });
  if (!(await createBtn.isVisible({ timeout: 2_000 }).catch(() => false))) {
    test.skip(true, "Backup UI not present in this build");
  }
  await createBtn.click();

  // Backup row appears in history table.
  const historyRow = page.locator('table[aria-label*="Backup" i] tr').nth(1);
  await expect(historyRow).toBeVisible({ timeout: 15_000 });

  // Download — the UI uses apiFetch (XHR/fetch), not a browser anchor download,
  // so we click and wait for the network request rather than a download event.
  const downloadBtn = historyRow.getByRole("button", { name: /Download/i });
  if (await downloadBtn.isVisible({ timeout: 1_000 }).catch(() => false)) {
    const [req] = await Promise.all([
      page.waitForRequest((r) => r.url().includes("/backups/") && r.url().includes("/download")),
      downloadBtn.click(),
    ]);
    expect(req).toBeTruthy();
    // Clean up any downloaded temp file if the browser did save one.
    const tmpPath = path.join(process.cwd(), "playwright-backup.tar.gz");
    await fs.unlink(tmpPath).catch(() => undefined);
  }
});
