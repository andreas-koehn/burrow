import { describe, it, expect } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import AiEndpoints from "@/pages/AiEndpoints";

function mount() {
  return renderApp(<AiEndpoints />, "/ai/endpoints");
}

describe("AI endpoints page (§4.19)", () => {
  it("renders the four-tile metric strip with the spec tooltip", async () => {
    mount();
    const strip = await screen.findByRole("list", { name: "AI endpoint metrics" });
    const { getAllByRole } = within(strip);
    const tiles = getAllByRole("listitem");
    expect(tiles).toHaveLength(4);
    // The four tiles, in spec order — DS MetricTile surfaces label in .label span.
    expect(tiles[0].querySelector(".label")?.textContent).toBe("Requests (24h)");
    expect(tiles[1].querySelector(".label")?.textContent).toBe("Tokens in/out (24h)");
    expect(tiles[2].querySelector(".label")?.textContent).toBe("Cost estimate (24h)");
    expect(tiles[3].querySelector(".label")?.textContent).toBe("Cache hit ratio (24h)");
    const cost = tiles[2];
    // DS MetricTile exposes tooltip via the title attribute.
    expect(cost).toHaveAttribute(
      "title",
      "Estimates from the bundled pricing table — operator-overridable.",
    );
  });

  it("renders one row per AI endpoint with name, mono alias, backend, key count, requests, cache, latency, status", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /ai endpoints/i });
    // Anchor by the unique mono alias text (the name "ollama" also appears in
    // the backend badge, so it isn't a safe anchor on its own).
    const alias = within(table).getByText("fast → llama3.1:8b");
    expect(alias.className).toContain("mono");
    const ollama = alias.closest("tr")!;
    // Name cell value.
    expect(within(ollama).getByText("ollama", { selector: "td.col-name > div" }))
      .toBeInTheDocument();
    // Backend type badge.
    expect(within(ollama).getByText("ollama", { selector: "span.badge" }))
      .toBeInTheDocument();
    // Spec metrics — key count, requests/24h, cache hits with mono ratio,
    // latency p95.
    expect(within(ollama).getByText("2")).toBeInTheDocument(); // api_key_count fixture
    expect(within(ollama).getByText("1,024")).toBeInTheDocument(); // requests_24h
    expect(within(ollama).getByText("200")).toBeInTheDocument(); // cache_hits_24h
    expect(within(ollama).getByText("1,200 ms")).toBeInTheDocument(); // latency_p95_ms
    expect(within(ollama).getByText(/connected/i)).toBeInTheDocument();
  });

  it("⋯ menu offers Inspect / Keys / Access settings / Cost / Disable", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /ai endpoints/i });
    const alias = within(table).getByText("fast → llama3.1:8b");
    const ollama = alias.closest("tr")!;
    const more = within(ollama).getByRole("button", { name: /more actions/i });
    await userEvent.click(more);
    const menu = await screen.findByRole("menu");
    const labels = within(menu).getAllByRole("menuitem").map((n) => n.textContent);
    expect(labels).toEqual(["Inspect", "Keys", "Access settings", "Cost", "Disable"]);
  });

  it("renders the verbatim empty state when there are no AI endpoints", async () => {
    db.services = db.services.filter((s) => s.access_mode !== "api_key");
    mount();
    expect(await screen.findByRole("heading", { name: "No AI endpoints yet" })).toBeInTheDocument();
    expect(
      await screen.findByText(
        "Create a service with API-key access mode and OpenAI-compatible upstream.",
      ),
    ).toBeInTheDocument();
  });

  it("does not show cost-summary tokens when there are no AI endpoints (L-6)", async () => {
    server.use(
      http.get("/api/v1/ai/endpoints", () => HttpResponse.json([])),
    );
    mount();
    await screen.findByText(/no ai endpoints yet/i);
    const tokensTile = screen.getByText(/tokens in\/out/i).closest(".metric-tile")!;
    expect(within(tokensTile).getByText("—")).toBeInTheDocument();
  });

  it("shows an error notice with Retry on failure and recovers when clicked", async () => {
    server.use(
      http.get("/api/v1/ai/endpoints", () =>
        HttpResponse.json({ error: "boom" }, { status: 500 }),
      ),
    );
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent(/boom|couldn't load/i);
    const retry = screen.getByRole("button", { name: /retry/i });
    server.resetHandlers();
    await userEvent.click(retry);
    await waitFor(() =>
      expect(screen.getByRole("table", { name: /ai endpoints/i })).toBeInTheDocument(),
    );
  });
});
