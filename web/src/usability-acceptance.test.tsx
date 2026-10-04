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
import AiEndpoints from "@/pages/AiEndpoints";
import Tokens from "@/pages/Tokens";
import AutomationTokens from "@/pages/AutomationTokens";
import Tunnels from "@/pages/Tunnels";
import Users from "@/pages/Users";
import Settings from "@/pages/Settings";

// ---------------------------------------------------------------------------
// INTUITIVE — navigation & mental-model alignment
// ---------------------------------------------------------------------------
describe("INTUITIVE — navigation & mental-model alignment", () => {
  // In-2: primary nav lists Home as the first entry — covered by Layout.test.tsx.
  // (renderApp(<App/>, "/") already mounts Layout; Layout.test.tsx owns that assertion.)

  it("In-1: / (root route) renders Home/Dashboard, not a blank page or redirect loop", async () => {
    renderApp(<App />, "/");
    // The Overview heading comes from Home.tsx <PageHeader title="Overview" …>
    const heading = await screen.findByRole("heading", { name: "Overview" });
    expect(heading).toBeDefined();
    // The top-level "Tunnels" page-heading should NOT appear on the home page
    expect(screen.queryByRole("heading", { name: "Tunnels" })).toBeNull();
  });

  it("In-3: Home page contains a 'How Burrow works' explainer section with Clients/Services/Tunnels", async () => {
    renderApp(<App />, "/");
    // The section header from Home.tsx <h2>How Burrow works</h2>
    await screen.findByRole("heading", { name: "How Burrow works" });
    // The explainer copy names all three concepts
    const body = document.body.textContent ?? "";
    expect(body).toContain("Client");
    expect(body).toContain("Service");
    expect(body).toContain("Tunnel");
  });

  it("In-4: Clients page — services-count badge links to the client detail view", async () => {
    renderApp(<Clients />, "/clients");
    // db seeds one client: session_id = "sess_4f7a9c0b2e81", service_count = 2
    // The aria-label is "View 2 services for office-box-1"
    const link = await screen.findByRole("link", { name: /View.*services for office-box-1/i });
    expect((link as HTMLAnchorElement).href).toContain("/clients/sess_4f7a9c0b2e81");
  });

  it("In-4: Tunnels page — http tunnel name links to the service detail view", async () => {
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
    renderApp(<Tunnels />, "/tunnels");
    const link = await screen.findByRole("link", { name: /Open service web/i });
    expect((link as HTMLAnchorElement).href).toContain("/services/svc_web01");
  });

  it("In-4: Services page — connected row's status badge links to /tunnels", async () => {
    renderApp(<Services />, "/services");
    // db seeds svc_web01 with connected=true; the status cell renders
    // <Link to="/tunnels" aria-label="View live tunnel for web">
    const link = await screen.findByRole("link", { name: /View live tunnel for web/i });
    expect((link as HTMLAnchorElement).href).toContain("/tunnels");
  });

  it("In-5: Tokens page includes a cross-link to /account/automation", async () => {
    renderApp(<Tokens />, "/tokens");
    // "Automation tokens" link in the muted helper text
    const link = await screen.findByRole("link", { name: /Automation tokens/i });
    expect((link as HTMLAnchorElement).href).toContain("/account/automation");
  });

  it("In-5: AutomationTokens page includes a cross-link to /tokens (client tokens)", async () => {
    renderApp(<AutomationTokens />, "/account/automation");
    const link = await screen.findByRole("link", { name: /Client tokens/i });
    expect((link as HTMLAnchorElement).href).toContain("/tokens");
  });

  // In-6: command palette (Ctrl+K) opens — Playwright-only; requires keyboard
  // interaction in a real browser (see web/e2e/usability.spec.ts).
});

// ---------------------------------------------------------------------------
// CLEAR — information architecture & labelling
// ---------------------------------------------------------------------------
describe("CLEAR — information architecture & labelling", () => {
  it("Clr-1: Tunnels page shows a live-vs-config explainer mentioning Services", async () => {
    renderApp(<Tunnels />, "/tunnels");
    // ErrorNotice variant="info" role="note" is the explainer in Tunnels.tsx
    const note = await screen.findByRole("note");
    expect(note.textContent).toContain("Services");
  });

  it("Clr-2: Services page shows a live-vs-config explainer mentioning Tunnels", async () => {
    renderApp(<Services />, "/services");
    // ErrorNotice variant="info" role="note" in Services.tsx
    const note = await screen.findByRole("note");
    expect(note.textContent).toContain("Tunnels");
  });

  it("Clr-4: AI-endpoints empty state renders a 'New AI service' CTA button (admin)", async () => {
    // Override /ai/endpoints to return empty list (no api_key services)
    server.use(
      http.get("/api/v1/ai/endpoints", () => HttpResponse.json([])),
    );
    renderApp(<AiEndpoints />, "/ai/endpoints");
    // Both PageHeader and EmptyState render the button for admin; at least one must exist.
    const btns = await screen.findAllByRole("button", { name: "New AI service" });
    expect(btns.length).toBeGreaterThanOrEqual(1);
  });

  it("Clr-6: Settings page shows a 'Custom domains (all services)' link to /settings/custom-domains", async () => {
    renderApp(<Settings />, "/settings");
    // settings-nav-card link in the Configuration section
    const link = await screen.findByRole("link", { name: /Custom domains \(all services\)/i });
    expect((link as HTMLAnchorElement).href).toContain("/settings/custom-domains");
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

  it("Ea-4: Users page renders an SMTP-not-configured notice with a link to Settings", async () => {
    // Default db.settings has no smtp.host, so smtpUnconfigured is true when settings loads.
    renderApp(<Users />, "/users");
    // ErrorNotice variant="warn" role="status" — the SMTP warning
    const notice = await screen.findByRole("status");
    expect(notice.textContent).toContain("Email");
    // The action link inside that notice
    const link = within(notice).getByRole("link", { name: /Set up email/i });
    expect((link as HTMLAnchorElement).href).toContain("/settings");
  });

  // ---------------------------------------------------------------------------
  // Ea-5: empty states have a next action
  // ---------------------------------------------------------------------------
  describe("Ea-5 — empty states have a next action", () => {
    it("Tunnels empty state has a 'Connect a client' CTA link", async () => {
      // Default mock returns [] for /tunnels (see handlers.ts line ~350)
      renderApp(<Tunnels />, "/tunnels");
      // Added in Tunnels.tsx empty-state: <Link to="/clients/connect">
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

    it("AI Endpoints empty state (admin) has a 'New AI service' button", async () => {
      server.use(
        http.get("/api/v1/ai/endpoints", () => HttpResponse.json([])),
      );
      renderApp(<AiEndpoints />, "/ai/endpoints");
      // Both PageHeader and EmptyState render the button for admin; assert at least one.
      const btns = await screen.findAllByRole("button", { name: /^New AI service$/i });
      expect(btns.length).toBeGreaterThanOrEqual(1);
    });

    it("Tokens empty state has a 'Connect a client' CTA in the page header", async () => {
      server.use(
        http.get("/api/v1/tokens", () => HttpResponse.json([])),
      );
      renderApp(<Tokens />, "/tokens");
      // PageHeader actions has the "Connect a client" link regardless of table content
      const link = await screen.findByRole("link", { name: /Connect a client/i });
      expect((link as HTMLAnchorElement).href).toContain("/clients/connect");
    });
  });
});
