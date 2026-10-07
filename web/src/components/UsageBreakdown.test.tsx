import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import UsageBreakdown from "@/components/UsageBreakdown";

function renderBreakdown() {
  return renderApp(<UsageBreakdown window="today" />, "/gateway/cost");
}

describe("UsageBreakdown", () => {
  it("switches the grouping and shows key names instead of ids", async () => {
    renderBreakdown();
    const table = await screen.findByRole("table", { name: "Usage by model" });
    expect(within(table).getByText("burrow-simple")).toBeInTheDocument();
    expect(within(table).getByRole("columnheader", { name: "Tokens in" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: "Key" }));
    const byKey = await screen.findByRole("table", { name: "Usage by key" });
    expect(await within(byKey).findByText("laptop")).toBeInTheDocument();
    expect(within(byKey).queryByText("gk_laptop1")).toBeNull();
    // A key the caller cannot see or that was deleted: shortened id, never blank.
    expect(within(byKey).getByText("deleted key (gk_gone0…)")).toBeInTheDocument();
    expect(within(byKey).getByText("no gateway key")).toBeInTheDocument();
  });

  it("formats cost with two decimals and counts with grouping", async () => {
    server.use(http.get("/api/v1/cost/summary", () => HttpResponse.json({
      window: "today", total_usd: 1, tokens_in: 1, tokens_out: 1, top_consumers: [], group_by: "model",
      groups: [{ key: "burrow-simple", requests: 1234, tokens_in: 1250000, tokens_out: 5, usd: 12.5 }],
    })));
    renderBreakdown();
    const row = await screen.findByRole("row", { name: /burrow-simple/ });
    expect(within(row).getByText("1,250,000")).toBeInTheDocument();
    expect(within(row).getByText("1,234")).toBeInTheDocument();
    expect(within(row).getByText("$12.50")).toBeInTheDocument();
  });

  it("labels flat-rate providers instead of showing $0.00", async () => {
    server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json([
      { slug: "zai", name: "zai", kind: "direct", api_format: "openai", service_id: "s", billing: "flat" },
      { slug: "ollama", name: "ollama", kind: "tunnel", api_format: "openai", service_id: "t" },
    ])));
    renderBreakdown();
    await userEvent.click(await screen.findByRole("radio", { name: "Provider" }));
    const table = await screen.findByRole("table", { name: "Usage by provider" });
    const zai = await within(table).findByRole("row", { name: /zai/ });
    expect(await within(zai).findByText("flat rate")).toBeInTheDocument();
    expect(within(zai).queryByText("$0.00")).toBeNull();
    expect(within(within(table).getByRole("row", { name: /ollama/ })).getByText("$1.23")).toBeInTheDocument();
  });

  it("names traffic that did not come through a model", async () => {
    renderBreakdown();
    const table = await screen.findByRole("table", { name: "Usage by model" });
    expect(within(table).getByText("not routed by model")).toBeInTheDocument();
  });

  it("shows an empty state, not an empty table", async () => {
    server.use(http.get("/api/v1/cost/summary", () => HttpResponse.json({ window: "today", total_usd: 0, tokens_in: 0, tokens_out: 0, top_consumers: [], group_by: "model", groups: [] })));
    renderBreakdown();
    expect(await screen.findByText("No usage in this period")).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("says not permitted on a 403 instead of spinning", async () => {
    server.use(http.get("/api/v1/cost/summary", () => HttpResponse.json({ error: "quotas:read:any required" }, { status: 403 })));
    renderBreakdown();
    expect(await screen.findByText("You can't view cost data")).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows other failures in an alert", async () => {
    server.use(http.get("/api/v1/cost/summary", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    renderBreakdown();
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
  });
});
