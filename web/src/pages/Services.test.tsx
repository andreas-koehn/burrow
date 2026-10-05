import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
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

function mount() {
  return renderApp(<Services />, "/services");
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
  it("shows a durable-config explainer with a link to /tunnels (role=note, NOT alert)", async () => {
    mount();
    // explainer is always rendered — wait for it after mount
    const explainer = await screen.findByText(/durable saved config/i);
    expect(explainer).toBeInTheDocument();
    // must contain a link to /tunnels
    const wrapper = explainer.closest("[role='note']") ?? explainer.parentElement!;
    const link = wrapper.querySelector("a[href='/tunnels']");
    expect(link).not.toBeNull();
    // must NOT be a role=alert (that role is reserved for the error notice)
    expect(screen.queryByRole("alert", { name: /durable saved config/i })).toBeNull();
  });

  it("connected service status badge links to /tunnels (P3B.2)", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    // "web" service is connected — its badge must be wrapped in a link to /tunnels
    const web = within(table).getByText("web").closest("tr")!;
    const connectedLink = within(web).getByRole("link", { name: /view live tunnel for web/i });
    expect(connectedLink).toHaveAttribute("href", "/tunnels");
  });

  it("idle service status badge is NOT wrapped in a link to /tunnels (P3B.2)", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    // "grafana" service is idle — its status badge must NOT be a link to /tunnels
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
