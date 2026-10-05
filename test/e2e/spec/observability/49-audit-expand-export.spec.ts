// test-only — never deploy this shape.
//
// Spec 49 — Audit log ROW EXPAND + EXPORT.
//
// Spec 15 already proves "UI mint → token.mint audit row → chain valid". This
// spec goes after the two audit affordances spec 15 does not touch:
//
//   ROW EXPAND — each audit <tr> is clickable and toggles an inline detail row
//                that renders the event's structured payload (a JSON <pre>) plus
//                the prev_hash / hash line. See web/src/pages/AuditLog.tsx Row().
//
//   EXPORT     — the "Export" button in the page header. NOTE (reported as a UX
//                gap): AuditLog.tsx wires it as
//                  onClick={() => { void apiFetch("/audit/export?format=ndjson"); }}
//                i.e. a fire-and-DISCARD fetch — the NDJSON body is read by
//                apiFetch and thrown away, so NO browser download is triggered
//                and nothing visible happens in the UI. The server DOES set
//                Content-Disposition: attachment (internal/api/audit_handlers.go
//                GetAuditExport), so the feature is one apiFetch→download swap
//                away from working. Until then the only observable effect is the
//                200 network response, which this spec asserts via
//                page.waitForResponse on /audit/export.
//
// To guarantee an expandable row exists we create + delete a throwaway user via
// the API (emits user.create / user.delete audit events) and search for it.
// The user is deleted in finally.

import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

test.use({ storageState: AUTH_STORAGE_PATH });

test("49-audit-expand-export: row expands to JSON payload + Export hits the export endpoint", async ({ page, request }) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (e) => pageErrors.push(e.message));

  // Seed a guaranteed audit event: create a throwaway user (→ user.create).
  const email = `auditseed-${Date.now()}@e2e.local`;
  const created = await request.post("/api/v1/users", {
    headers: adminHeaders(),
    data: { email, password: "init-pass-123", role: "user" },
  });
  expect(created.status(), "create throwaway user").toBe(201);
  const userId = (await created.json()).id as string;
  expect(userId).toBeTruthy();

  try {
    await page.goto("/settings/audit");
    await expect(page.getByRole("heading", { name: "Audit log", level: 1 })).toBeVisible();

    const table = page.locator('table[aria-label="Audit events"]');
    await expect(table).toBeVisible({ timeout: 10_000 });

    // Narrow to our seed event so the first row is deterministic. The audit
    // event JSON / subject contains the new user's email or id.
    await page.getByRole("searchbox", { name: "Search audit events" }).fill(email);

    // A matching row must appear. Restrict to clickable data rows (the expand
    // detail row is also a <tr> but is not clickable).
    const dataRows = table.locator("tbody tr.clickable");
    await expect(dataRows.first()).toBeVisible({ timeout: 10_000 });

    // ---- ROW EXPAND ----
    // Before clicking, there is no <pre> JSON block in the table body.
    await expect(table.locator("tbody pre")).toHaveCount(0);

    await dataRows.first().click();

    // The expanded detail row renders a <pre> with the event's JSON payload and
    // a muted prev_hash/hash line. Assert both are present.
    const payloadPre = table.locator("tbody pre").first();
    await expect(payloadPre).toBeVisible({ timeout: 5_000 });
    // The structured payload is valid JSON — at minimum a brace-delimited blob.
    await expect(payloadPre).toContainText("{");
    // The hash-chain detail line proves this is the audit detail (not a body
    // diff or other pre): "prev_hash: … · hash: …".
    await expect(table.getByText(/prev_hash:/i)).toBeVisible();
    await expect(table.getByText(/hash:/i).first()).toBeVisible();

    // Collapsing again hides the payload (toggle behaviour).
    await dataRows.first().click();
    await expect(table.locator("tbody pre")).toHaveCount(0);

    // ---- EXPORT ----
    // The Export button fires GET /audit/export?format=ndjson via apiFetch and
    // DISCARDS the body (no browser download). Assert the network call returns
    // 200. This is a discard-style fetch — flagged as a UX gap in the header
    // comment above.
    const exportRespPromise = page.waitForResponse(
      (resp) => resp.url().includes("/audit/export") && resp.request().method() === "GET",
      { timeout: 10_000 },
    );
    await page.getByRole("button", { name: "Export", exact: true }).click();
    const exportResp = await exportRespPromise;
    expect(exportResp.status(), "audit export endpoint returns 200").toBe(200);
    // Confirm it served the signed NDJSON content type (proves the right route).
    expect(exportResp.headers()["content-type"] ?? "").toContain("ndjson");

    // No uncaught render errors on any of the above.
    expect(
      pageErrors,
      `unexpected page errors: ${pageErrors.join(" | ")}`,
    ).toHaveLength(0);
  } finally {
    await request
      .delete(`/api/v1/users/${userId}`, { headers: adminHeaders() })
      .catch(() => undefined);
  }
});
