import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act, waitFor, fireEvent, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, Link, useLocation } from "react-router-dom";
import { ThemeProvider } from "./theme-provider";
import { Layout } from "./Layout";

interface Options {
  path?: string;
  services?: unknown[];
  /** GET /clients/known; "missing" is a relay that predates it (404). */
  known?: unknown[] | "missing";
  /** GET /clients: the live sessions, which only a relay without the list above is asked for. */
  clients?: unknown[];
  /** GET /settings; email is set up unless a test says otherwise. */
  settings?: Record<string, string> | "fail";
  /** What /me says the caller may do; left out, as on a relay from before /me carried it (the role decides). */
  permissions?: string[];
}

const EMAIL_SET_UP = { "smtp.host": "smtp.example.com" };

/** The path the router is on, for tests that follow a sidebar link. */
function Where() {
  return <span data-testid="where">{useLocation().pathname}</span>;
}

const settingsRequests = { count: 0 };

/** A row of GET /clients/known: an online client and its live sessions. */
const online = (token_id: string, token_name: string, ...sessionIds: string[]) => ({
  token_id, token_name, user_id: "u1", status: "online", last_seen: null,
  os: "", arch: "", client_version: "", remote_addr: "", served: [],
  sessions: sessionIds.map((session_id) => ({ session_id, token_name, user_id: "u1" })),
});

function renderLayout(meRole?: "admin" | "user" | "operator" | null, { path = "/services", services, known, clients, settings = EMAIL_SET_UP, permissions }: Options = {}) {
  // Mock fetch: /api/v1/me returns a user with the given role, or 401 if null.
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
    const u = String(url);
    if (u.includes("/api/v1/me")) {
      if (meRole == null) {
        return new Response(JSON.stringify({ error: "unauthorized" }), { status: 401 }) as Response;
      }
      return new Response(
        JSON.stringify({ id: "u1", email: "alice@example.com", role: meRole, ...(permissions ? { permissions } : {}) }),
        { status: 200 }
      ) as Response;
    }
    if (services && u.endsWith("/api/v1/services")) return new Response(JSON.stringify(services), { status: 200 }) as Response;
    if (u.endsWith("/api/v1/settings")) {
      settingsRequests.count += 1;
      return (settings === "fail"
        ? new Response(JSON.stringify({ error: "boom" }), { status: 500 })
        : new Response(JSON.stringify(settings), { status: 200 })) as Response;
    }
    if (known === "missing" && u.endsWith("/api/v1/clients/known")) return new Response(JSON.stringify({ error: "client not found" }), { status: 404 }) as Response;
    if (known && u.endsWith("/api/v1/clients/known")) return new Response(JSON.stringify(known), { status: 200 }) as Response;
    if (clients && u.endsWith("/api/v1/clients")) return new Response(JSON.stringify(clients), { status: 200 }) as Response;
    return new Response("{}", { status: 200 }) as Response;
  });

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route element={<Layout />}>
              <Route
                path="*"
                element={<div>PAGE <Where /> <Link to="/gateway/cost">to gateway</Link> <Link to="/settings">to settings</Link></div>}
              />
            </Route>
            <Route path="/login" element={<div>LOGIN</div>} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>
  );
}

// The profile link names the email, so it is there once /me has answered — and the permissions with it.
const chip = () => screen.findByRole("link", { name: /^Your profile, alice@example.com/ });
const httpService = (id: string, name: string, connected = true) => ({ id, name, type: "http", connected });
const sidebarLinks = (name: string) =>
  within(screen.getByRole("navigation", { name })).getAllByRole("link").map((a) => a.getAttribute("aria-label"));
/** The workspace tabs at the head of the sidebar: [label, href, aria-current] of each. */
const tabs = () =>
  within(screen.getByRole("navigation", { name: "Workspace" })).getAllByRole("link")
    .map((a) => [a.textContent, a.getAttribute("href"), a.getAttribute("aria-current")]);
const tab = (name: string) => within(screen.getByRole("navigation", { name: "Workspace" })).getByRole("link", { name });
const noTabs = () => {
  expect(screen.queryByRole("navigation", { name: "Workspace" })).toBeNull();
  expect(screen.queryByRole("link", { name: "AI Gateway" })).toBeNull();
};

function resetEnvironment() {
  document.documentElement.classList.remove("dark");
  localStorage.clear();
  vi.mocked(window.matchMedia).mockImplementation((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: vi.fn(),
    removeListener: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }));
}

describe("Layout theme toggle", () => {
  beforeEach(resetEnvironment);

  it("renders the toggle button with correct aria-label in light mode", () => {
    renderLayout();
    const btn = screen.getByRole("button", { name: "Switch to dark theme" });
    expect(btn).toBeInTheDocument();
  });

  it("clicking the toggle switches to dark and updates aria-label", async () => {
    renderLayout();
    const btn = screen.getByRole("button", { name: "Switch to dark theme" });
    await act(async () => { btn.click(); });
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    expect(screen.getByRole("button", { name: "Switch to light theme" })).toBeInTheDocument();
  });

  it("clicking toggle twice returns to light and correct aria-label", async () => {
    renderLayout();
    const btn = screen.getByRole("button", { name: "Switch to dark theme" });
    await act(async () => { btn.click(); });
    const darkBtn = screen.getByRole("button", { name: "Switch to light theme" });
    await act(async () => { darkBtn.click(); });
    expect(document.documentElement.classList.contains("dark")).toBe(false);
    expect(screen.getByRole("button", { name: "Switch to dark theme" })).toBeInTheDocument();
  });

  it("renders Log out as an icon button in the footer row (L2)", async () => {
    renderLayout("user");
    const logout = await screen.findByRole("button", { name: "Log out" });
    expect(logout.className).toContain("icon-btn");
    expect(logout.closest(".sidebar-footer")).not.toBeNull();
    // Same row as the theme toggle, not a row of its own.
    expect(logout.parentElement).toBe(
      screen.getByRole("button", { name: /switch to/i }).parentElement,
    );
  });

  it("theme toggle exposes a title matching aria-label for hover-tooltip parity (D1)", () => {
    renderLayout("user");
    const btn = screen.getByRole("button", { name: /switch to dark theme/i });
    expect(btn.getAttribute("title")).toBe(btn.getAttribute("aria-label"));
  });
});

describe("Layout workspace shell", () => {
  beforeEach(resetEnvironment);
  afterEach(() => vi.restoreAllMocks());

  it("starts with the sidebar collapsed on a narrow screen, unless the reader chose otherwise", async () => {
    const width = window.innerWidth;
    const setWidth = (w: number) => Object.defineProperty(window, "innerWidth", { value: w, configurable: true, writable: true });
    try {
      localStorage.removeItem("burrow.sidebarCollapsed");
      setWidth(390);
      const narrow = renderLayout("admin", { path: "/gateway/models" });
      await chip();
      expect(document.querySelector(".sidebar")).toHaveClass("is-collapsed");
      narrow.unmount();
      // A stored choice wins over the width.
      localStorage.setItem("burrow.sidebarCollapsed", "0");
      const chosen = renderLayout("admin", { path: "/gateway/models" });
      await chip();
      expect(document.querySelector(".sidebar")).not.toHaveClass("is-collapsed");
      chosen.unmount();
      // A wide screen starts open.
      localStorage.removeItem("burrow.sidebarCollapsed");
      setWidth(1440);
      renderLayout("admin", { path: "/gateway/models" });
      await chip();
      expect(document.querySelector(".sidebar")).not.toHaveClass("is-collapsed");
    } finally {
      setWidth(width);
      localStorage.removeItem("burrow.sidebarCollapsed");
    }
  });

  it("shows the Services sidebar on a services path", async () => {
    renderLayout("admin", { path: "/" });
    await chip();
    expect(sidebarLinks("Services")).toEqual(["Overview", "Services", "Clients", "Traffic"]);
    expect(screen.getByRole("link", { name: "Overview" })).toHaveAttribute("aria-current", "page");
    await screen.findByRole("navigation", { name: "Workspace" });
    expect(tabs()).toEqual([["Services", "/", "true"], ["AI Gateway", "/gateway", null]]);
    expect(screen.queryByRole("button", { name: /^Workspace/ })).toBeNull();
  });

  it("shows the AI Gateway sidebar on a gateway path", async () => {
    renderLayout("admin", { path: "/gateway" });
    await screen.findByRole("navigation", { name: "AI Gateway" });
    expect(tabs()).toEqual([["Services", "/", null], ["AI Gateway", "/gateway", "true"]]);
    expect(sidebarLinks("AI Gateway")).toEqual(["Overview", "Clients", "Providers", "Models", "Gateway keys", "Guardrails", "Prompt cache", "Requests", "Cost & budgets"]);
    expect(screen.getByRole("link", { name: /^Clients(,|$)/ })).toHaveAttribute("href", "/clients");
  });

  // Clients belong to the relay: one page at /clients, shown in the workspace the reader came from.
  it("Clients opened from the AI Gateway keeps the AI Gateway sidebar and breadcrumb", async () => {
    renderLayout("admin", { path: "/gateway/providers", known: [online("tok_1", "office-box-1", "sess_1")] });
    const gateway = await screen.findByRole("navigation", { name: "AI Gateway" });
    await userEvent.click(await within(gateway).findByRole("link", { name: "Clients, 1 online" }));
    expect(screen.getByTestId("where")).toHaveTextContent("/clients");
    const nav = within(screen.getByRole("navigation", { name: "AI Gateway" }));
    expect(nav.getByRole("link", { name: "Providers" })).not.toHaveAttribute("aria-current");
    expect(nav.getByRole("link", { name: "Models" })).toBeInTheDocument();
    expect(nav.getByRole("link", { name: "Clients, 1 online" })).toHaveAttribute("aria-current", "page");
    expect(document.querySelectorAll('.sidebar [aria-current="page"]')).toHaveLength(1);
    // The tab follows the sidebar, not the URL: Clients has no workspace of its own.
    expect(tabs()).toEqual([["Services", "/", null], ["AI Gateway", "/gateway", "true"]]);
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumbs).getByRole("link", { name: "AI Gateway" })).toHaveAttribute("href", "/gateway");
    expect(within(crumbs).getByText("Clients")).toHaveAttribute("aria-current", "page");
    expect(crumbs).toHaveTextContent(/^AI Gateway\W*Clients$/);
    // Being on Clients changes nothing about where the reader came from.
    expect(localStorage.getItem("burrow.lastWorkspace")).toBe("gateway");
  });

  it("Clients opened from Services keeps the Services sidebar", async () => {
    localStorage.setItem("burrow.lastWorkspace", "gateway"); // an earlier visit
    renderLayout("admin", { path: "/services" });
    const services = await screen.findByRole("navigation", { name: "Services" });
    await userEvent.click(await within(services).findByRole("link", { name: /^Clients(,|$)/ }));
    expect(screen.getByTestId("where")).toHaveTextContent("/clients");
    expect(sidebarLinks("Services")).toEqual(["Overview", "Services", "Clients", "Traffic"]);
    expect(screen.getByRole("link", { name: /^Clients(,|$)/ })).toHaveAttribute("aria-current", "page");
    expect(screen.queryByRole("navigation", { name: "AI Gateway" })).toBeNull();
    expect(tabs()).toEqual([["Services", "/", "true"], ["AI Gateway", "/gateway", null]]);
    expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toHaveTextContent(/^Services\W*Clients$/);
  });

  it("Clients opened directly, with nothing remembered, shows the Services sidebar", async () => {
    renderLayout("admin", { path: "/clients" });
    await chip();
    expect(screen.getByRole("navigation", { name: "Services" })).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "AI Gateway" })).toBeNull();
    expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toHaveTextContent(/^Services\W*Clients$/);
    expect(localStorage.getItem("burrow.lastWorkspace")).toBeNull();
  });

  it("a client's own page stays in the AI Gateway when the reader came from there", async () => {
    localStorage.setItem("burrow.lastWorkspace", "gateway");
    renderLayout("admin", { path: "/clients/sess_1", known: [online("tok_1", "office-box-1", "sess_1")] });
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(await within(crumbs).findByText("office-box-1")).toHaveAttribute("aria-current", "page");
    expect(crumbs).toHaveTextContent(/^AI Gateway\W*Clients\W*office-box-1$/);
    expect(within(screen.getByRole("navigation", { name: "AI Gateway" })).getByRole("link", { name: "Clients, 1 online" }))
      .toHaveAttribute("aria-current", "page");
  });

  it("without the AI gateway, Clients shows the Services sidebar whatever is remembered", async () => {
    localStorage.setItem("burrow.lastWorkspace", "gateway");
    renderLayout("user", { path: "/clients", services: [httpService("svc1", "ollama", false)] });
    await screen.findByRole("link", { name: "Services, 1" });
    await chip();
    expect(screen.getByRole("navigation", { name: "Services" })).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "AI Gateway" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Providers" })).toBeNull();
    noTabs();
    expect(screen.getByRole("link", { name: "Clients" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toHaveTextContent(/^Services\W*Clients$/);
    // What was remembered is left as it was.
    expect(localStorage.getItem("burrow.lastWorkspace")).toBe("gateway");
  });

  it("shows the Settings sidebar on a settings path, with a way back", async () => {
    renderLayout("admin", { path: "/settings" });
    expect(await screen.findByRole("link", { name: "Users" })).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Settings" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to Services" })).toHaveAttribute("href", "/");
    expect(screen.queryByRole("navigation", { name: "Workspace" })).toBeNull();
    // The footer shortcuts would only repeat the list above them.
    expect(screen.queryByRole("link", { name: "Users & roles" })).toBeNull();
  });

  it("marks the page's entry in the Settings navigation", async () => {
    renderLayout("admin", { path: "/settings/email" });
    const nav = within(await screen.findByRole("navigation", { name: "Settings" }));
    expect(await nav.findByRole("link", { name: "Email" })).toHaveAttribute("aria-current", "page");
    expect(nav.getAllByRole("link").filter((a) => a.getAttribute("aria-current") === "page")).toHaveLength(1);
    expect(sidebarLinks("Settings")).toEqual([
      "General", "Email", "Retention", "Database", "Backups", "Users", "Roles", "Audit log",
      "Webhooks", "API reference", "Profile & password", "Sessions", "Automation tokens",
    ]);
  });

  it("a non-admin sees only the Personal group in Settings", async () => {
    renderLayout("user", { path: "/settings/sessions" });
    await chip();
    expect(sidebarLinks("Settings")).toEqual(["Profile & password", "Sessions", "Automation tokens"]);
    const nav = within(screen.getByRole("navigation", { name: "Settings" }));
    expect(nav.getByRole("link", { name: "Sessions" })).toHaveAttribute("aria-current", "page");
    expect(nav.queryByText("Relay")).toBeNull();
    expect(nav.getByText("Personal")).toBeInTheDocument();
  });

  it("nav links use the design-system .nav-item class and mark the current page", async () => {
    renderLayout("user");
    await chip();
    const services = within(screen.getByRole("navigation", { name: "Services" })).getByRole("link", { name: /^Services(,|$)/ });
    expect(services.className).toContain("nav-item");
    expect(services).toHaveAttribute("aria-current", "page");
  });

  it("the footer zone is the same in both workspaces", async () => {
    const shape = () =>
      [...document.querySelectorAll(".sidebar-footer a, .sidebar-footer button")].map((el) => [el.getAttribute("aria-label"), el.getAttribute("href")]);
    const first = renderLayout("admin", { path: "/services" });
    await screen.findByRole("link", { name: "Users & roles" });
    const inServices = shape();
    first.unmount();
    renderLayout("admin", { path: "/gateway/cost" });
    await screen.findByRole("link", { name: "Users & roles" });
    expect(shape()).toEqual(inServices);
    expect(inServices).toEqual([
      ["Users & roles", "/settings/users"],
      ["Settings", "/settings/general"],
      ["Your profile, alice@example.com", "/settings/profile"],
      ["Switch to dark theme", null],
      ["Log out", null],
    ]);
  });

  it("marks the Settings shortcut while email is not set up", async () => {
    renderLayout("admin", { settings: {} });
    const settings = await screen.findByRole("link", { name: "Settings, needs attention" });
    expect(settings).toHaveAttribute("href", "/settings/email");
    expect(within(settings).getByText("needs attention")).toHaveClass("visually-hidden");
  });

  it("inside Settings, where the shortcut is gone, the Email entry carries the mark", async () => {
    renderLayout("admin", { path: "/settings/general", settings: {} });
    const email = await screen.findByRole("link", { name: "Email, needs attention" });
    expect(email).toHaveAttribute("href", "/settings/email");
    expect(email.closest("nav")).toHaveAttribute("aria-label", "Settings");
    expect(document.querySelectorAll(".nav-attention")).toHaveLength(1);
  });

  it("inside Settings with email set up no entry is marked", async () => {
    settingsRequests.count = 0;
    renderLayout("admin", { path: "/settings/general", settings: EMAIL_SET_UP });
    await screen.findByRole("link", { name: "Email" });
    await waitFor(() => expect(settingsRequests.count).toBe(1));
    await new Promise((r) => setTimeout(r, 20));
    expect(document.querySelector(".nav-attention")).toBeNull();
  });

  it("leaves the Settings shortcut unmarked when email is set up, or when the settings cannot be read", async () => {
    for (const settings of [EMAIL_SET_UP, "fail"] as const) {
      settingsRequests.count = 0;
      const view = renderLayout("admin", { settings });
      await screen.findByRole("link", { name: "Settings" });
      await waitFor(() => expect(settingsRequests.count).toBe(1));
      await new Promise((r) => setTimeout(r, 20));
      expect(screen.queryByRole("link", { name: /needs attention/ })).toBeNull();
      view.unmount();
      vi.restoreAllMocks();
    }
  });

  it("does not ask for the settings on behalf of a non-admin", async () => {
    settingsRequests.count = 0;
    renderLayout("user", { settings: {} });
    await waitFor(() => expect(screen.getByRole("link", { name: /^Your profile, alice/ })).toBeInTheDocument());
    await new Promise((r) => setTimeout(r, 20));
    expect(settingsRequests.count).toBe(0);
  });

  it("a non-admin sees no Settings or Users shortcut, and no tabs without AI access", async () => {
    renderLayout("user");
    await chip();
    await waitFor(() => expect(screen.getByRole("link", { name: /^Your profile, alice/ })).toBeInTheDocument());
    expect(screen.queryByRole("link", { name: "Users & roles" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Settings" })).toBeNull();
    noTabs();
    // The head still says where the reader is: the mark and the workspace's name, as text.
    const head = document.querySelector(".sidebar-brand") as HTMLElement;
    expect(head).toHaveTextContent(/^Services$/);
    expect(within(head).queryByRole("link")).toBeNull();
    expect(sidebarLinks("Services")).toEqual(["Overview", "Services", "Clients", "Traffic"]);
  });

  it("a non-admin with a connected http service gets the tabs", async () => {
    renderLayout("user", { services: [httpService("svc1", "ollama")] });
    await screen.findByRole("navigation", { name: "Workspace" });
    expect(tabs()).toEqual([["Services", "/", "true"], ["AI Gateway", "/gateway", null]]);
  });

  it("a custom role with ai:read:any gets the tabs without a connected service", async () => {
    renderLayout("operator", { permissions: ["ai:read:any"] });
    await screen.findByRole("navigation", { name: "Workspace" });
    expect(tabs()).toEqual([["Services", "/", "true"], ["AI Gateway", "/gateway", null]]);
  });

  it("a click on the AI Gateway tab opens the gateway and its navigation", async () => {
    renderLayout("admin", { path: "/services" });
    await screen.findByRole("navigation", { name: "Workspace" });
    await userEvent.click(tab("AI Gateway"));
    expect(screen.getByTestId("where")).toHaveTextContent(/^\/gateway$/);
    expect(sidebarLinks("AI Gateway")).toEqual(["Overview", "Clients", "Providers", "Models", "Gateway keys", "Guardrails", "Prompt cache", "Requests", "Cost & budgets"]);
    expect(screen.queryByRole("navigation", { name: "Services" })).toBeNull();
    expect(tabs()).toEqual([["Services", "/", null], ["AI Gateway", "/gateway", "true"]]);
    expect(within(screen.getByRole("navigation", { name: "AI Gateway" })).getByRole("link", { name: "Overview" })).toHaveAttribute("aria-current", "page");
    expect(localStorage.getItem("burrow.lastWorkspace")).toBe("gateway");
  });

  it("a click on the Services tab from a gateway page opens the Services overview", async () => {
    renderLayout("admin", { path: "/gateway/models" });
    await screen.findByRole("navigation", { name: "Workspace" });
    await userEvent.click(tab("Services"));
    expect(screen.getByTestId("where")).toHaveTextContent(/^\/$/);
    expect(sidebarLinks("Services")).toEqual(["Overview", "Services", "Clients", "Traffic"]);
    expect(screen.queryByRole("navigation", { name: "AI Gateway" })).toBeNull();
    expect(tabs()).toEqual([["Services", "/", "true"], ["AI Gateway", "/gateway", null]]);
  });

  it("the tabs come before the navigation in the tab order, and Enter follows one", async () => {
    renderLayout("admin", { path: "/services" });
    await screen.findByRole("navigation", { name: "Workspace" });
    const order = [...document.querySelectorAll(".sidebar a, .sidebar button")].map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(order.slice(0, 4)).toEqual(["Services", "AI Gateway", "Search", "Overview"]);
    tab("AI Gateway").focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getByTestId("where")).toHaveTextContent(/^\/gateway$/);
  });

  it("the relay namespace is no longer in the sidebar", async () => {
    renderLayout("admin", { path: "/gateway" });
    await screen.findByRole("navigation", { name: "Workspace" });
    expect(document.querySelector(".sidebar")!.textContent).not.toMatch(/\/svc\/|\/ai\//);
  });

  it("a /gateway deep link without AI access keeps the Services shell and offers no way in", async () => {
    renderLayout("user", { path: "/gateway/providers", services: [httpService("svc1", "ollama", false)] });
    await waitFor(() => expect(screen.getByRole("link", { name: /^Your profile, alice/ })).toBeInTheDocument());
    await screen.findByRole("link", { name: "Services, 1" });
    expect(screen.getByRole("navigation", { name: "Services" })).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "AI Gateway" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Providers" })).toBeNull();
    noTabs();
    // No entry claims to be the current page, and the breadcrumb names no gateway page.
    expect(document.querySelector('.sidebar [aria-current="page"]')).toBeNull();
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(crumbs).toHaveTextContent(/^Services$/);
    expect(within(crumbs).getByRole("link", { name: "Services" })).toHaveAttribute("href", "/");
    // Nor is the gateway remembered as the place Settings leads back to.
    expect(localStorage.getItem("burrow.lastWorkspace")).toBeNull();
    // The page itself renders as before.
    expect(screen.getByText(/PAGE/)).toBeInTheDocument();
  });

  it("stores the last workspace on a gateway path and leaves it alone in Settings", async () => {
    renderLayout("admin", { path: "/services" });
    await screen.findByRole("link", { name: "Users & roles" });
    expect(localStorage.getItem("burrow.lastWorkspace")).toBe("services");
    await userEvent.click(screen.getByRole("link", { name: "to gateway" }));
    expect(localStorage.getItem("burrow.lastWorkspace")).toBe("gateway");
    await userEvent.click(screen.getByRole("link", { name: "to settings" }));
    expect(localStorage.getItem("burrow.lastWorkspace")).toBe("gateway");
    expect(screen.getByRole("link", { name: "Back to AI Gateway" })).toHaveAttribute("href", "/gateway");
  });

  it("shows live counts for services and connected clients", async () => {
    renderLayout("admin", {
      path: "/",
      services: [httpService("svc1", "ollama"), httpService("svc2", "grafana")],
      known: [
        online("tok_1", "office-box-1", "sess_1"),
        { ...online("tok_2", "koehn-laptop"), status: "offline" },
        { ...online("tok_3", "build-server"), status: "never_connected" },
      ],
    });
    expect(await screen.findByRole("link", { name: "Services, 2" })).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Clients, 1 online" })).toBeInTheDocument();
  });

  // The count is the number of rows the Clients page shows as Online.
  it("counts two online tokens of the same name as two clients", async () => {
    renderLayout("admin", { path: "/", known: [online("tok_1", "laptop", "sess_1"), online("tok_2", "laptop", "sess_2")] });
    expect(await screen.findByRole("link", { name: "Clients, 2 online" })).toBeInTheDocument();
  });

  // No page has to ask: the shell keeps its own count fresh.
  it("refreshes the count every 30 seconds without any page action", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const known = [online("tok_1", "office-box-1", "sess_1")];
      renderLayout("admin", { path: "/services", known });
      expect(await screen.findByRole("link", { name: "Clients, 1 online" })).toBeInTheDocument();
      const asked = () => vi.mocked(globalThis.fetch).mock.calls.filter((c) => String(c[0]).endsWith("/api/v1/clients/known")).length;
      expect(asked()).toBe(1);
      // A second client connects; the relay says so from the next answer on.
      known.push(online("tok_2", "koehn-laptop", "sess_2"));
      await act(async () => { await vi.advanceTimersByTimeAsync(29_000); });
      expect(asked()).toBe(1);
      expect(screen.getByRole("link", { name: "Clients, 1 online" })).toBeInTheDocument();
      await act(async () => { await vi.advanceTimersByTimeAsync(1_500); });
      expect(await screen.findByRole("link", { name: "Clients, 2 online" })).toBeInTheDocument();
      expect(asked()).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not keep asking on behalf of a non-admin", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      renderLayout("user", { known: [online("tok_1", "office-box-1", "sess_1")] });
      await chip();
      await act(async () => { await vi.advanceTimersByTimeAsync(61_000); });
      const urls = vi.mocked(globalThis.fetch).mock.calls.map((c) => String(c[0]));
      expect(urls.some((u) => u.includes("/api/v1/clients"))).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("on a relay without the list of known clients, counts its live sessions, as the Clients page lists them", async () => {
    renderLayout("admin", {
      path: "/clients/sess_2",
      known: "missing",
      clients: [
        { session_id: "sess_1", user_id: "u1", token_name: "office-box-1", service_count: 1 },
        { session_id: "sess_2", user_id: "u1", token_name: "office-box-1", service_count: 1 },
      ],
    });
    expect(await screen.findByRole("link", { name: "Clients, 2 online" })).toBeInTheDocument();
    // The breadcrumb still names the client of a session.
    expect(within(screen.getByRole("navigation", { name: "Breadcrumb" })).getByText("office-box-1")).toHaveAttribute("aria-current", "page");
  });

  it("names every session of a client in the breadcrumb", async () => {
    renderLayout("admin", { path: "/clients/sess_2", known: [online("tok_1", "office-box-1", "sess_1", "sess_2")] });
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(await within(crumbs).findByText("office-box-1")).toHaveAttribute("aria-current", "page");
  });

  it("counts a client that is connected twice once, as the Clients page lists it", async () => {
    renderLayout("admin", { path: "/", known: [online("tok_1", "office-box-1", "sess_1", "sess_2")] });
    expect(await screen.findByRole("link", { name: "Clients, 1 online" })).toBeInTheDocument();
  });

  it("lists Clients without a count and asks for no client list without tunnels:read:any", async () => {
    renderLayout("user");
    await chip();
    await waitFor(() => expect(screen.getByRole("link", { name: /^Your profile, alice/ })).toBeInTheDocument());
    const urls = vi.mocked(globalThis.fetch).mock.calls.map((c) => String(c[0]));
    expect(urls.some((u) => u.includes("/api/v1/services"))).toBe(true);
    expect(urls.some((u) => u.includes("/api/v1/clients"))).toBe(false);
    // The user role gets the Clients entry (tokens, connect flow) without a count, and none is asked for.
    expect(screen.getByRole("link", { name: "Clients" })).toBeInTheDocument();
  });

  it("asks for clients, and counts them, for a custom role with tunnels:read:any", async () => {
    renderLayout("user", { known: [online("tok_1", "office-box-1", "sess_1")], permissions: ["tunnels:read:any"] });
    expect(await screen.findByRole("link", { name: "Clients, 1 online" })).toBeInTheDocument();
  });

  it("the breadcrumb names a service instead of showing its id", async () => {
    renderLayout("admin", { path: "/gateway/requests/svc1/req9", services: [httpService("svc1", "ollama")] });
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(await within(crumbs).findByRole("link", { name: "ollama" })).toHaveAttribute("href", "/gateway/requests/svc1");
    expect(within(crumbs).getByText("req9")).toHaveAttribute("aria-current", "page");
    expect(within(screen.getByRole("navigation", { name: "AI Gateway" })).getByRole("link", { name: "Requests" }))
      .toHaveAttribute("aria-current", "page");
  });

  it("the breadcrumb of a service reads Services / its name, with one Services link", async () => {
    renderLayout("admin", { path: "/services/svc1", services: [httpService("svc1", "ollama")] });
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(await within(crumbs).findByText("ollama")).toHaveAttribute("aria-current", "page");
    expect(crumbs).toHaveTextContent(/^Services\W*ollama$/);
    expect(within(crumbs).getByRole("link", { name: "Services" })).toHaveAttribute("href", "/services");
  });

  it("the breadcrumb says where the connect page is", async () => {
    renderLayout("admin", { path: "/clients/connect" });
    await chip();
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumbs).getByText("Connect a client")).toHaveAttribute("aria-current", "page");
    expect(within(crumbs).getByRole("link", { name: "Clients" })).toHaveAttribute("href", "/clients");
  });

  it("collapses the sidebar and remembers it across a remount", async () => {
    const first = renderLayout("admin", { path: "/" });
    await chip();
    expect(document.querySelector(".sidebar")).not.toHaveClass("is-collapsed");
    await userEvent.click(screen.getByRole("button", { name: "Collapse sidebar" }));
    expect(document.querySelector(".sidebar")).toHaveClass("is-collapsed");
    expect(localStorage.getItem("burrow.sidebarCollapsed")).toBe("1");
    expect(screen.getByRole("link", { name: "Traffic" })).toBeInTheDocument();
    first.unmount();

    renderLayout("admin", { path: "/" });
    expect(document.querySelector(".sidebar")).toHaveClass("is-collapsed");
    await userEvent.click(screen.getByRole("button", { name: "Expand sidebar" }));
    expect(document.querySelector(".sidebar")).not.toHaveClass("is-collapsed");
    expect(localStorage.getItem("burrow.sidebarCollapsed")).toBe("0");
  });

  it("never throws when storage is unavailable", async () => {
    // Only the shell's own keys: the theme provider reads storage unguarded (not this shell's concern).
    const real = { get: Storage.prototype.getItem, set: Storage.prototype.setItem };
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(function (this: Storage, k: string) {
      if (k.startsWith("burrow.")) throw new Error("denied");
      return real.get.call(this, k);
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(function (this: Storage, k: string, v: string) {
      if (k.startsWith("burrow.")) throw new Error("denied");
      real.set.call(this, k, v);
    });
    renderLayout("admin", { path: "/gateway" });
    await chip();
    expect(document.querySelector(".sidebar")).not.toHaveClass("is-collapsed");
    await userEvent.click(screen.getByRole("button", { name: "Collapse sidebar" }));
    expect(document.querySelector(".sidebar")).toHaveClass("is-collapsed");
    expect(Storage.prototype.setItem).toHaveBeenCalledWith("burrow.sidebarCollapsed", "1");
  });

  it("keeps the skip link and the main landmark it targets", async () => {
    renderLayout("user");
    await chip();
    expect(screen.getByRole("link", { name: "Skip to content" })).toHaveAttribute("href", "#main");
    const main = screen.getByRole("main");
    expect(main).toHaveAttribute("id", "main");
    expect(main).toHaveTextContent("PAGE");
    // The top bar sits outside the main landmark the skip link jumps to.
    expect(main.querySelector(".topbar")).toBeNull();
  });

  it("does NOT render a Provisioning nav link (backend pending — issue tracked in BACKLOG_1.0.0.md)", async () => {
    renderLayout("admin");
    await screen.findByRole("link", { name: "Users & roles" });
    expect(screen.queryByRole("link", { name: "Provisioning" })).toBeNull();
  });
});

describe("Layout ⌘K command palette (P6A.3)", () => {
  beforeEach(resetEnvironment);

  it("Ctrl+K on window opens the command palette (role=dialog visible)", async () => {
    renderLayout("admin");
    // Wait for layout to settle
    await chip();
    // Dialog should not be present yet
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    // Fire Ctrl+K
    await act(async () => {
      fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    });
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("the Search shortcut affordance button is present in the sidebar", async () => {
    renderLayout("admin");
    await chip();
    expect(screen.getByRole("button", { name: /search/i })).toBeInTheDocument();
  });

  it("shows a platform-appropriate search shortcut (U3)", async () => {
    renderLayout("user");
    const search = await screen.findByRole("button", { name: "Search" });
    // jsdom reports an empty platform → non-Apple label.
    expect(search).toHaveTextContent("Ctrl K");
  });

  it("clicking the Search button opens the palette", async () => {
    renderLayout("admin");
    await chip();
    const searchBtn = screen.getByRole("button", { name: /search/i });
    await act(async () => { searchBtn.click(); });
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("palette opened as user excludes admin-only 'Users' destination", async () => {
    renderLayout("user");
    // Wait for auth to settle (the user chip is role-neutral)
    await chip();
    // Open palette via Ctrl+K
    await act(async () => {
      fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    });
    // Wait for dialog
    await screen.findByRole("dialog");
    // "Users" destination should not appear for non-admin
    await waitFor(() => {
      expect(screen.queryByRole("option", { name: /users/i })).not.toBeInTheDocument();
    });
  });

  it("palette opened as admin includes admin-only 'Users' destination", async () => {
    renderLayout("admin");
    await screen.findByRole("link", { name: "Users & roles" });
    await act(async () => {
      fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    });
    await screen.findByRole("dialog");
    // Option accessible name includes the group label "Settings" so match broadly.
    expect(await screen.findByRole("option", { name: /^users/i })).toBeInTheDocument();
  });

  it("⌘K reopen resets palette query (stale-query bug fix)", async () => {
    renderLayout("admin");
    await chip();

    // Open via Ctrl+K
    await act(async () => {
      fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    });
    await screen.findByRole("dialog");

    // Type a query that would narrow the list
    const input = screen.getByRole("searchbox");
    fireEvent.change(input, { target: { value: "zzz" } });
    // "No matches" confirms the query filtered everything out
    expect(screen.getByText(/no matches/i)).toBeInTheDocument();

    // Close via Escape on the input
    fireEvent.keyDown(input, { key: "Escape" });
    // Wait for dialog to close
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });

    // Reopen via Ctrl+K — must reset state
    await act(async () => {
      fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    });
    await screen.findByRole("dialog");

    // After reopen the search input should be empty (query reset)
    const inputAfterReopen = screen.getByRole("searchbox");
    expect(inputAfterReopen).toHaveValue("");
    // And the full list is visible again (not filtered)
    expect(screen.queryByText(/no matches/i)).not.toBeInTheDocument();
    expect(screen.getAllByRole("option").length).toBeGreaterThan(0);
  });
});
