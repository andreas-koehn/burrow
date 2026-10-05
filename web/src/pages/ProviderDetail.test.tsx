import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { Route, Routes, useLocation } from "react-router-dom";
import ProviderDetail from "@/pages/ProviderDetail";
import { addDirectProvider, db } from "@/mocks/db";

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

async function openTab(name: string) {
  await userEvent.click(await screen.findByRole("tab", { name }));
}

// OPENROUTER is set in the fixtures, ZAI is not.
function mountDirect(slug: "openrouter" | "zai" = "openrouter", row: Parameters<typeof addDirectProvider>[1] = {}) {
  addDirectProvider(slug, {
    name: slug === "zai" ? "z.ai" : "OpenRouter",
    upstream_base_url: slug === "zai" ? "https://api.z.ai/api/coding/paas/v4" : "https://openrouter.ai/api/v1",
    credential_slot: slug === "zai" ? "ZAI" : "OPENROUTER",
    billing: slug === "zai" ? "flat" : "metered",
    ...row,
  });
  return mountAt(`/gateway/providers/${slug}`);
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

  it("rename: a 400 that is not about the slug shows in the dialog", async () => {
    server.use(http.put("/api/v1/ai/providers/ollama", () =>
      HttpResponse.json({ error: "kind must be 'tunnel'" }, { status: 400 })));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Rename" }));
    const dialog = await screen.findByRole("dialog", { name: /rename provider/i });
    const name = within(dialog).getByLabelText("Name");
    await userEvent.clear(name);
    await userEvent.type(name, "Other");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("kind must be 'tunnel'");
    expect(within(dialog).getByLabelText("Provider slug")).not.toHaveAttribute("aria-invalid");
  });

  it("lets an admin delete the provider after confirming, and returns to the list", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await screen.findByRole("heading", { name: /connect a client/i });
    await userEvent.click(screen.getByRole("button", { name: /more actions/i }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete provider" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete provider · ollama" });
    expect(dialog).toHaveTextContent("https://tunnels.example.com/ai/ollama/v1 stops working at once.");
    expect(dialog).toHaveTextContent("The service, its API keys and its tunnel are kept.");
    expect(within(dialog).getByRole("button", { name: "Delete provider" })).toHaveClass("btn-destructive");
    fetchSpy.mockClear();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete provider" }));
    expect(await screen.findByText("PROVIDERS_PAGE")).toBeInTheDocument();
    expect(db.aiProviders).toHaveLength(0);
    // Nothing asks for the deleted provider again: no GET follows the DELETE.
    const calls = fetchSpy.mock.calls.map(([url, init]) =>
      `${(init as RequestInit | undefined)?.method ?? "GET"} ${String(url)}`);
    const del = calls.findIndex((c) => c === "DELETE /api/v1/ai/providers/ollama");
    expect(del).toBeGreaterThanOrEqual(0);
    expect(calls.slice(del + 1).filter((c) => c.includes("/ai/providers/ollama"))).toEqual([]);
    // The backing service is untouched: nothing was sent to /services.
    expect(db.services.some((s) => s.id === "svc_ai001")).toBe(true);
    expect(fetchSpy.mock.calls.some(([url, init]) =>
      (init as RequestInit | undefined)?.method === "DELETE" && /\/api\/v1\/services\/[^/]+$/.test(String(url)),
    )).toBe(false);
  });

  it("delete: cancel sends nothing and hands focus back to the menu button", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    fetchSpy.mockClear(); // the spy is shared with earlier cases
    mount();
    await screen.findByRole("heading", { name: /connect a client/i });
    const more = screen.getByRole("button", { name: /more actions/i });
    await userEvent.click(more);
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete provider" }));
    const dialog = await screen.findByRole("dialog", { name: /delete provider/i });
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(more).toHaveFocus());
    expect(fetchSpy.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === "DELETE")).toBe(false);
    expect(db.aiProviders).toHaveLength(1);
  });

  it("delete: a failure shows in the dialog and keeps it open", async () => {
    server.use(http.delete("/api/v1/ai/providers/ollama", () =>
      HttpResponse.json({ error: "internal error" }, { status: 500 })));
    mount();
    await screen.findByRole("heading", { name: /connect a client/i });
    await userEvent.click(screen.getByRole("button", { name: /more actions/i }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete provider" }));
    const dialog = await screen.findByRole("dialog", { name: /delete provider/i });
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete provider" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("internal error");
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/gateway\/providers\/ollama$/);
  });

  it("offers neither Delete provider nor the old Disable to a non-admin", async () => {
    db.me = { ...db.me, role: "user" };
    mount();
    await screen.findByRole("heading", { name: /connect a client/i });
    await userEvent.click(screen.getByRole("button", { name: /more actions/i }));
    await screen.findByRole("menuitem", { name: "Clear cache" });
    expect(screen.queryByRole("menuitem", { name: "Delete provider" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Disable" })).toBeNull();
  });

  it("has no Disable item for an admin either", async () => {
    mount();
    await screen.findByRole("heading", { name: /connect a client/i });
    await userEvent.click(screen.getByRole("button", { name: /more actions/i }));
    await screen.findByRole("menuitem", { name: "Delete provider" });
    expect(screen.queryByRole("menuitem", { name: "Disable" })).toBeNull();
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
    await openTab("Routing");
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
    await openTab("Routing");
    await userEvent.click(screen.getByLabelText(/routing strategy/i));
    expect(await screen.findByRole("option", { name: /multi-provider \(cross-backend\)/i })).toBeInTheDocument();
  });

  it("Selecting Multi-provider shows the cross-provider failover banner verbatim", async () => {
    mount();
    await screen.findByLabelText("requests per minute, last 24h");
    await openTab("Routing");
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
    await openTab("Routing");
    const backendsSection = await screen.findByRole("heading", { name: /backends/i });
    expect(backendsSection).toBeInTheDocument();
    const table = await screen.findByRole("table", { name: /backends/i });
    expect(within(table).getByRole("columnheader", { name: /provider/i })).toBeInTheDocument();
    expect(within(table).getByRole("columnheader", { name: /priority/i })).toBeInTheDocument();
  });

  it("shows a centred empty row when no backend is configured (C6)", async () => {
    mount();
    await openTab("Routing");
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
    await openTab("Routing");
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
    await openTab("Routing");
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

  it("a tunnel provider has the tabs Connect, API keys, Models and Routing, and no Upstream", async () => {
    mount();
    await screen.findByRole("heading", { name: /connect a client/i });
    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Connect", "API keys", "Models", "Routing"]);
    expect(screen.getByRole("tab", { name: "Connect" })).toHaveAttribute("aria-selected", "true");
  });

  it("links to the request inspector of the backing service", async () => {
    mountDirect();
    expect(await screen.findByRole("link", { name: "Open request inspector" }))
      .toHaveAttribute("href", "/inspector/prov-openrouter");
  });

  it("opens the tab named in the URL fragment", async () => {
    mountAt("/gateway/providers/ollama#api-keys");
    expect(await screen.findByRole("tab", { name: "API keys" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("table", { name: "API keys" })).toBeInTheDocument();
  });

  it("the API keys tab manages the keys of the backing service", async () => {
    mount();
    await openTab("API keys");
    const table = await screen.findByRole("table", { name: "API keys" });
    expect(await within(table).findByText("prod")).toBeInTheDocument();
  });

  it("the Models tab lists the stored models and the example uses the first of them", async () => {
    db.aiProviderModels["ollama"] = [
      { id: "qwen3:8b", display_name: "", context_length: 0, synced_at: "2026-10-05T00:00:00Z" },
    ];
    mount();
    expect(await screen.findByText(/"model": "qwen3:8b"/)).toBeInTheDocument();
    await openTab("Models");
    expect(await within(await screen.findByRole("table", { name: "Models" })).findByText("qwen3:8b")).toBeInTheDocument();
  });

  describe("direct provider", () => {
    it("shows the tabs Connect, API keys, Models, Routing and Upstream", async () => {
      mountDirect();
      expect(await screen.findByRole("heading", { name: "Provider · OpenRouter" })).toBeInTheDocument();
      expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Connect", "API keys", "Models", "Routing", "Upstream"]);
      expect(screen.getByText("https://tunnels.example.com/ai/openrouter/v1")).toBeInTheDocument();
    });

    it("has nothing that assumes a tunnel or a client", async () => {
      mountDirect();
      await screen.findByRole("heading", { name: /connect a client/i });
      expect(screen.queryByRole("switch", { name: /pause provider/i })).toBeNull();
      expect(screen.queryByText(/last seen/i)).toBeNull();
      expect(screen.queryByRole("link", { name: /^sess_/ })).toBeNull();
      // No link to the hidden backing service.
      expect(document.querySelector('a[href^="/services"]')).toBeNull();
    });

    it("says how a flat-rate plan is counted, above the metrics", async () => {
      mountDirect("zai");
      const note = await screen.findByText("Flat-rate plan: usage is counted in tokens, not in USD.");
      const strip = screen.getByRole("list", { name: "Provider metrics" });
      expect(note.compareDocumentPosition(strip) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
      const cost = within(strip).getByText("Cost (24h)").closest("[role=listitem]") as HTMLElement;
      expect(within(cost).getByText("—")).toBeInTheDocument();
    });

    it("a metered provider shows its cost and no flat-rate note", async () => {
      mountDirect();
      const strip = await screen.findByRole("list", { name: "Provider metrics" });
      expect(within(strip).getByText("$1.23")).toBeInTheDocument();
      expect(screen.queryByText(/flat-rate plan/i)).toBeNull();
    });

    it("Upstream tab: shows where requests go and that the credential is configured", async () => {
      mountDirect("openrouter", { extra_headers: { "X-Title": "Burrow" } });
      await openTab("Upstream");
      const panel = screen.getByRole("tabpanel");
      expect(within(panel).getByText("https://openrouter.ai/api/v1")).toBeInTheDocument();
      expect(within(panel).getByText("OPENROUTER")).toBeInTheDocument();
      expect(within(panel).getByText("configured", { selector: "span.badge" })).toBeInTheDocument();
      expect(within(panel).getByText("Metered")).toBeInTheDocument();
      expect(within(panel).getByText("Bearer {key}")).toBeInTheDocument();
      // Extra headers by name only; their values never reach the page.
      expect(within(panel).getByText("X-Title")).toBeInTheDocument();
      expect(document.body).not.toHaveTextContent("Burrow\"");
      expect(within(panel).queryByText(/BURROW_UPSTREAM_KEY_/)).toBeNull();
    });

    it("Upstream tab: names the variable to set when the credential is missing", async () => {
      mountDirect("zai");
      await openTab("Upstream");
      const panel = screen.getByRole("tabpanel");
      expect(within(panel).getByText("not configured", { selector: "span.badge" })).toBeInTheDocument();
      const note = within(panel).getByRole("note");
      expect(note).toHaveTextContent("BURROW_UPSTREAM_KEY_ZAI");
      expect(note).toHaveTextContent(/restart/i);
      expect(within(panel).getByText("Flat rate")).toBeInTheDocument();
    });

    it("Upstream tab: a non-admin reads it and gets no Edit", async () => {
      db.me = { ...db.me, role: "user" };
      mountDirect();
      await openTab("Upstream");
      const panel = screen.getByRole("tabpanel");
      expect(within(panel).getByText("https://openrouter.ai/api/v1")).toBeInTheDocument();
      expect(within(panel).queryByRole("button", { name: "Edit" })).toBeNull();
      expect(within(panel).queryByText("Auth header")).toBeNull();
    });

    async function openEdit() {
      await openTab("Upstream");
      const opener = within(screen.getByRole("tabpanel")).getByRole("button", { name: "Edit" });
      await userEvent.click(opener);
      const dialog = await screen.findByRole("dialog", { name: "Edit upstream · OpenRouter" });
      return { opener, dialog };
    }

    function upstreamPuts(spy: ReturnType<typeof vi.spyOn>) {
      return (spy.mock.calls as [unknown, RequestInit | undefined][])
        .filter(([url, init]) => String(url).endsWith("/api/v1/ai/providers/openrouter/upstream") && init?.method === "PUT")
        .map(([, init]) => JSON.parse(String(init!.body)) as Record<string, unknown>);
    }

    it("Edit upstream: sends only what changed and leaves the extra headers alone", async () => {
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      fetchSpy.mockClear();
      mountDirect("openrouter", { extra_headers: { "X-Title": "Burrow" } });
      const { opener, dialog } = await openEdit();
      const url = within(dialog).getByLabelText("Base URL");
      await waitFor(() => expect(url).toHaveFocus());
      expect(url).toHaveValue("https://openrouter.ai/api/v1");
      expect(within(dialog).getByLabelText("Credential slot")).toHaveValue("OPENROUTER");
      expect(within(dialog).getByLabelText("Auth header")).toHaveValue("Authorization");
      expect(within(dialog).getByLabelText("Auth format")).toHaveValue("Bearer {key}");
      // The stored values are not shown; the field says what leaving it empty means.
      const extra = within(dialog).getByLabelText("Extra headers");
      expect(extra).toHaveValue("");
      expect(extra).toHaveAccessibleDescription(/Set now: X-Title\..*Leave this empty to keep them.*replaces all of them/);
      expect(within(dialog).queryByLabelText(/api key|secret|token/i)).toBeNull();
      // Nothing changed yet.
      expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
      await userEvent.click(within(dialog).getByLabelText("Billing"));
      await userEvent.click(await screen.findByRole("option", { name: "Flat rate" }));
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(upstreamPuts(fetchSpy)).toEqual([{ billing: "flat" }]);
      expect(db.aiProviders.at(-1)).toMatchObject({ billing: "flat", extra_headers: { "X-Title": "Burrow" } });
      expect(await within(screen.getByRole("tabpanel")).findByText("Flat rate")).toBeInTheDocument();
      await waitFor(() => expect(opener).toHaveFocus());
    });

    it("Edit upstream: entered extra headers replace all of them", async () => {
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      fetchSpy.mockClear();
      mountDirect("openrouter", { extra_headers: { "X-Title": "Burrow" } });
      const { dialog } = await openEdit();
      await userEvent.type(within(dialog).getByLabelText("Extra headers"), "X-Team: blue{Enter}HTTP-Referer: https://example.org");
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(upstreamPuts(fetchSpy)).toEqual([
        { extra_headers: { "X-Team": "blue", "HTTP-Referer": "https://example.org" } },
      ]);
    });

    it("Edit upstream: removing all extra headers is an explicit choice", async () => {
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      fetchSpy.mockClear();
      mountDirect("openrouter", { extra_headers: { "X-Title": "Burrow" } });
      const { dialog } = await openEdit();
      await userEvent.click(within(dialog).getByRole("checkbox", { name: "Remove all extra headers" }));
      expect(within(dialog).getByLabelText("Extra headers")).toBeDisabled();
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(upstreamPuts(fetchSpy)).toEqual([{ extra_headers: {} }]);
      expect(db.aiProviders.at(-1)!.extra_headers).toEqual({});
    });

    it("Edit upstream: a malformed extra header line is refused before sending", async () => {
      mountDirect();
      const { dialog } = await openEdit();
      const extra = within(dialog).getByLabelText("Extra headers");
      await userEvent.type(extra, "no colon here");
      expect(extra).toHaveAttribute("aria-invalid", "true");
      expect(extra).toHaveAccessibleDescription(/One header per line, as Name: value/);
      expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    });

    it("Edit upstream: a refused base URL is reported on its field", async () => {
      mountDirect();
      const { dialog } = await openEdit();
      const url = within(dialog).getByLabelText("Base URL");
      await userEvent.clear(url);
      await userEvent.type(url, "https://10.0.0.5/v1");
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(url).toHaveAttribute("aria-invalid", "true"));
      expect(url).toHaveAccessibleDescription("base URL resolves to a private or loopback address");
      expect(screen.getByRole("dialog")).toBeInTheDocument();
    });

    it("Edit upstream: a lost update asks to try again, in the dialog", async () => {
      server.use(http.put("/api/v1/ai/providers/openrouter/upstream", () =>
        HttpResponse.json({ error: "the provider was changed by someone else at the same time; try again" }, { status: 409 })));
      mountDirect();
      const { dialog } = await openEdit();
      await userEvent.click(within(dialog).getByLabelText("Billing"));
      await userEvent.click(await screen.findByRole("option", { name: "Flat rate" }));
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      expect(await within(dialog).findByRole("alert")).toHaveTextContent(/changed by someone else.*try again/);
    });

    it("delete says that the API keys and the model list go with it", async () => {
      mountDirect();
      await screen.findByRole("heading", { name: /connect a client/i });
      await userEvent.click(screen.getByRole("button", { name: /more actions/i }));
      await userEvent.click(await screen.findByRole("menuitem", { name: "Delete provider" }));
      const dialog = await screen.findByRole("dialog", { name: "Delete provider · OpenRouter" });
      expect(dialog).toHaveTextContent("Its API keys, its AI configuration and its model list are deleted with it.");
      expect(dialog).toHaveTextContent("The credential slot OPENROUTER on the relay is not touched.");
      expect(dialog).not.toHaveTextContent(/are kept/);
      await userEvent.click(within(dialog).getByRole("button", { name: "Delete provider" }));
      expect(await screen.findByText("PROVIDERS_PAGE")).toBeInTheDocument();
      expect(db.services.some((s) => s.id === "prov-openrouter")).toBe(false);
    });
  });
});
