import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { Route, Routes, useLocation, useNavigate } from "react-router-dom";
import ProviderDetail from "@/pages/ProviderDetail";
import { addDirectProvider, db } from "@/mocks/db";

function PathProbe() {
  const loc = useLocation();
  const nav = useNavigate();
  return (
    <>
      <div data-testid="path">{loc.pathname}</div>
      <div data-testid="hash">{loc.hash}</div>
      <div data-testid="search">{loc.search}</div>
      <button onClick={() => nav({ hash: "#models" })}>GO_MODELS_FRAGMENT</button>
    </>
  );
}

function mountAt(route: string) {
  return renderApp(
    <>
      <Routes>
        <Route path="/gateway/providers" element={<div>PROVIDERS_PAGE</div>} />
        <Route path="/gateway/providers/:slug" element={<ProviderDetail />} />
        <Route path="/clients/:id" element={<div>CLIENT_PAGE</div>} />
        <Route path="/gateway/requests/:serviceId/:requestId?" element={<div>INSPECTOR_PAGE</div>} />
        <Route path="/gateway/keys" element={<div>KEYS_PAGE</div>} />
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

// Rename, pause, delete and the limits live on the Settings tab.
function mountSettings(slug = "ollama") {
  return mountAt(`/gateway/providers/${slug}#settings`);
}

// The routing settings are behind "Advanced" on the Settings tab.
async function openRouting() {
  await openTab("Settings");
  await userEvent.click(await screen.findByRole("button", { name: "Advanced" }));
}

const selectedTab = () => screen.getByRole("tab", { selected: true }).textContent;

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
  it("shows how to use it in an app", async () => {
    mountAt("/gateway/providers/ollama");
    expect(await screen.findByRole("heading", { name: "Use it in an app" })).toBeInTheDocument();
    expect(screen.getByText("Use this as the base URL of any OpenAI-compatible app, with a gateway key.")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /connect a client/i })).toBeNull();
    expect(screen.getByText("https://tunnels.example.com/ai/ollama/v1")).toBeInTheDocument();
    // The example uses a model this provider serves.
    expect(await screen.findByText(/"model": "mistral"/)).toBeInTheDocument();
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
    mountSettings();
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
    // Still on Settings, where the button is.
    expect(screen.getByTestId("hash")).toHaveTextContent("#settings");
    expect(selectedTab()).toBe("Settings");
    await openTab("Overview");
    expect(await screen.findByText("https://tunnels.example.com/ai/local/v1")).toBeInTheDocument();
    expect(db.aiProviders[0]).toMatchObject({ slug: "local", name: "Local models" });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("rename: a taken slug is reported on the slug field", async () => {
    db.services.push({ ...db.services.find((x) => x.id === "svc_ai001")!, id: "svc_ai002", name: "vllm", slug: "vl9k2p" });
    db.aiProviders.push({ slug: "vllm", name: "vllm", kind: "tunnel", api_format: "openai", service_id: "svc_ai002" });
    mountSettings();
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
    mountSettings();
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
    mountSettings();
    await userEvent.click(await screen.findByRole("button", { name: "Rename" }));
    const dialog = await screen.findByRole("dialog", { name: /rename provider/i });
    const name = within(dialog).getByLabelText("Name");
    await userEvent.clear(name);
    await userEvent.type(name, "Other");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("kind must be 'tunnel'");
    expect(within(dialog).getByLabelText("Provider slug")).not.toHaveAttribute("aria-invalid");
  });

  it("delete: a provider a model still targets is refused, and the dialog names the models", async () => {
    mountSettings();
    await userEvent.click(await screen.findByRole("button", { name: "Delete provider" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete provider · ollama" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete provider" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("provider is used by model(s): burrow-simple");
    expect(db.aiProviders.map((p) => p.slug)).toContain("ollama");
  });

  it("lets an admin delete the provider after confirming, and returns to the list", async () => {
    // No model targets the provider any more.
    db.aiModels = db.aiModels.filter((m) => m.name !== "burrow-simple");
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mountSettings();
    await userEvent.click(await screen.findByRole("button", { name: "Delete provider" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete provider · ollama" });
    expect(dialog).toHaveTextContent("https://tunnels.example.com/ai/ollama/v1 stops working at once.");
    expect(dialog).toHaveTextContent("The service, its API keys and its client connection are kept.");
    expect(within(dialog).getByRole("button", { name: "Delete provider" })).toHaveClass("btn-destructive");
    fetchSpy.mockClear();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete provider" }));
    expect(await screen.findByText("PROVIDERS_PAGE")).toBeInTheDocument();
    expect(db.aiProviders.map((p) => p.slug)).not.toContain("ollama");
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

  it("delete: cancel sends nothing and hands focus back to the button", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    fetchSpy.mockClear(); // the spy is shared with earlier cases
    const before = db.aiProviders.map((p) => p.slug);
    mountSettings();
    const more = await screen.findByRole("button", { name: "Delete provider" });
    await userEvent.click(more);
    const dialog = await screen.findByRole("dialog", { name: /delete provider/i });
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(more).toHaveFocus());
    expect(fetchSpy.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === "DELETE")).toBe(false);
    expect(before).toContain("ollama");
    expect(db.aiProviders.map((p) => p.slug)).toEqual(before);
  });

  it("delete: a failure shows in the dialog and keeps it open", async () => {
    server.use(http.delete("/api/v1/ai/providers/ollama", () =>
      HttpResponse.json({ error: "internal error" }, { status: 500 })));
    mountSettings();
    await userEvent.click(await screen.findByRole("button", { name: "Delete provider" }));
    const dialog = await screen.findByRole("dialog", { name: /delete provider/i });
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete provider" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("internal error");
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/gateway\/providers\/ollama$/);
  });

  it("offers neither Delete provider nor Rename to a non-admin", async () => {
    db.me = { ...db.me, role: "user" };
    mountSettings();
    // Pausing is not an admin's alone.
    await screen.findByRole("switch", { name: "Pause provider" });
    expect(screen.queryByRole("button", { name: "Delete provider" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Rename" })).toBeNull();
  });

  it("the header keeps View requests and a menu with Clear cache and Export logs, nothing else", async () => {
    mount();
    await screen.findByRole("heading", { name: "Use it in an app" });
    const header = document.querySelector(".page-header") as HTMLElement;
    expect(within(header).getByRole("link", { name: "View requests" })).toHaveAttribute("href", "/gateway/requests?provider=ollama");
    // Rename, pause and delete moved to Settings.
    expect(within(header).queryByRole("button", { name: "Rename" })).toBeNull();
    expect(within(header).queryByRole("switch")).toBeNull();
    expect(within(header).queryByRole("link", { name: /request inspector/i })).toBeNull();
    await userEvent.click(within(header).getByRole("button", { name: /more actions/i }));
    await screen.findByRole("menuitem", { name: "Clear cache" });
    expect(screen.getAllByRole("menuitem").map((i) => i.textContent)).toEqual(["Clear cache", "Export logs (NDJSON)"]);
  });

  describe("client panel", () => {
    const panel = () => screen.findByRole("region", { name: "Client" });
    const run = () => screen.queryByRole("group", { name: "Run command" });

    it("online: sits at the top of Overview and names the client with a link", async () => {
      mount();
      const client = await panel();
      expect(client).toHaveTextContent("Online · Served by office-box-1");
      // The live session is the one the backing service names.
      expect(within(client).getByRole("link", { name: "office-box-1" })).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
      expect(run()).toBeNull();
      const useIt = screen.getByRole("region", { name: "Use it in an app" });
      expect(client.compareDocumentPosition(useIt) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });

    it("offline: says who served it last and shows the command that starts it", async () => {
      mountAt("/gateway/providers/llamacpp");
      const client = await panel();
      // Seeded months ago: past 30 days the date stands for "how long ago".
      expect(client).toHaveTextContent("Offline · last served by office-box-1 12 May 2026");
      // By the name of the backing service; no address is known, so the usual port.
      expect(run()).toHaveTextContent(/^burrow http 11434 --name llamacpp$/);
      expect(screen.queryByRole("group", { name: "Install command" })).toBeNull();
      expect(within(client).getByRole("link", { name: "Connect another machine" }))
        .toHaveAttribute("href", "/clients/connect?provider=llamacpp");
    });

    it("never connected: only the run line while the relay knows a client", async () => {
      mountAt("/gateway/providers/lmstudio");
      const client = await panel();
      expect(client).toHaveTextContent("Never connected");
      await waitFor(() => expect(run()).toHaveTextContent(/^burrow http 11434 --name lmstudio$/));
      expect(screen.queryByRole("group", { name: "Install command" })).toBeNull();
    });

    it("never connected: says that it waits for the client", async () => {
      mountAt("/gateway/providers/lmstudio");
      const client = await panel();
      expect(await within(client).findByText("Waiting for the client…")).toBeInTheDocument();
    });

    it("stops asking for the status when Overview is left", async () => {
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      fetchSpy.mockClear();
      const asked = () => fetchSpy.mock.calls.filter(([url, init]) =>
        String(url).endsWith("/api/v1/ai/providers/llamacpp") && ((init as RequestInit | undefined)?.method ?? "GET") === "GET").length;
      mountAt("/gateway/providers/llamacpp");
      await panel();
      // The page asks once, the panel once and then again every two seconds.
      await waitFor(() => expect(asked()).toBeGreaterThanOrEqual(3), { timeout: 6000 });
      await openTab("Settings");
      const then = asked();
      await new Promise((r) => setTimeout(r, 2600));
      expect(asked()).toBe(then);
      // And it starts again with the panel.
      await openTab("Overview");
      await waitFor(() => expect(asked()).toBeGreaterThan(then), { timeout: 6000 });
    }, 20000);

    it("never connected: all three lines while the relay knows no client", async () => {
      db.tokens = [];
      db.clients = [];
      mountAt("/gateway/providers/lmstudio");
      await panel();
      await waitFor(() => expect(screen.getByRole("group", { name: "Install command" })).toBeInTheDocument());
      expect(screen.getByRole("group", { name: "Sign-in command" })).toHaveTextContent(/^burrow login /);
      expect(run()).toHaveTextContent(/^burrow http 11434 --name lmstudio$/);
    });

    it("uses the name of the backing service where it differs from the provider's slug", async () => {
      db.services.find((x) => x.id === "svc_llama01")!.name = "gpu-box";
      mountAt("/gateway/providers/llamacpp");
      await panel();
      expect(run()).toHaveTextContent(/^burrow http 11434 --name gpu-box$/);
    });

    it("turns Online by itself once the client is started", async () => {
      mountAt("/gateway/providers/llamacpp");
      const client = await panel();
      const line = client.querySelector("[role=status]") as HTMLElement;
      expect(line).toHaveTextContent(/^Offline · last served by office-box-1/);
      // It has been offline for long: nothing says that it waits, and nothing spins.
      await screen.findByRole("group", { name: "Run command" });
      expect(screen.queryByText("Waiting for the client…")).toBeNull();
      expect(client.querySelector(".spinner")).toBeNull();
      // The command was run: office-box-1 now serves the backing service.
      const svc = db.services.find((x) => x.id === "svc_llama01")!;
      svc.connected = true;
      db.clients[0]!.services.push({ ...db.clients[0]!.services[1]!, id: "tnl_llama", name: svc.name });
      db.aiMeta[svc.id] = { ...db.aiMeta["svc_ai001"]!, status: "Connected", client_session_id: "" };
      // The same line, so the change is announced.
      await waitFor(() => expect(line).toHaveTextContent("Online · Served by office-box-1"), { timeout: 8000 });
      expect(line).toBeInTheDocument();
      expect(within(client).getByRole("link", { name: "office-box-1" })).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
      expect(run()).toBeNull();
      // The header says the same.
      const header = document.querySelector(".page-header") as HTMLElement;
      expect(within(header).getByText("Online")).toHaveClass("badge", "status-online");
    }, 15000);

    it("a hosted provider has no client panel: Overview says whether its credential is set", async () => {
      mountAt("/gateway/providers/openai-team");
      const credential = await screen.findByRole("region", { name: "Credential" });
      expect(credential).toHaveTextContent("Ready · The credential is set on the relay.");
      expect(screen.queryByRole("region", { name: "Client" })).toBeNull();
      expect(document.body).not.toHaveTextContent("burrow http");
    });

    it("a hosted provider without its credential says what to set, and links to the settings", async () => {
      mountAt("/gateway/providers/zai");
      const credential = await screen.findByRole("region", { name: "Credential" });
      expect(credential).toHaveTextContent("Not configured · Slot ZAI is not set on the relay, or is set to an empty value.");
      expect(credential).toHaveTextContent("BURROW_UPSTREAM_KEY_ZAI");
      await userEvent.click(within(credential).getByRole("link", { name: "Upstream settings" }));
      expect(selectedTab()).toBe("Settings");
      expect(await screen.findByRole("region", { name: "Upstream" })).toBeInTheDocument();
    });
  });

  it("the figures and recent requests are on Overview", async () => {
    mount();
    const strip = await screen.findByRole("list", { name: "Provider metrics" });
    expect(within(strip).getAllByRole("listitem").map((t) => t.textContent)).toEqual([
      expect.stringContaining("Requests (24h)"), expect.stringContaining("Tokens (24h)"),
      expect.stringContaining("Cost (24h)"), expect.stringContaining("Cache hit ratio"),
    ]);
    expect(selectedTab()).toBe("Overview");
    const recent = screen.getByRole("table", { name: "Recent requests" });
    // In the order of the page: how to use it, the figures, what came in.
    const useIt = screen.getByRole("region", { name: "Use it in an app" });
    expect(useIt.compareDocumentPosition(strip) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(strip.compareDocumentPosition(recent) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    for (const other of ["Catalog", "Settings"]) {
      await openTab(other);
      expect(screen.queryByRole("list", { name: "Provider metrics" })).toBeNull();
      expect(screen.queryByRole("table", { name: "Recent requests" })).toBeNull();
    }
  });

  it("an empty recent-requests table says so", async () => {
    mountAt("/gateway/providers/llamacpp");
    const table = await screen.findByRole("table", { name: "Recent requests" });
    const cell = (await within(table).findByText("No requests yet")).closest("td")!;
    expect(cell).toHaveClass("table-empty");
    expect(cell.getAttribute("colspan")).toBe("5");
    expect(within(table).queryByRole("button")).toBeNull();
  });

  it("a table with requests has no empty row", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Recent requests" });
    await within(table).findAllByRole("button");
    expect(within(table).queryByText("No requests yet")).toBeNull();
  });

  it("a provider without requests has a dash for its cache hit ratio, not 0%", async () => {
    mountAt("/gateway/providers/llamacpp");
    const strip = await screen.findByRole("list", { name: "Provider metrics" });
    const ratio = within(strip).getAllByRole("listitem")[3]!;
    expect(ratio.querySelector(".label")).toHaveTextContent("Cache hit ratio");
    expect(ratio.querySelector(".value")).toHaveTextContent("—");
    expect(strip.textContent).not.toMatch(/\b0%/);
  });

  it("with requests and no cache hit the ratio is 0%", async () => {
    server.use(http.get("/api/v1/ai/providers/:slug/metrics", () => HttpResponse.json({
      requests_24h: 10, tokens_in_24h: 0, tokens_out_24h: 0, cost_usd_24h: 0, cache_hit_ratio_24h: 0,
      requests_by_minute: [],
    })));
    mount();
    const strip = await screen.findByRole("list", { name: "Provider metrics" });
    expect(within(strip).getAllByRole("listitem")[3]!.querySelector(".value")).toHaveTextContent("0%");
  });

  it("recent requests say how long ago, and have no Latency column", async () => {
    const entry = db.inspectorEntries["svc_ai001"]![0]!;
    db.inspectorEntries["svc_ai001"] = [
      { ...entry, id: "req_a", ts: "2026-05-19T08:00:00Z", duration_ms: 0 },
      { ...entry, id: "req_b", ts: "2026-05-19T07:00:00Z", duration_ms: 812 },
    ];
    mount();
    const table = await screen.findByRole("table", { name: "Recent requests" });
    const [first, second] = await within(table).findAllByRole("button");
    const when = first!.querySelector("time")!;
    expect(when).toHaveTextContent("19 May 2026");
    expect(when).toHaveAttribute("title", "19 May 2026, 08:00:00 UTC");
    // As on the Requests list: the relay sets no duration on a captured request.
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["When", "Method", "Path", "Status", "Cache"]);
    for (const row of [first!, second!]) {
      expect(within(row).getAllByRole("cell")).toHaveLength(5);
      expect(row.textContent).not.toMatch(/\d ms\b/);
    }
  });

  it("there is no line 'last seen —' and no bar without a label", async () => {
    mountAt("/gateway/providers/llamacpp");
    await screen.findByRole("table", { name: "Recent requests" });
    expect(document.body).not.toHaveTextContent(/last seen/i);
    // The chart above the table had no visible label; no picture stands in its place.
    expect(screen.queryByLabelText("requests per minute, last hour")).toBeNull();
    expect(screen.queryByRole("img")).toBeNull();
  });

  it("editing routing strategy + failure_pct + Save issues PUT /services/:id/ai-config", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    // Wait for the form to hydrate.
    await screen.findByRole("list", { name: "Provider metrics" });
    await openRouting();
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

  it("toggling Pause on Settings issues PUT /services/:id/ai-config with routing.paused=true", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mountSettings();
    const pause = await screen.findByRole("switch", { name: /pause provider/i });
    expect(pause).not.toBeChecked();
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

  it.each([
    ["ollama", "Online", "status-online"],
    ["llamacpp", "Offline", "status-offline"],
    ["lmstudio", "Never connected", "status-never"],
    ["openai-team", "Ready", "status-online"],
    ["zai", "Not configured", "status-never"],
  ])("writes the state of %s out as %s in the header", async (slug, label, kind) => {
    mountAt(`/gateway/providers/${slug}`);
    await screen.findByRole("heading", { name: `Provider · ${slug}` });
    const header = document.querySelector(".page-header") as HTMLElement;
    expect(within(header).getByText(label)).toHaveClass("badge", kind);
  });

  it("has one cache action in the kebab: Clear cache", async () => {
    mount();
    await screen.findByRole("heading", { name: "Use it in an app" });
    await userEvent.click(screen.getByRole("button", { name: /more actions/i }));
    expect(await screen.findByRole("menuitem", { name: "Clear cache" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /rotate cache/i })).toBeNull();
  });

  it("Clear cache in the kebab calls DELETE /services/:id/cache/entries", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await screen.findByRole("list", { name: "Provider metrics" });
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

  it("recent requests row click navigates to /gateway/requests/:serviceId/:requestId", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /recent requests/i });
    // Wait for the rows to populate from the inspector query (the table renders
    // immediately with just a thead row, then fills in).
    const rows = await within(table).findAllByRole("button");
    await userEvent.click(rows[0]!);
    expect(await screen.findByText("INSPECTOR_PAGE")).toBeInTheDocument();
  });

  it("recent requests row opens with the Space key too", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /recent requests/i });
    const rows = await within(table).findAllByRole("button");
    rows[0]!.focus();
    await userEvent.keyboard(" ");
    expect(await screen.findByText("INSPECTOR_PAGE")).toBeInTheDocument();
  });

  it("confirms a copy from the Overview tab", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Copy curl example" }));
    expect(writeText).toHaveBeenCalled();
    expect(await screen.findByText("Copied.")).toBeInTheDocument();
  });

  it("Routing strategy Select includes 'Multi-provider (cross-backend)' option", async () => {
    mount();
    await screen.findByRole("list", { name: "Provider metrics" });
    await openRouting();
    await userEvent.click(screen.getByLabelText(/routing strategy/i));
    expect(await screen.findByRole("option", { name: /multi-provider \(cross-backend\)/i })).toBeInTheDocument();
  });

  it("Selecting Multi-provider shows the cross-provider failover banner verbatim", async () => {
    mount();
    await screen.findByRole("list", { name: "Provider metrics" });
    await openRouting();
    await userEvent.click(screen.getByLabelText(/routing strategy/i));
    await userEvent.click(await screen.findByRole("option", { name: /multi-provider \(cross-backend\)/i }));
    const banner = await screen.findByTestId("multi-provider-banner");
    const text = banner.textContent ?? "";
    expect(text).toMatch(/Cross-provider failover is allowed only when/);
    expect(text).toMatch(/Idempotency-Key/);
    expect(text).toMatch(/and zero bytes have streamed\. See routing docs\./);
  });

  it("Backends table lists service, model and weight; model aliases are gone", async () => {
    const asked: string[] = [];
    server.use(http.all("/api/v1/models/aliases*", ({ request }) => {
      asked.push(request.url);
      return HttpResponse.json({ error: "not found" }, { status: 404 });
    }));
    mount();
    await screen.findByRole("list", { name: "Provider metrics" });
    await openRouting();
    expect(await screen.findByRole("heading", { name: /backends/i })).toBeInTheDocument();
    const table = await screen.findByRole("table", { name: /backends/i });
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Service", "Concrete model", "Weight"]);
    // Synthetic models replaced aliases: no alias control, and the removed routes are never called.
    expect(screen.queryByRole("button", { name: /add alias/i })).toBeNull();
    expect(asked).toEqual([]);
  });

  it("shows a centred empty row when no backend is configured (C6)", async () => {
    mount();
    await openRouting();
    const table = await screen.findByRole("table", { name: /backends/i });
    const cell = within(table).getByText("No backends configured.").closest("td")!;
    expect(cell).toHaveClass("table-empty");
    expect(cell.getAttribute("colspan")).toBe("3");
  });

  it("has the tabs Overview, Catalog, Settings", async () => {
    mount();
    await screen.findByRole("heading", { name: "Use it in an app" });
    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Overview", "Catalog", "Settings"]);
    expect(screen.getByRole("tab", { name: "Overview" })).toHaveAttribute("aria-selected", "true");
  });

  it.each([
    ["#overview", "Overview"], ["#catalog", "Catalog"], ["#settings", "Settings"],
  ])("the hash %s opens %s", async (hash, tab) => {
    mountAt(`/gateway/providers/ollama${hash}`);
    expect(await screen.findByRole("tab", { name: tab })).toHaveAttribute("aria-selected", "true");
  });

  it.each([
    ["#connect", "Overview"], ["#models", "Catalog"], ["#api-keys", "Settings"],
    ["#routing", "Settings"], ["#upstream", "Settings"],
  ])("the old hash %s opens %s", async (hash, tab) => {
    mountAt(`/gateway/providers/ollama${hash}`);
    expect(await screen.findByRole("tab", { name: tab })).toHaveAttribute("aria-selected", "true");
  });

  it("the old hash #upstream opens Settings of a hosted provider, where its upstream settings are", async () => {
    mountAt("/gateway/providers/openai-team#upstream");
    expect(await screen.findByRole("tab", { name: "Settings" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("region", { name: "Upstream" })).toBeInTheDocument();
  });

  it("a hash that names no tab opens Overview", async () => {
    mountAt("/gateway/providers/ollama#nope");
    expect(await screen.findByRole("tab", { name: "Overview" })).toHaveAttribute("aria-selected", "true");
  });

  it("#routing opens Settings with Advanced expanded", async () => {
    mountAt("/gateway/providers/ollama#routing");
    const advanced = await screen.findByRole("button", { name: "Advanced" });
    expect(advanced).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByLabelText(/routing strategy/i)).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Backends" })).toBeInTheDocument();
    // And it can be closed again.
    await userEvent.click(advanced);
    expect(advanced).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByLabelText(/routing strategy/i)).toBeNull();
  });

  it("Advanced is collapsed on Settings until it is asked for", async () => {
    mountSettings();
    const advanced = await screen.findByRole("button", { name: "Advanced" });
    expect(advanced).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByLabelText(/routing strategy/i)).toBeNull();
    expect(screen.queryByRole("table", { name: "Backends" })).toBeNull();
    await userEvent.click(advanced);
    expect(advanced).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("heading", { name: "Routing" })).toBeInTheDocument();
    expect(document.getElementById(advanced.getAttribute("aria-controls")!)).toContainElement(screen.getByLabelText(/routing strategy/i));
  });

  it("Settings holds its sections in order", async () => {
    mountSettings();
    const panel = await screen.findByRole("tabpanel");
    await within(panel).findByRole("checkbox", { name: /Offers the Responses API/ });
    const marks = [
      within(panel).getByRole("region", { name: "Responses API" }),
      within(panel).getByRole("region", { name: "Concurrency limit" }),
      within(panel).getByRole("region", { name: "Keys" }),
      within(panel).getByRole("button", { name: "Advanced" }),
      within(panel).getByRole("switch", { name: "Pause provider" }),
      within(panel).getByRole("button", { name: "Rename" }),
      within(panel).getByRole("button", { name: "Delete provider" }),
    ];
    for (let i = 1; i < marks.length; i++) {
      expect(marks[i - 1]!.compareDocumentPosition(marks[i]!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
  });

  it("View requests leads to the requests of this provider", async () => {
    mountDirect();
    expect(await screen.findByRole("link", { name: "View requests" }))
      .toHaveAttribute("href", "/gateway/requests?provider=openrouter");
  });

  it("keeps the open tab in the URL fragment, both ways", async () => {
    mount();
    await openTab("Settings");
    expect(screen.getByTestId("hash")).toHaveTextContent("#settings");
    await openTab("Catalog");
    expect(screen.getByTestId("hash")).toHaveTextContent("#catalog");
    await openTab("Overview");
    expect(screen.getByTestId("hash")).toHaveTextContent("#overview");
    // A fragment that changes while the page is open (an old link, the back button) switches the tab.
    await userEvent.click(screen.getByRole("button", { name: "GO_MODELS_FRAGMENT" }));
    expect(await screen.findByRole("tab", { name: "Catalog" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/providers/ollama");
  });

  describe("keys", () => {
    const keys = () => screen.findByRole("region", { name: "Keys" });

    it("there is no API keys tab", async () => {
      mountAt("/gateway/providers/ollama#api-keys");
      await screen.findByRole("tablist");
      expect(screen.queryByRole("tab", { name: /keys/i })).toBeNull();
      // Nor the key list of the service: it is managed on the service's own page.
      await keys();
      expect(screen.queryByRole("table", { name: "API keys" })).toBeNull();
      expect(screen.queryByRole("button", { name: /create (api )?key/i })).toBeNull();
    });

    it("says that apps use a gateway key, and links to them", async () => {
      mountSettings();
      const section = await keys();
      expect(section).toHaveTextContent("Apps use a gateway key.");
      const link = within(section).getByRole("link", { name: "Manage gateway keys" });
      expect(link).toHaveAttribute("href", "/gateway/keys");
      await userEvent.click(link);
      expect(await screen.findByText("KEYS_PAGE")).toBeInTheDocument();
    });

    it("says how many service keys exist and links to them", async () => {
      mountSettings();
      const section = await keys();
      expect(section).toHaveTextContent("This provider's service also has 2 API keys from before gateway keys. They keep working.");
      expect(within(section).getByRole("link", { name: /Manage them/ })).toHaveAttribute("href", "/services/svc_ai001#api-keys");
    });

    it("says it in the singular for one service key", async () => {
      db.serviceApiKeys["svc_ai001"] = db.serviceApiKeys["svc_ai001"]!.slice(0, 1);
      mountSettings();
      const section = await keys();
      expect(section).toHaveTextContent("This provider's service also has 1 API key from before gateway keys. It keeps working.");
      expect(within(section).getByRole("link", { name: /Manage it/ })).toHaveAttribute("href", "/services/svc_ai001#api-keys");
    });

    it("says nothing about service keys when there are none", async () => {
      mountSettings("llamacpp");
      const section = await keys();
      expect(section).toHaveTextContent("Apps use a gateway key.");
      expect(section).not.toHaveTextContent(/API keys?/);
      expect(document.querySelector('a[href^="/services"]')).toBeNull();
    });

    // The keys of a hosted provider still authenticate, and its service has no page of its own.
    function hostedWithKeys(names: string[]) {
      const p = addDirectProvider("openrouter", { name: "OpenRouter", credential_slot: "OPENROUTER" });
      db.serviceApiKeys[p.service_id] = names.map((name) => (
        { id: `sak_${name}`, name, last_used: null, created_at: "2026-05-01T08:00:00Z" }
      ));
      return p;
    }

    it("a hosted provider lists its service keys here, to revoke only", async () => {
      hostedWithKeys(["old", "older"]);
      mountSettings("openrouter");
      const section = await keys();
      expect(section).toHaveTextContent("Apps use a gateway key.");
      expect(section).toHaveTextContent("This provider also has 2 API keys from before gateway keys. They keep working until you revoke them.");
      const table = await within(section).findByRole("table", { name: "API keys" });
      expect(await within(table).findByText("old")).toBeInTheDocument();
      expect(within(table).getByRole("button", { name: "Revoke key older" })).toBeInTheDocument();
      // New keys for models are gateway keys.
      expect(within(section).queryByRole("button", { name: "New key" })).toBeNull();
      // No link to the hidden backing service.
      expect(document.querySelector('a[href^="/services"]')).toBeNull();
    });

    it("a hosted provider: a service key is revoked here, and the count follows", async () => {
      const p = hostedWithKeys(["old", "older"]);
      mountSettings("openrouter");
      const section = await keys();
      const table = await within(section).findByRole("table", { name: "API keys" });
      await userEvent.click(await within(table).findByRole("button", { name: "Revoke key old" }));
      const dialog = await screen.findByRole("dialog", { name: "Revoke API key" });
      await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
      await waitFor(() => expect(db.serviceApiKeys[p.service_id]!.map((k) => k.name)).toEqual(["older"]));
      await waitFor(() => expect(section).toHaveTextContent("This provider also has 1 API key from before gateway keys. It keeps working until you revoke it."));
      expect(within(table).queryByText("old")).toBeNull();
      expect(within(table).getByText("older")).toBeInTheDocument();
      // One toaster on the page, with the key list on it too.
      expect(document.querySelectorAll("section[aria-label^='Notifications']").length).toBe(1);
    });

    it("a hosted provider: revoking the last service key leaves the gateway-key line only", async () => {
      hostedWithKeys(["old"]);
      mountSettings("openrouter");
      const section = await keys();
      await userEvent.click(await within(section).findByRole("button", { name: "Revoke key old" }));
      await userEvent.click(within(await screen.findByRole("dialog", { name: "Revoke API key" })).getByRole("button", { name: "Revoke" }));
      await waitFor(() => expect(section).not.toHaveTextContent(/API keys?/));
      expect(within(section).queryByRole("table")).toBeNull();
      expect(section).toHaveTextContent("Apps use a gateway key.");
    });

    it("a hosted provider without service keys shows the gateway-key line only", async () => {
      mountSettings("openai-team");
      const section = await keys();
      expect(section).toHaveTextContent("Apps use a gateway key.");
      expect(section).not.toHaveTextContent(/API keys?/);
      expect(within(section).queryByRole("table")).toBeNull();
      expect(document.querySelector('a[href^="/services"]')).toBeNull();
    });
  });

  it("the Catalog tab lists the stored models and the example uses the first of them", async () => {
    db.aiProviderModels["ollama"] = [
      { id: "qwen3:8b", display_name: "", context_length: 0, synced_at: "2026-10-05T00:00:00Z" },
    ];
    mount();
    expect(await screen.findByText(/"model": "qwen3:8b"/)).toBeInTheDocument();
    await openTab("Catalog");
    expect(await within(await screen.findByRole("table", { name: "Models" })).findByText("qwen3:8b")).toBeInTheDocument();
  });

  it("a provider of the Anthropic format is used in an Anthropic-compatible app", async () => {
    mountAt("/gateway/providers/zai-anthropic");
    expect(await screen.findByText("Use this as the base URL of any Anthropic-compatible app, with a gateway key.")).toBeInTheDocument();
    // What such an app takes: it adds /v1/messages itself.
    expect(screen.getByText("https://tunnels.example.com/ai/zai-anthropic")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy settings for Claude Code" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Copy curl example" })).toBeNull();
  });

  describe("direct provider", () => {
    it("shows the same three tabs: its upstream settings are on Settings", async () => {
      mountDirect();
      expect(await screen.findByRole("heading", { name: "Provider · OpenRouter" })).toBeInTheDocument();
      expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Overview", "Catalog", "Settings"]);
      expect(screen.getByText("https://tunnels.example.com/ai/openrouter/v1")).toBeInTheDocument();
    });

    it("has nothing that assumes a tunnel or a client", async () => {
      mountDirect();
      await screen.findByRole("heading", { name: "Use it in an app" });
      const nothing = () => {
        expect(screen.queryByRole("switch", { name: /pause provider/i })).toBeNull();
        expect(screen.queryByRole("region", { name: "Client" })).toBeNull();
        expect(document.body).not.toHaveTextContent(/last seen|last served|burrow http/i);
        // No link to a client, nor to the hidden backing service.
        expect(document.querySelector('a[href^="/clients"]')).toBeNull();
        expect(document.querySelector('a[href^="/services"]')).toBeNull();
      };
      nothing();
      await openTab("Settings");
      await screen.findByRole("region", { name: "Upstream" });
      nothing();
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

    it("Upstream settings: shows where requests go and that the credential is configured", async () => {
      mountDirect("openrouter", { extra_headers: { "X-Title": "Burrow" } });
      await openTab("Settings");
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

    it("Upstream settings: names the variable to set when the credential is missing", async () => {
      mountDirect("zai");
      await openTab("Settings");
      const panel = screen.getByRole("tabpanel");
      expect(within(panel).getByText("not configured", { selector: "span.badge" })).toBeInTheDocument();
      const note = within(panel).getByRole("note");
      expect(note).toHaveTextContent("BURROW_UPSTREAM_KEY_ZAI");
      expect(note).toHaveTextContent(/restart/i);
      expect(within(panel).getByText("Flat rate")).toBeInTheDocument();
    });

    it("says when the gateway is skipping the provider", async () => {
      db.aiBreakerOpen.add("openrouter");
      mountDirect();
      const note = await screen.findByText(
        "Burrow is skipping this provider for now because recent requests failed. It will be tried again automatically.",
      );
      expect(note.closest('[role="note"]')).toHaveAttribute("data-variant", "info");
      // Above the tabs, whichever is open.
      const tabs = screen.getByRole("tablist");
      expect(note.compareDocumentPosition(tabs) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });

    it("says nothing about skipping while the provider is used", async () => {
      mountDirect();
      await screen.findByRole("tablist");
      expect(screen.queryByText(/skipping this provider/)).toBeNull();
    });

    it("Upstream settings: lists every slot of a provider with several, each configured or not", async () => {
      db.upstreamSlots.push("ZAI");
      mountDirect("zai", { credential_slot: "ZAI,ZAI2" });
      await openTab("Settings");
      const panel = screen.getByRole("tabpanel");
      const slots = within(panel).getByRole("list", { name: "Credential slots, in the order they are tried" });
      const items = within(slots).getAllByRole("listitem");
      expect(items).toHaveLength(2);
      expect(items[0]).toHaveTextContent("ZAI");
      expect(within(items[0]!).getByText("configured", { selector: "span.badge" })).toBeInTheDocument();
      expect(items[1]).toHaveTextContent("ZAI2");
      expect(within(items[1]!).getByText("not configured", { selector: "span.badge" })).toBeInTheDocument();
      // Only the slot that is missing is named in the note.
      const note = within(panel).getByRole("note");
      expect(note).toHaveTextContent("Slot ZAI2 is not set on the relay");
      expect(note).toHaveTextContent("BURROW_UPSTREAM_KEY_ZAI2");
      expect(note).not.toHaveTextContent(/BURROW_UPSTREAM_KEY_ZAI\b(?!2)/);
    });

    it("Upstream settings: the edit dialog takes several slots and sends them joined", async () => {
      let put: Record<string, unknown> | null = null;
      server.use(http.put("/api/v1/ai/providers/openrouter/upstream", async ({ request }) => {
        put = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ error: "stop here" }, { status: 500 });
      }));
      mountDirect();
      await openTab("Settings");
      await userEvent.click(within(screen.getByRole("tabpanel")).getByRole("button", { name: "Edit" }));
      const dialog = await screen.findByRole("dialog");
      const slot = within(dialog).getByLabelText("Credential slot");
      expect(slot).toHaveAccessibleDescription(/One slot, or up to four separated by commas — tried in order\./);
      await userEvent.type(slot, ", backup");
      expect(slot).toHaveValue("OPENROUTER, BACKUP");
      // Under the field, one line per name: the first is set on the relay, the second is not.
      const lines = within(await within(dialog).findByRole("list", { name: "Credential slots on the relay" })).getAllByRole("listitem");
      expect(lines.map((li) => li.textContent)).toEqual([
        "OPENROUTER is set on the relay.",
        "BACKUP is not set. Add BURROW_UPSTREAM_KEY_BACKUP to the relay's environment and restart it.",
      ]);
      expect(within(dialog).getByRole("button", { name: "Copy BURROW_UPSTREAM_KEY_BACKUP" })).toBeInTheDocument();
      expect(within(dialog).queryByText(/BURROW_UPSTREAM_KEY_OPENROUTER/)).toBeNull();
      // The names set on the relay that the field does not hold yet are offered.
      const offered = document.getElementById(slot.getAttribute("list") ?? "")!.querySelectorAll("option");
      expect(Array.from(offered).map((o) => o.value)).toEqual(["OPENROUTER, ANTHROPIC_TEAM_A", "OPENROUTER, OPENAI"]);
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(put).toEqual({ credential_slot: "OPENROUTER,BACKUP" }));
    });

    it("Upstream settings: a non-admin reads it and gets no Edit", async () => {
      db.me = { ...db.me, role: "user" };
      mountDirect();
      await openTab("Settings");
      const panel = screen.getByRole("tabpanel");
      expect(within(panel).getByText("https://openrouter.ai/api/v1")).toBeInTheDocument();
      expect(within(panel).queryByRole("button", { name: "Edit" })).toBeNull();
      expect(within(panel).queryByText("Auth header")).toBeNull();
    });

    async function openEdit() {
      await openTab("Settings");
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

    it("Edit upstream: a refusal that may pass on a second try does not block Save", async () => {
      let calls = 0;
      server.use(http.put("/api/v1/ai/providers/openrouter/upstream", () => {
        calls++;
        return HttpResponse.json({ error: "base URL host could not be resolved" }, { status: 400 });
      }));
      mountDirect();
      const { dialog } = await openEdit();
      const url = within(dialog).getByLabelText("Base URL");
      await userEvent.type(url, "x");
      const save = within(dialog).getByRole("button", { name: "Save" });
      await userEvent.click(save);
      await waitFor(() => expect(url).toHaveAccessibleDescription("base URL host could not be resolved"));
      // Nothing was edited, yet the same request can be sent again.
      expect(save).toBeEnabled();
      await userEvent.click(save);
      await waitFor(() => expect(calls).toBe(2));
    });

    it("Edit upstream: a retry takes the last refusal off the screen while it is pending", async () => {
      let calls = 0;
      let release!: () => void;
      const held = new Promise<void>((resolve) => { release = resolve; });
      server.use(http.put("/api/v1/ai/providers/openrouter/upstream", async () => {
        if (++calls > 1) await held;
        return HttpResponse.json({ error: "base URL host could not be resolved" }, { status: 400 });
      }));
      mountDirect();
      const { dialog } = await openEdit();
      const url = within(dialog).getByLabelText("Base URL");
      await userEvent.type(url, "x");
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(url).toHaveAttribute("aria-invalid", "true"));
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      // The second attempt is on its way: the first verdict is gone.
      await within(dialog).findByRole("button", { name: "Saving…" });
      expect(url).not.toHaveAttribute("aria-invalid");
      expect(within(dialog).queryByText("base URL host could not be resolved")).toBeNull();
      release();
      await waitFor(() => expect(url).toHaveAccessibleDescription("base URL host could not be resolved"));
    });

    it("Edit upstream: a slot name ending in _FILE is refused before sending", async () => {
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      fetchSpy.mockClear();
      mountDirect();
      const { dialog } = await openEdit();
      const slot = within(dialog).getByLabelText("Credential slot");
      await userEvent.clear(slot);
      await userEvent.type(slot, "openrouter_file");
      expect(slot).toHaveValue("OPENROUTER_FILE");
      expect(slot).toHaveAttribute("aria-invalid", "true");
      expect(slot).toHaveAccessibleDescription(
        "A slot name cannot end in _FILE: the relay reads BURROW_UPSTREAM_KEY_OPENROUTER_FILE as the path of a key file for slot OPENROUTER.",
      );
      expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
      expect(upstreamPuts(fetchSpy)).toEqual([]);
    });

    it("Edit upstream: editing any field clears what the server said about another", async () => {
      server.use(http.put("/api/v1/ai/providers/openrouter/upstream", () =>
        HttpResponse.json({ error: 'extra header "X-Title" is not allowed' }, { status: 400 })));
      mountDirect("openrouter", { extra_headers: { "X-Title": "Burrow" } });
      const { dialog } = await openEdit();
      const authHeader = within(dialog).getByLabelText("Auth header");
      await userEvent.clear(authHeader);
      await userEvent.type(authHeader, "X-Title");
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      const extra = within(dialog).getByLabelText("Extra headers");
      await waitFor(() => expect(extra).toHaveAttribute("aria-invalid", "true"));
      expect(extra).toHaveAccessibleDescription(/extra header "X-Title" is not allowed/);
      // The cause was the auth header: changing it back clears the verdict on the untouched field.
      await userEvent.clear(authHeader);
      await userEvent.type(authHeader, "Authorization");
      expect(extra).not.toHaveAttribute("aria-invalid");
      await userEvent.type(authHeader, "2");
      expect(within(dialog).getByRole("button", { name: "Save" })).toBeEnabled();
    });

    it("Routing: the backing service is not shown by id", async () => {
      const p = addDirectProvider("openrouter", { name: "OpenRouter", credential_slot: "OPENROUTER" });
      db.aiConfigs[p.service_id] = {
        ...db.aiConfigs["svc_ai001"]!,
        routing: { ...db.aiConfigs["svc_ai001"]!.routing, backends: [{ service_id: p.service_id, weight: 1, concrete_model: "acme/large-1" }] },
      };
      mountAt("/gateway/providers/openrouter");
      await openRouting();
      const table = await screen.findByRole("table", { name: /backends/i });
      expect(within(table).getByText("This provider")).toBeInTheDocument();
      expect(within(table).queryByText("prov-openrouter")).toBeNull();
    });

    it("delete says that the API keys and the model list go with it", async () => {
      mountDirect();
      await openTab("Settings");
      await userEvent.click(await screen.findByRole("button", { name: "Delete provider" }));
      const dialog = await screen.findByRole("dialog", { name: "Delete provider · OpenRouter" });
      expect(dialog).toHaveTextContent("Its API keys, its AI configuration and its model list are deleted with it.");
      expect(dialog).toHaveTextContent("The credential slot OPENROUTER on the relay is not touched.");
      expect(dialog).not.toHaveTextContent(/are kept/);
      await userEvent.click(within(dialog).getByRole("button", { name: "Delete provider" }));
      expect(await screen.findByText("PROVIDERS_PAGE")).toBeInTheDocument();
      expect(db.services.some((s) => s.id === "prov-openrouter")).toBe(false);
    });
  });

  describe("Responses API", () => {
    const RESPONSES = "Offers the Responses API (needed by Codex)";

    function providerPuts(spy: ReturnType<typeof vi.spyOn>, slug: string, suffix = "") {
      return (spy.mock.calls as [unknown, RequestInit | undefined][])
        .filter(([url, init]) => String(url).endsWith(`/api/v1/ai/providers/${slug}${suffix}`) && init?.method === "PUT")
        .map(([, init]) => JSON.parse(String(init!.body)) as Record<string, unknown>);
    }

    it("tunnel provider: Settings flags it through PUT, and the state survives a refetch", async () => {
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      fetchSpy.mockClear();
      const first = mountSettings();
      const box = await screen.findByRole("checkbox", { name: RESPONSES });
      expect(box).not.toBeChecked();
      expect(box).toHaveAccessibleDescription(
        "Leave off unless the provider documents POST /responses. While off, /openai/v1/responses refuses this provider's models.",
      );
      await userEvent.click(box);
      await waitFor(() => expect(providerPuts(fetchSpy, "ollama")).toEqual([{ slug: "ollama", name: "ollama", supports_responses: true }]));
      await waitFor(() => expect(db.aiProviders[0]).toMatchObject({ slug: "ollama", supports_responses: true }));
      await waitFor(() => expect(screen.getByRole("checkbox", { name: RESPONSES })).toBeChecked());
      // A fresh page reads it from the server.
      first.unmount();
      mountSettings();
      expect(await screen.findByRole("checkbox", { name: RESPONSES })).toBeChecked();
      await userEvent.click(screen.getByRole("checkbox", { name: RESPONSES }));
      await waitFor(() => expect(db.aiProviders[0]).toMatchObject({ supports_responses: false }));
      await waitFor(() => expect(screen.getByRole("checkbox", { name: RESPONSES })).not.toBeChecked());
    });

    it("tunnel provider: a refusal is shown and the box goes back", async () => {
      server.use(http.put("/api/v1/ai/providers/ollama", () =>
        HttpResponse.json({ error: "the provider was changed by someone else at the same time; try again" }, { status: 409 })));
      mountSettings();
      const box = await screen.findByRole("checkbox", { name: RESPONSES });
      await userEvent.click(box);
      expect(await screen.findByText("the provider was changed by someone else at the same time; try again")).toBeInTheDocument();
      expect(screen.getByRole("checkbox", { name: RESPONSES })).not.toBeChecked();
      expect(db.aiProviders[0]!.supports_responses ?? false).toBe(false);
    });

    it("tunnel provider: a non-admin reads the state and cannot change it", async () => {
      db.me = { ...db.me, role: "user" };
      db.aiProviders[0]!.supports_responses = true;
      mountSettings();
      expect(await screen.findByText("This provider offers the Responses API.")).toBeInTheDocument();
      expect(screen.queryByRole("checkbox", { name: RESPONSES })).toBeNull();
    });

    it("tunnel provider: a non-admin is told which endpoint refuses its models while it is off", async () => {
      db.me = { ...db.me, role: "user" };
      mountSettings();
      expect(await screen.findByText("This provider does not offer the Responses API: /openai/v1/responses refuses its models.")).toBeInTheDocument();
      expect(screen.queryByText(/refused for this provider/)).toBeNull();
    });

    it("direct provider: the upstream settings show it and Edit sends only the flag", async () => {
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      fetchSpy.mockClear();
      mountDirect();
      await openTab("Settings");
      const panel = screen.getByRole("tabpanel");
      expect(within(panel).getByText("Responses API")).toBeInTheDocument();
      expect(within(panel).getByText("not offered")).toBeInTheDocument();
      // The control of its own is for providers without upstream settings.
      expect(within(panel).queryByRole("checkbox", { name: RESPONSES })).toBeNull();
      await userEvent.click(within(panel).getByRole("button", { name: "Edit" }));
      const dialog = await screen.findByRole("dialog", { name: "Edit upstream · OpenRouter" });
      const box = within(dialog).getByRole("checkbox", { name: RESPONSES });
      expect(box).not.toBeChecked();
      expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
      await userEvent.click(box);
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(providerPuts(fetchSpy, "openrouter", "/upstream")).toEqual([{ supports_responses: true }]);
      expect(db.aiProviders.at(-1)).toMatchObject({ slug: "openrouter", supports_responses: true });
      expect(await within(screen.getByRole("tabpanel")).findByText("offered")).toBeInTheDocument();
      expect(providerPuts(fetchSpy, "openrouter")).toEqual([]);
    });

    it("the control is on Settings, not on Overview", async () => {
      mount();
      await screen.findByRole("heading", { name: "Use it in an app" });
      expect(screen.queryByRole("checkbox", { name: RESPONSES })).toBeNull();
      await openTab("Settings");
      expect(await screen.findByRole("checkbox", { name: RESPONSES })).toBeInTheDocument();
    });

    it("a provider of the Anthropic format is not offered the choice", async () => {
      mountDirect("openrouter", { api_format: "anthropic" });
      await openTab("Settings");
      const panel = screen.getByRole("tabpanel");
      expect(within(panel).queryByText("Responses API")).toBeNull();
      await userEvent.click(within(panel).getByRole("button", { name: "Edit" }));
      const dialog = await screen.findByRole("dialog", { name: "Edit upstream · OpenRouter" });
      expect(within(dialog).queryByRole("checkbox", { name: RESPONSES })).toBeNull();
    });
  });

  describe("Requests at once", () => {
    const FIELD = "Requests at once";
    const HELP = "How many requests this provider serves in parallel. More wait for a free place. Leave empty for no limit — set it for a local model on one GPU.";
    const RANGE = "max concurrent requests must be between 0 and 1000";

    function providerPuts(spy: ReturnType<typeof vi.spyOn>, slug: string, suffix = "") {
      return (spy.mock.calls as [unknown, RequestInit | undefined][])
        .filter(([url, init]) => String(url).endsWith(`/api/v1/ai/providers/${slug}${suffix}`) && init?.method === "PUT")
        .map(([, init]) => JSON.parse(String(init!.body)) as Record<string, unknown>);
    }

    it("tunnel provider: saving 2 sends max_concurrent 2, clearing the field sends 0", async () => {
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      fetchSpy.mockClear();
      db.aiProviderInUse.ollama = 1;
      mountSettings();
      const field = await screen.findByRole("textbox", { name: FIELD });
      expect(field).toHaveValue("");
      expect(field).toHaveAccessibleDescription(HELP);
      const save = screen.getByRole("button", { name: "Save limit" });
      expect(save).toBeDisabled(); // nothing to save yet
      expect(screen.queryByText(/in use$/)).toBeNull();

      await userEvent.type(field, "2");
      await userEvent.click(save);
      await waitFor(() => expect(providerPuts(fetchSpy, "ollama")).toEqual([{ slug: "ollama", name: "ollama", max_concurrent: 2 }]));
      await waitFor(() => expect(db.aiProviders[0]).toMatchObject({ slug: "ollama", max_concurrent: 2 }));
      expect(await screen.findByText("1 of 2 in use")).toBeInTheDocument();
      await waitFor(() => expect(screen.getByRole("button", { name: "Save limit" })).toBeDisabled());

      await userEvent.clear(screen.getByRole("textbox", { name: FIELD }));
      await userEvent.click(screen.getByRole("button", { name: "Save limit" }));
      await waitFor(() => expect(providerPuts(fetchSpy, "ollama")).toHaveLength(2));
      expect(providerPuts(fetchSpy, "ollama")[1]).toEqual({ slug: "ollama", name: "ollama", max_concurrent: 0 });
      await waitFor(() => expect(db.aiProviders[0]).toMatchObject({ max_concurrent: 0 }));
      await waitFor(() => expect(screen.queryByText(/in use$/)).toBeNull());
    });

    it("tunnel provider: a fresh page shows the stored limit", async () => {
      db.aiProviders[0]!.max_concurrent = 3;
      mountSettings();
      expect(await screen.findByRole("textbox", { name: FIELD })).toHaveValue("3");
      expect(screen.getByText("0 of 3 in use")).toBeInTheDocument();
    });

    it("tunnel provider: a value of 1001 shows the server's reason on the field", async () => {
      mountSettings();
      const field = await screen.findByRole("textbox", { name: FIELD });
      await userEvent.type(field, "1001");
      await userEvent.click(screen.getByRole("button", { name: "Save limit" }));
      expect(await screen.findByRole("alert")).toHaveTextContent(RANGE);
      expect(field).toBeInvalid();
      expect(field).toHaveAccessibleDescription(RANGE);
      expect(db.aiProviders[0]!.max_concurrent ?? 0).toBe(0);
      // The next edit takes the refusal away.
      await userEvent.type(field, "{backspace}");
      expect(screen.queryByText(RANGE)).toBeNull();
      expect(field).toBeValid();
    });

    it("tunnel provider: anything but a whole number is refused before sending", async () => {
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      fetchSpy.mockClear();
      mountSettings();
      const field = await screen.findByRole("textbox", { name: FIELD });
      await userEvent.type(field, "1.5");
      expect(await screen.findByRole("alert")).toHaveTextContent("Enter a whole number, or leave empty for no limit.");
      expect(field).toBeInvalid();
      expect(screen.getByRole("button", { name: "Save limit" })).toBeDisabled();
      expect(providerPuts(fetchSpy, "ollama")).toEqual([]);
    });

    it("tunnel provider: a refusal that is not about the value shows below the field", async () => {
      server.use(http.put("/api/v1/ai/providers/ollama", () =>
        HttpResponse.json({ error: "the provider was changed by someone else at the same time; try again" }, { status: 409 })));
      mountSettings();
      const field = await screen.findByRole("textbox", { name: FIELD });
      await userEvent.type(field, "2");
      await userEvent.click(screen.getByRole("button", { name: "Save limit" }));
      expect(await screen.findByText("the provider was changed by someone else at the same time; try again")).toBeInTheDocument();
      expect(field).toBeValid();
      expect(field).toHaveValue("2"); // what was typed is kept for a second try
    });

    it("tunnel provider: a non-admin reads the limit and cannot change it", async () => {
      db.me = { ...db.me, role: "user" };
      db.aiProviders[0]!.max_concurrent = 2;
      mountSettings();
      expect(await screen.findByText("Serves at most 2 requests at once; more wait for a free place.")).toBeInTheDocument();
      expect(screen.queryByRole("textbox", { name: FIELD })).toBeNull();
      expect(screen.queryByRole("button", { name: "Save limit" })).toBeNull();
    });

    it("tunnel provider: a non-admin is told when there is no limit", async () => {
      db.me = { ...db.me, role: "user" };
      mountSettings();
      expect(await screen.findByText("No limit on requests served at once.")).toBeInTheDocument();
    });

    it("direct provider: the upstream settings show it and Edit sends only the limit", async () => {
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      fetchSpy.mockClear();
      db.aiProviderInUse.openrouter = 1;
      mountDirect();
      await openTab("Settings");
      const panel = screen.getByRole("tabpanel");
      // The field of its own is for providers without upstream settings.
      expect(within(panel).queryByRole("textbox", { name: FIELD })).toBeNull();
      expect(within(panel).getByText(FIELD)).toBeInTheDocument();
      expect(within(panel).getByText("no limit")).toBeInTheDocument();
      await userEvent.click(within(panel).getByRole("button", { name: "Edit" }));
      const dialog = await screen.findByRole("dialog", { name: "Edit upstream · OpenRouter" });
      const field = within(dialog).getByRole("textbox", { name: FIELD });
      expect(field).toHaveValue("");
      expect(field).toHaveAccessibleDescription(HELP);
      expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
      await userEvent.type(field, "4");
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(providerPuts(fetchSpy, "openrouter", "/upstream")).toEqual([{ max_concurrent: 4 }]);
      expect(db.aiProviders.at(-1)).toMatchObject({ slug: "openrouter", max_concurrent: 4 });
      expect(await within(screen.getByRole("tabpanel")).findByText("1 of 4 in use")).toBeInTheDocument();

      // Clearing the field lifts the limit.
      await userEvent.click(within(screen.getByRole("tabpanel")).getByRole("button", { name: "Edit" }));
      const again = await screen.findByRole("dialog", { name: "Edit upstream · OpenRouter" });
      expect(within(again).getByRole("textbox", { name: FIELD })).toHaveValue("4");
      await userEvent.clear(within(again).getByRole("textbox", { name: FIELD }));
      await userEvent.click(within(again).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(providerPuts(fetchSpy, "openrouter", "/upstream")[1]).toEqual({ max_concurrent: 0 });
      expect(await within(screen.getByRole("tabpanel")).findByText("no limit")).toBeInTheDocument();
    });

    it("direct provider: 1001 shows the server's reason on the field and keeps the dialog open", async () => {
      mountDirect();
      await openTab("Settings");
      await userEvent.click(within(screen.getByRole("tabpanel")).getByRole("button", { name: "Edit" }));
      const dialog = await screen.findByRole("dialog", { name: "Edit upstream · OpenRouter" });
      const field = within(dialog).getByRole("textbox", { name: FIELD });
      await userEvent.type(field, "1001");
      await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      expect(await within(dialog).findByText(RANGE)).toBeInTheDocument();
      expect(field).toBeInvalid();
      expect(field).toHaveAccessibleDescription(RANGE);
      expect(db.aiProviders.at(-1)!.max_concurrent ?? 0).toBe(0);
      // Not a whole number: Save is blocked before anything is sent.
      await userEvent.clear(field);
      await userEvent.type(field, "two");
      expect(within(dialog).getByText("Enter a whole number, or leave empty for no limit.")).toBeInTheDocument();
      expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    });
  });

  it("mounts exactly one toaster, whichever tab is open", async () => {
    mount();
    await screen.findByRole("heading", { name: "Use it in an app" });
    const sections = () => document.querySelectorAll("section[aria-label^='Notifications']").length;
    expect(sections()).toBe(1);
    await openTab("Catalog");
    await screen.findByRole("table", { name: "Models" });
    expect(sections()).toBe(1);
    await openRouting();
    expect(sections()).toBe(1);
  });

  it("describes the whole page in its subtitle", async () => {
    mount();
    await screen.findByRole("heading", { name: "Use it in an app" });
    expect(screen.getByText("Status, use in apps, catalog and settings of this provider.")).toBeInTheDocument();
  });
});
