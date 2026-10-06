import { describe, it, expect } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { addDirectProvider, db } from "@/mocks/db";
import GatewayModels from "@/pages/GatewayModels";

function mount() {
  return renderApp(<GatewayModels />, "/gateway/models");
}

function mountAs(role: "admin" | "user") {
  db.me = { ...db.me, role };
  return mount();
}

describe("Models page", () => {
  it("lists models with one column per format", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(
      expect.arrayContaining(["Name", "OpenAI format", "Anthropic format", "Status"]),
    );
    const simple = within(table).getByRole("row", { name: /burrow-simple/ });
    expect(within(simple).getByText("ollama/mistral")).toBeInTheDocument();
    expect(within(simple).getByText("not served")).toBeInTheDocument();
    expect(within(simple).getByText("enabled")).toBeInTheDocument();
    const smart = within(table).getByRole("row", { name: /burrow-intelligence/ });
    expect(within(smart).getByText("zai/glm-5.1")).toBeInTheDocument();
    expect(within(smart).getByText("zai-anthropic/glm-5.1")).toBeInTheDocument();
  });

  it("says in words when a model is switched off", async () => {
    db.aiModels[0]!.enabled = false;
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    expect(within(within(table).getByRole("row", { name: /burrow-simple/ })).getByText("disabled")).toBeInTheDocument();
  });

  it("shows both base URLs as copyable address chips and the connect card", async () => {
    mount();
    expect(await screen.findByRole("button", { name: "Copy URL https://tunnels.example.com/openai/v1" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy URL https://tunnels.example.com/anthropic" })).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Connect a client" })).toBeInTheDocument();
  });

  it("offers only enabled models to the connect card", async () => {
    db.aiModels.find((m) => m.name === "burrow-intelligence")!.enabled = false;
    mount();
    await screen.findByRole("heading", { name: "Connect a client" });
    // The only model served in the Anthropic format is switched off.
    expect(screen.getByText(/no model is served in the Anthropic format yet/i)).toBeInTheDocument();
  });

  it("opens the dialog for a new model and for an existing one", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New model" }));
    await userEvent.click(within(await screen.findByRole("dialog", { name: "New model" })).getByRole("button", { name: "Cancel" }));
    const table = screen.getByRole("table", { name: "Models" });
    await userEvent.click(within(table).getByRole("button", { name: "More actions for burrow-simple" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog", { name: "Edit burrow-simple" });
    expect(within(dialog).getByLabelText("Model name")).toHaveValue("burrow-simple");
  });

  it("asks before deleting and removes the row", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    await userEvent.click(within(table).getByRole("button", { name: "More actions for burrow-simple" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Delete" }));
    const dialog = await screen.findByRole("dialog", { name: /delete burrow-simple/i });
    expect(within(dialog).getByText("Clients that ask for this model will get an error.")).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete model" }));
    await waitFor(() => expect(screen.queryByRole("row", { name: /burrow-simple/ })).toBeNull());
  });

  it("keeps the dialog open and says why when the relay refuses the delete", async () => {
    server.use(http.delete("/api/v1/ai/models/:name", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    await userEvent.click(within(table).getByRole("button", { name: "More actions for burrow-simple" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Delete" }));
    const dialog = await screen.findByRole("dialog", { name: /delete burrow-simple/i });
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete model" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("You don't have permission to delete models.");
  });

  it("is read-only for non-admins", async () => {
    mountAs("user");
    await screen.findByRole("heading", { name: "Models" });
    const table = await screen.findByRole("table", { name: "Models" });
    expect(screen.queryByRole("button", { name: "New model" })).toBeNull();
    expect(within(table).queryByRole("button", { name: /More actions/ })).toBeNull();
  });

  it("guides a first-time operator with no providers", async () => {
    server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json([])),
               http.get("/api/v1/ai/models", () => HttpResponse.json([])));
    mount();
    expect(await screen.findByText("Add a provider first")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Go to Providers" })).toHaveAttribute("href", "/gateway/providers");
  });

  it("with providers but no model: says so and offers to create one", async () => {
    db.aiModels = [];
    mount();
    expect(await screen.findByText("No models yet")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "New model" }).length).toBeGreaterThan(0);
    expect(screen.queryByRole("table", { name: "Models" })).toBeNull();
  });

  it("models failing: an error with a retry", async () => {
    server.use(http.get("/api/v1/ai/models", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't load models: boom");
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("shows each format's chain and marks the target that is serving", async () => {
    // zai's slot is not set on this relay; openrouter's is.
    addDirectProvider("openrouter");
    db.aiModels.push({
      name: "burrow-smart", description: "", enabled: true, fallback_on_rate_limit: false,
      attempt_timeout_s: 60, total_timeout_s: 120,
      targets: [
        { dialect: "openai", provider: "zai", model: "glm-5.1", available: false },
        { dialect: "openai", provider: "openrouter", model: "google/gemini-x", available: false },
      ],
      dialects: ["openai"], serving: {}, created_at: "2026-05-21T00:00:00Z", updated_at: "2026-05-21T00:00:00Z",
    });
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    const row = within(table).getByRole("row", { name: /burrow-smart/ });
    const chain = within(row).getByRole("list", { name: "OpenAI format targets of burrow-smart, in the order they are tried" });
    const items = within(chain).getAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(chain).toHaveTextContent(/zai\/glm-5\.1.*→.*openrouter\/google\/gemini-x/);
    expect(within(items[0]!).getByText("zai/glm-5.1")).toBeInTheDocument();
    // In words anyone can see, not by colour.
    expect(within(items[0]!).getByText("unavailable", { selector: "span.badge" })).toBeVisible();
    expect(within(items[0]!).getByText("unavailable")).not.toHaveClass("visually-hidden");
    expect(within(items[1]!).getByText("openrouter/google/gemini-x")).toBeInTheDocument();
    expect(within(items[1]!).getByText("serving now")).toHaveClass("visually-hidden");
    expect(within(items[1]!).queryByText("unavailable")).toBeNull();
    expect(within(row).queryByText("no target available")).toBeNull();
  });

  it("says so when no target of a format is available", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    // Both targets of burrow-intelligence use slot ZAI, which is not set.
    const smart = within(table).getByRole("row", { name: /burrow-intelligence/ });
    expect(within(smart).getAllByText("no target available", { selector: "span.badge" })).toHaveLength(2);
    // The local model's client is connected.
    const simple = within(table).getByRole("row", { name: /burrow-simple/ });
    expect(within(simple).queryByText("no target available")).toBeNull();
    expect(within(simple).getByText("serving now")).toBeInTheDocument();
  });

  it("a provider the gateway is skipping does not serve", async () => {
    db.aiBreakerOpen.add("ollama");
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    const simple = within(table).getByRole("row", { name: /burrow-simple/ });
    expect(within(simple).getByText("no target available")).toBeInTheDocument();
    expect(within(simple).getByText("unavailable")).toBeInTheDocument();
  });

  it("a disabled model serves nothing: no target is marked, and the row says disabled", async () => {
    db.aiModels[0]!.enabled = false;
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    const simple = within(table).getByRole("row", { name: /burrow-simple/ });
    expect(within(simple).getByText("disabled")).toBeInTheDocument();
    expect(within(simple).getByText("ollama/mistral")).toBeInTheDocument();
    expect(within(simple).queryByText("serving now")).toBeNull();
    // Its target could be tried; it is the model that is off.
    expect(within(simple).queryByText("unavailable")).toBeNull();
    expect(within(simple).queryByText("no target available")).toBeNull();
  });

  it("offers the attempt lookup to admins only", async () => {
    const { unmount } = mount();
    const heading = await screen.findByRole("heading", { name: "Why did a request fall back?" });
    expect(heading).toBeInTheDocument();
    expect(screen.getByText("The id is in the Burrow-Request-Id response header.")).toBeInTheDocument();
    // Below the connect card.
    const connect = screen.getByRole("heading", { name: "Connect a client" });
    expect(connect.compareDocumentPosition(heading) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    unmount();
    mountAs("user");
    await screen.findByRole("table", { name: "Models" });
    expect(screen.queryByRole("heading", { name: "Why did a request fall back?" })).toBeNull();
    expect(screen.queryByLabelText("Request id")).toBeNull();
  });
});
