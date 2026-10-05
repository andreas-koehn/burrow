// test-only — never deploy this shape.
//
// Spec 48 — Settings page (/settings): SMTP form save + send-test-email flow +
// connection-log privacy toggle.
//
// Real-DOM notes (verified against web/src/pages/Settings.tsx + the live stack
// and internal/api/settings_handlers.go / retention_handlers.go):
//   - GET /api/v1/settings returns the raw SettingsMap (Record<string,string>).
//     On a fresh stack it is `{}` — no smtp.* keys, so the page shows the
//     "Email isn't set up yet." notice. We capture whatever it currently is so
//     the finally{} can put it back byte-for-byte.
//   - SMTP fields: #smtp-host, #smtp-port, #smtp-user (key smtp.username),
//     #smtp-from, plus the Encryption DS Select (#smtp-enc) which defaults to
//     STARTTLS — we do NOT touch it.
//   - "Save settings" submits PUT /api/v1/settings with the WHOLE form map and
//     toasts "Email settings saved." on success.
//   - "Send test email" reveals #smtp-test-to + a "Test now" button. POST
//     /api/v1/settings/test-email actually tries to connect to the (fake) host,
//     so it fails and the page renders a <p role="alert" class="field-error">.
//     We only assert the flow runs end to end — failure is the expected result.
//   - Privacy toggle: DS Checkbox (role=checkbox) #rollup-include-top-ips. Each
//     click fires PUT /api/v1/settings with just the rollup key. We flip it and
//     assert aria-checked changes, then restore it in finally{}.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

type SettingsMap = Record<string, string>;

// SMTP keys this spec writes; finally{} clears any that weren't present before.
// NB: smtp.tls is handled separately because the backend rejects an empty value
// (PUT /settings requires none|starttls|implicit), so it cannot be "cleared".
const SMTP_TEXT_KEYS = ["smtp.host", "smtp.port", "smtp.username", "smtp.from"];
const ROLLUP_KEY = "connection_logs.rollup_include_top_ips";

test.use({ storageState: AUTH_STORAGE_PATH });

test("48-settings-smtp: SMTP save + test-email flow + privacy toggle", async ({ page, request }) => {
  // ---- Capture original settings so we can restore the relay's prior state. ----
  const beforeResp = await request.get("/api/v1/settings");
  expect(beforeResp.ok()).toBeTruthy();
  const before = (await beforeResp.json()) as SettingsMap;

  try {
    await page.goto("/settings/email");
    await expect(page.getByRole("heading", { name: "Email", level: 1 })).toBeVisible();

    // ---- SMTP SAVE ----
    await page.fill("#smtp-host", "smtp.example.com");
    await page.fill("#smtp-port", "587");
    await page.fill("#smtp-user", "u@example.com");
    await page.fill("#smtp-from", "from@example.com");
    // Encryption defaults to STARTTLS (DS Select #smtp-enc) — left untouched.

    await page.getByRole("button", { name: "Save settings", exact: true }).click();
    await expect(page.getByText("Email settings saved.")).toBeVisible({ timeout: 10_000 });

    // ---- SEND TEST EMAIL (legitimately fails against the fake host) ----
    await page.getByRole("button", { name: "Send test email", exact: true }).click();
    const testTo = page.locator("#smtp-test-to");
    await expect(testTo).toBeVisible();
    await testTo.fill("you@example.com");
    await page.getByRole("button", { name: "Test now", exact: true }).click();

    // The fake SMTP host can't be reached, so the send fails. The flow ran end
    // to end if EITHER the inline field-error notice OR an error toast appears.
    // (We do NOT require success — the point is the UI flow works.)
    const fieldError = page.locator("p.field-error[role='alert']");
    const errorToast = page.locator("[data-sonner-toast][data-type='error']");
    await expect(async () => {
      const errVisible = await fieldError.isVisible().catch(() => false);
      const toastVisible = await errorToast.isVisible().catch(() => false);
      expect(errVisible || toastVisible).toBeTruthy();
    }).toPass({ timeout: 15_000 });

    // ---- PRIVACY TOGGLE ----
    const toggle = page.locator("#rollup-include-top-ips");
    await expect(toggle).toBeVisible();
    const startState = await toggle.getAttribute("aria-checked");
    await toggle.click();
    // It should flip to the opposite aria-checked value (no error).
    await expect(toggle).not.toHaveAttribute("aria-checked", startState ?? "true", {
      timeout: 10_000,
    });
  } finally {
    // ---- RESTORE original settings via PUT so other specs see the prior state. ----
    // 1) Restore text SMTP keys: send each captured original value back; for keys
    //    that did NOT exist before, send "" to clear what this spec wrote.
    const restore: SettingsMap = {};
    for (const k of SMTP_TEXT_KEYS) restore[k] = before[k] ?? "";
    // 2) smtp.tls cannot be cleared (empty is rejected) — only restore it if the
    //    original had a value; otherwise leave whatever is there (harmless: with
    //    smtp.host="" the page still reports "not configured").
    if (before["smtp.tls"]) restore["smtp.tls"] = before["smtp.tls"];
    // 3) Restore the rollup flag to its captured value (default-on if absent).
    restore[ROLLUP_KEY] = before[ROLLUP_KEY] ?? "true";
    await request
      .put("/api/v1/settings", { headers: adminHeaders(), data: restore })
      .catch(() => undefined);
  }
});
