// test-only — never deploy this shape.
//
// Spec 42 — Backup create / verify / restore-surface (SAFE).
//
// !!! SAFETY: This spec deliberately NEVER executes a real restore. A real
// restore wipes the live database and would corrupt the shared, already-running
// Docker stack that every other e2e spec depends on. We exercise the restore
// UI only up to the confirmation dialog ("Restore from backup?") and then click
// CANCEL — never the destructive "Restore" button inside that dialog. The
// actual restore swap/genesis logic is covered by Go unit tests
// (cmd/server/restore_test.go) and the API handler tests
// (internal/api/backup_test.go), not here.
//
// Real-DOM notes (verified against web/src/pages/BackupRestore.tsx + the live
// stack and internal/api/backup_handlers.go):
//   - Page heading is "Backup & restore" (h1).
//   - "Create backup" button → POST /api/v1/backups; toasts "Backup queued."
//   - History table is <table className="data" aria-label="Backup history">.
//   - Per-row "Verify" button toasts "Backup is intact." / "Backup mismatch."
//     (NOT /verified|ok/i), so we assert verification via the API instead:
//     POST /api/v1/backups/{id}/verify → 200 {ok:true, sha256_match:true}.
//   - Restore: a visually-hidden #restore-file input; selecting a file enables
//     the destructive "Restore" button which opens the confirm Dialog
//     "Restore from backup?" warning "Active client sessions and the audit
//     chain will be reset." Footer: Cancel + (destructive) Restore.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { ensureReauth } from "../../fixtures/reauth";
import { adminHeaders } from "../../fixtures/api";

interface BackupRow { id: string }

test.use({ storageState: AUTH_STORAGE_PATH });
// No services domain: guarded writes need a recent password confirmation (T02b).
test.beforeEach(async ({ request }) => { await ensureReauth(request); });

test("42-backup-manage: create + verify + restore safety-gate", async ({ page, request }) => {
  // Capture the set of pre-existing backup ids so we can identify ours later.
  const beforeResp = await request.get("/api/v1/backups");
  expect(beforeResp.ok()).toBeTruthy();
  const beforeIds = new Set(((await beforeResp.json()) as BackupRow[]).map((b) => b.id));

  let createdId: string | undefined;

  try {
    await page.goto("/settings/backups");
    await expect(page.getByRole("heading", { name: "Backup & restore", level: 1 })).toBeVisible();

    // --- CREATE ---
    await page.getByRole("button", { name: "Create backup", exact: true }).click();
    await expect(page.getByText(/backup queued|created/i)).toBeVisible({ timeout: 10_000 });

    // Poll the history table until at least one row appears.
    const historyTable = page.locator('table[aria-label="Backup history"]');
    await expect(historyTable.locator("tbody tr").first()).toBeVisible({ timeout: 15_000 });

    // Identify the backup we just created (the new id not present before).
    let afterIds: BackupRow[] = [];
    await expect
      .poll(
        async () => {
          const r = await request.get("/api/v1/backups");
          afterIds = r.ok() ? ((await r.json()) as BackupRow[]) : [];
          return afterIds.filter((b) => !beforeIds.has(b.id)).length;
        },
        { timeout: 15_000, message: "Expected a new backup id after Create" },
      )
      .toBeGreaterThanOrEqual(1);
    createdId = afterIds.find((b) => !beforeIds.has(b.id))!.id;

    // --- VERIFY (via API — the UI toast text isn't a clean machine signal) ---
    // The newest backup is ours; verify it returns ok:true + sha256_match.
    const verifyResp = await request.post(`/api/v1/backups/${createdId}/verify`, {
      headers: adminHeaders(),
      data: {},
    });
    expect(verifyResp.status()).toBe(200);
    const verifyBody = (await verifyResp.json()) as { ok: boolean; sha256_match: boolean };
    expect(verifyBody.ok).toBe(true);
    expect(verifyBody.sha256_match).toBe(true);

    // Also exercise the UI Verify button (best-effort) for coverage — its toast
    // is "Backup is intact." but we don't gate on the exact text.
    const newestRow = historyTable.locator("tbody tr").first();
    await newestRow.getByRole("button", { name: "Verify", exact: true }).click();

    // --- RESTORE SAFETY-GATE ONLY (never executes a real restore) ---
    // Select a dummy archive so the Restore button enables. The bytes are
    // intentionally not a valid archive: we Cancel before any upload happens.
    await page.setInputFiles("#restore-file", {
      name: "dummy.tar.gz",
      mimeType: "application/gzip",
      buffer: Buffer.from("not-a-real-archive"),
    });

    const restoreBtn = page.getByRole("button", { name: "Restore", exact: true });
    await expect(restoreBtn).toBeEnabled();
    await restoreBtn.click();

    // Confirm dialog appears with the destructive warning.
    const confirm = page.getByRole("dialog", { name: "Restore from backup?" });
    await expect(confirm).toBeVisible();
    await expect(confirm).toContainText("audit chain will be reset");
    await expect(confirm).toContainText("Active client sessions");

    // CANCEL — we must NEVER click the destructive Restore inside this dialog
    // (it would wipe the shared stack's DB). Restore logic is covered by
    // cmd/server/restore_test.go + internal/api/backup_test.go.
    await confirm.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(confirm).not.toBeVisible();
  } finally {
    // Delete the backup we created so we leave no residue.
    if (createdId) {
      await request
        .delete(`/api/v1/backups/${createdId}`, { headers: adminHeaders() })
        .catch(() => undefined);
    }
  }
});
