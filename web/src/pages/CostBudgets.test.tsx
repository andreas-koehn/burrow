import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import CostBudgets from "@/pages/CostBudgets";

function mount() {
  return renderApp(<CostBudgets />, "/gateway/cost");
}

describe("Cost & budgets (§4.24)", () => {
  it("renders the verbatim pricing disclosure", async () => {
    mount();
    expect(
      await screen.findByText(
        "Estimates from the pricing table shipped with Burrow v0.4. Operators can edit this table in Settings.",
      ),
    ).toBeInTheDocument();
  });

  it("renders four spend tiles (today/week/month/year)", async () => {
    mount();
    const strip = await screen.findByRole("list", { name: /spend by window/i });
    expect(strip).toBeInTheDocument();
    const tiles = screen.getAllByRole("listitem", { hidden: false });
    const spendTiles = tiles.filter((tile) => tile.querySelector(".label"));
    expect(spendTiles.length).toBeGreaterThanOrEqual(4);
  });

  it("New budget validates daily_usd > 0 and posts /budgets", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /new budget/i }));
    await userEvent.type(await screen.findByLabelText("Subject"), "ak_1");
    const daily = await screen.findByLabelText(/daily usd/i);
    await userEvent.clear(daily);
    await userEvent.type(daily, "0");
    await userEvent.click(screen.getByRole("button", { name: /^create$/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/must be greater than zero/i);
    await userEvent.clear(daily);
    await userEvent.type(daily, "25");
    await userEvent.click(screen.getByRole("button", { name: /^create$/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          String(url).endsWith("/api/v1/budgets")
          && (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true);
    });
  });

  it("keeps Create disabled until a non-global scope has a subject", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    const create = within(dialog).getByRole("button", { name: "Create" });
    await userEvent.type(within(dialog).getByLabelText(/daily usd/i), "25");
    expect(create).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText("Subject"), "   ");
    expect(create).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText("Subject"), "ak_1");
    expect(create).toBeEnabled();
    // Global has no subject, so nothing is missing.
    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Global" }));
    expect(create).toBeEnabled();
    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "User" }));
    expect(create).toBeDisabled();
  });

  it("Export cost report triggers GET /cost/export?format=ndjson", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /export cost report/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url]) =>
          String(url).includes("/api/v1/cost/export?")
          && String(url).includes("format=ndjson"),
        ),
      ).toBe(true);
    });
  });

  it("renders a percentage-sized fill inside each meter, empty at $0 (D-5/L-7)", async () => {
    // Override GET /cost/summary so pct_of_budget = 0 and total_usd = 0 for all windows
    server.use(
      http.get("/api/v1/cost/summary", ({ request }) => {
        const url = new URL(request.url);
        const w = url.searchParams.get("window") ?? "today";
        return HttpResponse.json({
          window: w,
          total_usd: 0,
          tokens_in: 0,
          tokens_out: 0,
          top_consumers: [],
          pct_of_budget: 0,
        });
      }),
    );
    const { container } = renderApp(<CostBudgets />, "/gateway/cost");
    // Wait for the MetricStrip "Spend by window" list to appear
    await screen.findByRole("list", { name: /spend by window/i });
    const bars = container.querySelectorAll(".pct-bar");
    expect(bars.length).toBeGreaterThanOrEqual(4);
    for (const bar of bars) {
      const fill = bar.querySelector(".fill") as HTMLElement | null;
      expect(fill).not.toBeNull();
      expect(fill!.style.width).toBe("0%");
    }
  });

  it("labels the token counts on the spend tiles (L6)", async () => {
    mount();
    const strip = await screen.findByRole("list", { name: /spend by window/i });
    await waitFor(() => {
      expect(strip.textContent).toMatch(/tokens in · .* out/);
    });
    expect(strip.textContent).not.toContain("→");
  });

  it("offers a service picker for the Service scope and hides Subject for Global (U4)", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json([
      { id: "svc-a", name: "alpha", type: "http", access_mode: "api_key", connected: true },
    ])));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });

    // A subject typed for one scope must not survive a scope switch.
    await userEvent.type(within(dialog).getByLabelText("Subject"), "ak_stale");

    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Service" }));
    await userEvent.click(within(dialog).getByLabelText("Subject"));
    expect(await screen.findByRole("option", { name: "alpha" })).toBeInTheDocument();
    // Close the list by toggling the trigger: Escape would also close the dialog.
    await userEvent.click(within(dialog).getByLabelText("Subject"));
    expect(screen.queryByRole("option", { name: "alpha" })).toBeNull();

    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "User" }));
    expect(within(dialog).getByLabelText("Subject")).toHaveValue("");

    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Global" }));
    expect(within(dialog).queryByLabelText("Subject")).toBeNull();
    expect(within(dialog).queryByText("Subject")).toBeNull();
  });

  // Records every POST /budgets body so the tests can assert what was sent.
  function captureBudgetPosts() {
    const bodies: Record<string, unknown>[] = [];
    server.use(http.post("/api/v1/budgets", async ({ request }) => {
      const b = (await request.json()) as Record<string, unknown>;
      bodies.push(b);
      return HttpResponse.json({ id: "bdg_test", current_usd: 0, exceeded: false, alert_webhook_id: null, ...b }, { status: 201 });
    }));
    return bodies;
  }

  it("posts the picked service id as subject_id for the Service scope (U4)", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json([
      { id: "svc-a", name: "alpha", type: "http", access_mode: "api_key", connected: true },
    ])));
    const bodies = captureBudgetPosts();
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Service" }));
    await userEvent.click(within(dialog).getByLabelText("Subject"));
    await userEvent.click(await screen.findByRole("option", { name: "alpha" }));
    await userEvent.type(within(dialog).getByLabelText("Daily USD"), "5");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ scope: "service", subject_id: "svc-a", daily_usd: 5 });
  });

  it("posts an empty subject_id for the Global scope even after a subject was typed (U4)", async () => {
    const bodies = captureBudgetPosts();
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await userEvent.type(within(dialog).getByLabelText("Subject"), "ak_stale");
    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Global" }));
    await userEvent.type(within(dialog).getByLabelText("Daily USD"), "5");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ scope: "global", subject_id: "", daily_usd: 5 });
  });

  it("says so when the service list cannot be loaded instead of showing an empty picker (U4)", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Service" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("Couldn't load services.");
    expect(within(dialog).queryByText("The service this budget applies to.")).toBeNull();
  });
});
