import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { Route, Routes, useLocation } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { addDirectProvider, db } from "@/mocks/db";
import Providers from "@/pages/Providers";

function mount() {
  return renderApp(<Providers />, "/gateway/providers");
}

function HashProbe() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + l.hash}</div>;
}

function mountAs(role: "admin" | "user") {
  db.me = { ...db.me, role };
  return mount();
}

describe("Providers page", () => {
  it("renders the four-tile metric strip with the spec tooltip", async () => {
    mount();
    const strip = await screen.findByRole("list", { name: "Provider metrics" });
    const { getAllByRole } = within(strip);
    const tiles = getAllByRole("listitem");
    expect(tiles).toHaveLength(4);
    // The four tiles, in spec order — DS MetricTile surfaces label in .label span.
    expect(tiles[0].querySelector(".label")?.textContent).toBe("Requests (24h)");
    expect(tiles[1].querySelector(".label")?.textContent).toBe("Tokens in/out (24h)");
    expect(tiles[2].querySelector(".label")?.textContent).toBe("Cost estimate (24h)");
    expect(tiles[3].querySelector(".label")?.textContent).toBe("Cache hit ratio (24h)");
    const cost = tiles[2];
    // DS MetricTile exposes tooltip via the title attribute.
    expect(cost).toHaveAttribute(
      "title",
      "Estimates from the bundled pricing table — operator-overridable.",
    );
  });

  it("renders one row per provider with name, mono alias, backend, key count, requests, cache, latency, status", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    // Anchor by the unique mono alias text (the name "ollama" also appears in
    // the backend badge, so it isn't a safe anchor on its own).
    const alias = within(table).getByText("fast → llama3.1:8b");
    expect(alias.className).toContain("mono");
    const ollama = alias.closest("tr")!;
    // Name cell value.
    expect(within(ollama).getByText("ollama", { selector: "td.col-name a" }))
      .toBeInTheDocument();
    // Backend type badge.
    expect(within(ollama).getByText("ollama", { selector: "span.badge" }))
      .toBeInTheDocument();
    // Spec metrics — key count, requests/24h, cache hits with mono ratio,
    // latency p95.
    expect(within(ollama).getByText("2")).toBeInTheDocument(); // api_key_count fixture
    expect(within(ollama).getByText("1,024")).toBeInTheDocument(); // requests_24h
    expect(within(ollama).getByText("200")).toBeInTheDocument(); // cache_hits_24h
    expect(within(ollama).getByText("1,200 ms")).toBeInTheDocument(); // latency_p95_ms
    expect(within(ollama).getByText("connected", { selector: "span.badge" })).toBeInTheDocument();
  });

  it("⋯ menu offers Inspect / Keys / Access settings / Cost", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    const alias = within(table).getByText("fast → llama3.1:8b");
    const ollama = alias.closest("tr")!;
    const more = within(ollama).getByRole("button", { name: /more actions/i });
    await userEvent.click(more);
    const menu = await screen.findByRole("menu");
    const labels = within(menu).getAllByRole("menuitem").map((n) => n.textContent);
    expect(labels).toEqual(["Inspect", "Keys", "Access settings", "Cost"]);
  });

  it("renders the empty state when there are no providers", async () => {
    db.aiProviders = [];
    mount();
    expect(await screen.findByRole("heading", { name: "No providers yet" })).toBeInTheDocument();
    expect(screen.getByText(
      "Add one from a service in API-key mode, or add a hosted API. Switching a service to API-key mode does not create a provider.",
    )).toBeInTheDocument();
  });

  it("tells a non-admin who can add a provider, without an action", async () => {
    db.aiProviders = [];
    mountAs("user");
    expect(await screen.findByRole("heading", { name: "No providers yet" })).toBeInTheDocument();
    expect(screen.getByText(
      "An administrator can add one from a service in API-key mode, or add a hosted API. Switching a service to API-key mode does not create a provider.",
    )).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New provider" })).toBeNull();
    expect(screen.queryByRole("button", { name: "New AI service" })).toBeNull();
  });

  // P5.5 — admin empty-state CTA + concept explainer
  it("P5.5: admin sees 'New AI service' and 'New provider' with an empty list", async () => {
    // db.me is admin by default. A service whose provider is gone, or that was
    // switched to API-key mode later, has no provider until an admin adds one.
    db.services = db.services.filter((s) => s.access_mode !== "api_key");
    mount();
    expect(await screen.findByRole("heading", { name: "No providers yet" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "New AI service" })).toBeInTheDocument();
    // One in the page header, one in the empty state.
    expect(screen.getAllByRole("button", { name: "New provider" })).toHaveLength(2);
  });

  it("P5.5: concept explainer link to /services is always visible", async () => {
    mount();
    // The explainer "A provider serves a Service ..." renders regardless of empty/non-empty
    const link = await screen.findByRole("link", { name: /^service$/i });
    expect(link).toHaveAttribute("href", "/services");
  });

  it("P5.5: featureAbsent branch has NO 'New AI service' button", async () => {
    server.use(
      http.get("/api/v1/ai/providers", () =>
        HttpResponse.json({ error: "not found" }, { status: 404 }),
      ),
    );
    mount();
    await screen.findByRole("heading", { name: /ai gateway isn't available/i });
    expect(screen.queryByRole("button", { name: /new ai service/i })).toBeNull();
  });

  it("does not show cost-summary tokens when there are no providers (L-6)", async () => {
    server.use(
      http.get("/api/v1/ai/providers", () => HttpResponse.json([])),
    );
    mount();
    await screen.findByText(/no providers yet/i);
    const tokensTile = screen.getByText(/tokens in\/out/i).closest(".metric-tile") as HTMLElement;
    expect(within(tokensTile).getByText("—")).toBeInTheDocument();
  });

  it("shows an error notice with Retry on failure and recovers when clicked", async () => {
    server.use(
      http.get("/api/v1/ai/providers", () =>
        HttpResponse.json({ error: "boom" }, { status: 500 }),
      ),
    );
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent(/boom|couldn't load/i);
    const retry = screen.getByRole("button", { name: /retry/i });
    server.resetHandlers();
    await userEvent.click(retry);
    await waitFor(() =>
      expect(screen.getByRole("table", { name: /providers/i })).toBeInTheDocument(),
    );
  });

  it("lists providers with their base URL path and links to the detail page", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    const link = within(table).getByRole("link", { name: "ollama" });
    expect(link).toHaveAttribute("href", "/gateway/providers/ollama");
    expect(within(table).getByText("/ai/ollama/v1")).toBeInTheDocument();
  });

  it("copies the full base URL from the row", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    await userEvent.click(within(table).getByRole("button", {
      name: "Copy base URL https://tunnels.example.com/ai/ollama/v1",
    }));
    expect(writeText).toHaveBeenCalledWith("https://tunnels.example.com/ai/ollama/v1");
  });

  it("⋯ Inspect goes to the provider's detail route", async () => {
    renderApp(
      <Routes>
        <Route path="/gateway/providers" element={<Providers />} />
        <Route path="/gateway/providers/:slug" element={<div>DETAIL_PAGE</div>} />
      </Routes>,
      "/gateway/providers",
    );
    const table = await screen.findByRole("table", { name: /providers/i });
    await userEvent.click(within(table).getByRole("button", { name: /more actions for ollama/i }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Inspect" }));
    expect(await screen.findByText("DETAIL_PAGE")).toBeInTheDocument();
  });

  it("lets an admin add a provider from an API-key service", async () => {
    // A second API-key service that backs no provider yet: the only option.
    db.services.push({ ...db.services.find((x) => x.id === "svc_ai001")!, id: "svc_ai002", name: "vllm", slug: "vl9k2p" });
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/providers", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ slug: "local", name: "Local" }, { status: 201 });
    }));
    mount();
    const opener = await screen.findByRole("button", { name: "New provider" });
    await userEvent.click(opener);
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    // Focus lands on the first control: the choice of what the provider serves.
    await waitFor(() => expect(within(dialog).getByRole("radio", { name: /a service behind a burrow client/i })).toHaveFocus());
    await userEvent.type(within(dialog).getByLabelText("Name"), "Local");
    // Only http services in API-key mode that back no provider yet are offered.
    await userEvent.click(within(dialog).getByLabelText("Service"));
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options).toEqual(["vllm"]);
    await userEvent.click(screen.getByRole("option", { name: "vllm" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(posted).toMatchObject({ name: "Local", kind: "tunnel", service_id: "svc_ai002" }));
    expect(posted).not.toHaveProperty("slug"); // empty: the server derives it
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(opener).toHaveFocus());
  });

  it("New provider: says so when no service is eligible and keeps Create disabled", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New provider" }));
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    expect(await within(dialog).findByText(/no eligible service/i)).toBeInTheDocument();
    expect(within(dialog).getByRole("link", { name: /create a service in api-key mode/i }))
      .toHaveAttribute("href", "/services?new=ai");
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeDisabled();
  });

  it("New provider: rejects the reserved slug v1 on the field", async () => {
    db.services.push({ ...db.services.find((x) => x.id === "svc_ai001")!, id: "svc_ai002", name: "vllm", slug: "vl9k2p" });
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New provider" }));
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "Local");
    const slug = within(dialog).getByLabelText("Provider slug");
    await userEvent.type(slug, "v1");
    expect(slug).toHaveAttribute("aria-invalid", "true");
    expect(slug).toHaveAccessibleDescription('"v1" is reserved.');
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeDisabled();
    await userEvent.clear(slug);
    await userEvent.type(slug, "local");
    expect(slug).toHaveAccessibleDescription(`${window.location.origin}/ai/local/v1`);
  });

  it("New provider: shows a server conflict in the dialog and stays open", async () => {
    db.services.push({ ...db.services.find((x) => x.id === "svc_ai001")!, id: "svc_ai002", name: "vllm", slug: "vl9k2p" });
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New provider" }));
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "Other");
    await userEvent.type(within(dialog).getByLabelText("Provider slug"), "ollama"); // taken
    await userEvent.click(within(dialog).getByLabelText("Service"));
    await userEvent.click(await screen.findByRole("option", { name: "vllm" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("provider slug or service already in use");
  });

  it("New provider: a 400 that is not about the slug shows in the dialog, not on the slug field", async () => {
    db.services.push({ ...db.services.find((x) => x.id === "svc_ai001")!, id: "svc_ai002", name: "vllm", slug: "vl9k2p" });
    server.use(http.post("/api/v1/ai/providers", () =>
      HttpResponse.json({ error: "kind must be 'tunnel'" }, { status: 400 })));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New provider" }));
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "Local");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("kind must be 'tunnel'");
    expect(within(dialog).getByLabelText("Provider slug")).not.toHaveAttribute("aria-invalid");
  });

  it("New provider: a 400 about the slug shows on the slug field", async () => {
    db.services.push({ ...db.services.find((x) => x.id === "svc_ai001")!, id: "svc_ai002", name: "vllm", slug: "vl9k2p" });
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New provider" }));
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    // "AI" derives a slug that is too short; the server says so.
    await userEvent.type(within(dialog).getByLabelText("Name"), "AI");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    const slug = within(dialog).getByLabelText("Provider slug");
    await waitFor(() => expect(slug).toHaveAttribute("aria-invalid", "true"));
    expect(slug).toHaveAccessibleDescription(/^slug must be 3-40 characters/);
  });

  it("hides 'New provider' from non-admins", async () => {
    mountAs("user");
    await screen.findByRole("heading", { name: "Providers" });
    await screen.findByRole("table", { name: /providers/i });
    expect(screen.queryByRole("button", { name: "New provider" })).toBeNull();
  });

  it("has no dead 'Disable' item in the row menu (U7)", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    await userEvent.click(within(table).getAllByRole("button", { name: /more actions for/i })[0]);
    expect(screen.getByRole("menuitem", { name: "Inspect" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Disable" })).toBeNull();
  });

  it("shows the kind of each provider, and for a direct one whether its credential is configured", async () => {
    addDirectProvider("openrouter", { name: "OpenRouter", credential_slot: "OPENROUTER" });
    addDirectProvider("zai", { name: "z.ai", credential_slot: "ZAI" }); // slot not set in the fixtures
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    expect(within(table).getByRole("columnheader", { name: "Kind" })).toBeInTheDocument();
    const row = (name: string) => within(table).getByRole("link", { name }).closest("tr")!;
    expect(within(row("ollama")).getByText("tunnel", { selector: "span.badge" })).toBeInTheDocument();
    expect(within(row("ollama")).getByText("connected", { selector: "span.badge" })).toBeInTheDocument();
    expect(within(row("OpenRouter")).getByText("direct", { selector: "span.badge" })).toBeInTheDocument();
    expect(within(row("OpenRouter")).getByText("ready", { selector: "span.badge" })).toBeInTheDocument();
    expect(within(row("z.ai")).getByText("direct", { selector: "span.badge" })).toBeInTheDocument();
    expect(within(row("z.ai")).getByText("not configured", { selector: "span.badge" })).toBeInTheDocument();
    // A direct provider has no client that could be offline.
    expect(within(row("z.ai")).queryByText("offline")).toBeNull();
  });

  it("a direct provider's row menu stays on the provider: no link to its hidden service", async () => {
    addDirectProvider("openrouter", { name: "OpenRouter", credential_slot: "OPENROUTER" });
    renderApp(
      <Routes>
        <Route path="/gateway/providers" element={<Providers />} />
        <Route path="/gateway/providers/:slug" element={<HashProbe />} />
      </Routes>,
      "/gateway/providers",
    );
    const table = await screen.findByRole("table", { name: /providers/i });
    await userEvent.click(within(table).getByRole("button", { name: "More actions for OpenRouter" }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getAllByRole("menuitem").map((n) => n.textContent)).toEqual(["Inspect", "Keys", "Cost"]);
    await userEvent.click(within(menu).getByRole("menuitem", { name: "Keys" }));
    expect(await screen.findByTestId("where")).toHaveTextContent("/gateway/providers/openrouter#api-keys");
  });

  it("lets an admin add a hosted API from a preset", async () => {
    mount();
    const opener = await screen.findByRole("button", { name: "New provider" });
    await userEvent.click(opener);
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    await userEvent.click(within(dialog).getByRole("radio", { name: /a hosted api/i }));
    await userEvent.click(within(dialog).getByLabelText("Provider"));
    await userEvent.click(await screen.findByRole("option", { name: "OpenRouter" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const table = screen.getByRole("table", { name: /providers/i });
    expect(await within(table).findByRole("link", { name: "OpenRouter" })).toHaveAttribute("href", "/gateway/providers/openrouter");
    await waitFor(() => expect(opener).toHaveFocus());
  });
});
