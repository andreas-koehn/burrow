import { describe, it, expect, vi, afterEach } from "vitest";
import { act, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { Route, Routes, useLocation } from "react-router-dom";
import Services from "@/pages/Services";

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

  it("leaves gateway-only services out by default", async () => {
    db.services.find((x) => x.id === "svc_ai001")!.gateway_only = true;
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getByRole("link", { name: "web" })).toBeInTheDocument();
    expect(within(table).queryByRole("link", { name: "ollama" })).toBeNull();
    expect(screen.getByRole("button", { name: "AI" })).toHaveAttribute("aria-pressed", "false");
    // Not every service is hidden, so nothing is said about the filter.
    expect(screen.queryByText("All services here belong to the AI gateway.")).toBeNull();
  });

  it("the AI filter shows them with a link to their provider", async () => {
    db.services.find((x) => x.id === "svc_ai001")!.gateway_only = true;
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    const before = within(table).getAllByRole("row").length;
    await userEvent.click(screen.getByRole("button", { name: "AI" }));
    expect(screen.getByRole("button", { name: "AI" })).toHaveAttribute("aria-pressed", "true");
    // The choice is in the URL, so a reload or a shared link keeps it.
    expect(currentLocation()).toBe("/services?ai=1");
    // The others stay listed: the filter adds rows, it does not swap the list.
    expect(within(table).getAllByRole("row")).toHaveLength(before + 1);
    const row = within(table).getByRole("link", { name: "ollama" }).closest("tr")!;
    expect(within(row).getByRole("link", { name: "Provider: ollama" })).toHaveAttribute("href", "/gateway/providers/ollama");
    // It has no address of its own to show or copy.
    expect(within(row).getByText("gateway only")).toBeInTheDocument();
    expect(within(row).queryByText(/\/svc\//)).toBeNull();
    expect(within(row).queryByRole("button", { name: /copy url/i })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "AI" }));
    expect(currentLocation()).toBe("/services");
    expect(within(table).queryByRole("link", { name: "ollama" })).toBeNull();
  });

  it("opens with the AI filter on when the URL says so", async () => {
    db.services.find((x) => x.id === "svc_ai001")!.gateway_only = true;
    mountAt("/services?ai=1");
    const table = await screen.findByRole("table", { name: "Services" });
    expect(screen.getByRole("button", { name: "AI" })).toHaveAttribute("aria-pressed", "true");
    expect(within(table).getByRole("link", { name: "ollama" })).toBeInTheDocument();
  });

  it("a gateway-only service whose provider is gone is shown without a provider link", async () => {
    const svc = db.services.find((x) => x.id === "svc_web01")!;
    svc.gateway_only = true;
    mountAt("/services?ai=1");
    const table = await screen.findByRole("table", { name: "Services" });
    const row = within(table).getByRole("link", { name: "web" }).closest("tr")!;
    expect(within(row).getByText("gateway only")).toBeInTheDocument();
    expect(within(row).queryByRole("link", { name: /^Provider:/ })).toBeNull();
  });

  it("says when the filter hides everything", async () => {
    db.services = db.services.filter((x) => x.id === "svc_ai001");
    db.services[0]!.gateway_only = true;
    mount();
    expect(await screen.findByText("All services here belong to the AI gateway.")).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.queryByText("No services yet")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Show gateway-only services" }));
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getByRole("link", { name: "ollama" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "AI" })).toHaveAttribute("aria-pressed", "true");
    expect(currentLocation()).toBe("/services?ai=1");
  });

  it("the filter reads All | Online and there is no banner explaining it", async () => {
    mount();
    const filter = await screen.findByRole("radiogroup", { name: "Show" });
    expect(within(filter).getAllByRole("radio").map((r) => r.textContent)).toEqual(["All", "Online"]);
    await screen.findByRole("table", { name: "Services" });
    expect(screen.queryByRole("note")).toBeNull();
    expect(screen.queryByText(/saved configuration/)).toBeNull();
    expect(screen.queryByRole("radio", { name: "Live" })).toBeNull();
  });

  it("old links keep working", async () => {
    mountAt("/services?live=1");
    expect(await screen.findByRole("radio", { name: "Online" })).toBeChecked();
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getByRole("columnheader", { name: "Client" })).toBeInTheDocument();
  });

  it("shows the full URL", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    const row = within(table).getByRole("link", { name: "grafana" }).closest("tr")!;
    expect(within(row).getByText("https://tunnels.example.com/svc/gf7x1p/")).toBeInTheDocument();
  });

  it("copies each http service's full URL", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getByRole("columnheader", { name: "URL" })).toBeInTheDocument();
    expect(within(table).queryByRole("columnheader", { name: "Hostname" })).toBeNull();
    expect(within(table).getByText("https://tunnels.example.com/svc/k7p2qx/")).toBeInTheDocument();
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
    expect(within(table).getAllByRole("row")).toHaveLength(6); // header + 5 http rows
  });

  it("renders a row per service with name, type, URL, access badge", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    const web = within(table).getByText("web").closest("tr")!;
    expect(within(web).getByText("http")).toBeInTheDocument();
    // http row: URL path shown mono with an aria-labelled copy button
    expect(within(web).getByText("https://tunnels.example.com/svc/k7p2qx/")).toBeInTheDocument();
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

  it("writes every status out with its own badge, not bare muted text (D-12/L-15)", async () => {
    mount();
    const table = await screen.findByRole("table");
    const statusOf = (name: string) => {
      const row = within(table).getByText(name).closest("tr")!;
      return row.querySelector(".badge[class*='status-']:not(.status-idle)")!;
    };
    expect(statusOf("web")).toHaveTextContent(/^Online$/);
    expect(statusOf("web")).toHaveClass("status-online");
    expect(statusOf("grafana")).toHaveTextContent(/^Offline$/);
    expect(statusOf("grafana")).toHaveClass("status-offline");
    expect(statusOf("grafana")).not.toHaveClass("muted");
    expect(statusOf("lmstudio")).toHaveTextContent(/^Never connected$/);
    expect(statusOf("lmstudio")).toHaveClass("status-never");
    for (const old of ["idle", "connected"]) expect(within(table).queryByText(old)).toBeNull();
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

  describe("a click anywhere in a row", () => {
    function mountWithDetail() {
      return renderApp(
        <>
          <Routes>
            <Route path="/services" element={<Services />} />
            <Route path="/services/:id" element={<div>SERVICE_PAGE</div>} />
          </Routes>
          <LocationProbe />
        </>,
        "/services",
      );
    }
    const webRow = async () =>
      within(await screen.findByRole("table", { name: "Services" })).getByRole("link", { name: "web" }).closest("tr")!;

    it("opens the service", async () => {
      mountWithDetail();
      const row = await webRow();
      expect(row).toHaveClass("clickable");
      // The type cell: no control of its own.
      await userEvent.click(within(row).getByText("http"));
      expect(await screen.findByText("SERVICE_PAGE")).toBeInTheDocument();
      expect(currentLocation()).toBe("/services/svc_web01");
    });

    it("the name stays a real link, the target of the keyboard", async () => {
      mountWithDetail();
      const row = await webRow();
      // The row itself takes no focus and claims no role: one stop per row, the link.
      expect(row).not.toHaveAttribute("tabindex");
      expect(row).not.toHaveAttribute("role");
      const link = within(row).getByRole("link", { name: "web" });
      expect(link).toHaveAttribute("href", "/services/svc_web01");
      link.focus();
      await userEvent.keyboard("{Enter}");
      expect(await screen.findByText("SERVICE_PAGE")).toBeInTheDocument();
    });

    it("the copy button and Configure do not navigate", async () => {
      const writeText = vi.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
      mountWithDetail();
      const row = await webRow();
      await userEvent.click(within(row).getByRole("button", { name: /^Copy URL/ }));
      expect(writeText).toHaveBeenCalledWith("https://tunnels.example.com/svc/k7p2qx/");
      expect(currentLocation()).toBe("/services");
      await userEvent.click(within(row).getByRole("button", { name: "Configure" }));
      expect(await screen.findByRole("dialog", { name: "Access · web" })).toBeInTheDocument();
      expect(currentLocation()).toBe("/services");
      // Nor does a click inside the dialog that Configure opened.
      await userEvent.click(within(screen.getByRole("dialog")).getByRole("radio", { name: "API key" }));
      expect(currentLocation()).toBe("/services");
    });

    it("the status link of an online service leads to the Online filter, not to the service", async () => {
      mountWithDetail();
      const row = await webRow();
      await userEvent.click(within(row).getByRole("link", { name: /^Online/ }));
      expect(currentLocation()).toBe("/services?live=1");
    });

    it("a selection made in the row is not taken for a click", async () => {
      mountWithDetail();
      const row = await webRow();
      const selection = vi.spyOn(window, "getSelection").mockReturnValue({ toString: () => "tunnels.example.com" } as Selection);
      try {
        await userEvent.click(within(row).getByText("http"));
        expect(currentLocation()).toBe("/services");
      } finally {
        selection.mockRestore();
      }
    });

    it("a connection without a service has nothing to open", async () => {
      renderApp(<><Services /><LocationProbe /></>, "/services?live=1");
      const table = await screen.findByRole("table", { name: "Services" });
      const row = within(table).getByText("web-staging").closest("tr")!;
      expect(row).not.toHaveClass("clickable");
      await userEvent.click(within(row).getByText("tcp"));
      expect(currentLocation()).toBe("/services?live=1");
    });
  });

  it("online service status badge links to the Live filter (P3B.2)", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    const web = within(table).getByText("web").closest("tr")!;
    // The accessible name contains the visible label.
    const onlineLink = within(web).getByRole("link", { name: "Online: view live tunnel for web" });
    expect(onlineLink).toHaveAttribute("href", "/services?live=1");
    expect(onlineLink).toHaveTextContent(/^Online$/);
  });

  it("offline service status badge is NOT wrapped in a link (P3B.2)", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    const gf = within(table).getByText("grafana").closest("tr")!;
    expect(within(gf).queryByRole("link", { name: /view live tunnel for grafana/i })).toBeNull();
    // The badge itself must still be present
    expect(within(gf).getByText("Offline")).toBeInTheDocument();
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

    // The Access mode combobox is present (Select renders a combobox with aria-haspopup="listbox")
    const accessModeBtn = screen.getByRole("combobox", { name: /access mode/i });
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

  // The flow that made a service and its provider from here lives in "New provider" now.
  it("/services?new=ai leads to the guided steps under Providers, in place of this entry", async () => {
    let posted = 0;
    server.use(http.post("/api/v1/services", () => { posted++; return HttpResponse.json({ id: "x" }, { status: 201 }); }));
    renderApp(
      <>
        <Routes>
          <Route path="/services" element={<Services />} />
          <Route path="/gateway/providers" element={<div>PROVIDERS_PAGE</div>} />
        </Routes>
        <LocationProbe />
      </>,
      "/services?new=ai",
    );
    expect(await screen.findByText("PROVIDERS_PAGE")).toBeInTheDocument();
    expect(currentLocation()).toBe("/gateway/providers?new=local");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(posted).toBe(0);
  });

  it("/services?new=ai keeps nothing of the old address behind: the redirect is not undone", async () => {
    renderApp(
      <>
        <Routes>
          <Route path="/services" element={<Services />} />
          <Route path="/gateway/providers" element={<div>PROVIDERS_PAGE</div>} />
        </Routes>
        <LocationProbe />
      </>,
      "/services?new=ai&live=1",
    );
    await screen.findByText("PROVIDERS_PAGE");
    // The page's own handling of ?new must not run after the redirect and bring the reader back.
    await new Promise((r) => setTimeout(r, 100));
    expect(currentLocation()).toBe("/gateway/providers?new=local");
  });

  it("offers one dialog only: a service is created without a provider", async () => {
    let providerCalls = 0;
    server.use(http.post("/api/v1/ai/providers", () => { providerCalls++; return HttpResponse.json({ slug: "x" }, { status: 201 }); }));
    mount();
    await screen.findByRole("table", { name: /services/i });
    expect(screen.getAllByRole("button", { name: /^new /i }).map((b) => b.textContent)).toEqual(["New service"]);
    await userEvent.click(screen.getByRole("button", { name: "New service" }));
    const dialog = await screen.findByRole("dialog", { name: "New service" });
    expect(within(dialog).getByLabelText("Access mode")).toBeInTheDocument();
    await userEvent.type(within(dialog).getByLabelText("Service ID"), "plain-svc");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await screen.findByText("Service plain-svc created.");
    expect(providerCalls).toBe(0);
    expect(db.services.find((x) => x.id === "plain-svc")?.gateway_only).toBe(false);
  });

  it("the plain New service dialog still shows the slug field and the /svc/ hint", async () => {
    renderApp(<Services />, "/services?new=1");
    const dialog = await screen.findByRole("dialog", { name: "New service" });
    expect(within(dialog).getByLabelText(/url slug/i)).toBeInTheDocument();
    expect(dialog.textContent).toMatch(/\/svc\//);
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

describe("Services page — All | Online", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    FakeES.last = undefined;
  });

  const rowCount = () => within(screen.getByRole("table", { name: "Services" })).getAllByRole("row").length;

  it("sorts by status: Online first, then Offline, then Never connected, by name within each", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    const order = () => Array.from(table.querySelectorAll("tbody tr")).map((tr) => [
      tr.querySelector("td.col-name")!.textContent,
      tr.querySelector("td:nth-last-child(2) .badge")!.textContent,
    ]);
    await userEvent.click(within(table).getByRole("button", { name: /^Sort by status/ }));
    expect(within(table).getByRole("button", { name: /^Sort by status/ }).closest("th")).toHaveAttribute("aria-sort", "ascending");
    expect(order()).toEqual([
      ["ollama", "Online"], ["postgres", "Online"], ["web", "Online"],
      ["grafana", "Offline"], ["llamacpp", "Offline"],
      ["lmstudio", "Never connected"],
    ]);
    await userEvent.click(within(table).getByRole("button", { name: /^Sort by status/ }));
    expect(order().map((r) => r[1])).toEqual(["Never connected", "Offline", "Offline", "Online", "Online", "Online"]);
  });

  it("says which column the table is sorted by, and in which direction", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    const header = (name: string) => within(table).getByRole("button", { name: new RegExp(`^Sort by ${name}`) }).closest("th")!;
    // Default: by type, ascending.
    expect(header("type")).toHaveAttribute("aria-sort", "ascending");
    expect(header("name")).not.toHaveAttribute("aria-sort");
    await userEvent.click(within(table).getByRole("button", { name: /^Sort by name/ }));
    expect(header("name")).toHaveAttribute("aria-sort", "ascending");
    expect(header("type")).not.toHaveAttribute("aria-sort");
    await userEvent.click(within(table).getByRole("button", { name: /^Sort by name/ }));
    expect(header("name")).toHaveAttribute("aria-sort", "descending");
  });

  it("has an All | Online filter that follows the URL", async () => {
    mount();
    const filter = await screen.findByRole("radiogroup", { name: "Show" });
    expect(within(filter).getByRole("radio", { name: "All" })).toBeChecked();
    await screen.findByRole("table", { name: "Services" });
    const all = rowCount();
    await userEvent.click(within(filter).getByRole("radio", { name: "Online" }));
    await waitFor(() => expect(within(screen.getByRole("table", { name: "Services" })).getByRole("columnheader", { name: "Client" })).toBeInTheDocument());
    expect(rowCount()).toBeLessThan(all); // the fixtures contain services no client serves
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
    const live = screen.getByRole("radio", { name: "Online" });
    expect(live).toBeChecked();
    expect(live).toHaveFocus();
    expect(live).toHaveAttribute("tabindex", "0");
    expect(all).toHaveAttribute("tabindex", "-1");
  });

  it("opens on Live when the URL says so and shows who holds each connection", async () => {
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    expect(screen.getByRole("radio", { name: "Online" })).toBeChecked();
    expect(within(table).getByRole("columnheader", { name: "Client" })).toBeInTheDocument();
    const holders = await within(table).findAllByRole("link", { name: /office-box-1/ });
    expect(holders).toHaveLength(2);
    expect(holders[0]).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
    expect(within(table).queryByText("Offline")).toBeNull();
    expect(within(table).queryByText("Never connected")).toBeNull();
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
    expect(within(row).getByText("https://tunnels.example.com/svc/ai4m2q/")).toBeInTheDocument();
    expect(within(row).getByText("API key")).toBeInTheDocument();
    expect(within(row).getByText("127.0.0.1:11434")).toBeInTheDocument();
    expect(within(row).queryByText(":0")).toBeNull(); // an http tunnel has no remote port
    expect(within(row).getByText("Online")).toHaveClass("badge", "status-online");
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
    await userEvent.click(screen.getByRole("radio", { name: "Online" }));
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

  it("leaves the connection of a gateway-only service out of Online too, until AI is on", async () => {
    db.services.find((x) => x.id === "svc_ai001")!.gateway_only = true;
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getByText("web-staging")).toBeInTheDocument();
    expect(within(table).queryByRole("link", { name: "ollama" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "AI" }));
    expect(currentLocation()).toBe("/services?live=1&ai=1");
    const row = within(table).getByRole("link", { name: "ollama" }).closest("tr")!;
    expect(within(row).getByRole("link", { name: "Provider: ollama" })).toHaveAttribute("href", "/gateway/providers/ollama");
  });

  it("says so when nothing is live", async () => {
    server.use(http.get("/api/v1/tunnels", () => HttpResponse.json([])));
    mountAt("/services?live=1");
    expect(await screen.findByText("Nothing is online right now")).toBeInTheDocument();
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

  it("a non-admin in Live makes no /clients request, not even on a stream event", async () => {
    vi.stubGlobal("EventSource", FakeES);
    db.me.role = "user";
    const asked: string[] = [];
    server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
    try {
      mountAt("/services?live=1");
      const table = await screen.findByRole("table", { name: "Services" });
      await waitFor(() => expect(asked).toContain("/api/v1/me"));
      const tunnelCalls = () => asked.filter((p) => p === "/api/v1/tunnels").length;
      const before = tunnelCalls();
      act(() => FakeES.last!.emit("tunnels"));
      await waitFor(() => expect(tunnelCalls()).toBeGreaterThan(before));
      expect(asked.filter((p) => p.startsWith("/api/v1/clients") && p !== "/api/v1/clients/connect-info")).toEqual([]);
      expect(within(table).queryByRole("link", { name: /office-box-1/ })).toBeNull();
    } finally {
      server.events.removeAllListeners();
    }
  });

  it("a stream event refreshes the tunnels and the clients list, not every client's detail", async () => {
    vi.stubGlobal("EventSource", FakeES);
    const asked: string[] = [];
    server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
    try {
      mountAt("/services?live=1");
      const table = await screen.findByRole("table", { name: "Services" });
      await within(table).findAllByRole("link", { name: /office-box-1/ });
      const count = (path: string) => asked.filter((p) => p === path).length;
      const lists = count("/api/v1/clients");
      act(() => FakeES.last!.emit("tunnels"));
      await waitFor(() => expect(count("/api/v1/clients")).toBeGreaterThan(lists));
      expect(count("/api/v1/clients/sess_4f7a9c0b2e81")).toBe(1);
    } finally {
      server.events.removeAllListeners();
    }
  });

  it("polls the tunnels, the clients and their details every 30 s, for when there is no event stream", async () => {
    // Time moves on by itself too, so the first load and findBy keep working.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const asked: string[] = [];
    server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
    try {
      mountAt("/services?live=1");
      const table = await screen.findByRole("table", { name: "Services" });
      await within(table).findAllByRole("link", { name: /office-box-1/ });
      const paths = ["/api/v1/tunnels", "/api/v1/clients", "/api/v1/clients/sess_4f7a9c0b2e81"];
      const counts = () => paths.map((p) => asked.filter((a) => a === p).length);
      const before = counts();
      await act(() => vi.advanceTimersByTimeAsync(25_000));
      expect(counts()).toEqual(before);
      await act(() => vi.advanceTimersByTimeAsync(6_000));
      await waitFor(() => expect(counts()).toEqual(before.map((n) => n + 1)));
    } finally {
      vi.useRealTimers();
      server.events.removeAllListeners();
    }
  });

  it("Live does not wait for the saved services: the tunnel rows show while that request is open", async () => {
    server.use(http.get("/api/v1/services", () => new Promise<never>(() => {})));
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getAllByRole("row")).toHaveLength(3); // header + the two live tunnels
    expect(within(table).getByText("ollama")).toBeInTheDocument();
  });

  it("Live still shows the tunnel rows when the saved services cannot be loaded", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getAllByRole("row")).toHaveLength(3); // header + the two live tunnels
    expect(screen.queryByRole("alert")).toBeNull();
    // Without the join the row falls back to what the tunnel itself says.
    const row = within(table).getByText("ollama").closest("tr")!;
    expect(within(row).getByRole("link", { name: "ollama" })).toHaveAttribute("href", "/services/svc_ai001");
    expect(within(row).getByText("https://tunnels.example.com/svc/ai4m2q/")).toBeInTheDocument();
    expect(within(row).getByText("API key")).toBeInTheDocument();
    // All still reports the failure.
    await userEvent.click(screen.getByRole("radio", { name: "All" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/boom/);
  });

  it("Configure works on a live http row without a joined service row", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mountAt("/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    const row = within(table).getByText("ollama").closest("tr")!;
    await userEvent.click(within(row).getByRole("button", { name: /configure/i }));
    expect(await screen.findByRole("dialog", { name: "Access · ollama" })).toBeInTheDocument();
    const modes = await screen.findByRole("radiogroup", { name: /access mode/i });
    expect(within(modes).getByRole("radio", { name: /API key/ })).toBeChecked();
    // A tcp tunnel has no service to configure.
    const tcp = within(table).getByText("web-staging").closest("tr")!;
    expect(within(tcp).queryByRole("button", { name: /configure/i })).toBeNull();
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
    await screen.findByText("Nothing is online right now");
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
