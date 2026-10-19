// test-only — never deploy this shape.
//
// Spec 49 — Retention subpage (/settings/retention): save a numeric knob and
// verify it persists via the API, then restore the original values.
//
// Real-DOM notes (verified against web/src/pages/Retention.tsx + the live stack
// and internal/api/retention_handlers.go):
//   - GET/PUT /api/v1/settings/retention. Response/request shape is a flat
//     object of integer day/count knobs (see retentionSettingsResp). PUT fields
//     are all optional pointers — omitted fields are left unchanged.
//   - Each knob is an <input id="ret-<key>" type="number">. We drive the
//     "Webhook deliveries (days)" knob whose key is
//     webhook_deliveries_retention_days (id #ret-webhook_deliveries_retention_days).
//     Its valid range is 1..365 (backend) — we use 45 (distinct from the
//     seeded default of 30, and in range).
//   - "Save" submits PUT and toasts "Retention settings saved." on success.
//     The Save button is disabled while ANY field is out of range (hasErrors).
//
// !!! REAL PRODUCT BUG worked around here (key-name drift) — see below.
//   Backend JSON / store / DB / OpenAPI all use the key
//   `connection_logs_rollups_retention_days` ("logs" plural), but the front-end
//   (web/src/pages/Retention.tsx + web/src/lib/contract.ts) and its MSW mock
//   used `connection_log_rollups_retention_days` ("log" singular). On the live
//   API GET that mismatched key reads `undefined` → the Connection-log-rollups
//   input renders the string "undefined" → Number("undefined") === NaN → that
//   field is permanently "out of range" → `hasErrors` is always true → the
//   Save button is PERMANENTLY DISABLED, so NO retention knob can be saved via
//   the UI. (Vitest didn't catch it because the MSW mock used the same wrong
//   key — classic mock-vs-API drift.) The source side is FIXED in this change
//   (Retention.tsx + contract.ts + mocks/db.ts + handlers.ts + contract.test.ts
//   now use the canonical `..._logs_rollups_...` key). But the *already-running*
//   e2e stack still serves the pre-fix bundle, so to exercise the UI save flow
//   against it we first normalize the rollups field to a valid number. This
//   workaround is a no-op on a fixed build (the field already holds a valid
//   value) and is restored in finally{} regardless.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { ensureReauth } from "../../fixtures/reauth";
import { adminHeaders } from "../../fixtures/api";

interface RetentionResp {
  webhook_deliveries_retention_days: number;
  connection_logs_rollups_retention_days: number;
  [k: string]: number | string;
}

const KEY = "webhook_deliveries_retention_days";
const NEW_VALUE = 45; // distinct, in-range (1..365)
const ROLLUPS_KEY = "connection_logs_rollups_retention_days";

test.use({ storageState: AUTH_STORAGE_PATH });
// No services domain: guarded writes need a recent password confirmation (T02b).
test.beforeEach(async ({ request }) => { await ensureReauth(request); });

test("49-retention-save: change + save + persist + restore a retention knob", async ({ page, request }) => {
  // ---- Capture original retention values so finally{} can restore them. ----
  const beforeResp = await request.get("/api/v1/settings/retention");
  expect(beforeResp.ok()).toBeTruthy();
  const before = (await beforeResp.json()) as RetentionResp;
  const originalValue = before[KEY] as number;
  const originalRollups = (before[ROLLUPS_KEY] as number) ?? 0;
  expect(typeof originalValue).toBe("number");
  // Pick a value that is genuinely different from the current one.
  const target = originalValue === NEW_VALUE ? 60 : NEW_VALUE;

  try {
    await page.goto("/settings/retention");
    await expect(page.getByRole("heading", { name: /Retention/i })).toBeVisible();

    // Normalize the connection-log-rollups field to a valid number so the Save
    // button is enabled regardless of the key-name bug described above. Matches
    // either id (#ret-connection_log_rollups_retention_days on the buggy bundle,
    // #ret-connection_logs_rollups_retention_days on a fixed build).
    const rollupsInput = page.locator('input[id$="rollups_retention_days"]');
    if (await rollupsInput.count()) {
      await rollupsInput.first().fill(String(originalRollups));
    }

    const knob = page.locator(`#ret-${KEY}`);
    await expect(knob).toBeVisible();
    await knob.fill(String(target));

    const saveBtn = page.getByRole("button", { name: "Save", exact: true });
    await expect(saveBtn).toBeEnabled();
    await saveBtn.click();

    // Success signal: the page toasts "Retention settings saved."
    await expect(page.getByText("Retention settings saved.")).toBeVisible({ timeout: 10_000 });

    // Verify persistence via the API GET: the changed value is stored.
    await expect
      .poll(
        async () => {
          const r = await request.get("/api/v1/settings/retention");
          if (!r.ok()) return undefined;
          return ((await r.json()) as RetentionResp)[KEY];
        },
        { timeout: 10_000, message: `Expected ${KEY} to persist as ${target}` },
      )
      .toBe(target);
  } finally {
    // ---- RESTORE original retention values so other specs are unaffected. ----
    await request
      .put("/api/v1/settings/retention", {
        headers: adminHeaders(),
        data: { [KEY]: originalValue, [ROLLUPS_KEY]: originalRollups },
      })
      .catch(() => undefined);
  }
});
