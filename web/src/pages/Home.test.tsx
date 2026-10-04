import { describe, it, expect, afterEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, resetDb } from "@/mocks/db";
import Home from "./Home";

describe("Home (Overview)", () => {
  afterEach(() => {
    resetDb();
  });

  it("renders the Overview heading", async () => {
    renderApp(<Home />);
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
  });

  it("renders a 'Last 24 hours' strip with AI-cost tile showing $1.23 and labelled token counts", async () => {
    renderApp(<Home />);
    const strip = await screen.findByRole("list", { name: "Last 24 hours" });
    const { getAllByRole } = within(strip);
    await waitFor(() => {
      const tiles = getAllByRole("listitem");
      // AI cost tile must be present and show seed values
      const costTile = tiles.find((t) => t.querySelector(".label")?.textContent === "AI cost (24h)");
      expect(costTile).toBeTruthy();
      expect(costTile!.querySelector(".value")?.textContent).toBe("$1.23");
      expect(costTile!.querySelector(".sub")?.textContent).toBe("12,000 tokens in · 8,000 out");
    });
  });

  it("hides the AI-cost tile when /cost/summary returns 404 (featureAbsent)", async () => {
    server.use(
      http.get("/api/v1/cost/summary", () =>
        HttpResponse.json({ error: "x" }, { status: 404 }),
      ),
    );
    renderApp(<Home />);
    // Overview strip still renders
    await screen.findByRole("list", { name: "Overview" });
    // AI cost tile is absent
    await waitFor(() => {
      expect(screen.queryByText("AI cost (24h)")).toBeNull();
    });
  });

  it("renders counts MetricStrip with Clients=1, Services=4, Live tunnels=3", async () => {
    renderApp(<Home />);
    const strip = await screen.findByRole("list", { name: "Overview" });
    const { getAllByRole } = within(strip);
    // Wait for all three async queries (/me → isAdmin, /clients, /services) to settle.
    await waitFor(() => {
      const tiles = getAllByRole("listitem");
      expect(tiles).toHaveLength(3);
      // Clients tile — seeded mock has 1 client (admin-gated: resolves after /me)
      expect(tiles[0].querySelector(".value")?.textContent).toBe("1");
      // Services tile — seeded mock has 4 services
      expect(tiles[1].querySelector(".value")?.textContent).toBe("4");
      // Live tunnels tile — 3 services have connected=true
      expect(tiles[2].querySelector(".value")?.textContent).toBe("3");
    });
  });

  it("stat tiles show short context, not glossary sentences or backticks (L6)", async () => {
    renderApp(<Home />);
    const strip = await screen.findByRole("list", { name: "Overview" });
    expect(strip.textContent).not.toContain("`");
    expect(within(strip).getByText("machines connected")).toBeInTheDocument();
    expect(within(strip).getByText("saved configurations")).toBeInTheDocument();
    expect(within(strip).getByText("forwarding right now")).toBeInTheDocument();
    for (const tile of within(strip).getAllByRole("listitem")) {
      expect(tile.getAttribute("title")).toBeTruthy();
      expect(tile.getAttribute("title")).not.toContain("`");
    }
  });

  it("labels the token counts on the AI cost tile (L6)", async () => {
    renderApp(<Home />);
    const strip = await screen.findByRole("list", { name: "Last 24 hours" });
    await waitFor(() => {
      expect(strip.textContent).toMatch(/tokens in · .* out/);
    });
    expect(within(strip).getByText("in / out")).toBeInTheDocument();
  });

  // ---- Alerts strip ----

  describe("SMTP alert", () => {
    it("shows SMTP alert when settings is empty (admin, smtp.host absent)", async () => {
      // Seed: settings={} → smtp.host absent → alert should appear
      renderApp(<Home />);
      await screen.findByRole("heading", { name: "Overview" });
      await waitFor(() => {
        expect(
          screen.getByText(/Email isn't set up\. Invites and password resets are unavailable until SMTP is configured\./),
        ).toBeInTheDocument();
      });
      // The alert must contain a link to /settings
      const link = screen.getByRole("link", { name: /set up email/i });
      expect(link).toBeInTheDocument();
      expect(link).toHaveAttribute("href", "/settings");
    });

    it("hides SMTP alert when smtp.host is set", async () => {
      server.use(
        http.get("/api/v1/settings", () =>
          HttpResponse.json({ "smtp.host": "smtp.example.com" }),
        ),
      );
      renderApp(<Home />);
      await screen.findByRole("heading", { name: "Overview" });
      await waitFor(() => {
        expect(screen.queryByText(/email isn't set up/i)).toBeNull();
      });
    });
  });

  describe("Budget alert", () => {
    it("shows no budget alert when no budget is exceeded (seed)", async () => {
      renderApp(<Home />);
      await screen.findByRole("heading", { name: "Overview" });
      await waitFor(() => {
        expect(screen.queryByText(/budget.*exceeded/i)).toBeNull();
      });
    });

    it("shows budget alert when a budget is exceeded", async () => {
      server.use(
        http.get("/api/v1/budgets", () =>
          HttpResponse.json([
            {
              id: "b1",
              scope: "global",
              subject_id: "",
              daily_usd: 1,
              action_on_exceed: "alert_webhook",
              alert_webhook_id: null,
              current_usd: 5,
              exceeded: true,
            },
          ]),
        ),
      );
      renderApp(<Home />);
      await screen.findByRole("heading", { name: "Overview" });
      await waitFor(() => {
        expect(screen.getByText(/budget.*exceeded/i)).toBeInTheDocument();
      });
      const link = screen.getByRole("link", { name: /view budgets/i });
      expect(link).toBeInTheDocument();
      expect(link).toHaveAttribute("href", "/cost");
    });
  });

  // ---- Quick actions ----

  describe("Quick actions", () => {
    it("renders 'Connect a client' link pointing to /clients/connect", async () => {
      renderApp(<Home />);
      await screen.findByRole("heading", { name: "Overview" });
      const link = screen.getByRole("link", { name: /connect a client/i });
      expect(link).toBeInTheDocument();
      expect(link).toHaveAttribute("href", "/clients/connect");
    });

    it("renders 'New service' link pointing to /services", async () => {
      renderApp(<Home />);
      await screen.findByRole("heading", { name: "Overview" });
      const link = screen.getByRole("link", { name: /new service/i });
      expect(link).toBeInTheDocument();
      expect(link).toHaveAttribute("href", "/services");
    });
  });

  // ---- Explainer ----

  describe("How Burrow works explainer", () => {
    it("renders the 'How Burrow works' heading", async () => {
      renderApp(<Home />);
      await screen.findByRole("heading", { name: "Overview" });
      expect(screen.getByText(/how burrow works/i)).toBeInTheDocument();
    });

    it("mentions Clients, Services, and Tunnels in the explainer section", async () => {
      renderApp(<Home />);
      await screen.findByRole("heading", { name: "Overview" });
      const explainer = document.querySelector(".home-explainer");
      expect(explainer).not.toBeNull();
      expect(explainer!.textContent).toMatch(/clients/i);
      expect(explainer!.textContent).toMatch(/services/i);
      expect(explainer!.textContent).toMatch(/tunnels/i);
    });
  });

  describe("Cert alert", () => {
    it("shows no cert alert when all domains are active and far from expiry (seed)", async () => {
      renderApp(<Home />);
      await screen.findByRole("heading", { name: "Overview" });
      await waitFor(() => {
        expect(screen.queryByText(/certificate is expiring/i)).toBeNull();
      });
    });

    it("shows cert alert when a domain has status cert_expiring", async () => {
      server.use(
        http.get("/api/v1/services/:id/domains", () =>
          HttpResponse.json([
            {
              id: "d1",
              service_id: "svc_web01",
              hostname: "x.example.com",
              cert_sha256: "abc123",
              not_before: new Date().toISOString(),
              not_after: new Date().toISOString(),
              created_at: new Date().toISOString(),
              updated_at: new Date().toISOString(),
              status: "cert_expiring",
              status_updated_at: new Date().toISOString(),
            },
          ]),
        ),
      );
      renderApp(<Home />);
      await screen.findByRole("heading", { name: "Overview" });
      await waitFor(() => {
        expect(
          screen.getByText(/custom-domain certificate is expiring/i),
        ).toBeInTheDocument();
      });
      const link = screen.getByRole("link", { name: /review/i });
      expect(link).toBeInTheDocument();
      expect(link.getAttribute("href")).toMatch(/\/services\/svc_/);
    });
  });

  // ---- Role degradation ----

  describe("Role degradation (non-admin)", () => {
    it("non-admin: Clients tile shows '—', explainer + quick actions still render", async () => {
      // Set role to user — admin-only queries (clients, settings, budgets) must not
      // fire. Because their `enabled: isAdmin` guard is false, React Query never
      // issues those fetches, so no MSW handler is invoked for them.
      db.me = { ...db.me, role: "user" };
      renderApp(<Home />);

      // Page heading always renders
      await screen.findByRole("heading", { name: "Overview" });

      // Wait for the strip to settle (services query resolves for non-admin too)
      const strip = await screen.findByRole("list", { name: "Overview" });
      await waitFor(() => {
        const tiles = within(strip).getAllByRole("listitem");
        // Clients tile (first) must show "—" for non-admin
        const clientsTile = tiles[0];
        expect(clientsTile!.querySelector(".value")?.textContent).toBe("—");
      });

      // Quick actions still visible
      expect(screen.getByRole("link", { name: /connect a client/i })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /new service/i })).toBeInTheDocument();

      // Explainer still visible
      expect(screen.getByText(/how burrow works/i)).toBeInTheDocument();

      // SMTP/budget alerts must NOT appear (admin-only queries didn't fire)
      await waitFor(() => {
        expect(screen.queryByText(/email isn't set up/i)).toBeNull();
        expect(screen.queryByText(/budget.*exceeded/i)).toBeNull();
      });
    });
  });

  // ---- Independent loading/error guards ----

  describe("Hard-failure isolation", () => {
    it("services 500: explainer + quick actions still render; page does not go blank", async () => {
      server.use(
        http.get("/api/v1/services", () =>
          HttpResponse.json({ error: "boom" }, { status: 500 }),
        ),
      );
      renderApp(<Home />);

      // Heading must be visible
      await screen.findByRole("heading", { name: "Overview" });

      // Explainer must still render despite services failure
      await waitFor(() => {
        expect(screen.getByText(/how burrow works/i)).toBeInTheDocument();
      });

      // Quick actions must still render
      expect(screen.getByRole("link", { name: /connect a client/i })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /new service/i })).toBeInTheDocument();

      // Counts strip still renders once the error state settles (values default
      // to 0 / "—" from null-safe access — not a crash, not a blank page)
      await waitFor(() => {
        expect(screen.getByRole("list", { name: "Overview" })).toBeInTheDocument();
      });
    });
  });
});
