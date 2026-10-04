import { describe, it, expect, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { db, resetDb } from "@/mocks/db";
import Retention from "@/pages/Retention";

beforeEach(() => resetDb());

describe("Retention & compliance", () => {
  it("renders heading 'Retention & compliance'", async () => {
    renderApp(<Retention />);
    expect(await screen.findByRole("heading", { name: /retention & compliance/i })).toBeInTheDocument();
  });

  it("renders seven number inputs with their current values from /settings/retention", async () => {
    renderApp(<Retention />);
    // Wait for data to load by waiting for a known input label
    await screen.findByLabelText(/audit log/i);
    const inputs = document.querySelectorAll('input[type="number"]');
    expect(inputs.length).toBeGreaterThanOrEqual(7);
  });

  it("audit retention note appears as a muted advisory below the audit days input", async () => {
    renderApp(<Retention />);
    expect(
      await screen.findByText(/Audit retention only deletes the six rate-limited leaf event types/i),
    ).toBeInTheDocument();
  });

  it("Save → PUT /settings/retention + toast 'Retention settings saved.'", async () => {
    const user = userEvent.setup();
    renderApp(<Retention />);
    const input = await screen.findByLabelText(/audit log/i);
    // Clear and type a valid value
    await user.clear(input);
    await user.type(input, "30");
    await user.click(screen.getByRole("button", { name: /save/i }));
    expect(await screen.findByText(/Retention settings saved\./i)).toBeInTheDocument();
  });

  it("out-of-range value disables Save", async () => {
    const user = userEvent.setup();
    renderApp(<Retention />);
    const input = await screen.findByLabelText(/audit log/i);
    await user.clear(input);
    await user.type(input, "9999");
    const saveBtn = screen.getByRole("button", { name: /save/i });
    expect(saveBtn).toBeDisabled();
  });

  // v0.5.1 P3.7 (deferred to v0.5.2): on first visit, validation errors must
  // not appear until the user has interacted with a field. Pre-fix, a backend
  // default that fell outside the per-field range surfaced the red error
  // inline before the user had a chance to read the field's defaults — a
  // confusing first-visit UX. Post-fix the touched gate suppresses errors
  // until the field has been blurred (or the form submitted).
  it("does not show validation errors on first visit even when a backend default is out of range (v0.5.1 P3.7)", async () => {
    // Seed an out-of-range default for audit_retention_days (max 3650). Pre-fix
    // the page would render an inline error on first paint; post-fix the
    // touched gate suppresses it.
    db.retentionSettings = {
      ...db.retentionSettings,
      audit_retention_days: 9999,
    };
    renderApp(<Retention />);
    // Wait until the data has populated the field — observed via the input's
    // value matching the seeded backend default.
    const auditInput = (await screen.findByLabelText(/audit log/i)) as HTMLInputElement;
    await waitFor(() => expect(auditInput.value).toBe("9999"));
    // No alert role + no "must be between" copy should be present.
    expect(screen.queryAllByRole("alert")).toHaveLength(0);
    expect(screen.queryByText(/must be between/i)).toBeNull();
  });

  it("error surfaces only AFTER the field is blurred (v0.5.1 P3.7)", async () => {
    const user = userEvent.setup();
    renderApp(<Retention />);
    const usageInput = await screen.findByLabelText(/usage events/i);
    // Focus + clear the usage field — value is now NaN, out of range — but
    // the user has NOT yet blurred / left the field. Pre-fix this would
    // surface the red error immediately. Post-fix the error is deferred.
    await user.click(usageInput);
    await user.clear(usageInput);
    // Type-but-not-blur: still no error (user is mid-edit).
    expect(screen.queryAllByRole("alert")).toHaveLength(0);
    // Tab away → blur → error appears.
    await user.tab();
    expect(screen.queryAllByRole("alert")).toHaveLength(1);
  });
});

describe("Retention back link", () => {
  it("links back to Settings (U1)", async () => {
    renderApp(<Retention />);
    expect(await screen.findByRole("link", { name: "Back to Settings" })).toHaveAttribute("href", "/settings");
  });
});
