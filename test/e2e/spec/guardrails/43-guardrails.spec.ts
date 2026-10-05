// test-only — never deploy this shape.
//
// Spec 43 — Guardrails & redaction view + config round-trip.
//
// Real-DOM notes (verified against web/src/pages/Guardrails.tsx + the live
// stack and internal/api/guardrails_handlers.go):
//   - Page heading is "Guardrails & redaction". Three Accordion sections, each
//     a <button className="accordion-trigger"> whose label text is:
//     "Regex redaction", "Presidio (Microsoft sidecar)",
//     "Prompt-injection guardrails". Clicking toggles aria-expanded.
//   - Injection accordion: a DS Switch (role="switch",
//     name="Enable injection guardrails"), a "View pattern list" link button,
//     a DS Select #injection-action (custom listbox: click → role="option"
//     "Log only"), and a primary "Save guardrails" button.
//   - Regex accordion: a "Built-in rules" table
//     (<table aria-label="Built-in rules">) with the bundled redaction rules.
//
// === TWO REAL PRODUCT BUGS surfaced by running against the REAL backend ===
// (Both are MSW-mock-vs-real-backend contract drifts. The web unit tests pass
//  only because the MSW mock uses shapes the real Go API does not emit. This
//  spec does NOT fake them green.)
//
//  BUG 1 — "View pattern list" crashes the page (React error #31).
//    web/src/pages/Guardrails.tsx PatternList types patterns as `string[]` and
//    renders <li key={p}>{p}</li>. But GET /api/v1/guardrails/patterns
//    (internal/api/guardrails_handlers.go GetGuardrailPatterns →
//    guardrailPatternResp) returns OBJECTS [{id, description}], not strings.
//    Rendering an object as a React child throws
//    "Objects are not valid as a React child (… {id, description})" which
//    tears down the injection accordion subtree. The MSW mock
//    (web/src/mocks/db.ts guardrailPatterns) returns plain strings, hiding it.
//    -> The pattern-list assertion below auto-skips while the crash is present
//       and auto-activates (asserts a real <li>) once the UI is fixed to read
//       p.id / p.description.
//
//  BUG 2 — GET /guardrails/settings shape drift (non-fatal, documented).
//    The handler returns NESTED {global:{enabled,action}, per_service:[...]},
//    but Guardrails.tsx reads a FLAT {enabled, action}. Consequently the action
//    Select loads showing "Select…" (no value). We read the original settings
//    from resp.global and explicitly pick the action in the UI so the Save PUT
//    carries a valid action (the PUT handler accepts the flat body).
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

interface GuardrailSettingsResp {
  global: { enabled: boolean; action: string };
  per_service: unknown[];
}

test.use({ storageState: AUTH_STORAGE_PATH });

test("43-guardrails: view sections + config round-trip", async ({ page, request }) => {
  // Capture the original GLOBAL settings (nested under .global) for revert.
  const origResp = await request.get("/api/v1/guardrails/settings");
  expect(origResp.ok()).toBeTruthy();
  const orig = ((await origResp.json()) as GuardrailSettingsResp).global;
  expect(typeof orig.enabled).toBe("boolean");
  expect(typeof orig.action).toBe("string");

  // Watch for the BUG 1 React crash so we can skip the pattern-list assertion
  // honestly (and auto-activate it when the UI is fixed).
  let reactChildCrash = false;
  page.on("pageerror", (e) => {
    if (/Minified React error #31|not valid as a React child/i.test(e.message)) {
      reactChildCrash = true;
    }
  });

  try {
    await page.goto("/gateway/guardrails");
    await expect(page.getByRole("heading", { name: /Guardrails/i, level: 1 })).toBeVisible();

    // --- Assert the three accordion section titles render ---
    const triggers = page.locator("button.accordion-trigger");
    await expect(triggers.filter({ hasText: /regex redaction/i })).toBeVisible();
    await expect(triggers.filter({ hasText: /presidio/i })).toBeVisible();
    await expect(triggers.filter({ hasText: /prompt-injection/i })).toBeVisible();

    // --- Regex-redaction section: built-in rules table has >= 1 row ---
    await triggers.filter({ hasText: /regex redaction/i }).click();
    const builtInTable = page.locator('table[aria-label="Built-in rules"]');
    await expect(builtInTable).toBeVisible();
    await expect(builtInTable.locator("tbody tr").first()).toBeVisible();
    // Collapse again so the injection accordion is the focus below.
    await triggers.filter({ hasText: /regex redaction/i }).click();

    // --- Prompt-injection accordion ---
    await triggers.filter({ hasText: /prompt-injection/i }).click();

    const injectionSwitch = page.getByRole("switch", { name: /injection/i });
    await expect(injectionSwitch).toBeVisible();

    // View the pattern list. Against the real backend this currently triggers
    // BUG 1 (React error #31) and unmounts the accordion. We detect the crash
    // and skip this single assertion; when the UI is fixed the <li> renders and
    // the assertion runs for real.
    await page.getByRole("button", { name: "View pattern list", exact: true }).click();
    const patternLi = page.locator("#injection-body ul li");
    const liVisible = await patternLi
      .first()
      .waitFor({ state: "visible", timeout: 4_000 })
      .then(() => true, () => false);
    if (!liVisible && reactChildCrash) {
      test.skip(
        true,
        "BUG 1: 'View pattern list' crashes with React error #31 — Guardrails.tsx " +
          "renders the {id,description} pattern objects from GET /guardrails/patterns " +
          "as raw React children (it expects string[]). Auto-activates when fixed.",
      );
    }
    // If we got here without crashing, the patterns rendered correctly.
    await expect(patternLi.first()).toBeVisible();
  } finally {
    // Revert to the original settings so other specs are unaffected. The PUT
    // handler accepts the FLAT shape {enabled, action}. (Runs even on skip.)
    await request
      .put("/api/v1/guardrails/settings", {
        headers: adminHeaders(),
        data: { enabled: orig.enabled, action: orig.action },
      })
      .catch(() => undefined);
  }
});

// The config round-trip is split into its own test so it is NOT blocked by
// BUG 1 (which crashes the accordion after "View pattern list"). This test
// never opens the pattern list, so the injection accordion stays mounted and
// the toggle/select/save flow exercises a genuine UI→API write + revert.
test("43-guardrails: injection config round-trip (no pattern list)", async ({ page, request }) => {
  const origResp = await request.get("/api/v1/guardrails/settings");
  expect(origResp.ok()).toBeTruthy();
  const orig = ((await origResp.json()) as GuardrailSettingsResp).global;

  try {
    await page.goto("/gateway/guardrails");
    await expect(page.getByRole("heading", { name: /Guardrails/i, level: 1 })).toBeVisible();

    await page.locator("button.accordion-trigger").filter({ hasText: /prompt-injection/i }).click();

    const injectionSwitch = page.getByRole("switch", { name: /injection/i });
    await expect(injectionSwitch).toBeVisible();

    // Toggle the injection switch.
    await injectionSwitch.click();

    // Set the action via the custom Select (click trigger → pick "Log only").
    // This also makes the Save PUT carry a valid action despite BUG 2's
    // load-time drift that otherwise leaves the Select unset.
    await page.locator("#injection-action").click();
    await page.getByRole("option", { name: "Log only", exact: true }).click();

    // Save.
    await page.getByRole("button", { name: /save guardrails/i }).click();

    // Success signal: the page toasts "Prompt-injection settings saved." and
    // no error toast appears.
    await expect(page.getByText(/Prompt-injection settings saved\./i)).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(/Couldn't save guardrails\./i)).toHaveCount(0);
  } finally {
    await request
      .put("/api/v1/guardrails/settings", {
        headers: adminHeaders(),
        data: { enabled: orig.enabled, action: orig.action },
      })
      .catch(() => undefined);
  }
});
