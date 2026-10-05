import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import EmailSettings from "@/pages/settings/EmailSettings";

describe("Settings / Email / SMTP", () => {
  it("shows the unconfigured notice initially (P6B.2 — corrected copy)", async () => {
    renderApp(<EmailSettings />);
    expect(await screen.findByText(/Email isn't set up\. Password resets and test emails are unavailable until SMTP is configured\./)).toBeInTheDocument();
    expect(screen.queryByText(/invites are disabled/i)).not.toBeInTheDocument();
  });

  it("saves whitelisted SMTP settings", async () => {
    renderApp(<EmailSettings />);
    await screen.findByLabelText(/SMTP server/i);
    await userEvent.type(screen.getByLabelText(/SMTP server/i), "mx.acme.io");
    await userEvent.type(screen.getByLabelText(/^Port$/i), "587");
    await userEvent.click(screen.getByRole("button", { name: /save settings/i }));
    expect(await screen.findByText(/settings saved/i)).toBeInTheDocument();
  });

  it("surfaces the 409 when testing email while unconfigured", async () => {
    renderApp(<EmailSettings />);
    await screen.findByLabelText(/SMTP server/i);
    await userEvent.click(screen.getByRole("button", { name: /send test email/i }));
    await userEvent.type(screen.getByLabelText(/test recipient/i), "ops@acme.io");
    await userEvent.click(screen.getByRole("button", { name: /^test now$/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/not configured/i);
  });
});

describe("Settings / Email page", () => {
  it("is headed Email and says what the mail is for", async () => {
    renderApp(<EmailSettings />);
    expect(await screen.findByRole("heading", { name: "Email", level: 1 })).toBeInTheDocument();
    expect(screen.getByText("Outgoing mail for invitations and notifications.")).toBeInTheDocument();
  });

  it("shows the SMTP form, its Save button and the test-connection section", async () => {
    renderApp(<EmailSettings />);
    expect(await screen.findByLabelText(/SMTP server/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /save settings/i })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Test connection" })).toBeInTheDocument();
  });

  it("leaves the privacy toggle to the General page", async () => {
    renderApp(<EmailSettings />);
    await screen.findByLabelText(/SMTP server/i);
    expect(screen.queryByRole("checkbox", { name: /include top source ips/i })).toBeNull();
  });
});

describe("Settings / Email / SMTP form width (D-11/L-12)", () => {
  it("SMTP form fields share one width class (no ragged right edge) (D-11/L-12)", async () => {
    renderApp(<EmailSettings />);
    await screen.findByLabelText(/SMTP server/i);
    const fields = [...document.querySelectorAll(".pw-form .form-field")];
    const widths = fields.map(f => [...f.classList].find(c => c.startsWith("field-w-")));
    const nonPort = widths.filter(w => w !== "field-w-sm");
    expect(nonPort[0]).toBeDefined();
    expect(new Set(nonPort).size).toBe(1);
  });
});
