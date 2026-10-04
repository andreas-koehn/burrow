import { describe, it, expect, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import { resetDb, db } from "@/mocks/db";
import { Route, Routes } from "react-router-dom";
import { CustomDomainsPanel } from "./CustomDomains";
import type { CustomDomainStatus } from "@/lib/contract";

// Wrap in a route so we can supply serviceId from the URL
function mount(serviceId = "svc_ai001") {
  return renderApp(
    <Routes>
      <Route path="/services/:id" element={<CustomDomainsPanel serviceId={serviceId} />} />
    </Routes>,
    `/services/${serviceId}`,
  );
}

beforeEach(() => {
  resetDb();
});

describe("CustomDomainsPanel", () => {
  it("lists seeded domain with hostname, status badge, expiry, and truncated fingerprint", async () => {
    mount();
    // hostname visible as mono text
    const hostname = await screen.findByText("foo.example.com");
    expect(hostname).toBeTruthy();

    // status badge — human label, not snake_case (v0.5.2 Task 10)
    const badge = await screen.findByText("Active");
    expect(badge).toBeTruthy();

    // truncated fingerprint: first 12 chars of "deadbeef0123456789abcdef" + "…"
    const fp = await screen.findByText("deadbeef0123…");
    expect(fp).toBeTruthy();
  });

  it("renders the four-state badge labels (v0.5.2 Task 10)", async () => {
    // Seed one row per status in the MSW DB before mounting.
    const cases: { status: CustomDomainStatus; label: string }[] = [
      { status: "active", label: "Active" },
      { status: "pending", label: "Pending" },
      { status: "cert_expiring", label: "Expiring" },
      { status: "cert_expired", label: "Expired" },
    ];
    db.customDomains = cases.map((c, i) => ({
      id: `dom_${c.status}`,
      service_id: "svc_ai001",
      hostname: `host-${i}.example.com`,
      cert_sha256: `cert${i}deadbeef`,
      not_before: "2026-05-01T00:00:00Z",
      not_after: new Date(Date.now() + 90 * 24 * 60 * 60 * 1000).toISOString(),
      created_at: `2026-05-${10 + i}T00:00:00Z`,
      updated_at: `2026-05-${10 + i}T00:00:00Z`,
      status: c.status,
      status_updated_at: `2026-05-${10 + i}T00:00:00Z`,
    }));

    mount();
    for (const c of cases) {
      const el = await screen.findByText(c.label);
      expect(el).toBeTruthy();
    }
  });

  it("Add domain dialog opens", async () => {
    mount();
    await screen.findByText("foo.example.com");
    const addBtn = screen.getByRole("button", { name: /add domain/i });
    await userEvent.click(addBtn);
    // Dialog title should appear
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/add custom domain/i)).toBeTruthy();
  });

  it("SAN-mismatch rejection shows verbatim alert", async () => {
    mount();
    await screen.findByText("foo.example.com");

    // Open add dialog
    await userEvent.click(screen.getByRole("button", { name: /add domain/i }));
    const dialog = await screen.findByRole("dialog");

    // Fill hostname
    const hostnameInput = within(dialog).getByLabelText(/hostname/i);
    await userEvent.clear(hostnameInput);
    await userEvent.type(hostnameInput, "san-mismatch.example.com");

    // Fill cert and key PEM
    const certTa = within(dialog).getByLabelText("Certificate (PEM)");
    await userEvent.clear(certTa);
    await userEvent.type(certTa, "-----BEGIN CERTIFICATE-----\nMOCK\n-----END CERTIFICATE-----");

    const keyTa = within(dialog).getByLabelText("Private key (PEM)");
    await userEvent.clear(keyTa);
    await userEvent.type(keyTa, "-----BEGIN PRIVATE KEY-----\nMOCK\n-----END PRIVATE KEY-----");

    // Submit
    const submitBtn = within(dialog).getByRole("button", { name: /add$/i });
    await userEvent.click(submitBtn);

    // Verbatim error message
    await screen.findByText("The certificate's SAN does not include this hostname.");
  });

  it("successful Add → POST /services/:id/domains and dialog closes", async () => {
    mount();
    await screen.findByText("foo.example.com");

    await userEvent.click(screen.getByRole("button", { name: /add domain/i }));
    const dialog = await screen.findByRole("dialog");

    const hostnameInput = within(dialog).getByLabelText(/hostname/i);
    await userEvent.clear(hostnameInput);
    await userEvent.type(hostnameInput, "new.example.com");

    const certTa = within(dialog).getByLabelText("Certificate (PEM)");
    await userEvent.clear(certTa);
    await userEvent.type(certTa, "-----BEGIN CERTIFICATE-----\nDATA\n-----END CERTIFICATE-----");

    const keyTa = within(dialog).getByLabelText("Private key (PEM)");
    await userEvent.clear(keyTa);
    await userEvent.type(keyTa, "-----BEGIN PRIVATE KEY-----\nDATA\n-----END PRIVATE KEY-----");

    const submitBtn = within(dialog).getByRole("button", { name: /add$/i });
    await userEvent.click(submitBtn);

    // Dialog should close (dialog element gone)
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).toBeNull();
    });

    // New hostname should appear in the list
    await screen.findByText("new.example.com");
  });

  it("Delete row → confirms then DELETE /services/:id/domains/:did", async () => {
    mount();
    await screen.findByText("foo.example.com");

    // Click delete on the seeded row
    const deleteBtn = screen.getByRole("button", { name: /delete/i });
    await userEvent.click(deleteBtn);

    // Confirm dialog should appear
    const confirmDialog = await screen.findByRole("dialog");
    expect(within(confirmDialog).getByText(/foo\.example\.com/i)).toBeTruthy();

    // Click confirm
    const confirmBtn = within(confirmDialog).getByRole("button", { name: /remove/i });
    await userEvent.click(confirmBtn);

    // Domain should be gone
    await waitFor(() => {
      expect(screen.queryByText("foo.example.com")).toBeNull();
    });
  });
});

describe("CustomDomainsPanel empty state", () => {
  it("shows the empty row only after a successful empty load, never while loading", async () => {
    db.customDomains = [];
    mount();
    expect(screen.queryByText("No custom domains yet.")).toBeNull();
    expect(await screen.findByText("No custom domains yet.")).toBeInTheDocument();
    expect(screen.getByText(/Add a domain to serve this service/)).toBeInTheDocument();
  });

  it("shows an error notice, not the empty row, when the request fails with no data", async () => {
    server.use(http.get("/api/v1/services/:id/domains", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mount();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.queryByText("No custom domains yet.")).toBeNull();
  });
});
