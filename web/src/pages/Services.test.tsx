import { describe, it, expect, vi, afterEach } from "vitest";
import { act, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { Route, Routes, useLocation } from "react-router-dom";
import Services from "@/pages/Services";

function PathProbe() {
  return <div data-testid="path">{useLocation().pathname}</div>;
}

function LocationProbe() {
  const { pathname, search } = useLocation();
  return <div data-testid="location">{pathname + search}</div>;
}
const currentLocation = () => screen.getByTestId("location").textContent ?? "";

function mountAt(route: string) {
  return renderApp(<><Services /><LocationProbe /></>, route);
}
function mount() {
  return mountAt("/services");
}

describe("Services page", () => {
  it("shows a loading skeleton before data arrives", () => {
    const { container } = mount();
    expect(container.querySelector(".skel")).toBeTruthy();
  });

  it("links a service without a title by its id", async () => {
    const svc = db.services.find((s) => s.id === "svc_graf01")!;
    const name = svc.name;
    svc.name = "";
    try {
      mount();
      const table = await screen.findByRole("table", { name: "Services" });
      expect(within(table).getByRole("link", { name: "svc_graf01" })).toHaveAttribute("href", "/services/svc_graf01");
    } finally {
      svc.name = name;
    }
  });

  it("shows each http service's path and copies the full URL", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getByRole("columnheader", { name: "URL" })).toBeInTheDocument();
    expect(within(table).queryByRole("columnheader", { name: "Hostname" })).toBeNull();
    expect(within(table).getByText("/svc/k7p2qx/")).toBeInTheDocument();
    await userEvent.click(
      within(table).getByRole("button", { name: "Copy URL https://tunnels.example.com/svc/k7p2qx/" }),
    );
    expect(writeText).toHaveBeenCalledWith("https://tunnels.example.com/svc/k7p2qx/");
  });

  it("never renders a subdomain-style host", async () => {
    mount();
    await screen.findByRole("table", { name: "Services" });
    expect(document.body.textContent).not.toMatch(/k7p2qx\.tunnels\.example\.com/);
  });

  it("filters by slug", async () => {
    mount();
    await screen.findByRole("table", { name: "Services" });
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter services" }), "gf7x1p");
    const table = screen.getByRole("table", { name: "Services" });
    expect(within(table).getAllByRole("row")).toHaveLength(2); // header + grafana
  });

  it("filters by the URL's domain or /svc/ path", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    const box = screen.getByRole("searchbox", { name: "Filter services" });
    await userEvent.type(box, "tunnels.example.com/svc/gf7x");
    expect(within(table).getAllByRole("row")).toHaveLength(2); // header + grafana
    await userEvent.clear(box);
    await userEvent.type(box, "/svc/");
    expect(within(table).getAllByRole("row")).toHaveLength(4); // header + 3 http rows
  });

  it("renders a row per service with name, type, URL, access badge", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    const web = within(table).getByText("web").closest("tr")!;
    expect(within(web).getByText("http")).toBeInTheDocument();
    // http row: URL path shown mono with an aria-labelled copy button
    expect(within(web).getByText("/svc/k7p2qx/")).toBeInTheDocument();
    expect(
      within(web).getByRole("button", { name: /copy url https:\/\/tunnels\.example\.com\/svc\/k7p2qx\//i }),
    ).toBeInTheDocument();
    expect(within(web).getByText("Open")).toBeInTheDocument();

    const ai = within(table).getByText("ollama").closest("tr")!;
    expect(within(ai).getByText("API key")).toBeInTheDocument();

    const gf = within(table).getByText("grafana").closest("tr")!;
    expect(within(gf).getByText("Burrow login")).toBeInTheDocument();

    // tcp row: no URL, em-dash, no copy button
    const pg = within(table).getByText("postgres").closest("tr")!;
    expect(within(pg).getByText("tcp")).toBeInTheDocument();
    expect(within(pg).queryByRole("button", { name: /copy url/i })).toBeNull();
  });

  it("Configure opens the AccessModePanel for that service", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    const web = within(table).getByText("web").closest("tr")!;
    await userEvent.click(within(web).getByRole("button", { name: /configure/i }));
    expect(await screen.findByRole("radiogroup", { name: /access mode/i })).toBeInTheDocument();
  });

  it("shows the empty state when there are no services", async () => {
    db.services = [];
    mount();
    // EmptyState renders <h4>title</h4>; body text is in a <p> with <code> children.
    expect(await screen.findByText("No services yet")).toBeInTheDocument();
    // Verify at least one keyword from the body is present in the document.
    expect(screen.getByText("burrow connect")).toBeInTheDocument();
  });

  it("renders idle services as a status-idle badge, not bare muted text (D-12/L-15)", async () => {
    mount();
    const table = await screen.findByRole("table");
    const gf = within(table).getByText("grafana").closest("tr")!;
    const idle = within(gf).getByText("idle");
    expect(idle).toHaveClass("badge", "status-idle");
    expect(idle).not.toHaveClass("muted");
  });

  it("shows an error notice with Retry on failure", async () => {
    server.use(
      http.get("/api/v1/services", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
    );
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent(/boom|couldn't load/i);
    const retry = screen.getByRole("button", { name: /retry/i });
    // Restore a working handler, click Retry, expect the table to appear.
    server.resetHandlers();
    await userEvent.click(retry);
    await waitFor(() => expect(screen.getByRole("table", { name: /services/i })).toBeInTheDocument());
  });

  // P3A.2 — Services explainer
  it("explains saved configuration against Live (role=note, NOT alert)", async () => {
    mount();
    const note = await screen.findByRole("note");
    expect(note).toHaveTextContent("Services are the saved configuration. Switch to Live to see what is connected right now.");
    expect(note.querySelector("a")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("connected service status badge links to the Live filter (P3B.2)", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    const web = within(table).getByText("web").closest("tr")!;
    const connectedLink = within(web).getByRole("link", { name: /view live tunnel for web/i });
    expect(connectedLink).toHaveAttribute("href", "/services?live=1");
  });

  it("idle service status badge is NOT wrapped in a link (P3B.2)", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    const gf = within(table).getByText("grafana").closest("tr")!;
    expect(within(gf).queryByRole("link", { name: /view live tunnel for grafana/i })).toBeNull();
    // The idle badge itself must still be present
    expect(within(gf).getByText("idle")).toBeInTheDocument();
  });

  it("creates a new service via the New service dialog and shows a success toast", async () => {
    mount();
    // Wait for initial data to load (table must be present first)
    await screen.findByRole("table", { name: /services/i });

    // Open the create dialog
    await userEvent.click(screen.getByRole("button", { name: /^new service$/i }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();

    // Fill in the service ID and an optional title
    const idInput = screen.getByLabelText(/service id/i);
    await userEvent.type(idInput, "ai-svc");
    const titleInput = screen.getByLabelText(/title/i);
    await userEvent.type(titleInput, "My AI Service");

    // Click Create
    await userEvent.click(screen.getByRole("button", { name: /^create$/i }));

    // Success toast appears with the service_id
    await screen.findByText("Service ai-svc created.");

    // Dialog should be closed
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    // The new service appears in the table on refetch (by its title/name)
    const table = await screen.findByRole("table", { name: /services/i });
    expect(within(table).getByText("My AI Service")).toBeInTheDocument();
  });

  // P5.1 — Access mode Select in the create dialog
  it("P5.1: dialog has an Access mode combobox; selecting API key sends access_mode in POST body", async () => {
    let captured: Record<string, unknown> | null = null;
    server.use(
      http.post("/api/v1/services", async ({ request }) => {
        captured = await request.json() as Record<string, unknown>;
        return HttpResponse.json({ id: "svc-x", created_at: "2026-05-31T00:00:00Z" }, { status: 201 });
      }),
    );

    mount();
    await screen.findByRole("table", { name: /services/i });
    await userEvent.click(screen.getByRole("button", { name: /^new service$/i }));
    await screen.findByRole("dialog");

    // The Access mode combobox is present (Select renders a button with aria-haspopup="listbox")
    const accessModeBtn = screen.getByRole("button", { name: /access mode/i });
    expect(accessModeBtn).toBeInTheDocument();

    // Open and select "API key"
    await userEvent.click(accessModeBtn);
    const apiKeyOption = await screen.findByRole("option", { name: /api key/i });
    await userEvent.click(apiKeyOption);

    // Fill required service ID
    const idInput = screen.getByLabelText(/service id/i);
    await userEvent.type(idInput, "new-ai-svc");

    // Submit
    await userEvent.click(screen.getByRole("button", { name: /^create$/i }));

    await waitFor(() => expect(captured).not.toBeNull());
    expect(captured).toMatchObject({ access_mode: "api_key" });
  });

  // P5.2 — ?new=ai auto-opens the dialog; access mode is fixed to API key, no picker
  it("P5.2: ?new=ai auto-opens the New AI service dialog without an access-mode picker", async () => {
    renderApp(<Services />, "/services?new=ai");
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /new ai service/i })).toBeInTheDocument();
    // Access mode is fixed to API key for AI services: no picker is offered.
    expect(screen.queryByRole("button", { name: /access mode/i })).toBeNull();
  });

  it("P5.2: ?new=ai still POSTs access_mode api_key", async () => {
    let captured: Record<string, unknown> | null = null;
    server.use(
      http.post("/api/v1/services", async ({ request }) => {
        captured = await request.json() as Record<string, unknown>;
        return HttpResponse.json({ id: "svc-ai", created_at: "2026-05-31T00:00:00Z" }, { status: 201 });
      }),
    );
    renderApp(<Services />, "/services?new=ai");
    const dialog = await screen.findByRole("dialog", { name: "New AI service" });
    await userEvent.type(within(dialog).getByLabelText(/service id/i), "ai-svc-2");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create and continue" }));
    await waitFor(() => expect(captured).not.toBeNull());
    expect(captured).toMatchObject({ access_mode: "api_key" });
  });

  it("explains the AI flow when opened via ?new=ai (F8)", async () => {
    renderApp(<Services />, "/services?new=ai");
    const dialog = await screen.findByRole("dialog", { name: "New AI service" });
    expect(within(dialog).getByText("Creates a service with API-key access and registers it as a model provider.")).toBeInTheDocument();
    // Access mode is fixed for AI services, so the picker is not offered.
    expect(within(dialog).queryByLabelText("Access mode")).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Create and continue" })).toBeInTheDocument();
  });

  it("?new=ai registers the new service as a provider and opens its page", async () => {
    renderApp(
      <Routes>
        <Route path="/services" element={<Services />} />
        <Route path="/gateway/providers/:slug" element={<PathProbe />} />
        <Route path="/services/:id" element={<PathProbe />} />
      </Routes>,
      "/services?new=ai",
    );
    const dialog = await screen.findByRole("dialog", { name: "New AI service" });
    await userEvent.type(within(dialog).getByLabelText(/service id/i), "local-llm");
    await userEvent.type(within(dialog).getByLabelText("Title"), "Local LLM");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create and continue" }));
    // The mock derives the slug from the name, as the server does.
    expect(await screen.findByTestId("path")).toHaveTextContent(/^\/gateway\/providers\/local-llm$/);
    expect(db.aiProviders.at(-1)).toMatchObject({ slug: "local-llm", name: "Local LLM", service_id: "local-llm" });
  });

  it("?new=ai falls back to the services list and says why when the provider cannot be created", async () => {
    // The title "AI" derives a slug the server rejects (too short): the
    // service exists afterwards, the provider does not.
    renderApp(
      <>
        <Routes>
          <Route path="/services" element={<Services />} />
          <Route path="/gateway/providers" element={<div>PROVIDERS_PAGE</div>} />
          <Route path="/gateway/providers/:slug" element={<div>PROVIDER_PAGE</div>} />
          <Route path="/services/:id" element={<div>SERVICE_PAGE</div>} />
        </Routes>
        <PathProbe />
      </>,
      "/services?new=ai",
    );
    const dialog = await screen.findByRole("dialog", { name: "New AI service" });
    await userEvent.type(within(dialog).getByLabelText(/service id/i), "local-llm");
    await userEvent.type(within(dialog).getByLabelText("Title"), "AI");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create and continue" }));
    // The message names the server's reason and where to finish the job. It
    // stays on the Services page: its toaster would not survive a navigation.
    const toastText = await screen.findByText(/created, but it was not registered as a provider/i);
    expect(toastText).toHaveTextContent(/slug must be 3-40 characters/);
    expect(toastText).toHaveTextContent(/add it under Providers/i);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/services$/);
    expect(db.services.some((s) => s.id === "local-llm")).toBe(true);
    expect(db.aiProviders.some((p) => p.service_id === "local-llm")).toBe(false);
    // The service that was created is in the list behind the closed dialog.
    const table = screen.getByRole("table", { name: /services/i });
    expect(await within(table).findByRole("link", { name: "AI" })).toHaveAttribute("href", "/services/local-llm");
    // The toast is the only pointer to the next step, so it carries the way there.
    await userEvent.click(screen.getByRole("button", { name: "Open Providers" }));
    expect(await screen.findByText("PROVIDERS_PAGE")).toBeInTheDocument();
  });

  it("keeps the generic dialog for the normal flow", async () => {
    renderApp(<Services />, "/services?new=1");
    const dialog = await screen.findByRole("dialog", { name: "New service" });
    expect(within(dialog).getByLabelText("Access mode")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeInTheDocument();
  });

  it("prefills the suggested slug in the new-service dialog and sends the edited one", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/services", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ id: "web-prod", created_at: "2026-10-04T00:00:00Z" }, { status: 201 });
    }));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New service" }));
    const slug = await screen.findByLabelText("URL slug");
    await waitFor(() => expect(slug).toHaveValue("q4m7kx"));
    await userEvent.type(screen.getByLabelText("Service ID"), "web-prod");
    await userEvent.clear(slug);
    await userEvent.type(slug, "web");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(posted).toMatchObject({ service_id: "web-prod", slug: "web" }));
  });

  it("blocks Create while the slug is invalid", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New service" }));
    await userEvent.type(screen.getByLabelText("Service ID"), "web-prod");
    const slug = await screen.findByLabelText("URL slug");
    await userEvent.clear(slug);
    await userEvent.type(slug, "-bad");
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
  });
});

// A fake event stream: jsdom has none. Installed per test, so the other suites keep running without one.
class FakeES {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  static last: FakeES | undefined;
  readyState: number = FakeES.OPEN;
  onerror: ((e: unknown) => void) | null = null;
  listeners: Record<string, ((e: unknown) => void)[]> = {};
  constructor() { FakeES.last = this; }
  addEventListener(t: string, fn: (e: unknown) => void) { (this.listeners[t] ||= []).push(fn); }
  removeEventListener(t: string, fn: (e: unknown) => void) {
    this.listeners[t] = (this.listeners[t] || []).filter((f) => f !== fn);
  }
  close() { this.readyState = FakeES.CLOSED; }
  emit(t: string) { (this.listeners[t] || []).forEach((fn) => fn({})); }
}

describe("Services page — All | Live", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    FakeES.last = undefined;
  });

  const rowCount = () => within(screen.getByRole("table", { name: "Services" })).getAllByRole("row").length;

  it("has an All | Live filter that follows the URL", async () => {
    mount();
    const filter = await screen.findByRole("radiogroup", { name: "Show" });
    expect(within(filter).getByRole("radio", { name: "All" })).toBeChecked();
    await screen.findByRole("table", { name: "Services" });
    const all = rowCount();
    await userEvent.click(within(filter).getByRole("radio", { name: "Live" }));
    await waitFor(() => expect(within(screen.getByRole("table", { name: "Services" })).getByRole("columnheader", { name: "Client" })).toBeInTheDocument());
    expect(rowCount()).toBeLessThan(all); // the fixtures contain an idle service
    // the choice is in the URL, so a reload or a shared link keeps it
    expect(currentLocation()).toMatch(/[?&]live=1/);
    await userEvent.click(within(filter).getByRole("radio", { name: "All" }));
    expect(currentLocation()).toBe("/services");
    expect(rowCount()).toBe(all);
  });

  it("moves between the two choices with the arrow keys", async () => {
    mount();
    const all = await screen.findByRole("radio", { name: "All" });
    all.focus();
    await userEvent.keyboard("{ArrowRight}");
    const live = screen.getByRole("radio", { name: "Live" });
    expect(live).toBeChecked();
    expect(live).toHaveFocus();
    expect(live).toHaveAttribute("tabindex", "0");
    expect(all).toHaveAttribute("tabindex", "-1");
  });

  it("opens on Live when the URL says so and shows who holds each connection", async () => {
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    expect(screen.getByRole("radio", { name: "Live" })).toBeChecked();
    expect(within(table).getByRole("columnheader", { name: "Client" })).toBeInTheDocument();
    const holders = await within(table).findAllByRole("link", { name: /office-box-1/ });
    expect(holders).toHaveLength(2);
    expect(holders[0]).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
    expect(within(table).queryByText("idle")).toBeNull();
  });

  it("has no Client column outside Live", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    for (const name of ["Client", "Local", "Remote", "Traffic"]) {
      expect(within(table).queryByRole("columnheader", { name })).toBeNull();
    }
  });

  it("shows a dash where the client is not known (the clients list is admin only)", async () => {
    server.use(http.get("/api/v1/clients", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getAllByRole("row")).toHaveLength(3); // header + the two live tunnels
    expect(within(table).queryByRole("link", { name: /office-box-1/ })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("joins a live http tunnel to its service: name, URL, access and Configure", async () => {
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    const row = within(table).getByRole("link", { name: "ollama" }).closest("tr")!;
    expect(within(table).getByRole("link", { name: "ollama" })).toHaveAttribute("href", "/services/svc_ai001");
    expect(within(row).getByText("/svc/ai4m2q/")).toBeInTheDocument();
    expect(within(row).getByText("API key")).toBeInTheDocument();
    expect(within(row).getByText("127.0.0.1:11434")).toBeInTheDocument();
    expect(within(row).queryByText(":0")).toBeNull(); // an http tunnel has no remote port
    expect(within(row).getByText("connected")).toBeInTheDocument();
    // Configure from a live row opens the access dialog of the durable service.
    await userEvent.click(within(row).getByRole("button", { name: /configure/i }));
    expect(await screen.findByRole("dialog", { name: "Access · ollama" })).toBeInTheDocument();
    expect(await screen.findByRole("radiogroup", { name: /access mode/i })).toBeInTheDocument();
  });

  it("gives a tcp tunnel without a service row its own line, with the relay's host:port to copy", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    const row = within(table).getByText("web-staging").closest("tr")!;
    expect(within(row).queryByRole("link", { name: "web-staging" })).toBeNull();
    expect(within(row).getByText("tcp")).toBeInTheDocument();
    expect(within(row).getByText(":9000")).toBeInTheDocument();
    expect(within(row).getByText("127.0.0.1:3000")).toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: /copy url/i })).toBeNull();
    expect(within(row).queryByRole("button", { name: /configure/i })).toBeNull();
    await userEvent.click(await within(row).findByRole("button", { name: "Copy endpoint relay.example.com:9000" }));
    expect(writeText).toHaveBeenCalledWith("relay.example.com:9000");
  });

  it("shows traffic in one cell, in and out", async () => {
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getByRole("columnheader", { name: "Traffic" })).toBeInTheDocument();
    expect(within(table).queryByRole("columnheader", { name: /^in$/i })).toBeNull();
    const row = within(table).getByText("web-staging").closest("tr")!;
    expect(within(row).getByTitle("In: 2048 bytes")).toBeInTheDocument();
    expect(within(row).getByTitle("Out: 1024 bytes")).toBeInTheDocument();
  });

  it("keeps the text filter when switching between All and Live", async () => {
    mount();
    await screen.findByRole("table", { name: "Services" });
    const box = screen.getByRole("searchbox", { name: "Filter services" });
    await userEvent.type(box, "ollama");
    expect(rowCount()).toBe(2); // header + ollama
    await userEvent.click(screen.getByRole("radio", { name: "Live" }));
    await waitFor(() => expect(within(screen.getByRole("table", { name: "Services" })).getByRole("columnheader", { name: "Client" })).toBeInTheDocument());
    expect(screen.getByRole("searchbox", { name: "Filter services" })).toHaveValue("ollama");
    expect(rowCount()).toBe(2); // header + the ollama tunnel
    await userEvent.click(screen.getByRole("radio", { name: "All" }));
    expect(screen.getByRole("searchbox", { name: "Filter services" })).toHaveValue("ollama");
    expect(rowCount()).toBe(2);
  });

  it("filters Live by local address and sorts it by name", async () => {
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    const names = () => within(table).getAllByRole("row").slice(1).map((r) => r.querySelector("td")!.textContent);
    expect(names()).toEqual(["ollama", "web-staging"]); // type asc: http before tcp
    await userEvent.click(within(table).getByRole("button", { name: /sort by name/i }));
    await userEvent.click(within(table).getByRole("button", { name: /sort by name/i }));
    expect(names()).toEqual(["web-staging", "ollama"]);
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter services" }), "127.0.0.1:3000");
    expect(names()).toEqual(["web-staging"]);
  });

  it("says so when nothing is live", async () => {
    server.use(http.get("/api/v1/tunnels", () => HttpResponse.json([])));
    mountAt("/services?live=1");
    expect(await screen.findByText("Nothing is live right now")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/clients/connect");
    expect(screen.queryByRole("table")).toBeNull();
    // The way back to the saved services stays on the page.
    await userEvent.click(screen.getByRole("radio", { name: "All" }));
    expect(await screen.findByRole("table", { name: "Services" })).toBeInTheDocument();
  });

  it("shows an error notice when the live tunnels cannot be loaded", async () => {
    server.use(http.get("/api/v1/tunnels", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mountAt("/services?live=1");
    expect(await screen.findByRole("alert")).toHaveTextContent(/boom/);
  });

  it("keeps the Live filter when ?new=1 opens the dialog", async () => {
    mountAt("/services?live=1&new=1");
    expect(await screen.findByRole("dialog", { name: "New service" })).toBeInTheDocument();
    expect(currentLocation()).toBe("/services?live=1");
  });

  it("does not ask for tunnels or clients outside Live", async () => {
    const asked: string[] = [];
    server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
    try {
      mount();
      await screen.findByRole("table", { name: "Services" });
      expect(asked).toContain("/api/v1/services");
      expect(asked.filter((p) => /\/(tunnels|clients)/.test(p))).toEqual([]);
    } finally {
      server.events.removeAllListeners();
    }
  });

  it("refetches the live tunnels on an event from the stream", async () => {
    vi.stubGlobal("EventSource", FakeES);
    let calls = 0;
    server.use(http.get("/api/v1/tunnels", () => { calls++; return HttpResponse.json([]); }));
    mountAt("/services?live=1");
    await screen.findByText("Nothing is live right now");
    const before = calls;
    act(() => FakeES.last!.emit("tunnels"));
    await waitFor(() => expect(calls).toBeGreaterThan(before));
  });

  it("asks for /me again when the stream closes, and not while it is reconnecting", async () => {
    vi.stubGlobal("EventSource", FakeES);
    const { qc } = mountAt("/services?live=1");
    await screen.findByRole("table", { name: "Services" });
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    const es = FakeES.last!;
    act(() => { es.readyState = FakeES.CONNECTING; es.onerror?.({}); });
    expect(invalidate).not.toHaveBeenCalledWith(expect.objectContaining({ queryKey: ["me"] }));
    act(() => { es.readyState = FakeES.CLOSED; es.onerror?.({}); });
    expect(invalidate).toHaveBeenCalledWith(expect.objectContaining({ queryKey: ["me"] }));
  });

  it("opens no event stream outside Live", async () => {
    vi.stubGlobal("EventSource", FakeES);
    mount();
    await screen.findByRole("table", { name: "Services" });
    expect(FakeES.last).toBeUndefined();
  });
});
