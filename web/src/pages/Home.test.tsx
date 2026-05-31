import { describe, it, expect } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import Home from "./Home";

describe("Home (Overview)", () => {
  it("renders the Overview heading", async () => {
    renderApp(<Home />);
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
  });

  it("renders a 'Last 24 hours' strip with AI-cost tile showing $1.23 and 12,000 → 8,000", async () => {
    renderApp(<Home />);
    const strip = await screen.findByRole("list", { name: "Last 24 hours" });
    const { getAllByRole } = within(strip);
    await waitFor(() => {
      const tiles = getAllByRole("listitem");
      // AI cost tile must be present and show seed values
      const costTile = tiles.find((t) => t.querySelector(".label")?.textContent === "AI cost (24h)");
      expect(costTile).toBeTruthy();
      expect(costTile!.querySelector(".value")?.textContent).toBe("$1.23");
      expect(costTile!.querySelector(".sub")?.textContent).toBe("12,000 → 8,000");
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
});
