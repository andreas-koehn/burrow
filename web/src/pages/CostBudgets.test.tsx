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

// The ds Select is a button with a listbox, not a native <select>.
async function chooseOption(dialog: HTMLElement, label: string, option: string) {
  await userEvent.click(within(dialog).getByLabelText(label));
  await userEvent.click(await screen.findByRole("option", { name: option }));
}

/** The paths of the key lists asked for so far. */
function keyListsAsked(): () => string[] {
  const spy = vi.spyOn(globalThis, "fetch");
  // The spy outlives a test; what earlier tests asked for does not count.
  spy.mockClear();
  return () => spy.mock.calls
    .map(([input]) => new URL(input instanceof Request ? input.url : String(input), "http://localhost").pathname)
    .filter((path) => path.endsWith("/api-keys"));
}

async function openDialog() {
  await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
  return screen.findByRole("dialog", { name: "New budget" });
}

describe("Cost & budgets (§4.24)", () => {
  it("renders the verbatim pricing disclosure", async () => {
    mount();
    expect(
      await screen.findByText(
        "Estimates from the price table shipped with Burrow.",
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
    await chooseOption(await openDialog(), "Gateway key", "laptop");
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
    await openDialog();
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
    // The scope it opens on has a subject, and none is chosen yet.
    expect(create).toBeDisabled();
    await chooseOption(dialog, "Gateway key", "laptop");
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

    // A subject chosen for one scope must not survive a scope switch.
    await chooseOption(dialog, "Gateway key", "laptop");

    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Service" }));
    await userEvent.click(within(dialog).getByLabelText("Subject"));
    expect(await screen.findByRole("option", { name: "alpha" })).toBeInTheDocument();
    // Close the list by toggling the trigger: Escape would also close the dialog.
    await userEvent.click(within(dialog).getByLabelText("Subject"));
    expect(screen.queryByRole("option", { name: "alpha" })).toBeNull();

    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "User" }));
    expect(within(dialog).getByLabelText("User")).toHaveTextContent("Select a user…");

    await userEvent.click(within(dialog).getByLabelText("Scope"));
    await userEvent.click(await screen.findByRole("option", { name: "Global" }));
    expect(within(dialog).queryByLabelText("Subject")).toBeNull();
    expect(within(dialog).queryByText("Subject")).toBeNull();
    expect(within(dialog).queryByLabelText("User")).toBeNull();
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

  it("posts an empty subject_id for the Global scope even after a subject was chosen (U4)", async () => {
    const bodies = captureBudgetPosts();
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New budget" }));
    const dialog = await screen.findByRole("dialog", { name: "New budget" });
    await chooseOption(dialog, "Gateway key", "laptop");
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

  it("says why the spend tiles are empty when /cost/summary is refused", async () => {
    server.use(http.get("/api/v1/cost/summary", () => HttpResponse.json({ error: "quotas:read:any required" }, { status: 403 })));
    mount();
    const note = await screen.findByRole("note");
    expect(note).toHaveTextContent("You can't view cost data: the spend totals need the quotas:read:any permission.");
    // The tiles stay, with a dash each.
    const strip = screen.getByRole("list", { name: /spend by window/i });
    expect(within(strip).getAllByText("—").length).toBeGreaterThanOrEqual(4);
  });

  it("shows no such note while cost data loads fine", async () => {
    mount();
    expect(await screen.findByRole("table", { name: "Usage by model" })).toBeInTheDocument();
    expect(screen.queryByRole("note")).toBeNull();
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
    await userEvent.type(within(dialog).getByLabelText("Daily tokens"), "1.5");
    expect(within(dialog).getByText("daily_tokens must be a whole number")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeDisabled();
  });

  it("refuses a typed user id with surrounding spaces instead of trimming it", async () => {
    server.use(http.get("/api/v1/users", () => HttpResponse.json({ error: "users:read required" }, { status: 403 })));
    mount();
    const dialog = await openDialog();
    await chooseOption(dialog, "Scope", "User");
    await userEvent.type(await within(dialog).findByLabelText("User id"), "bur_usr_bob0002 ");
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
    // It opens on the gateway key scope.
    expect(within(dialog).getByText(/revokes the key permanently/i)).toBeInTheDocument();
    await chooseOption(dialog, "Scope", "Model");
    expect(within(dialog).getByText(/only blocks; no key is revoked/i)).toBeInTheDocument();
    await chooseOption(dialog, "Scope", "Global");
    expect(within(dialog).getByText(/no key is revoked/i)).toBeInTheDocument();
    expect(within(dialog).queryByText(/revokes the key permanently/i)).toBeNull();
    await chooseOption(dialog, "Scope", "Gateway key");
    expect(within(dialog).getByText(/revokes the key permanently/i)).toBeInTheDocument();
  });

  it("opens on the gateway key scope", async () => {
    mount();
    const dialog = await openDialog();
    expect(within(dialog).getByLabelText("Scope")).toHaveTextContent("Gateway key");
    expect(within(dialog).getByLabelText("Gateway key")).toHaveTextContent("Select a key…");
    // Nothing is wrong yet: no error before anything was entered.
    expect(within(dialog).queryByRole("alert")).toBeNull();
  });

  it("API key scope: choosing a service loads its keys, and the key's id is what is sent", async () => {
    const asked = keyListsAsked();
    const bodies = captureBudgetPosts();
    db.budgets = [];
    mount();
    const dialog = await openDialog();
    await chooseOption(dialog, "Scope", "API key");
    // The key is chosen once its service is.
    expect(within(dialog).queryByLabelText("Key")).toBeNull();
    expect(asked()).toEqual([]);
    await chooseOption(dialog, "Service", "ollama");
    await userEvent.click(await within(dialog).findByLabelText("Key"));
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(["ci", "prod"]);
    expect(asked()).toEqual(["/api/v1/services/svc_ai001/api-keys"]);
    await userEvent.click(screen.getByRole("option", { name: "prod" }));
    await userEvent.type(within(dialog).getByLabelText("Daily USD"), "4");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ scope: "api_key", subject_id: "sak_prod1", daily_usd: 4 });
  });

  it("API key scope: another service drops the key chosen before, and a service without keys says so", async () => {
    mount();
    const dialog = await openDialog();
    await chooseOption(dialog, "Scope", "API key");
    await chooseOption(dialog, "Service", "ollama");
    await chooseOption(dialog, "Key", "ci");
    await userEvent.type(within(dialog).getByLabelText("Daily USD"), "4");
    const create = within(dialog).getByRole("button", { name: "Create" });
    expect(create).toBeEnabled();
    await chooseOption(dialog, "Service", "web");
    expect(await within(dialog).findByText("This service has no API keys.")).toBeInTheDocument();
    expect(within(dialog).getByLabelText("Key")).toHaveTextContent("Select a key…");
    expect(create).toBeDisabled();
  });

  it("User scope: picks a user by email and sends the id", async () => {
    const bodies = captureBudgetPosts();
    mount();
    const dialog = await openDialog();
    await chooseOption(dialog, "Scope", "User");
    await chooseOption(dialog, "User", "bob@acme.io");
    await userEvent.type(within(dialog).getByLabelText("Daily USD"), "2");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ scope: "user", subject_id: "bur_usr_bob0002", daily_usd: 2 });
  });

  it("User scope: a caller who may not list users types the user's id", async () => {
    server.use(http.get("/api/v1/users", () => HttpResponse.json({ error: "users:read required" }, { status: 403 })));
    const bodies = captureBudgetPosts();
    mount();
    const dialog = await openDialog();
    await chooseOption(dialog, "Scope", "User");
    const field = await within(dialog).findByLabelText("User id");
    expect(field.tagName).toBe("INPUT");
    expect(within(dialog).queryByLabelText("User")).toBeNull();
    // The reason is help text, not an error.
    expect(field).toHaveAccessibleDescription("The users can't be listed here. Type the user's id.");
    expect(within(dialog).queryByRole("alert")).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Create" })).toHaveAccessibleDescription("Type the user's id.");
    await userEvent.type(field, "bur_usr_bob0002");
    await userEvent.type(within(dialog).getByLabelText("Daily USD"), "2");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ scope: "user", subject_id: "bur_usr_bob0002" });
  });

  it("User scope: with more users than one answer holds, the id is typed", async () => {
    server.use(http.get("/api/v1/users", () => HttpResponse.json({ users: db.users.slice(0, 2), total: 250 })));
    mount();
    const dialog = await openDialog();
    await chooseOption(dialog, "Scope", "User");
    // They can be listed, only not all of them: the help says which it is.
    expect(await within(dialog).findByLabelText("User id")).toHaveAccessibleDescription("Too many users to list. Type the user's id.");
    expect(within(dialog).queryByText(/can't be listed here/)).toBeNull();
  });

  it("says beside Create what is still missing: the subject of the scope first, then a limit", async () => {
    mount();
    const dialog = await openDialog();
    const create = within(dialog).getByRole("button", { name: "Create" });
    // A limit alone is not enough on the scope the dialog opens on.
    await userEvent.type(within(dialog).getByLabelText("Daily USD"), "5");
    expect(create).toBeDisabled();
    expect(create).toHaveAccessibleDescription("Choose a gateway key.");
    expect(within(dialog).getByText("Choose a gateway key.")).toBeVisible();

    await chooseOption(dialog, "Scope", "API key");
    expect(create).toHaveAccessibleDescription("Choose a service and a key.");
    await chooseOption(dialog, "Service", "ollama");
    expect(create).toHaveAccessibleDescription("Choose a service and a key.");
    await chooseOption(dialog, "Key", "ci");
    expect(create).toBeEnabled();
    expect(create).not.toHaveAttribute("aria-describedby");

    await chooseOption(dialog, "Scope", "Model");
    expect(create).toHaveAccessibleDescription("Choose a model.");
    await chooseOption(dialog, "Model", "Other address…");
    expect(create).toHaveAccessibleDescription("Enter a model address.");
    await chooseOption(dialog, "Scope", "Service");
    expect(create).toHaveAccessibleDescription("Choose a service.");
    await chooseOption(dialog, "Scope", "User");
    expect(create).toHaveAccessibleDescription("Choose a user.");

    // Nothing is missing for the global scope but the limit, once it is taken out.
    await chooseOption(dialog, "Scope", "Global");
    expect(create).toBeEnabled();
    await userEvent.clear(within(dialog).getByLabelText("Daily USD"));
    expect(create).toBeDisabled();
    expect(create).toHaveAccessibleDescription("Set a daily amount in USD, in tokens, or both.");
    // Said once, beside the button.
    expect(within(dialog).getAllByText("Set a daily amount in USD, in tokens, or both.")).toHaveLength(1);
  });

  it("the budgets table names an API key, and says when the key is gone", async () => {
    db.budgets.push({ id: "bdg_gone", scope: "api_key", subject_id: "sak_gone9", daily_usd: 1, daily_tokens: 0, action_on_exceed: "alert_webhook", alert_webhook_id: null, current_usd: 0, current_tokens: 0, exceeded: false });
    mount();
    const table = await screen.findByRole("table", { name: "Budgets" });
    // bdg_ci is on sak_ci01, the key "ci" of the service ollama.
    expect(await within(table).findByText("ci")).toBeInTheDocument();
    expect(within(table).queryByText("sak_ci01")).toBeNull();
    expect(await within(table).findByText("sak_gone9 (deleted)")).toBeInTheDocument();
  });

  it("the budgets table shows the id alone while a key list could not be read", async () => {
    server.use(http.get("/api/v1/services/svc_web01/api-keys", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    db.budgets.push({ id: "bdg_gone", scope: "api_key", subject_id: "sak_gone9", daily_usd: 1, daily_tokens: 0, action_on_exceed: "alert_webhook", alert_webhook_id: null, current_usd: 0, current_tokens: 0, exceeded: false });
    mount();
    const table = await screen.findByRole("table", { name: "Budgets" });
    expect(await within(table).findByText("ci")).toBeInTheDocument();
    // The key may be one of the service that could not be read: not called deleted.
    expect(within(table).getByText("sak_gone9")).toBeInTheDocument();
    expect(within(table).queryByText(/\(deleted\)/)).toBeNull();
  });

  it("asks for no key list while no budget is on an API key", async () => {
    const asked = keyListsAsked();
    db.budgets = db.budgets.filter((b) => b.scope !== "api_key");
    mount();
    expect(await within(await screen.findByRole("table", { name: "Budgets" })).findByText("burrow-simple")).toBeInTheDocument();
    expect(asked()).toEqual([]);
  });
});
