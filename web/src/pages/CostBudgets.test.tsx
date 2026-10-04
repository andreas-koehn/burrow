import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import CostBudgets from "@/pages/CostBudgets";

function mount() {
  return renderApp(<CostBudgets />, "/cost");
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
    const { container } = renderApp(<CostBudgets />, "/cost");
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

    // The DS Select trigger is a <button> tied to its <label> by id only;
    // Testing Library resolves neither getByLabelText nor a role name for
    // it, so reach it by the id the label points at.
    const trigger = (id: string) => {
      const el = dialog.querySelector<HTMLButtonElement>(`button#${id}`);
      if (!el) throw new Error(`no select trigger #${id}`);
      expect(dialog.querySelector(`label[for="${id}"]`)).not.toBeNull();
      return el;
    };
    // A subject typed for one scope must not survive a scope switch.
    await userEvent.type(within(dialog).getByLabelText("Subject"), "ak_stale");

    await userEvent.click(trigger("budget-scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Service" }));
    await userEvent.click(trigger("budget-subject"));
    expect(await screen.findByRole("option", { name: "alpha" })).toBeInTheDocument();
    // Close the list by toggling the trigger: Escape would also close the dialog.
    await userEvent.click(trigger("budget-subject"));
    expect(screen.queryByRole("option", { name: "alpha" })).toBeNull();

    await userEvent.click(trigger("budget-scope"));
    await userEvent.click(await screen.findByRole("option", { name: "User" }));
    expect(within(dialog).getByLabelText("Subject")).toHaveValue("");

    await userEvent.click(trigger("budget-scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Global" }));
    expect(within(dialog).queryByText("Subject")).toBeNull();
    expect(dialog.querySelector("#budget-subject")).toBeNull();
  });
});
