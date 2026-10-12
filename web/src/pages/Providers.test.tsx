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

function Where() {
  const l = useLocation();
  return <div data-testid="location">{l.pathname + l.search}</div>;
}

// The page under a router that also knows where a link leads.
function mountAt(route: string) {
  return renderApp(
    <>
      <Routes>
        <Route path="/gateway/providers" element={<Providers />} />
        <Route path="*" element={null} />
      </Routes>
      <Where />
    </>,
    route,
  );
}

const useExisting = (dialog: HTMLElement) =>
  userEvent.click(within(dialog).getByRole("button", { name: "Use a service that already exists" }));

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

  it("renders one row per provider with name, mono model line, backend, key count, requests, cache, latency, status", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    // Anchor by the unique mono model line (the name "ollama" also appears in
    // the backend badge, so it isn't a safe anchor on its own).
    const alias = within(table).getByText("burrow-simple → mistral");
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
    expect(within(ollama).getByText("Online", { selector: "span.badge" })).toHaveClass("status-online");
  });

  it("writes the state of every provider out in the same five words", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    const status = (name: string) => {
      const cells = within(table).getByRole("link", { name }).closest("tr")!.querySelectorAll("td");
      return cells[cells.length - 2]!;
    };
    expect(status("ollama")).toHaveTextContent(/^Online$/);
    expect(status("llamacpp")).toHaveTextContent(/^Offline$/);
    expect(status("lmstudio")).toHaveTextContent(/^Never connected$/);
    expect(status("openai-team")).toHaveTextContent(/^Ready$/);
    expect(status("zai")).toHaveTextContent(/^Not configured$/);
    expect(within(status("lmstudio")).getByText("Never connected")).toHaveClass("badge", "status-never");
    for (const old of ["connected", "offline", "ready", "not configured", "idle"]) {
      expect(within(table).queryByText(old)).toBeNull();
    }
  });

  it("⋯ menu offers Inspect / Keys / Access settings / Cost", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    const alias = within(table).getByText("burrow-simple → mistral");
    const ollama = alias.closest("tr")!;
    const more = within(ollama).getByRole("button", { name: /more actions/i });
    await userEvent.click(more);
    const menu = await screen.findByRole("menu");
    const labels = within(menu).getAllByRole("menuitem").map((n) => n.textContent);
    expect(labels).toEqual(["Inspect", "Keys", "Access settings", "Cost"]);
  });

  it("Keys of a tunnel provider leads to the API keys of its service", async () => {
    renderApp(
      <Routes>
        <Route path="/gateway/providers" element={<Providers />} />
        <Route path="/services/:id" element={<HashProbe />} />
      </Routes>,
      "/gateway/providers",
    );
    const table = await screen.findByRole("table", { name: /providers/i });
    await userEvent.click(within(table).getByRole("button", { name: "More actions for ollama" }));
    await userEvent.click(within(await screen.findByRole("menu")).getByRole("menuitem", { name: "Keys" }));
    expect(await screen.findByTestId("where")).toHaveTextContent("/services/svc_ai001#api-keys");
  });

  it("a provider whose service holds no API key has no Keys item", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    await userEvent.click(within(table).getByRole("button", { name: "More actions for llamacpp" }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getAllByRole("menuitem").map((n) => n.textContent)).toEqual(["Inspect", "Access settings", "Cost"]);
  });

  it("renders the empty state when there are no providers", async () => {
    db.aiProviders = [];
    mount();
    expect(await screen.findByRole("heading", { name: "No providers yet" })).toBeInTheDocument();
    expect(screen.getByText(
      "Add a model server on one of your machines, or a hosted API. Switching a service to API-key mode does not create a provider.",
    )).toBeInTheDocument();
  });

  it("tells a non-admin who can add a provider, without an action", async () => {
    db.aiProviders = [];
    mountAs("user");
    expect(await screen.findByRole("heading", { name: "No providers yet" })).toBeInTheDocument();
    expect(screen.getByText(
      "An admin can add a model server on one of their machines, or a hosted API. Switching a service to API-key mode does not create a provider.",
    )).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New provider" })).toBeNull();
  });

  // P5.5 — admin empty-state CTA + concept explainer
  it("P5.5: with an empty list an admin has 'New provider' twice and no second way in", async () => {
    // db.me is admin by default. A service whose provider is gone, or that was
    // switched to API-key mode later, has no provider until an admin adds one.
    db.services = db.services.filter((s) => s.access_mode !== "api_key");
    mount();
    expect(await screen.findByRole("heading", { name: "No providers yet" })).toBeInTheDocument();
    // One in the page header, one in the empty state.
    expect(screen.getAllByRole("button", { name: "New provider" })).toHaveLength(2);
    // The old second flow is gone: the dialog's first choice does what it did.
    expect(screen.getAllByRole("button").map((b) => b.textContent)).not.toContain("New service and provider");
    expect(screen.queryByRole("button", { name: /new .*service/i })).toBeNull();
  });

  it("the header has one button to add a provider", async () => {
    mount();
    await screen.findByRole("table", { name: /providers/i });
    const header = screen.getByRole("heading", { name: "Providers" }).closest("header, .page-header") ?? document.body;
    expect(within(header as HTMLElement).getAllByRole("button").map((b) => b.textContent)).toEqual(["New provider"]);
  });

  it("?new=local opens the dialog on the guided steps", async () => {
    mountAt("/gateway/providers?new=local");
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    expect(within(dialog).getByRole("radio", { name: "A model server on one of my machines" })).toBeChecked();
    expect(within(dialog).getByLabelText("Model server")).toBeInTheDocument();
    // The link stays in the address while the dialog is open: a reload opens it again.
    expect(screen.getByTestId("location")).toHaveTextContent("/gateway/providers?new=local");
  });

  it.each(["Cancel", "Close dialog"])("?new=local: closing the dialog with %s takes the parameter out of the address", async (name) => {
    mountAt("/gateway/providers?new=local");
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    await userEvent.click(within(dialog).getByRole("button", { name }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByTestId("location").textContent).toBe("/gateway/providers");
  });

  it("?new=local: Done leaves for the provider's page", async () => {
    mountAt("/gateway/providers?new=local");
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "Gemma");
    await userEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
    await within(dialog).findByRole("heading", { name: "Run this on the machine" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe("/gateway/providers/gemma"));
  });

  it("the provider that the guided steps created is in the list behind the dialog", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New provider" }));
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "Gemma");
    await userEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
    await within(dialog).findByRole("heading", { name: "Run this on the machine" });
    const table = screen.getByRole("table", { name: /providers/i });
    expect(await within(table).findByRole("link", { name: "Gemma" })).toHaveAttribute("href", "/gateway/providers/gemma");
    await userEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("?new=local tells someone who may not add a provider who can, and uses the link up", async () => {
    db.me = { ...db.me, role: "user" };
    mountAt("/gateway/providers?new=local");
    await screen.findByRole("table", { name: /providers/i });
    const notice = await screen.findByText("Adding a provider needs an admin.");
    expect(notice.closest("[role=status]")).not.toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe("/gateway/providers"));
    // It stays, though the parameter that asked is gone.
    expect(screen.getByText("Adding a provider needs an admin.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New provider" })).toBeNull();
  });

  it("says nothing of the kind to a non-admin who only opens the page, nor to an admin with the link", async () => {
    const first = mountAs("user");
    await screen.findByRole("table", { name: /providers/i });
    expect(screen.queryByText("Adding a provider needs an admin.")).toBeNull();
    first.unmount();
    db.me = { ...db.me, role: "admin" };
    mountAt("/gateway/providers?new=local");
    await screen.findByRole("dialog", { name: "New provider" });
    expect(screen.queryByText("Adding a provider needs an admin.")).toBeNull();
  });

  it("P5.5: concept explainer link to /services is always visible", async () => {
    mount();
    // The explainer "A provider serves a Service ..." renders regardless of empty/non-empty
    const link = await screen.findByRole("link", { name: /^service$/i });
    expect(link).toHaveAttribute("href", "/services");
  });

  it("P5.5: featureAbsent branch has no button to add anything", async () => {
    server.use(
      http.get("/api/v1/ai/providers", () =>
        HttpResponse.json({ error: "not found" }, { status: 404 }),
      ),
    );
    mount();
    await screen.findByRole("heading", { name: /ai gateway isn't available/i });
    expect(screen.queryByRole("button")).toBeNull();
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
    // The copy is confirmed, as on the other pages.
    expect(await screen.findByText("Copied.")).toBeInTheDocument();
  });

  it("names the actions column for assistive technology", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    expect(within(table).getByRole("columnheader", { name: "Actions" })).toBeInTheDocument();
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
    await waitFor(() => expect(within(dialog).getByRole("radio", { name: "A model server on one of my machines" })).toHaveFocus());
    await useExisting(dialog);
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
    await useExisting(dialog);
    expect(await within(dialog).findByText(/No service can be used/)).toBeInTheDocument();
    // The guided steps are the way to a new one: nothing leads to the Services page.
    expect(within(dialog).queryByRole("link")).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Set up a model server step by step" })).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeDisabled();
  });

  it("New provider: rejects the reserved slug v1 on the field", async () => {
    db.services.push({ ...db.services.find((x) => x.id === "svc_ai001")!, id: "svc_ai002", name: "vllm", slug: "vl9k2p" });
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New provider" }));
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    await useExisting(dialog);
    await userEvent.type(within(dialog).getByLabelText("Name"), "Local");
    const slug = within(dialog).getByLabelText("Provider slug");
    await userEvent.type(slug, "v1");
    // The button is held back at once; the field says why once it is left.
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeDisabled();
    expect(within(dialog).getByRole("button", { name: "Create" })).toHaveAccessibleDescription("Check the provider slug.");
    expect(slug).not.toHaveAttribute("aria-invalid");
    await userEvent.tab();
    expect(slug).toHaveAttribute("aria-invalid", "true");
    expect(slug).toHaveAccessibleDescription('"v1" is reserved.');
    await userEvent.clear(slug);
    await userEvent.type(slug, "local");
    expect(slug).toHaveAccessibleDescription(`${window.location.origin}/ai/local/v1`);
  });

  it("New provider: shows a server conflict in the dialog and stays open", async () => {
    db.services.push({ ...db.services.find((x) => x.id === "svc_ai001")!, id: "svc_ai002", name: "vllm", slug: "vl9k2p" });
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New provider" }));
    const dialog = await screen.findByRole("dialog", { name: "New provider" });
    await useExisting(dialog);
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
    await useExisting(dialog);
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
    await useExisting(dialog);
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
    expect(within(row("ollama")).getByText("Online", { selector: "span.badge" })).toBeInTheDocument();
    expect(within(row("OpenRouter")).getByText("direct", { selector: "span.badge" })).toBeInTheDocument();
    expect(within(row("OpenRouter")).getByText("Ready", { selector: "span.badge" })).toBeInTheDocument();
    expect(within(row("z.ai")).getByText("direct", { selector: "span.badge" })).toBeInTheDocument();
    expect(within(row("z.ai")).getByText("Not configured", { selector: "span.badge" })).toBeInTheDocument();
    // A direct provider has no client that could be offline.
    expect(within(row("z.ai")).queryByText(/offline/i)).toBeNull();
  });

  it("a direct provider's row menu stays on the provider: no link to its hidden service", async () => {
    const p = addDirectProvider("openrouter", { name: "OpenRouter", credential_slot: "OPENROUTER" });
    // A key from before gateway keys: it is listed on the provider's Settings tab.
    db.serviceApiKeys[p.service_id] = [{ id: "sak_old", name: "old", last_used: null, created_at: "2026-05-01T08:00:00Z" }];
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
    expect(await screen.findByTestId("where")).toHaveTextContent("/gateway/providers/openrouter#settings");
  });

  it("a direct provider without such keys has no Keys item", async () => {
    addDirectProvider("openrouter", { name: "OpenRouter", credential_slot: "OPENROUTER" });
    mount();
    const table = await screen.findByRole("table", { name: /providers/i });
    await userEvent.click(within(table).getByRole("button", { name: "More actions for OpenRouter" }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getAllByRole("menuitem").map((n) => n.textContent)).toEqual(["Inspect", "Cost"]);
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
