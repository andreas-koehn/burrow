import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
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

  it("New budget posts /budgets and shows the server's 400 verbatim", async () => {
    server.use(http.post("/api/v1/budgets", () => HttpResponse.json({ error: "subject_id too long (max 256 chars)" }, { status: 400 })));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /new budget/i }));
    await userEvent.type(await screen.findByLabelText("Subject"), "ak_1");
    const create = screen.getByRole("button", { name: /^create$/i });
    // Neither limit set: the form says why and does not send.
    expect(create).toBeDisabled();
    expect(screen.getByText("Set a daily amount in USD, in tokens, or both.")).toBeInTheDocument();
    await userEvent.type(await screen.findByLabelText(/daily usd/i), "25");
    await userEvent.click(create);
    expect(await screen.findByRole("alert")).toHaveTextContent("subject_id too long (max 256 chars)");
  });

  it("mirrors the server's rule for negative limits", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /new budget/i }));
    await userEvent.type(await screen.findByLabelText("Subject"), "ak_1");
    await userEvent.type(screen.getByLabelText(/daily usd/i), "-1");
    expect(screen.getByText("daily_usd must not be negative")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^create$/i })).toBeDisabled();
    await userEvent.clear(screen.getByLabelText(/daily usd/i));
    await userEvent.type(screen.getByLabelText("Daily tokens"), "-5");
    expect(screen.getByText("daily_tokens must not be negative")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^create$/i })).toBeDisabled();
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
    // Surrounding spaces are refused, not trimmed (the server does the same).
    expect(create).toBeDisabled();
    await userEvent.clear(within(dialog).getByLabelText("Subject"));
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

  async function chooseOption(dialog: HTMLElement, label: string, option: string) {
    await userEvent.click(within(dialog).getByLabelText(label));
    await userEvent.click(await screen.findByRole("option", { name: option }));
  }

  it("creates a token budget for a gateway key picked by name", async () => {
    const bodies = captureBudgetPosts();
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await chooseOption(dialog, "Scope", "Gateway key");
    await chooseOption(dialog, "Gateway key", "laptop");
    await userEvent.type(within(dialog).getByLabelText("Daily tokens"), "500000");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ scope: "gateway_key", subject_id: "gk_laptop1", daily_tokens: 500000, daily_usd: 0 });
  });

  it("picks a model by name or takes a provider address", async () => {
    const bodies = captureBudgetPosts();
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await chooseOption(dialog, "Scope", "Model");
    const create = within(dialog).getByRole("button", { name: "Create" });
    await chooseOption(dialog, "Model", "burrow-simple");
    expect(create).toBeDisabled();
    expect(within(dialog).getByText("Set a daily amount in USD, in tokens, or both.")).toBeInTheDocument();
    await chooseOption(dialog, "Model", "Other address…");
    await userEvent.type(within(dialog).getByLabelText("Model address"), "zai/glm-5.1");
    await userEvent.type(within(dialog).getByLabelText("Daily USD"), "3");
    await userEvent.click(create);
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ scope: "model", subject_id: "zai/glm-5.1", daily_usd: 3 });
  });

  it("explains the actions in words, disable_key as permanent", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await chooseOption(dialog, "Scope", "Gateway key");
    await chooseOption(dialog, "Action on exceed", "Disable key");
    expect(within(dialog).getByText(/revokes the key permanently/i)).toBeInTheDocument();
    await chooseOption(dialog, "Action on exceed", "Throttle to zero");
    expect(within(dialog).getByText(/refused until midnight UTC/i)).toBeInTheDocument();
  });

  it("shows token usage against a token budget and marks exceeded in words", async () => {
    db.budgets.push({ id: "bdg_over", scope: "gateway_key", subject_id: "gk_laptop1", daily_usd: 2, daily_tokens: 100, action_on_exceed: "disable_key", alert_webhook_id: null, current_usd: 2.5, current_tokens: 150, exceeded: true });
    mount();
    const table = await screen.findByRole("table", { name: "Budgets" });
    expect(await within(table).findByText("250,000 / 1,000,000 tokens")).toBeInTheDocument();
    const over = within(table).getByRole("row", { name: /laptop/ });
    expect(within(over).getByText("$2.50 / $2.00")).toBeInTheDocument();
    expect(within(over).getByText("150 / 100 tokens")).toBeInTheDocument();
    expect(within(over).getByText("Exceeded")).toBeInTheDocument();
  });

  it("keeps tiles and usage when the budgets list is refused, and offers no write controls", async () => {
    server.use(http.get("/api/v1/budgets", () => HttpResponse.json({ error: "admin required" }, { status: 403 })));
    mount();
    expect(await screen.findByText("You can't view budgets")).toBeInTheDocument();
    expect(screen.getByRole("list", { name: /spend by window/i })).toBeInTheDocument();
    expect(await screen.findByRole("table", { name: "Usage by model" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New budget" })).toBeNull();
  });

  it("shows a non-admin the cost view without budgets or write controls", async () => {
    db.me = { ...db.me, role: "user" };
    mount();
    expect(await screen.findByText("You can't view budgets")).toBeInTheDocument();
    expect(screen.getByRole("list", { name: /spend by window/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New budget" })).toBeNull();
  });

  it("retries a failed budgets load", async () => {
    let fail = true;
    server.use(http.get("/api/v1/budgets", () => fail ? HttpResponse.json({ error: "boom" }, { status: 500 }) : HttpResponse.json([])));
    mount();
    expect(await screen.findByText("boom")).toBeInTheDocument();
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table", { name: "Budgets" })).toBeInTheDocument();
  });

  it("labels keys in the picker: revoked, and same names told apart by prefix", async () => {
    db.aiGatewayKeys.push(
      { id: "gk_old", name: "old", key_prefix: "bgw_Old1", user_id: db.me.id, allowed_models: [], last_used: null, created_at: "2026-05-10T08:00:00Z", revoked_at: "2026-05-11T08:00:00Z" },
      { id: "gk_l2", name: "laptop", key_prefix: "bgw_Lap2", user_id: db.me.id, allowed_models: [], last_used: null, created_at: "2026-05-10T08:00:00Z", revoked_at: null },
    );
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Gateway key" }));
    await userEvent.click(within(dialog).getByLabelText("Gateway key"));
    expect(await screen.findByRole("option", { name: "old (revoked)" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "laptop (bgw_Ab3d)" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "laptop (bgw_Lap2)" })).toBeInTheDocument();
  });

  it("labels a budget on an unknown key by its short id", async () => {
    db.budgets.push({ id: "bdg_x", scope: "gateway_key", subject_id: "gk_other_user_key", daily_usd: 1, daily_tokens: 0, action_on_exceed: "throttle_zero", alert_webhook_id: null, current_usd: 0, current_tokens: 0, exceeded: false });
    mount();
    const table = await screen.findByRole("table", { name: "Budgets" });
    expect(await within(table).findByText("key gk_other…")).toBeInTheDocument();
  });

  async function openModelAddress() {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Model" }));
    await userEvent.click(within(dialog).getByLabelText("Model"));
    await userEvent.click(await screen.findByRole("option", { name: "Other address…" }));
    await waitFor(() => expect(within(dialog).getByLabelText("Model address")).toHaveFocus());
    await userEvent.type(within(dialog).getByLabelText("Daily USD"), "3");
    return dialog;
  }

  it.each(["zai", "/x", "x/", " zai/x"])("refuses the model address %j and says why", async (addr) => {
    const dialog = await openModelAddress();
    const field = within(dialog).getByLabelText("Model address");
    await userEvent.type(field, addr);
    expect(within(dialog).getByText(/Use the form <provider>\/<model>/)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeDisabled();
    expect(field).toHaveAttribute("aria-describedby", "budget-model-addr-desc");
  });

  it("refuses a fractional token count", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await userEvent.type(within(dialog).getByLabelText("Subject"), "ak_1");
    await userEvent.type(within(dialog).getByLabelText("Daily tokens"), "1.5");
    expect(within(dialog).getByText("daily_tokens must be a whole number")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeDisabled();
  });

  it("refuses a subject with surrounding spaces instead of trimming it", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await userEvent.type(within(dialog).getByLabelText("Subject"), "ak_1 ");
    await userEvent.type(within(dialog).getByLabelText("Daily USD"), "5");
    expect(within(dialog).getByText("Remove the spaces before or after the ID.")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeDisabled();
  });

  it("words disable_key by scope and wires the help to the control", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await chooseOption(dialog, "Action on exceed", "Disable key");
    expect(within(dialog).getByLabelText("Action on exceed")).toHaveAttribute("aria-describedby", "budget-action-help");
    // default scope is API key
    expect(within(dialog).getByText(/revokes the key permanently/i)).toBeInTheDocument();
    await chooseOption(dialog, "Scope", "Model");
    expect(within(dialog).getByText(/only blocks; no key is revoked/i)).toBeInTheDocument();
    await chooseOption(dialog, "Scope", "Global");
    expect(within(dialog).getByText(/no key is revoked/i)).toBeInTheDocument();
    expect(within(dialog).queryByText(/revokes the key permanently/i)).toBeNull();
    await chooseOption(dialog, "Scope", "Gateway key");
    expect(within(dialog).getByText(/revokes the key permanently/i)).toBeInTheDocument();
  });
});
