import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { Route, Routes, useLocation } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { addDirectProvider, db } from "@/mocks/db";
import GatewayModels from "@/pages/GatewayModels";
import { USER_PERMISSIONS, ALL_PERMISSIONS } from "@/auth/permissions";

function mount() {
  return renderApp(<GatewayModels />, "/gateway/models");
}

function Where() {
  const l = useLocation();
  return <div data-testid="location">{l.pathname + l.search}</div>;
}

function mountAt(route: string) {
  return renderApp(
    <>
      <Routes><Route path="/gateway/models" element={<GatewayModels />} /></Routes>
      <Where />
    </>,
    route,
  );
}

// The provider chosen in the first entry of a format's list.
const providerOf = (dialog: HTMLElement, format: "OpenAI" | "Anthropic") =>
  within(within(dialog).getByRole("group", { name: `${format} format, target 1` })).getByLabelText("Provider");

function mountAs(role: "admin" | "user") {
  db.me = { ...db.me, role, permissions: role === "admin" ? [...ALL_PERMISSIONS] : [...USER_PERMISSIONS] };
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

  it("shows per format whether a model is native, translated or not served", async () => {
    db.aiModels.find((m) => m.name === "burrow-simple")!.translate = true;
    mount();
    const row = within(await screen.findByRole("table", { name: "Models" })).getByRole("row", { name: /burrow-simple/ });
    expect(within(row).getByText("ollama/mistral")).toBeInTheDocument();
    // In words, as a badge, with the direction next to it.
    expect(within(row).getByText("translated", { selector: "span.badge" })).toBeVisible();
    expect(within(row).getByText("Anthropic Messages → Chat Completions")).toBeInTheDocument();
    expect(within(row).queryByText("not served")).toBeNull();
    // ollama does not offer the Responses API: Codex is translated too.
    expect(within(row).getByText("Responses API: translated (OpenAI Responses → Chat Completions)")).toBeInTheDocument();
  });

  it("a format whose stored target cannot serve any more says it is translated, not only that the target is unavailable", async () => {
    // The Anthropic target's provider speaks the OpenAI format now.
    const simple = db.aiModels.find((m) => m.name === "burrow-simple")!;
    simple.translate = true;
    simple.targets = [...simple.targets, { dialect: "anthropic", provider: "zai", model: "glm-5.1", available: false }];
    simple.dialects = ["anthropic", "openai"];
    mount();
    const row = within(await screen.findByRole("table", { name: "Models" })).getByRole("row", { name: /burrow-simple/ });
    expect(within(row).getByText("translated", { selector: "span.badge" })).toBeVisible();
    expect(within(row).getByText("Anthropic Messages → Chat Completions")).toBeInTheDocument();
    // The stored target stays visible, as unavailable.
    const chain = within(row).getByRole("list", { name: "Anthropic format targets of burrow-simple, in the order they are tried" });
    expect(within(chain).getByText("unavailable")).toBeInTheDocument();
    expect(within(row).queryByText("no target available")).toBeNull();
  });

  it("names every pair a translated request can go through", async () => {
    // zai (OpenAI format, no Responses API) and zai-anthropic.
    db.aiModels.find((m) => m.name === "burrow-intelligence")!.translate = true;
    mount();
    const row = within(await screen.findByRole("table", { name: "Models" })).getByRole("row", { name: /burrow-intelligence/ });
    expect(within(row).getByText(
      "Responses API: translated (OpenAI Responses → Chat Completions or OpenAI Responses → Anthropic Messages)",
    )).toBeInTheDocument();
  });

  it("says how the Responses API is served", async () => {
    db.aiProviders.find((p) => p.slug === "zai")!.supports_responses = true;
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    expect(within(within(table).getByRole("row", { name: /burrow-intelligence/ })).getByText("Responses API: served natively")).toBeInTheDocument();
    // Translation is off and ollama does not offer it.
    expect(within(within(table).getByRole("row", { name: /burrow-simple/ })).getByText("Responses API: not served")).toBeInTheDocument();
  });

  it("offers a translated model to the connect card, marked", async () => {
    db.aiModels.find((m) => m.name === "burrow-simple")!.translate = true;
    mount();
    await screen.findByRole("heading", { name: "Connect a client" });
    const offered = await screen.findByRole("list", { name: "Models for Claude Code" });
    expect(within(offered).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["burrow-intelligence", "burrow-simpletranslated"]);
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

  it("?new=1&provider=<slug> opens the dialog with that provider chosen for the format it speaks", async () => {
    mountAt("/gateway/models?new=1&provider=ollama");
    const dialog = await screen.findByRole("dialog", { name: "New model" });
    expect(providerOf(dialog, "OpenAI")).toHaveTextContent("ollama");
    expect(providerOf(dialog, "Anthropic")).toHaveTextContent("Not served");
    // The model itself is still the reader's to choose.
    expect(within(dialog).getByLabelText("Model name")).toHaveValue("");
    expect(within(within(dialog).getByRole("group", { name: "OpenAI format, target 1" })).getByLabelText(/^Target model/)).toBeInTheDocument();
    // The link stays while the dialog is open.
    expect(screen.getByTestId("location").textContent).toBe("/gateway/models?new=1&provider=ollama");
  });

  it("?new=1&provider=<slug>: a provider that speaks the Anthropic format is chosen there", async () => {
    mountAt("/gateway/models?new=1&provider=zai-anthropic");
    const dialog = await screen.findByRole("dialog", { name: "New model" });
    expect(providerOf(dialog, "Anthropic")).toHaveTextContent("zai-anthropic");
    expect(providerOf(dialog, "OpenAI")).toHaveTextContent("Not served");
  });

  it("?new=1 alone, or with a provider that is not there, opens the empty dialog", async () => {
    const first = mountAt("/gateway/models?new=1");
    let dialog = await screen.findByRole("dialog", { name: "New model" });
    expect(providerOf(dialog, "OpenAI")).toHaveTextContent("Not served");
    first.unmount();
    mountAt("/gateway/models?new=1&provider=gone");
    dialog = await screen.findByRole("dialog", { name: "New model" });
    expect(providerOf(dialog, "OpenAI")).toHaveTextContent("Not served");
    expect(providerOf(dialog, "Anthropic")).toHaveTextContent("Not served");
  });

  it.each(["Cancel", "Close dialog"])("?new=1&provider=<slug>: closing with %s takes both parameters out of the address", async (name) => {
    mountAt("/gateway/models?new=1&provider=ollama");
    const dialog = await screen.findByRole("dialog", { name: "New model" });
    await userEvent.click(within(dialog).getByRole("button", { name }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByTestId("location").textContent).toBe("/gateway/models");
    // The button opens an empty dialog afterwards: the link is used up.
    await userEvent.click(screen.getByRole("button", { name: "New model" }));
    expect(providerOf(await screen.findByRole("dialog", { name: "New model" }), "OpenAI")).toHaveTextContent("Not served");
  });

  it("?new=1&provider=<slug>: creating the model takes the parameters out as well", async () => {
    mountAt("/gateway/models?new=1&provider=ollama");
    const dialog = await screen.findByRole("dialog", { name: "New model" });
    await userEvent.type(within(dialog).getByLabelText("Model name"), "burrow-local");
    const row = within(dialog).getByRole("group", { name: "OpenAI format, target 1" });
    const target = await within(row).findByLabelText(/^Target model/);
    if (target.tagName === "INPUT") await userEvent.type(target, "llama3.2");
    else {
      await userEvent.click(target);
      await userEvent.click((await screen.findAllByRole("option"))[0]!);
    }
    await userEvent.click(within(dialog).getByRole("button", { name: /^(Create|Create model|Save)$/ }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(db.aiModels.at(-1)).toMatchObject({ name: "burrow-local" });
    expect(db.aiModels.at(-1)!.targets[0]).toMatchObject({ dialect: "openai", provider: "ollama" });
    expect(screen.getByTestId("location").textContent).toBe("/gateway/models");
  });

  it("?new=1 opens nothing for someone who may not write models", async () => {
    db.me = { ...db.me, role: "user", permissions: [...USER_PERMISSIONS] };
    mountAt("/gateway/models?new=1&provider=ollama");
    await screen.findByRole("table", { name: "Models" });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  // The command palette links a model by its name; jsdom has no layout, so the scroll is observed.
  describe("?focus=<name>", () => {
    const scrolled: Element[] = [];
    const had = Object.getOwnPropertyDescriptor(Element.prototype, "scrollIntoView");
    beforeEach(() => {
      scrolled.length = 0;
      Object.defineProperty(Element.prototype, "scrollIntoView", {
        configurable: true, writable: true, value(this: Element) { scrolled.push(this); },
      });
    });
    afterEach(() => {
      if (had) Object.defineProperty(Element.prototype, "scrollIntoView", had);
      else delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
    });
    const rowOf = (table: HTMLElement, name: string) => within(table).getByText(name).closest("tr")!;

    it("scrolls to the row of that model and marks it", async () => {
      mountAt("/gateway/models?focus=burrow-intelligence");
      const table = await screen.findByRole("table", { name: "Models" });
      const row = rowOf(table, "burrow-intelligence");
      await waitFor(() => expect(scrolled).toEqual([row]));
      expect(row).toHaveAttribute("aria-current", "true");
      expect(table.querySelectorAll("[aria-current]")).toHaveLength(1);
      // The link stays what it was: a reload shows the same row.
      expect(screen.getByTestId("location")).toHaveTextContent("/gateway/models?focus=burrow-intelligence");
      // Once: a refetch of the list does not pull the page back to the row.
      await userEvent.click(screen.getByRole("button", { name: "New model" }));
      await userEvent.keyboard("{Escape}");
      expect(scrolled).toHaveLength(1);
    });

    it("takes a name with characters a link escapes", async () => {
      db.aiModels.push({ ...db.aiModels[0]!, name: "team a/fast&cheap" });
      mountAt(`/gateway/models?focus=${encodeURIComponent("team a/fast&cheap")}`);
      const table = await screen.findByRole("table", { name: "Models" });
      await waitFor(() => expect(scrolled).toEqual([rowOf(table, "team a/fast&cheap")]));
    });

    it("marks nothing for a model that is not there, or without the parameter", async () => {
      for (const route of ["/gateway/models?focus=gone", "/gateway/models"]) {
        const view = mountAt(route);
        const table = await screen.findByRole("table", { name: "Models" });
        expect(within(table).getAllByRole("row").length).toBeGreaterThan(1);
        expect(table.querySelector("[aria-current]")).toBeNull();
        expect(scrolled).toEqual([]);
        view.unmount();
      }
    });
  });

  it("asks before deleting and removes the row", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    await userEvent.click(within(table).getByRole("button", { name: "More actions for burrow-simple" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Delete" }));
    const dialog = await screen.findByRole("dialog", { name: /delete burrow-simple/i });
    expect(within(dialog).getByText("Apps that ask for this model will get an error.")).toBeInTheDocument();
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
      name: "burrow-smart", description: "", system_text: "", routing: "ordered", enabled: true, fallback_on_rate_limit: false,
      attempt_timeout_s: 60, total_timeout_s: 120,
      targets: [
        { dialect: "openai", provider: "zai", model: "glm-5.1", available: false },
        { dialect: "openai", provider: "openrouter", model: "google/gemini-x", available: false },
      ],
      translate: false, dialect_modes: { openai: "native", anthropic: "not_served" }, responses_mode: "not_served", translation_pairs: {},
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

  describe("models follow ai:configure:any, attempts follow ai:read:any", () => {
    it("a custom role with ai:configure:any can create a model and has no attempt lookup without ai:read:any", async () => {
      db.me = { id: "u_op", email: "op@acme.io", role: "operator", permissions: ["ai:configure:any", "ai:read:own"] };
      mount();
      await screen.findByRole("table", { name: "Models" });
      expect(screen.getByRole("button", { name: "New model" })).toBeInTheDocument();
      expect(screen.queryByRole("heading", { name: "Why did a request fall back?" })).toBeNull();
    });

    it("a custom role with ai:read:any sees the attempt lookup and no New model", async () => {
      db.me = { id: "u_op", email: "op@acme.io", role: "operator", permissions: ["ai:read:any"] };
      mount();
      await screen.findByRole("table", { name: "Models" });
      expect(screen.queryByRole("button", { name: "New model" })).toBeNull();
      expect(await screen.findByRole("heading", { name: "Why did a request fall back?" })).toBeInTheDocument();
      expect(screen.queryByText(/admin/i)).toBeNull();
    });
  });

  it("marks a model with a routing mode and a system text", async () => {
    db.aiModels[0]!.routing = "fastest";
    db.aiModels[0]!.system_text = "Be brief.";
    db.aiModels[1]!.routing = "cheapest";
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    const simple = within(table).getByRole("row", { name: /burrow-simple/ });
    expect(within(simple).getByText("Fastest")).toBeInTheDocument();
    expect(within(simple).getByText("System text")).toBeInTheDocument();
    const intel = within(table).getByRole("row", { name: /burrow-intelligence/ });
    expect(within(intel).getByText("Cheapest")).toBeInTheDocument();
    expect(within(intel).queryByText("System text")).toBeNull();
  });

  it("shows no marker for ordered routing without a system text", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Models" });
    expect(within(table).queryByText("Fastest")).toBeNull();
    expect(within(table).queryByText("Cheapest")).toBeNull();
    expect(within(table).queryByText("System text")).toBeNull();
  });
});
