import { describe, it, expect, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { renderApp } from "@/mocks/test-utils";
import { db, resetDb } from "@/mocks/db";
import CustomDomainsOverview from "@/pages/CustomDomainsOverview";

describe("CustomDomainsOverview", () => {
  afterEach(() => resetDb());

  it("renders hostnames and service names for two services, links to /services/:id/domains", async () => {
    // Seed a second domain for svc_web01 so we have two services with domains.
    db.customDomains = [
      {
        id: "dom_001",
        service_id: "svc_ai001",
        hostname: "ai.example.com",
        cert_sha256: "deadbeef01",
        not_before: "2026-05-01T00:00:00Z",
        not_after: new Date(Date.now() + 90 * 24 * 60 * 60 * 1000).toISOString(),
        created_at: "2026-05-19T00:00:00Z",
        updated_at: "2026-05-19T00:00:00Z",
        status: "active",
        status_updated_at: "2026-05-19T00:00:00Z",
      },
      {
        id: "dom_002",
        service_id: "svc_web01",
        hostname: "web.example.com",
        cert_sha256: "deadbeef02",
        not_before: "2026-05-01T00:00:00Z",
        not_after: new Date(Date.now() + 60 * 24 * 60 * 60 * 1000).toISOString(),
        created_at: "2026-05-19T01:00:00Z",
        updated_at: "2026-05-19T01:00:00Z",
        status: "active",
        status_updated_at: "2026-05-19T01:00:00Z",
      },
    ];

    renderApp(<CustomDomainsOverview />, "/settings/custom-domains");

    // Both hostnames should appear
    expect(await screen.findByText("ai.example.com")).toBeInTheDocument();
    expect(await screen.findByText("web.example.com")).toBeInTheDocument();

    // Both service names should appear
    expect(screen.getByText("ollama")).toBeInTheDocument();
    expect(screen.getByText("web")).toBeInTheDocument();

    // Status labels
    const activeLabels = screen.getAllByText("Active");
    expect(activeLabels.length).toBeGreaterThanOrEqual(2);

    // Service links point to /services/:id/domains
    await waitFor(() => {
      const aiLink = screen.getByRole("link", { name: "ollama" });
      expect(aiLink).toHaveAttribute("href", "/services/svc_ai001/domains");
      const webLink = screen.getByRole("link", { name: "web" });
      expect(webLink).toHaveAttribute("href", "/services/svc_web01/domains");
    });
  });

  it("shows empty state when no domains exist", async () => {
    db.customDomains = [];
    renderApp(<CustomDomainsOverview />, "/settings/custom-domains");
    expect(await screen.findByText(/no custom domains/i)).toBeInTheDocument();
  });
});
