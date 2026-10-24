import { describe, it, expect, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import { resetDb, db } from "@/mocks/db";
import { Route, Routes } from "react-router-dom";
import { CustomDomainsPanel } from "./CustomDomains";
import { ReauthDialog } from "@/components/ReauthDialog";
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

  it("New domain opens the add dialog", async () => {
    mount();
    await screen.findByText("foo.example.com");
    const addBtn = screen.getByRole("button", { name: "New domain" });
    await userEvent.click(addBtn);
    // Dialog title should appear
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/add custom domain/i)).toBeTruthy();
  });

  it("SAN-mismatch rejection shows verbatim alert", async () => {
    mount();
    await screen.findByText("foo.example.com");

    // Open add dialog
    await userEvent.click(screen.getByRole("button", { name: "New domain" }));
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

    await userEvent.click(screen.getByRole("button", { name: "New domain" }));
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

  it("Add asks for the password first when the relay wants it, then goes through (shared dialog)", async () => {
    let tries = 0;
    server.use(http.post("/api/v1/services/:id/domains", () => {
      tries += 1;
      return tries === 1
        ? HttpResponse.json({ error: "reauth_required" }, { status: 403 })
        : HttpResponse.json({ id: "dom_new", service_id: "svc_ai001", hostname: "new.example.com", status: "pending" }, { status: 201 });
    }));
    renderApp(
      <>
        <Routes><Route path="/services/:id" element={<CustomDomainsPanel serviceId="svc_ai001" />} /></Routes>
        <ReauthDialog />
      </>,
      "/services/svc_ai001",
    );
    await screen.findByText("foo.example.com");
    await userEvent.click(screen.getByRole("button", { name: "New domain" }));
    const add = await screen.findByRole("dialog", { name: /custom domain/i });
    await userEvent.type(within(add).getByLabelText(/hostname/i), "new.example.com");
    await userEvent.type(within(add).getByLabelText("Certificate (PEM)"), "-----BEGIN CERTIFICATE-----\nDATA\n-----END CERTIFICATE-----");
    await userEvent.type(within(add).getByLabelText("Private key (PEM)"), "-----BEGIN PRIVATE KEY-----\nDATA\n-----END PRIVATE KEY-----");
    await userEvent.click(within(add).getByRole("button", { name: /add$/i }));

    const confirm = await screen.findByRole("dialog", { name: "Confirm your password" });
    await userEvent.type(within(confirm).getByLabelText("Your password"), "password123{Enter}");
    await waitFor(() => expect(tries).toBe(2));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
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
    expect(screen.getByRole("table", { name: "Custom domains" }).querySelector(".skel")).not.toBeNull();
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

// ---- H13: certificate source, status, the gRPC address, the relay's refusals ----

async function openAdd() {
  await screen.findByText("foo.example.com");
  await userEvent.click(screen.getByRole("button", { name: "New domain" }));
  return screen.findByRole("dialog", { name: /custom domain/i });
}

describe("CustomDomainsPanel host names", () => {
  it("shows the certificate source and status of each domain, and the gRPC address host:443", async () => {
    db.customDomains = [
      { ...db.customDomains[0]!, id: "d1", hostname: "up.example.com", cert_source: "uploaded", cert_status: "ready" },
      { ...db.customDomains[0]!, id: "d2", hostname: "auto.example.com", cert_source: "auto", cert_status: "pending", cert_sha256: "" },
      { ...db.customDomains[0]!, id: "d3", hostname: "proxied.example.com", cert_source: "auto", cert_status: "not_needed", cert_sha256: "" },
    ];
    mount();
    await screen.findByText("up.example.com");
    const t = screen.getByRole("table", { name: "Custom domains" });
    const rows = within(t).getAllByRole("row").slice(1);
    expect(rows[0]).toHaveTextContent(/up\.example\.com.*Uploaded.*Active/s);
    expect(rows[1]).toHaveTextContent(/auto\.example\.com.*Automatic.*Waiting for a certificate/s);
    expect(rows[2]).toHaveTextContent(/proxied\.example\.com.*Automatic.*Handled by the proxy/s);
    // The address a gRPC client dials, copyable.
    expect(within(rows[1]!).getByRole("button", { name: "Copy gRPC address auto.example.com:443" })).toBeInTheDocument();
    expect(rows[1]).toHaveTextContent("auto.example.com:443");
    // No expiry or fingerprint for an automatic certificate.
    expect(rows[1]!.querySelectorAll("td")[3]).toHaveTextContent("—");
  });

  it("a relay from before cert_source shows the domain as uploaded", async () => {
    db.customDomains = [{ ...db.customDomains[0]!, cert_source: undefined, cert_status: undefined }];
    mount();
    expect(await screen.findByText("Uploaded")).toBeInTheDocument();
  });

  it("an administrator can choose automatic: no certificate is asked for, and none is sent", async () => {
    let sent: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/services/:id/domains", async ({ request }) => {
      sent = (await request.clone().json()) as Record<string, unknown>;
      return HttpResponse.json({ ...db.customDomains[0]!, id: "dom_a", hostname: "grpc.example.com", cert_source: "auto", cert_status: "pending", cert_sha256: "" }, { status: 201 });
    }));
    mount();
    const dialog = await openAdd();
    await userEvent.type(within(dialog).getByLabelText(/hostname/i), "grpc.example.com");
    await userEvent.click(within(dialog).getByRole("radio", { name: /Automatic/ }));
    expect(within(dialog).queryByLabelText("Certificate (PEM)")).toBeNull();
    expect(within(dialog).getByText(/DNS.*points at this relay/i)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: /add$/i }));
    await waitFor(() => expect(sent).toEqual({ hostname: "grpc.example.com", cert_source: "auto" }));
  });

  it("someone who holds only services:configure:own is offered an uploaded certificate only", async () => {
    db.me = { ...db.me, role: "user", permissions: ["services:configure:own"] };
    db.services.find((s) => s.id === "svc_ai001")!.user_id = db.me.id;
    mount();
    const dialog = await openAdd();
    expect(within(dialog).queryByRole("radio", { name: /Automatic/ })).toBeNull();
    expect(within(dialog).getByLabelText("Certificate (PEM)")).toBeInTheDocument();
    expect(within(dialog).getByText(/Automatic certificates are set up by an administrator/i)).toBeInTheDocument();
  });

  it.each([
    [403, "automatic certificates need an administrator; upload a certificate instead"],
    [409, "hostname is a tcp host name"],
    [409, "hostname overlaps a wildcard custom domain of another service: it lies under one, or as a wildcard it covers another service's custom domain"],
    [429, "the relay has as many custom domains with automatic certificates as it may (BURROW_ACME_CUSTOM_MAX); upload a certificate instead, or remove one"],
  ])("shows the relay's %i text in the dialog: %s", async (status, text) => {
    server.use(http.post("/api/v1/services/:id/domains", () => HttpResponse.json({ error: text }, { status })));
    mount();
    const dialog = await openAdd();
    await userEvent.type(within(dialog).getByLabelText(/hostname/i), "x.example.com");
    await userEvent.click(within(dialog).getByRole("radio", { name: /Automatic/ }));
    await userEvent.click(within(dialog).getByRole("button", { name: /add$/i }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(text);
    // The dialog stays open so that the reader can change the name or the source.
    expect(screen.getByRole("dialog", { name: /custom domain/i })).toBeInTheDocument();
  });

  it("the mock relay refuses auto at the caps with the relay's 429", async () => {
    db.autoDomainCaps = { global: 0, perUser: 10 };
    mount();
    const dialog = await openAdd();
    await userEvent.type(within(dialog).getByLabelText(/hostname/i), "y.example.com");
    await userEvent.click(within(dialog).getByRole("radio", { name: /Automatic/ }));
    await userEvent.click(within(dialog).getByRole("button", { name: /add$/i }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/BURROW_ACME_CUSTOM_MAX/);
  });

  it("New domain and Delete are offered only with services:configure", async () => {
    db.me = { ...db.me, role: "user", permissions: ["tunnels:read:own"] };
    mount();
    await screen.findByText("foo.example.com");
    expect(screen.queryByRole("button", { name: "New domain" })).toBeNull();
    expect(screen.queryByRole("button", { name: /delete/i })).toBeNull();
  });
});
