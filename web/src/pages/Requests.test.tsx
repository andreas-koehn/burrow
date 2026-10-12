import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useLocation } from "react-router-dom";
import { delay, http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, resetDb } from "@/mocks/db";
import type { InspectorEntry } from "@/lib/contract";
import Requests from "@/pages/Requests";

function Probe() {
  const { pathname, search } = useLocation();
  return <div data-testid="path">{pathname + search}</div>;
}

function mount(route = "/gateway/requests") {
  return renderApp(<><Requests /><Probe /></>, route);
}

const minutesAgo = (n: number) => new Date(Date.now() - n * 60_000).toISOString();

/**
 * Captured requests in the buffer of a provider's service, the first one `ages[0]`
 * minutes old. They name no provider, model or gateway key unless `over` does.
 */
function capture(serviceId: string, ages: number[], over: Partial<InspectorEntry> = {}) {
  const seed = db.inspectorEntries.svc_ai001![0]!;
  db.inspectorEntries[serviceId] = ages.map((age, i) => ({
    ...seed, id: `ie_${serviceId}_${i}`, service_id: serviceId, ts: minutesAgo(age),
    provider: "", model: "", gateway_key_id: "", ...over,
  }));
}

/** Requests made with the gateway key "laptop". */
const viaGateway = (model: string): Partial<InspectorEntry> => ({ api_key_id: "", model, gateway_key_id: "gk_laptop1" });

const table = () => screen.findByRole("table", { name: "Requests" });
const bodyRows = (t: HTMLElement) => Array.from(t.querySelectorAll<HTMLElement>("tbody tr"));
/** What a column shows, row by row. */
const column = (t: HTMLElement, at: number) => bodyRows(t).map((r) => r.querySelectorAll("td")[at]!.textContent);
const PROVIDER = 1;
const MODEL = 2;

// The ds Select is a button with a listbox, not a native <select>.
const picker = (label: string) => screen.getByRole("combobox", { name: label });
async function choose(label: string, option: string) {
  await userEvent.click(picker(label));
  await userEvent.click(await screen.findByRole("option", { name: option }));
}
async function optionsOf(label: string) {
  await userEvent.click(picker(label));
  const names = (await screen.findAllByRole("option")).map((o) => o.textContent);
  await userEvent.keyboard("{Escape}");
  return names;
}

/** An older relay: no list across providers. */
const olderRelay = () => server.use(http.get("/api/v1/ai/requests", () => new HttpResponse(null, { status: 404 })));

describe("Requests page", () => {
  afterEach(() => {
    resetDb();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  const urls = (spy: { mock: { calls: unknown[][] } }) => spy.mock.calls.map(([url]) => String(url));
  /** The query strings of the list requests sent so far. */
  const listCalls = (spy: { mock: { calls: unknown[][] } }) =>
    urls(spy).filter((u) => u.includes("/ai/requests?")).map((u) => new URL(u, "http://x").searchParams);

  it("renders the heading 'Requests' and says the list spans the providers", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: /^requests$/i, level: 1 })).toBeInTheDocument();
    expect(screen.getByText("Requests the relay captured, across all providers. Open one to inspect and replay it.")).toBeInTheDocument();
  });

  it("names the table Requests and shows the columns the relay records", async () => {
    capture("svc_ai001", [1, 2]);
    mount();
    const t = await table();
    expect(within(t).getAllByRole("columnheader").map((h) => h.textContent))
      .toEqual(["When", "Provider", "Model", "Path", "Status", "Latency", "Cache", "Guardrail", "Translation"]);
    for (const h of within(t).getAllByRole("columnheader")) expect(h).toHaveAttribute("scope", "col");
  });

  it("opens on all providers", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    capture("svc_ai001", [1, 3]);
    capture("prov-zai", [2]);
    mount();
    expect(column(await table(), PROVIDER)).toEqual(["ollama", "zai", "ollama"]);
    expect(picker("Provider")).toHaveTextContent("All");
    expect(picker("Model")).toHaveTextContent("All");
    expect(picker("Gateway key")).toHaveTextContent("All");
    const sent = listCalls(spy).at(-1)!;
    for (const name of ["provider", "model", "gateway_key"]) expect(sent.has(name)).toBe(false);
    // One list for all of them, not one per service.
    expect(urls(spy).filter((u) => u.includes("/inspector/requests"))).toEqual([]);
  });

  it("filters by provider, model and gateway key, and keeps them in the URL", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    capture("svc_ai001", [1]);
    capture("prov-zai", [2, 3, 4], viaGateway("burrow-intelligence"));
    db.inspectorEntries["prov-zai"]![1] = { ...db.inspectorEntries["prov-zai"]![1]!, model: "zai/glm-5.1" };
    db.inspectorEntries["prov-zai"]![2] = { ...db.inspectorEntries["prov-zai"]![2]!, gateway_key_id: "gk_other" };
    const first = mount();
    expect(bodyRows(await table())).toHaveLength(4);

    await choose("Provider", "zai");
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?provider=zai");
    await waitFor(async () => expect(column(await table(), PROVIDER)).toEqual(["zai", "zai", "zai"]));
    expect(listCalls(spy).at(-1)!.get("provider")).toBe("zai");

    await choose("Model", "burrow-intelligence");
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?provider=zai&model=burrow-intelligence");
    await waitFor(async () => expect(bodyRows(await table())).toHaveLength(2));
    expect(listCalls(spy).at(-1)!.get("model")).toBe("burrow-intelligence");

    await choose("Gateway key", "laptop");
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?provider=zai&model=burrow-intelligence&gateway_key=gk_laptop1");
    await waitFor(async () => expect(bodyRows(await table())).toHaveLength(1));
    expect(listCalls(spy).at(-1)!.get("gateway_key")).toBe("gk_laptop1");

    // Opening that address again restores all three.
    first.unmount();
    mount("/gateway/requests?provider=zai&model=burrow-intelligence&gateway_key=gk_laptop1");
    expect(bodyRows(await table())).toHaveLength(1);
    expect(picker("Provider")).toHaveTextContent("zai");
    expect(picker("Model")).toHaveTextContent("burrow-intelligence");
    await waitFor(() => expect(picker("Gateway key")).toHaveTextContent("laptop"));

    // "All" takes a filter out again.
    await choose("Model", "All");
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?provider=zai&gateway_key=gk_laptop1");
    await waitFor(async () => expect(bodyRows(await table())).toHaveLength(2));
  });

  it("a link from a provider page arrives filtered", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    capture("svc_ai001", [1, 2]);
    capture("prov-zai", [1]);
    mount("/gateway/requests?provider=ollama");
    expect(column(await table(), PROVIDER)).toEqual(["ollama", "ollama"]);
    expect(picker("Provider")).toHaveTextContent("ollama");
    // Never asked for everything first.
    expect(listCalls(spy).map((p) => p.get("provider"))).toEqual(["ollama"]);
  });

  it("the pickers offer the providers by name, the models and the gateway keys, 'All' first", async () => {
    db.aiProviders[0] = { ...db.aiProviders[0]!, name: "Local Ollama" };
    capture("prov-zai", [1], viaGateway("zai/glm-5.1"));
    mount();
    await table();
    expect(await optionsOf("Provider")).toEqual(["All", "Local Ollama", "zai", "zai-anthropic", "llamacpp", "lmstudio", "openai-team"]);
    // A model a request asked for is offered even when GET /ai/models does not list it.
    await waitFor(async () => expect(await optionsOf("Model")).toEqual(["All", "burrow-simple", "burrow-intelligence", "zai/glm-5.1"]));
    expect(await optionsOf("Gateway key")).toEqual(["All", "laptop"]);
  });

  it("a filter the lists do not know stays selected and selectable", async () => {
    // A provider renamed since: its requests keep the old slug.
    capture("svc_ai001", [1], { provider: "ollama-old" });
    mount("/gateway/requests?provider=ollama-old&gateway_key=gk_gone");
    expect(await screen.findByText("No requests match these filters.")).toBeInTheDocument();
    expect(picker("Provider")).toHaveTextContent("ollama-old");
    expect(picker("Gateway key")).toHaveTextContent("gk_gone");
    await choose("Gateway key", "All");
    expect(column(await table(), PROVIDER)).toEqual(["ollama-old"]);
  });

  it("shows provider and model per row", async () => {
    db.aiProviders[0] = { ...db.aiProviders[0]!, name: "Local Ollama" };
    capture("svc_ai001", [1], viaGateway("burrow-simple"));
    capture("prov-zai", [2], viaGateway("burrow-intelligence"));
    mount();
    const t = await table();
    expect(column(t, PROVIDER)).toEqual(["Local Ollama", "zai"]);
    expect(column(t, MODEL)).toEqual(["burrow-simple", "burrow-intelligence"]);
  });

  it("a row opens the inspector of its own service", async () => {
    capture("svc_ai001", [1]);
    capture("prov-zai", [2], { path: "/v1/embeddings" });
    mount();
    const t = await table();
    expect(within(t).getByRole("link", { name: "/v1/chat/completions" }))
      .toHaveAttribute("href", "/gateway/requests/svc_ai001/ie_svc_ai001_0");
    expect(within(t).getByRole("link", { name: "/v1/embeddings" }))
      .toHaveAttribute("href", "/gateway/requests/prov-zai/ie_prov-zai_0");
  });

  it("an entry without provider or model shows a dash", async () => {
    // Made with a service API key: the relay names the provider, there is no model.
    capture("svc_ai001", [1]);
    const first = mount();
    const t = await table();
    expect(column(t, PROVIDER)).toEqual(["ollama"]);
    expect(column(t, MODEL)).toEqual(["—"]);
    first.unmount();

    // A relay that leaves the provider out as well.
    server.use(http.get("/api/v1/ai/requests", () => HttpResponse.json(db.inspectorEntries.svc_ai001)));
    mount();
    const bare = await table();
    expect(column(bare, PROVIDER)).toEqual(["—"]);
    expect(column(bare, MODEL)).toEqual(["—"]);
  });

  it("old links with ?service=<id> still filter", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    capture("svc_ai001", [1, 2]);
    capture("prov-zai", [1]);
    mount("/gateway/requests?service=prov-zai");
    expect(column(await table(), PROVIDER)).toEqual(["zai"]);
    expect(picker("Provider")).toHaveTextContent("zai");
    // The list waits for the provider behind the service; it is never asked for everything.
    expect(listCalls(spy).map((p) => p.get("provider"))).toEqual(["zai"]);
    // A choice in the picker replaces the old parameter.
    await choose("Provider", "ollama");
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?provider=ollama");
    await waitFor(async () => expect(column(await table(), PROVIDER)).toEqual(["ollama", "ollama"]));
  });

  it("an old link to a service that is no provider shows everything, without an error", async () => {
    capture("svc_ai001", [1, 2]);
    capture("prov-zai", [1]);
    mount("/gateway/requests?service=svc_gone");
    expect(bodyRows(await table())).toHaveLength(3);
    expect(picker("Provider")).toHaveTextContent("All");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("says when filters hide everything and offers to clear them", async () => {
    capture("svc_ai001", [1, 2]);
    mount("/gateway/requests?provider=zai&model=burrow-simple&q=chat");
    expect(await screen.findByText("No requests match these filters.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(bodyRows(await table())).toHaveLength(2);
    expect(screen.getByTestId("path")).toHaveTextContent(/\/gateway\/requests$/);
    expect(screen.getByRole("searchbox", { name: "Filter" })).toHaveValue("");
    expect(picker("Provider")).toHaveTextContent("All");
  });

  it("filters that hide older requests also offer the longer period", async () => {
    capture("svc_ai001", [3 * 24 * 60]);
    mount("/gateway/requests?provider=ollama");
    expect(await screen.findByText("No requests match these filters.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Clear filters" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Show the last 7 days" }));
    expect(bodyRows(await table())).toHaveLength(1);
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?provider=ollama&range=7d");
  });

  it("with no provider at all it links to Providers", async () => {
    db.aiProviders = [];
    mount();
    expect(await screen.findByText("No providers yet")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Add a provider" })).toHaveAttribute("href", "/gateway/providers");
    // The page itself stays: heading and all.
    expect(screen.getByRole("heading", { name: /^requests$/i, level: 1 })).toBeInTheDocument();
  });

  it("a provider list that cannot be read is not 'no providers': the requests still show", async () => {
    server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    capture("svc_ai001", [1], { provider: "ollama" });
    mount();
    expect(column(await table(), PROVIDER)).toEqual(["ollama"]);
    expect(screen.queryByText("No providers yet")).toBeNull();
  });

  it("falls back to one provider's list on an older relay", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    olderRelay();
    capture("svc_ai001", [1, 2]);
    capture("prov-zai", [1]);
    mount();
    // The first provider's requests, read from its service.
    expect(column(await table(), PROVIDER)).toEqual(["ollama", "ollama"]);
    expect(urls(spy).some((u) => u.includes("/services/svc_ai001/inspector/requests?"))).toBe(true);
    expect(screen.getByText("This relay lists requests one provider at a time.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    // One provider at a time: no "All", and nothing to tell models or gateway keys apart by.
    expect(picker("Provider")).toHaveTextContent("ollama");
    expect(await optionsOf("Provider")).not.toContain("All");
    expect(screen.queryByRole("combobox", { name: "Model" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Gateway key" })).toBeNull();

    await choose("Provider", "zai");
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?provider=zai");
    await waitFor(async () => expect(column(await table(), PROVIDER)).toEqual(["zai"]));
    expect(within(await table()).getByRole("link", { name: "/v1/chat/completions" }))
      .toHaveAttribute("href", "/gateway/requests/prov-zai/ie_prov-zai_0");
  });

  it("on an older relay the period and the Filter box go to the provider's own list", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    olderRelay();
    capture("prov-zai", [5, 30]);
    db.inspectorEntries["prov-zai"]![1] = { ...db.inspectorEntries["prov-zai"]![1]!, path: "/v1/embeddings" };
    mount("/gateway/requests?provider=zai&range=1h&q=embed");
    expect(bodyRows(await table())).toHaveLength(1);
    const sent = new URL(urls(spy).filter((u) => u.includes("/services/prov-zai/inspector/requests?")).at(-1)!, "http://x").searchParams;
    expect(sent.get("q")).toBe("embed");
    expect(sent.has("since")).toBe(true);
  });

  it("shows the latency, and a dash when none was measured", async () => {
    capture("svc_ai001", [1, 2], { duration_ms: 120 });
    db.inspectorEntries.svc_ai001![1] = { ...db.inspectorEntries.svc_ai001![1]!, duration_ms: 0 };
    mount();
    expect(column(await table(), 5)).toEqual(["120 ms", "—"]);
  });

  it("shows status, cache and the number of redactions", async () => {
    capture("svc_ai001", [1], { status: 502, cache: "HIT", redactions: [{ rule: "email", count: 2 }, { rule: "iban", count: 1 }] });
    mount();
    const row = within(bodyRows(await table())[0]!);
    expect(row.getByText("502")).toHaveClass("badge", "status-5xx");
    expect(row.getByText("HIT")).toBeInTheDocument();
    expect(row.getByText("3 redacted")).toBeInTheDocument();
  });

  it("marks a translated request in words and names the direction", async () => {
    capture("svc_ai001", [1], { translated: "messages-chat", dropped: ["cache_control", "top_k"] });
    mount();
    const row = within(bodyRows(await table())[0]!);
    expect(row.getByText("translated", { selector: "span.badge" })).toBeVisible();
    expect(row.getByText("Anthropic Messages → Chat Completions")).toHaveClass("visually-hidden");
    expect(row.getByText("2 left out")).toBeInTheDocument();
  });

  it("a request served in its own format has no translation mark", async () => {
    capture("svc_ai001", [1]);
    mount();
    const row = within(bodyRows(await table())[0]!);
    expect(row.queryByText("translated")).toBeNull();
    expect(row.queryByText(/left out/)).toBeNull();
  });

  it("the time range is sent to the relay as since and kept in the URL", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    capture("svc_ai001", [5, 30, 600]);
    mount();
    expect(bodyRows(await table())).toHaveLength(3);
    await userEvent.click(screen.getByRole("radio", { name: "15 min" }));
    await waitFor(async () => expect(bodyRows(await table())).toHaveLength(1));
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?range=15m");
    const age = Date.now() - Date.parse(listCalls(spy).at(-1)!.get("since")!);
    expect(age).toBeGreaterThan(14 * 60_000);
    expect(age).toBeLessThan(16 * 60_000);
  });

  it("the Filter box is sent to the relay as q", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    capture("svc_ai001", [1, 2]);
    db.inspectorEntries.svc_ai001![1] = { ...db.inspectorEntries.svc_ai001![1]!, path: "/v1/embeddings" };
    mount();
    await table();
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter" }), "embed");
    await waitFor(async () => expect(bodyRows(await table())).toHaveLength(1));
    expect(listCalls(spy).at(-1)!.get("q")).toBe("embed");
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?q=embed");
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter" }), "zzz");
    expect(await screen.findByText("No requests match these filters.")).toBeInTheDocument();
  });

  it("requests older than the period: says so and offers the last 7 days", async () => {
    capture("svc_ai001", [3 * 24 * 60]);
    mount();
    expect(await screen.findByText("No requests in this period")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Show the last 7 days" }));
    expect(bodyRows(await table())).toHaveLength(1);
  });

  it("requests older than 7 days stay reachable: Show all asks without since", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    capture("svc_ai001", [30 * 24 * 60]);
    mount("/gateway/requests?range=7d");
    expect(await screen.findByText("No requests in this period")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Show all" }));
    await waitFor(async () => expect(bodyRows(await table()).length).toBeGreaterThan(0));
    expect(screen.getByRole("radio", { name: "All" })).toBeChecked();
    expect(listCalls(spy).at(-1)!.has("since")).toBe(false);
  });

  it("without any captured request it says how the list fills", async () => {
    db.inspectorEntries = {};
    mount("/gateway/requests?range=all");
    expect(await screen.findByText("No requests yet")).toBeInTheDocument();
    expect(screen.getByText("Requests appear here as soon as traffic reaches a provider.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Show/ })).toBeNull();
  });

  it("says so when the relay returned a full page: only the newest 100 are shown", async () => {
    capture("svc_ai001", Array.from({ length: 100 }, (_, i) => i + 1));
    capture("prov-zai", [1, 2]);
    mount();
    expect(bodyRows(await table())).toHaveLength(100);
    expect(screen.getByText(/Showing the newest 100 requests that match/)).toBeInTheDocument();
    await choose("Provider", "zai");
    await waitFor(async () => expect(bodyRows(await table())).toHaveLength(2));
    expect(screen.queryByText(/Showing the newest 100 requests that match/)).toBeNull();
  });

  /** Request capture switched off for the services of these providers. */
  const captureOff = (...slugs: string[]) => {
    for (const p of db.aiProviders) {
      if (slugs.includes(p.slug)) db.aiConfigs[p.service_id] = { inspector: { enabled: false, max_requests: 100 } } as never;
    }
  };

  it("says when capturing is switched off for the chosen provider and where to turn it on", async () => {
    captureOff("ollama");
    mount("/gateway/requests?provider=ollama");
    expect(await screen.findByText("Request inspector is off for ollama")).toBeInTheDocument();
    // Nothing in the dashboard switches it; the hint names the API call instead of a page.
    const hint = screen.getByText(/The dashboard has no switch for this yet/);
    expect(hint).toHaveTextContent("inspector.enabled");
    expect(hint).toHaveTextContent("PUT /api/v1/services/svc_ai001/ai-config");
    expect(screen.queryByRole("link", { name: "Open the service" })).toBeNull();
    expect(document.querySelector("a[href^='/services/']")).toBeNull();
  });

  it("with all providers, capture that is off for some of them is not the reason given", async () => {
    captureOff("ollama", "zai");
    mount();
    expect(await screen.findByText("No requests in this period")).toBeInTheDocument();
    expect(screen.queryByText(/Request inspector is off/)).toBeNull();
    expect(screen.queryByText(/The dashboard has no switch for this yet/)).toBeNull();
  });

  it("with all providers, says so when capture is off for every one of them", async () => {
    captureOff(...db.aiProviders.map((p) => p.slug));
    mount();
    expect(await screen.findByText("Request inspector is off for every provider")).toBeInTheDocument();
    expect(screen.getByText(/The dashboard has no switch for this yet/)).toHaveTextContent("inspector.enabled");
    expect(screen.getByText("Choose a provider to see the call for it.")).toBeInTheDocument();
  });

  it("typing quickly sends one request, with the final q", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    capture("svc_ai001", [1, 2]);
    db.inspectorEntries.svc_ai001![1] = { ...db.inspectorEntries.svc_ai001![1]!, path: "/v1/embeddings" };
    mount();
    await table();
    const before = listCalls(spy).length;
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter" }), "embed");
    // The box and the URL follow every key; the relay is asked once typing pauses.
    expect(screen.getByRole("searchbox", { name: "Filter" })).toHaveValue("embed");
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?q=embed");
    await waitFor(async () => expect(bodyRows(await table())).toHaveLength(1));
    const sent = listCalls(spy).slice(before).map((p) => p.get("q"));
    expect(sent).toEqual(["embed"]);
  });

  it("keeps the rows on screen while a changed filter loads", async () => {
    capture("svc_ai001", [5, 30]);
    capture("prov-zai", [6]);
    mount();
    expect(bodyRows(await table())).toHaveLength(3);
    let release = () => {};
    const held = new Promise<void>((r) => { release = r; });
    server.use(http.get("/api/v1/ai/requests", async () => {
      await held;
      return HttpResponse.json([{ ...db.inspectorEntries["prov-zai"]![0], provider: "zai" }]);
    }));
    await choose("Provider", "zai");
    await new Promise((r) => setTimeout(r, 50));
    expect(document.querySelector(".skel")).toBeNull();
    // Every row names its provider, so the rows of the previous choice can stay.
    expect(bodyRows(screen.getByRole("table", { name: "Requests" }))).toHaveLength(3);
    release();
    await waitFor(() => expect(bodyRows(screen.getByRole("table", { name: "Requests" }))).toHaveLength(1));
  });

  it("an empty list does not speak for the new filter before its answer is in", async () => {
    capture("svc_ai001", [3 * 24 * 60]);
    mount();
    expect(await screen.findByText("No requests in this period")).toBeInTheDocument();
    let release = () => {};
    const held = new Promise<void>((r) => { release = r; });
    server.use(http.get("/api/v1/ai/requests", async () => {
      await held;
      return HttpResponse.json([]);
    }));
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter" }), "zzz");
    await new Promise((r) => setTimeout(r, 400));
    // The relay has not answered for "zzz" yet.
    expect(screen.queryByText("No requests match these filters.")).toBeNull();
    expect(document.querySelector(".skel")).not.toBeNull();
    release();
    expect(await screen.findByText("No requests match these filters.")).toBeInTheDocument();
  });

  it("rows have no detail button: a row opens the inspector", async () => {
    capture("svc_ai001", [1]);
    mount();
    await table();
    expect(screen.queryByRole("button", { name: /Show details/ })).toBeNull();
  });

  it("shows an error, not an empty state, when the requests cannot be loaded; Retry recovers", async () => {
    capture("svc_ai001", [1]);
    let calls = 0;
    server.use(http.get("/api/v1/ai/requests", () => {
      calls += 1;
      if (calls === 1) return HttpResponse.json({ error: "boom" }, { status: 500 });
      return HttpResponse.json(db.inspectorEntries.svc_ai001);
    }));
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Couldn't load requests: boom");
    expect(screen.queryByText("No requests yet")).not.toBeInTheDocument();
    // Any other failure than "no such route" is an error, not an older relay.
    expect(screen.queryByText("This relay lists requests one provider at a time.")).toBeNull();
    await userEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(bodyRows(await table())).toHaveLength(1);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("a caller who may not inspect requests is told so", async () => {
    server.use(http.get("/api/v1/ai/requests", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't load requests: forbidden");
  });

  it("shows a skeleton while the requests are loading", async () => {
    capture("svc_ai001", [1]);
    server.use(http.get("/api/v1/ai/requests", async () => {
      await delay(150);
      return HttpResponse.json(db.inspectorEntries.svc_ai001);
    }));
    const { container } = mount();
    await waitFor(() => expect(container.querySelector(".skel")).not.toBeNull());
    expect(screen.queryByRole("table", { name: "Requests" })).toBeNull();
    expect(bodyRows(await table())).toHaveLength(1);
    expect(container.querySelector(".skel")).toBeNull();
  });

  /** Stands in for the browser's EventSource, which jsdom does not have. */
  function stubEventSource() {
    const opened: { url: string; closed: boolean; emit: (type: string) => void }[] = [];
    vi.stubGlobal("EventSource", class {
      private listeners = new Map<string, Set<() => void>>();
      private self = { url: "", closed: false, emit: (type: string) => this.listeners.get(type)?.forEach((fn) => fn()) };
      constructor(url: string) { this.self.url = url; opened.push(this.self); }
      addEventListener(type: string, fn: () => void) {
        if (!this.listeners.has(type)) this.listeners.set(type, new Set());
        this.listeners.get(type)!.add(fn);
      }
      removeEventListener(type: string, fn: () => void) { this.listeners.get(type)?.delete(fn); }
      close() { this.self.closed = true; }
    });
    return opened;
  }
  const pause = (ms: number) => new Promise((r) => setTimeout(r, ms));

  it("with all providers the list is read again every 5 seconds, while the tab is visible", async () => {
    const opened = stubEventSource();
    // Only the interval is faked: the page's debounce and the test's own waits keep real time.
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    const spy = vi.spyOn(globalThis, "fetch");
    capture("svc_ai001", [1]);
    mount();
    expect(bodyRows(await table())).toHaveLength(1);
    expect(opened).toEqual([]);
    const before = listCalls(spy).length;

    vi.advanceTimersByTime(4900);
    await pause(50);
    expect(listCalls(spy)).toHaveLength(before);
    capture("svc_ai001", [1, 2]);
    vi.advanceTimersByTime(200);
    await pause(50);
    expect(listCalls(spy)).toHaveLength(before + 1);
    expect(bodyRows(await table())).toHaveLength(2);

    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    vi.advanceTimersByTime(5000);
    await pause(50);
    expect(listCalls(spy)).toHaveLength(before + 1);
    visibility.mockRestore();
  });

  it("with one provider chosen the list follows the event stream instead", async () => {
    const opened = stubEventSource();
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    const spy = vi.spyOn(globalThis, "fetch");
    capture("svc_ai001", [1]);
    const view = mount("/gateway/requests?provider=ollama");
    expect(bodyRows(await table())).toHaveLength(1);
    expect(opened.map((es) => es.url)).toEqual(["/api/v1/events"]);
    const before = listCalls(spy).length;

    vi.advanceTimersByTime(6000);
    await pause(50);
    expect(listCalls(spy)).toHaveLength(before);

    capture("svc_ai001", [1, 2]);
    opened[0]!.emit("request");
    await waitFor(() => expect(bodyRows(screen.getByRole("table", { name: "Requests" }))).toHaveLength(2));
    view.unmount();
    expect(opened[0]!.closed).toBe(true);
  });
});
