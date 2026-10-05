// usability-acceptance.test.tsx
//
// Acceptance suite for the Burrow "5-star usability" plan.
// DOM-assertable criteria are real assertions; journey/colour/screenshot
// criteria that require a live browser live in web/e2e/usability.spec.ts.
//
// In-2, In-6, Ea-1, Ea-2, Ea-3 → Playwright-only (see comments below).

import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import App from "@/App";
import Clients from "@/pages/Clients";
import Services from "@/pages/Services";
import ServicesOverview from "@/pages/ServicesOverview";
import Providers from "@/pages/Providers";
import AutomationTokens from "@/pages/AutomationTokens";

// ---------------------------------------------------------------------------
// INTUITIVE — navigation & mental-model alignment
// ---------------------------------------------------------------------------
describe("INTUITIVE — navigation & mental-model alignment", () => {
  // In-2: primary nav lists Overview (the home page) as the first entry — covered by Layout.test.tsx.
  // (renderApp(<App/>, "/") already mounts Layout; Layout.test.tsx owns that assertion.)

  it("In-1: / (root route) renders the Services overview, not a blank page or redirect loop", async () => {
    renderApp(<App />, "/");
    // The Overview heading comes from ServicesOverview.tsx <PageHeader title="Overview" …>
    const heading = await screen.findByRole("heading", { name: "Overview" });
    expect(heading).toBeDefined();
    // The top-level "Tunnels" page-heading should NOT appear on the home page
    expect(screen.queryByRole("heading", { name: "Tunnels" })).toBeNull();
  });

  it("In-3: the overview explains itself through its figures and setup steps, not a static card", async () => {
    renderApp(<App />, "/");
    const strip = await screen.findByRole("list", { name: "Overview" });
    // The tiles name the three concepts and lead to their pages.
    for (const label of ["Clients online", "Services", "Live now"]) {
      expect(within(strip).getByText(label)).toBeInTheDocument();
    }
    expect(screen.queryByRole("heading", { name: "How Burrow works" })).toBeNull();
  });

  it("In-4: Clients page — services-count badge links to the client detail view", async () => {
    renderApp(<Clients />, "/clients");
    // db seeds one client: session_id = "sess_4f7a9c0b2e81", service_count = 2
    // The aria-label is "View 2 services for office-box-1"
    const link = await screen.findByRole("link", { name: /View.*services for office-box-1/i });
    expect((link as HTMLAnchorElement).href).toContain("/clients/sess_4f7a9c0b2e81");
  });

  it("In-4: Services on Live — http tunnel name links to the service detail view", async () => {
    // Seed a non-empty tunnels list with an http tunnel that has a service_id
    server.use(
      http.get("/api/v1/tunnels", () =>
        HttpResponse.json([
          {
            id: "tnl_http01",
            name: "web",
            type: "http",
            remote_port: 0,
            local_addr: "127.0.0.1:3000",
            bytes_in: 0,
            bytes_out: 0,
            connected: true,
            url: "https://tunnels.example.com/svc/k7p2qx/",
            access_mode: "open",
            service_id: "svc_web01",
          },
        ]),
      ),
    );
    renderApp(<Services />, "/services?live=1");
    const link = await screen.findByRole("link", { name: "web" });
    expect((link as HTMLAnchorElement).href).toContain("/services/svc_web01");
  });

  it("In-4: Services page — connected row's status badge links to the Live filter", async () => {
    renderApp(<Services />, "/services");
    // db seeds svc_web01 with connected=true; the status cell renders
    // <Link to="/services?live=1" aria-label="View live tunnel for web">
    const link = await screen.findByRole("link", { name: /View live tunnel for web/i });
    expect(link).toHaveAttribute("href", "/services?live=1");
  });

  it("In-5: the Tokens tab of Clients includes a cross-link to /settings/automation", async () => {
    renderApp(<Clients />, "/clients?tab=tokens");
    // "Automation tokens" link in the muted helper text
    const link = await screen.findByRole("link", { name: /Automation tokens/i });
    expect((link as HTMLAnchorElement).href).toContain("/settings/automation");
  });

  it("In-5: AutomationTokens page includes a cross-link to the client tokens", async () => {
    renderApp(<AutomationTokens />, "/settings/automation");
    const link = await screen.findByRole("link", { name: /Client tokens/i });
    expect(link).toHaveAttribute("href", "/clients?tab=tokens");
  });

  // In-6: command palette (Ctrl+K) opens — Playwright-only; requires keyboard
  // interaction in a real browser (see web/e2e/usability.spec.ts).
});

// ---------------------------------------------------------------------------
// CLEAR — information architecture & labelling
// ---------------------------------------------------------------------------
describe("CLEAR — information architecture & labelling", () => {
  it("Clr-1/Clr-2: Services explains saved configuration against Live, in both views", async () => {
    // ErrorNotice variant="info" role="note" in Services.tsx
    const first = renderApp(<Services />, "/services");
    expect((await screen.findByRole("note")).textContent).toContain("Switch to Live");
    first.unmount();
    renderApp(<Services />, "/services?live=1");
    expect((await screen.findByRole("note")).textContent).toContain("saved configuration");
  });

  it("Clr-4: Providers page with no providers renders a 'New AI service' CTA button (admin)", async () => {
    // Override /ai/providers to return an empty list
    server.use(
      http.get("/api/v1/ai/providers", () => HttpResponse.json([])),
    );
    renderApp(<Providers />, "/gateway/providers");
    // The PageHeader renders the button for admin.
    const btns = await screen.findAllByRole("button", { name: "New AI service" });
    expect(btns.length).toBeGreaterThanOrEqual(1);
  });

});

// ---------------------------------------------------------------------------
// EASY — onboarding & discoverability
// ---------------------------------------------------------------------------
describe("EASY — onboarding & discoverability", () => {
  // Ea-1: ConnectClient onboarding form (real endpoint in command) — Playwright-only
  //   (requires form interaction + mint mutation; see web/e2e/usability.spec.ts).
  // Ea-2: command palette (Ctrl+K) opens a searchable dialog — Playwright-only.
  // Ea-3: InfoHint tooltips present — Playwright-only (hover required in a real browser).

  it("Ea-4: the overview renders an email-not-set-up notice with a link to the email settings", async () => {
    // Default db.settings has no smtp.host, so the relay notice is open once the settings load.
    renderApp(<ServicesOverview />, "/");
    // ErrorNotice variant="warn" role="status" — the SMTP warning
    const notice = await screen.findByRole("status");
    expect(notice.textContent).toContain("Email");
    // The action link inside that notice
    const link = within(notice).getByRole("link", { name: /Set up email/i });
    expect((link as HTMLAnchorElement).href).toContain("/settings/email");
  });

  // ---------------------------------------------------------------------------
  // Ea-5: empty states have a next action
  // ---------------------------------------------------------------------------
  describe("Ea-5 — empty states have a next action", () => {
    it("Services on Live: the empty state has a 'Connect a client' CTA link", async () => {
      server.use(
        http.get("/api/v1/tunnels", () => HttpResponse.json([])),
      );
      renderApp(<Services />, "/services?live=1");
      const link = await screen.findByRole("link", { name: /Connect a client/i });
      expect((link as HTMLAnchorElement).href).toContain("/clients/connect");
    });

    it("Clients empty state has a 'Connect a client' CTA in the page header", async () => {
      server.use(
        http.get("/api/v1/clients", () => HttpResponse.json([])),
      );
      renderApp(<Clients />, "/clients");
      // PageHeader actions has the "Connect a client" link even when the table is empty
      const link = await screen.findByRole("link", { name: /Connect a client/i });
      expect((link as HTMLAnchorElement).href).toContain("/clients/connect");
    });

    it("Services empty state has a 'New service' button in the page header", async () => {
      server.use(
        http.get("/api/v1/services", () => HttpResponse.json([])),
      );
      renderApp(<Services />, "/services");
      // PageHeader actions always renders the "New service" button
      const btn = await screen.findByRole("button", { name: /^New service$/i });
      expect(btn).toBeDefined();
    });

    it("Providers page with no providers (admin) has a 'New AI service' button", async () => {
      server.use(
        http.get("/api/v1/ai/providers", () => HttpResponse.json([])),
      );
      renderApp(<Providers />, "/gateway/providers");
      // The PageHeader renders the button for admin.
      const btns = await screen.findAllByRole("button", { name: /^New AI service$/i });
      expect(btns.length).toBeGreaterThanOrEqual(1);
    });

    it("Tokens tab: the empty state has a 'Connect a client' CTA in the page header", async () => {
      server.use(
        http.get("/api/v1/tokens", () => HttpResponse.json([])),
      );
      renderApp(<Clients />, "/clients?tab=tokens");
      // PageHeader actions has the "Connect a client" link regardless of table content
      const link = await screen.findByRole("link", { name: /Connect a client/i });
      expect((link as HTMLAnchorElement).href).toContain("/clients/connect");
    });
  });
});
