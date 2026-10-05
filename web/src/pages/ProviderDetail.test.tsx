import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { Route, Routes, useLocation } from "react-router-dom";
import ProviderDetail from "@/pages/ProviderDetail";
import { db } from "@/mocks/db";

function PathProbe() {
  return <div data-testid="path">{useLocation().pathname}</div>;
}

function mountAt(route: string) {
  return renderApp(
    <>
      <Routes>
        <Route path="/gateway/providers" element={<div>PROVIDERS_PAGE</div>} />
        <Route path="/gateway/providers/:slug" element={<ProviderDetail />} />
        <Route path="/clients/:id" element={<div>CLIENT_PAGE</div>} />
        <Route path="/inspector/:serviceId/:requestId?" element={<div>INSPECTOR_PAGE</div>} />
      </Routes>
      <PathProbe />
    </>,
    route,
  );
}

function mount() {
  return mountAt("/gateway/providers/ollama");
}

describe("Provider detail", () => {
  it("shows how to connect a client", async () => {
    mountAt("/gateway/providers/ollama");
    expect(await screen.findByRole("heading", { name: /connect a client/i })).toBeInTheDocument();
    expect(screen.getByText("https://tunnels.example.com/ai/ollama/v1")).toBeInTheDocument();
    // The example uses a model this provider serves.
    expect(screen.getByText(/"model": "llama3.1:8b"/)).toBeInTheDocument();
  });

  it("names the page after the provider and links back to the list", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Provider · ollama" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /providers/i })).toHaveAttribute("href", "/gateway/providers");
  });

  it("says so when the provider does not exist", async () => {
    mountAt("/gateway/providers/nope");
    expect(await screen.findByRole("alert")).toHaveTextContent(/couldn't load provider: provider not found/i);
  });

  it("lets an admin rename the provider and follows the new slug", async () => {
    mount();
    const opener = await screen.findByRole("button", { name: "Rename" });
    await userEvent.click(opener);
    const dialog = await screen.findByRole("dialog", { name: "Rename provider · ollama" });
    expect(within(dialog).getByRole("note")).toHaveTextContent("The old base URL stops working immediately.");
    const name = within(dialog).getByLabelText("Name");
    await waitFor(() => expect(name).toHaveFocus());
    expect(name).toHaveValue("ollama");
    const slug = within(dialog).getByLabelText("Provider slug");
    expect(slug).toHaveValue("ollama");
    // Nothing changed yet.
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await userEvent.clear(name);
    await userEvent.type(name, "Local models");
    await userEvent.clear(slug);
    await userEvent.type(slug, "local");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent(/^\/gateway\/providers\/local$/));
    expect(await screen.findByRole("heading", { name: "Provider · Local models" })).toBeInTheDocument();
    expect(screen.getByText("https://tunnels.example.com/ai/local/v1")).toBeInTheDocument();
    expect(db.aiProviders[0]).toMatchObject({ slug: "local", name: "Local models" });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("rename: a taken slug is reported on the slug field", async () => {
    db.services.push({ ...db.services.find((x) => x.id === "svc_ai001")!, id: "svc_ai002", name: "vllm", slug: "vl9k2p" });
    db.aiProviders.push({ slug: "vllm", name: "vllm", kind: "tunnel", api_format: "openai", service_id: "svc_ai002" });
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Rename" }));
    const dialog = await screen.findByRole("dialog", { name: /rename provider/i });
    const slug = within(dialog).getByLabelText("Provider slug");
    await userEvent.clear(slug);
    await userEvent.type(slug, "vllm");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(slug).toHaveAccessibleDescription("provider slug or service already in use"));
    expect(slug).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/gateway\/providers\/ollama$/);
  });

  it("rename: the reserved slug v1 is rejected before sending", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Rename" }));
    const dialog = await screen.findByRole("dialog", { name: /rename provider/i });
    const slug = within(dialog).getByLabelText("Provider slug");
    await userEvent.clear(slug);
    await userEvent.type(slug, "v1");
    expect(slug).toHaveAccessibleDescription('"v1" is reserved.');
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("hides Rename from non-admins", async () => {
    db.me = { ...db.me, role: "user" };
    mount();
    await screen.findByRole("heading", { name: /connect a client/i });
    expect(screen.queryByRole("button", { name: "Rename" })).toBeNull();
  });

  it("renders the meta strip with alias, client link, and last-seen", async () => {
    mount();
    // Model alias resolved to upstream.
    expect(await screen.findByText("fast → llama3.1:8b")).toBeInTheDocument();
    // Client link uses session_id.
    const clientLink = screen.getByRole("link", { name: /sess_4f7a9c0b2e81/i });
    expect(clientLink).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
  });

  it("renders the 4-tile metric strip and a 60px sparkline svg", async () => {
    mount();
    const spark = await screen.findByLabelText("requests per minute, last 24h");
    expect(spark.tagName.toLowerCase()).toBe("svg");
    expect(spark.getAttribute("viewBox")).toBe("0 0 240 60");
    const strip = screen.getByRole("list", { name: "Provider metrics" });
    const tiles = within(strip).getAllByRole("listitem");
    expect(tiles.length).toBeGreaterThanOrEqual(4);
  });

  it("editing routing strategy + failure_pct + Save issues PUT /services/:id/ai-config", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    // Wait for the form to hydrate.
    await screen.findByLabelText("requests per minute, last 24h");
    // Change strategy via the DS Select (custom listbox, not native <select>).
    await userEvent.click(screen.getByLabelText(/routing strategy/i));
    await userEvent.click(await screen.findByRole("option", { name: /weighted/i }));
    // Change failure_pct to 60.
    const failurePct = screen.getByLabelText(/circuit-breaker failure %/i);
    await userEvent.clear(failurePct);
    await userEvent.type(failurePct, "60");
    // Save.
    await userEvent.click(screen.getByRole("button", { name: /save routing/i }));
    await waitFor(() => {
      const putCalls = fetchSpy.mock.calls.filter(([url, init]) =>
        String(url).endsWith("/api/v1/services/svc_ai001/ai-config")
        && (init as RequestInit | undefined)?.method === "PUT",
      );
      expect(putCalls.length).toBeGreaterThanOrEqual(1);
      const body = JSON.parse(String((putCalls.at(-1)![1] as RequestInit).body));
      expect(body.routing.strategy).toBe("weighted");
      expect(body.routing.circuit_breaker.failure_pct).toBe(60);
    });
    expect((await screen.findAllByText(/routing saved/i)).length).toBeGreaterThan(0);
  });

  it("toggling Pause issues PUT /services/:id/ai-config with routing.paused=true", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await screen.findByLabelText("requests per minute, last 24h");
    const pause = screen.getByRole("switch", { name: /pause provider/i });
    await userEvent.click(pause);
    await waitFor(() => {
      const putCalls = fetchSpy.mock.calls.filter(([url, init]) =>
        String(url).endsWith("/api/v1/services/svc_ai001/ai-config")
        && (init as RequestInit | undefined)?.method === "PUT",
      );
      expect(putCalls.length).toBeGreaterThanOrEqual(1);
      const body = JSON.parse(String((putCalls.at(-1)![1] as RequestInit).body));
      expect(body.routing.paused).toBe(true);
    });
  });

  it("Clear cache in the kebab calls DELETE /services/:id/cache/entries", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await screen.findByLabelText("requests per minute, last 24h");
    await userEvent.click(screen.getByRole("button", { name: /more actions/i }));
    await userEvent.click(await screen.findByRole("menuitem", { name: /clear cache/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          String(url).endsWith("/api/v1/services/svc_ai001/cache/entries")
          && (init as RequestInit | undefined)?.method === "DELETE",
        ),
      ).toBe(true);
    });
  });

  it("recent requests row click navigates to /inspector/:serviceId/:requestId", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /recent requests/i });
    // Wait for the rows to populate from the inspector query (the table renders
    // immediately with just a thead row, then fills in).
    const rows = await within(table).findAllByRole("button");
    await userEvent.click(rows[0]!);
    expect(await screen.findByText("INSPECTOR_PAGE")).toBeInTheDocument();
  });

  it("Routing strategy Select includes 'Multi-provider (cross-backend)' option", async () => {
    mount();
    await screen.findByLabelText("requests per minute, last 24h");
    await userEvent.click(screen.getByLabelText(/routing strategy/i));
    expect(await screen.findByRole("option", { name: /multi-provider \(cross-backend\)/i })).toBeInTheDocument();
  });

  it("Selecting Multi-provider shows the cross-provider failover banner verbatim", async () => {
    mount();
    await screen.findByLabelText("requests per minute, last 24h");
    await userEvent.click(screen.getByLabelText(/routing strategy/i));
    await userEvent.click(await screen.findByRole("option", { name: /multi-provider \(cross-backend\)/i }));
    const banner = await screen.findByTestId("multi-provider-banner");
    const text = banner.textContent ?? "";
    expect(text).toMatch(/Cross-provider failover is allowed only when/);
    expect(text).toMatch(/Idempotency-Key/);
    expect(text).toMatch(/and zero bytes have streamed\. See routing docs\./);
  });

  it("Backends table shows Provider chip and Priority column", async () => {
    mount();
    await screen.findByLabelText("requests per minute, last 24h");
    const backendsSection = await screen.findByRole("heading", { name: /backends/i });
    expect(backendsSection).toBeInTheDocument();
    const table = await screen.findByRole("table", { name: /backends/i });
    expect(within(table).getByRole("columnheader", { name: /provider/i })).toBeInTheDocument();
    expect(within(table).getByRole("columnheader", { name: /priority/i })).toBeInTheDocument();
  });

  it("shows a centred empty row when no backend is configured (C6)", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /backends/i });
    const cell = within(table).getByText("No backends configured.").closest("td")!;
    expect(cell).toHaveClass("table-empty");
    expect(cell.getAttribute("colspan")).toBe("5");
    expect(within(cell).getByText("Add an alias to get started.")).toBeInTheDocument();
  });

  it("Priority input is editable and PUT /models/aliases/:alias fires on blur", async () => {
    // Seed a backend row so the Backends table has a row to interact with.
    const origBackends = db.aiConfigs["svc_ai001"]!.routing.backends;
    db.aiConfigs["svc_ai001"]!.routing.backends = [
      { service_id: "svc_ai001", weight: 1, concrete_model: "llama3.1:8b" },
    ];
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await screen.findByLabelText("requests per minute, last 24h");
    // The alias "fast" maps to svc_ai001 with priority 100.
    const priorityInput = await screen.findByLabelText(/priority for fast/i);
    expect(priorityInput).not.toBeDisabled();
    await userEvent.clear(priorityInput);
    await userEvent.type(priorityInput, "50");
    await userEvent.tab(); // blur triggers onBlur PUT
    await waitFor(() => {
      const putCalls = fetchSpy.mock.calls.filter(([url, init]) =>
        String(url).endsWith("/api/v1/models/aliases/fast")
        && (init as RequestInit | undefined)?.method === "PUT",
      );
      expect(putCalls.length).toBeGreaterThanOrEqual(1);
      const b = JSON.parse(String((putCalls.at(-1)![1] as RequestInit).body));
      expect(b.priority).toBe(50);
    });
    // Restore db state for other tests.
    db.aiConfigs["svc_ai001"]!.routing.backends = origBackends;
  });

  it("Add alias button opens dialog and POST /models/aliases creates the alias", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await screen.findByLabelText("requests per minute, last 24h");
    // Click "Add alias" button
    await userEvent.click(await screen.findByRole("button", { name: /add alias/i }));
    // Dialog should open
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toBeInTheDocument();
    // Fill in alias field
    const aliasInput = within(dialog).getByLabelText(/alias/i);
    await userEvent.clear(aliasInput);
    await userEvent.type(aliasInput, "smart");
    // Fill in concrete_model
    const modelInput = within(dialog).getByLabelText(/concrete model/i);
    await userEvent.clear(modelInput);
    await userEvent.type(modelInput, "llama3.1:70b");
    // Set priority to 90
    const priorityInput = within(dialog).getByLabelText(/priority/i);
    await userEvent.clear(priorityInput);
    await userEvent.type(priorityInput, "90");
    // Submit
    await userEvent.click(within(dialog).getByRole("button", { name: /create alias/i }));
    await waitFor(() => {
      const postCalls = fetchSpy.mock.calls.filter(([url, init]) =>
        String(url).endsWith("/api/v1/models/aliases")
        && (init as RequestInit | undefined)?.method === "POST",
      );
      expect(postCalls.length).toBeGreaterThanOrEqual(1);
      const b = JSON.parse(String((postCalls.at(-1)![1] as RequestInit).body));
      expect(b.alias).toBe("smart");
      expect(b.concrete_model).toBe("llama3.1:70b");
      expect(b.priority).toBe(90);
      expect(b.service_id).toBe("svc_ai001");
    });
    expect((await screen.findAllByText(/alias created/i)).length).toBeGreaterThan(0);
  });
});
