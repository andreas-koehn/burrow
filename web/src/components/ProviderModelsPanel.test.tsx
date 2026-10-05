import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse, delay } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { addDirectProvider, db } from "@/mocks/db";
import { ProviderModelsPanel, type ProviderModelsPanelProps } from "@/components/ProviderModelsPanel";

// OPENROUTER is set in the fixtures, ZAI is not.
function renderPanel(props: ProviderModelsPanelProps) {
  if (!db.aiProviders.some((p) => p.slug === props.slug)) {
    addDirectProvider(props.slug, { credential_slot: props.slug === "zai" ? "ZAI" : "OPENROUTER" });
  }
  if (!props.isAdmin) db.me = { ...db.me, role: "user" };
  return renderApp(<ProviderModelsPanel {...props} />);
}

function seedModels(slug: string, n: number) {
  db.aiProviderModels[slug] = Array.from({ length: n }, (_, i) => ({
    id: `vendor/model-${String(i).padStart(4, "0")}`, display_name: `Model ${i}`, context_length: 8000, synced_at: "2026-10-05T00:00:00Z",
  }));
}

describe("Provider models panel", () => {
  it("lists models, syncs, and reports the count", async () => {
    renderPanel({ slug: "openrouter", kind: "direct", isAdmin: true });
    expect(await screen.findByText(/no models yet/i)).toBeInTheDocument();
    expect(screen.getByText("Sync them from the provider or add one by id.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Sync models" }));
    expect(await screen.findByRole("status")).toHaveTextContent("2 models synced");
    await waitFor(() => expect(screen.getAllByRole("row")).toHaveLength(3)); // header + 2
    const table = screen.getByRole("table", { name: "Models" });
    expect(within(table).getByText("acme/large-1")).toHaveClass("mono");
    expect(within(table).getByText("Acme Large 1")).toBeInTheDocument();
    expect(within(table).getByText("200,000")).toBeInTheDocument();
  });

  it("shows why a sync failed", async () => {
    renderPanel({ slug: "zai", kind: "direct", isAdmin: true }); // slot absent in fixtures
    await userEvent.click(await screen.findByRole("button", { name: "Sync models" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("the credential slot ZAI is not set");
  });

  it("disables the button and marks the list busy while a sync runs", async () => {
    server.use(http.post("/api/v1/ai/providers/openrouter/models/sync", async () => {
      await delay(80);
      return HttpResponse.json({ count: 0 });
    }));
    renderPanel({ slug: "openrouter", kind: "direct", isAdmin: true });
    await userEvent.click(await screen.findByRole("button", { name: "Sync models" }));
    const busy = await screen.findByRole("button", { name: "Syncing…" });
    expect(busy).toBeDisabled();
    expect(screen.getByRole("region", { name: "Models" })).toHaveAttribute("aria-busy", "true");
    expect(screen.getByText("Syncing models…")).toHaveAttribute("aria-live", "polite");
    expect(await screen.findByRole("status")).toHaveTextContent("0 models synced");
    expect(screen.getByRole("button", { name: "Sync models" })).toBeEnabled();
    expect(screen.getByRole("region", { name: "Models" })).toHaveAttribute("aria-busy", "false");
  });

  it("adds and removes a model by hand", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    renderPanel({ slug: "openrouter", kind: "direct", isAdmin: true });
    await userEvent.type(await screen.findByLabelText("Model id"), "google/gemini-x");
    await userEvent.click(screen.getByRole("button", { name: "Add model" }));
    expect(await screen.findByText("google/gemini-x")).toBeInTheDocument();
    expect(screen.getByLabelText("Model id")).toHaveValue("");
    await userEvent.click(screen.getByRole("button", { name: "Remove google/gemini-x" }));
    await waitFor(() => expect(screen.queryByText("google/gemini-x")).toBeNull());
    // The id travels as a query parameter because it contains "/".
    expect(fetchSpy.mock.calls.some(([url, init]) =>
      (init as RequestInit | undefined)?.method === "DELETE"
      && String(url).endsWith("/api/v1/ai/providers/openrouter/models?id=google%2Fgemini-x"))).toBe(true);
  });

  it("ties a refused model id to its input", async () => {
    server.use(http.post("/api/v1/ai/providers/openrouter/models", () =>
      HttpResponse.json({ error: "id must be 1-200 characters without control characters" }, { status: 400 })));
    renderPanel({ slug: "openrouter", kind: "direct", isAdmin: true });
    const input = await screen.findByLabelText("Model id");
    await userEvent.type(input, "bad");
    await userEvent.click(screen.getByRole("button", { name: "Add model" }));
    await waitFor(() => expect(input).toHaveAttribute("aria-invalid", "true"));
    expect(input).toHaveAccessibleDescription("id must be 1-200 characters without control characters");
  });

  it("is read-only for non-admins", async () => {
    renderPanel({ slug: "openrouter", kind: "direct", isAdmin: false });
    await screen.findByText(/no models yet/i);
    expect(screen.queryByRole("button", { name: "Sync models" })).toBeNull();
    expect(screen.queryByLabelText("Model id")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Remove / })).toBeNull();
  });

  it("offers no sync for a tunnel provider, only adding by id", async () => {
    renderApp(<ProviderModelsPanel slug="ollama" kind="tunnel" isAdmin />);
    await screen.findByText(/no models yet/i);
    expect(screen.getByLabelText("Model id")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sync models" })).toBeNull();
  });

  it("stays usable with a long list: shows a page at a time and filters by id or name", async () => {
    addDirectProvider("openrouter", { credential_slot: "OPENROUTER" });
    seedModels("openrouter", 250);
    renderPanel({ slug: "openrouter", kind: "direct", isAdmin: true });
    const table = await screen.findByRole("table", { name: "Models" });
    expect(within(table).getAllByRole("row")).toHaveLength(101);
    expect(screen.getByText("Showing 100 of 250 models")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Show 100 more" }));
    expect(within(table).getAllByRole("row")).toHaveLength(201);
    await userEvent.type(screen.getByLabelText("Filter models"), "model-024");
    expect(within(table).getAllByRole("row")).toHaveLength(11); // 0240–0249
    expect(screen.getByText("Showing 10 of 10 matching models (250 in total)")).toBeInTheDocument();
    await userEvent.clear(screen.getByLabelText("Filter models"));
    await userEvent.type(screen.getByLabelText("Filter models"), "nothing-like-this");
    expect(screen.getByText("No model matches the filter.")).toBeInTheDocument();
  });

  it("reports a sync that is already running and an upstream that did not answer", async () => {
    renderPanel({ slug: "openrouter", kind: "direct", isAdmin: true });
    server.use(http.post("/api/v1/ai/providers/openrouter/models/sync", () =>
      HttpResponse.json({ error: "a sync for this provider is already running" }, { status: 409 })));
    await userEvent.click(await screen.findByRole("button", { name: "Sync models" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("a sync for this provider is already running");
    expect(screen.queryByRole("status")).toBeNull();
    server.use(http.post("/api/v1/ai/providers/openrouter/models/sync", () =>
      HttpResponse.json({ error: "the provider answered 401 to the model list request" }, { status: 502 })));
    await userEvent.click(screen.getByRole("button", { name: "Sync models" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("the provider answered 401 to the model list request"));
    // A later success replaces the error.
    server.resetHandlers();
    await userEvent.click(screen.getByRole("button", { name: "Sync models" }));
    expect(await screen.findByRole("status")).toHaveTextContent("2 models synced");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("disables every remove button while one removal runs, so no click is dropped silently", async () => {
    addDirectProvider("openrouter", { credential_slot: "OPENROUTER" });
    seedModels("openrouter", 3);
    let deletes = 0;
    server.use(http.delete("/api/v1/ai/providers/openrouter/models", async () => {
      deletes++;
      await delay(80);
      db.aiProviderModels["openrouter"]!.shift();
      return new HttpResponse(null, { status: 204 });
    }));
    renderPanel({ slug: "openrouter", kind: "direct", isAdmin: true });
    await userEvent.click(await screen.findByRole("button", { name: "Remove vendor/model-0000" }));
    const other = screen.getByRole("button", { name: "Remove vendor/model-0001" });
    await waitFor(() => expect(other).toBeDisabled());
    await userEvent.click(other);
    await waitFor(() => expect(screen.queryByText("vendor/model-0000")).toBeNull());
    expect(deletes).toBe(1);
    await waitFor(() => expect(screen.getByRole("button", { name: "Remove vendor/model-0001" })).toBeEnabled());
  });
});
