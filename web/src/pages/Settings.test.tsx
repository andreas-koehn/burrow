import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { db, resetDb } from "@/mocks/db";
import Settings from "@/pages/Settings";

describe("Settings / SMTP", () => {
  it("shows the unconfigured notice initially (P6B.2 — corrected copy)", async () => {
    renderApp(<Settings />);
    expect(await screen.findByText(/Email isn't set up\. Password resets and test emails are unavailable until SMTP is configured\./)).toBeInTheDocument();
    expect(screen.queryByText(/invites are disabled/i)).not.toBeInTheDocument();
  });

  it("saves whitelisted SMTP settings", async () => {
    renderApp(<Settings />);
    await screen.findByLabelText(/SMTP server/i);
    await userEvent.type(screen.getByLabelText(/SMTP server/i), "mx.acme.io");
    await userEvent.type(screen.getByLabelText(/^Port$/i), "587");
    await userEvent.click(screen.getByRole("button", { name: /save settings/i }));
    expect(await screen.findByText(/settings saved/i)).toBeInTheDocument();
  });

  it("surfaces the 409 when testing email while unconfigured", async () => {
    renderApp(<Settings />);
    await screen.findByLabelText(/SMTP server/i);
    await userEvent.click(screen.getByRole("button", { name: /send test email/i }));
    await userEvent.type(screen.getByLabelText(/test recipient/i), "ops@acme.io");
    await userEvent.click(screen.getByRole("button", { name: /^test now$/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/not configured/i);
  });
});

describe("Settings / nav cards", () => {
  // P1-14 + P2-10: OpenAPI viewer is now an in-app NavLink (no target=_blank);
  // Connection logs moved here from the sidebar.
  it("renders the configuration nav cards (Retention, Database, Backup, OpenAPI viewer, Connection logs, Custom domains)", async () => {
    renderApp(<Settings />);
    await screen.findByLabelText(/SMTP server/i);
    expect(screen.getByText("Retention & compliance")).toBeInTheDocument();
    expect(screen.getByText("Database backend")).toBeInTheDocument();
    expect(screen.getByText("Backup & restore")).toBeInTheDocument();
    expect(screen.getByText("OpenAPI viewer")).toBeInTheDocument();
    expect(screen.getByText("Connection logs")).toBeInTheDocument();
    // P7B.3: card is now relabeled and relinked
    expect(screen.getByText("Custom domains (all services)")).toBeInTheDocument();
    // OpenAPI viewer is now an internal NavLink (P1-14), no target=_blank.
    const openApiLink = screen.getByRole("link", { name: /openapi viewer/i });
    expect(openApiLink).not.toHaveAttribute("target");
    expect(openApiLink).toHaveAttribute("href", "/openapi");
  });

  it("existing SMTP form is still rendered below the nav cards", async () => {
    renderApp(<Settings />);
    expect(await screen.findByLabelText(/SMTP server/i)).toBeInTheDocument();
  });

  // P7B.3: Custom domains card links to /settings/custom-domains (not /services)
  it("Custom domains card links to /settings/custom-domains", async () => {
    renderApp(<Settings />);
    await screen.findByLabelText(/SMTP server/i);
    const link = screen.getByRole("link", { name: /custom domains \(all services\)/i });
    expect(link).toHaveAttribute("href", "/settings/custom-domains");
  });
});

describe("Settings / P7B.4 — General panel above Configuration cards", () => {
  it("General heading appears before Configuration heading in the DOM", async () => {
    renderApp(<Settings />);
    await screen.findByLabelText(/SMTP server/i);
    const headings = screen.getAllByRole("heading");
    const names = headings.map((h) => h.textContent ?? "");
    const generalIdx = names.findIndex((n) => /general/i.test(n));
    const configIdx = names.findIndex((n) => /configuration/i.test(n));
    expect(generalIdx).toBeGreaterThanOrEqual(0);
    expect(configIdx).toBeGreaterThanOrEqual(0);
    expect(generalIdx).toBeLessThan(configIdx);
  });

  it("SMTP fields and Save button are still present after reorder", async () => {
    renderApp(<Settings />);
    expect(await screen.findByLabelText(/SMTP server/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /save settings/i })).toBeInTheDocument();
  });
});

describe("Settings / SMTP form width (D-11/L-12)", () => {
  it("SMTP form fields share one width class (no ragged right edge) (D-11/L-12)", async () => {
    renderApp(<Settings />);
    await screen.findByLabelText(/SMTP server/i);
    const fields = [...document.querySelectorAll(".pw-form .form-field")];
    const widths = fields.map(f => [...f.classList].find(c => c.startsWith("field-w-")));
    const nonPort = widths.filter(w => w !== "field-w-sm");
    expect(nonPort[0]).toBeDefined();
    expect(new Set(nonPort).size).toBe(1);
  });
});

describe("Settings / Privacy — connection_logs.rollup_include_top_ips toggle (v0.5.1 Q12)", () => {
  afterEach(() => resetDb());

  it("toggle defaults to ON when the backend has no setting (default-true policy)", async () => {
    // db.settings does NOT carry the key by default.
    delete db.settings["connection_logs.rollup_include_top_ips"];
    renderApp(<Settings />);
    const toggle = await screen.findByRole("checkbox", { name: /include top source ips/i });
    expect(toggle).toBeChecked();
  });

  it("toggle reflects the persisted backend value 'false' on load", async () => {
    db.settings["connection_logs.rollup_include_top_ips"] = "false";
    renderApp(<Settings />);
    const toggle = await screen.findByRole("checkbox", { name: /include top source ips/i });
    await waitFor(() => expect(toggle).not.toBeChecked());
  });

  it("flipping the toggle issues PUT /settings with the correct key/value (true -> false)", async () => {
    delete db.settings["connection_logs.rollup_include_top_ips"];
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    renderApp(<Settings />);
    const toggle = await screen.findByRole("checkbox", { name: /include top source ips/i });
    expect(toggle).toBeChecked();
    await userEvent.click(toggle);
    await waitFor(() => {
      const sawPut = fetchSpy.mock.calls.some(([url, init]) => {
        const u = String(url);
        const method = (init as RequestInit | undefined)?.method ?? "GET";
        if (method !== "PUT" || !u.includes("/api/v1/settings")) return false;
        const body = (init as RequestInit | undefined)?.body;
        if (typeof body !== "string") return false;
        try {
          const parsed = JSON.parse(body) as Record<string, string>;
          return parsed["connection_logs.rollup_include_top_ips"] === "false";
        } catch {
          return false;
        }
      });
      expect(sawPut).toBe(true);
    });
    // Backend roundtrip — the MSW handler must whitelist the key.
    await waitFor(() => {
      expect(db.settings["connection_logs.rollup_include_top_ips"]).toBe("false");
    });
  });
});
