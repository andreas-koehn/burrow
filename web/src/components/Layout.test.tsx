import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act, waitFor, fireEvent, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, Link } from "react-router-dom";
import { ThemeProvider } from "./theme-provider";
import { Layout } from "./Layout";

interface Options {
  path?: string;
  services?: unknown[];
  clients?: unknown[];
}

function renderLayout(meRole?: "admin" | "user" | null, { path = "/tunnels", services, clients }: Options = {}) {
  // Mock fetch: /api/v1/me returns a user with the given role, or 401 if null.
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
    const u = String(url);
    if (u.includes("/api/v1/me")) {
      if (meRole == null) {
        return new Response(JSON.stringify({ error: "unauthorized" }), { status: 401 }) as Response;
      }
      return new Response(
        JSON.stringify({ id: "u1", email: "alice@example.com", role: meRole }),
        { status: 200 }
      ) as Response;
    }
    if (services && u.endsWith("/api/v1/services")) return new Response(JSON.stringify(services), { status: 200 }) as Response;
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
                element={<div>PAGE <Link to="/gateway/cost">to gateway</Link> <Link to="/settings">to settings</Link></div>}
              />
            </Route>
            <Route path="/login" element={<div>LOGIN</div>} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>
  );
}

const chip = () => screen.findByRole("link", { name: /^Your profile/ });
const httpService = (id: string, name: string, connected = true) => ({ id, name, type: "http", connected });
const sidebarLinks = (name: string) =>
  within(screen.getByRole("navigation", { name })).getAllByRole("link").map((a) => a.getAttribute("aria-label"));

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

  it("shows the Services sidebar on a services path", async () => {
    renderLayout("admin", { path: "/" });
    await chip();
    // Tunnels and Tokens are temporary until W04.
    expect(sidebarLinks("Services")).toEqual(["Overview", "Services", "Clients", "Tunnels", "Tokens", "Traffic"]);
    expect(screen.getByRole("link", { name: "Overview" })).toHaveAttribute("aria-current", "page");
    expect(await screen.findByRole("button", { name: "Workspace: Services" })).toBeInTheDocument();
  });

  it("shows the AI Gateway sidebar on a gateway path", async () => {
    renderLayout("admin", { path: "/gateway" });
    expect(await screen.findByRole("button", { name: "Workspace: AI Gateway" })).toBeInTheDocument();
    expect(sidebarLinks("AI Gateway")).toEqual(["Overview", "Providers", "Guardrails", "Prompt cache", "Requests", "Cost & budgets"]);
    expect(screen.queryByRole("link", { name: "Tunnels" })).toBeNull();
  });

  it("shows the Settings sidebar on a settings path, with a way back", async () => {
    renderLayout("admin", { path: "/settings" });
    expect(await screen.findByRole("link", { name: "Users" })).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Settings" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to Services" })).toHaveAttribute("href", "/");
    expect(screen.queryByRole("button", { name: /^Workspace:/ })).toBeNull();
    // The footer shortcuts would only repeat the list above them.
    expect(screen.queryByRole("link", { name: "Users & roles" })).toBeNull();
  });

  it("nav links use the design-system .nav-item class and mark the current page", async () => {
    renderLayout("user");
    await chip();
    const tunnels = screen.getByRole("link", { name: "Tunnels" });
    expect(tunnels.className).toContain("nav-item");
    expect(tunnels).toHaveAttribute("aria-current", "page");
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

  it("a non-admin sees no Settings or Users shortcut, and no switcher without AI access", async () => {
    renderLayout("user");
    await chip();
    await waitFor(() => expect(screen.getByRole("link", { name: /^Your profile, alice/ })).toBeInTheDocument());
    expect(screen.queryByRole("link", { name: "Users & roles" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Settings" })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Workspace:/ })).toBeNull();
    expect(sidebarLinks("Services")).toEqual(["Overview", "Services", "Clients", "Tunnels", "Tokens", "Traffic"]);
  });

  it("a non-admin with a connected http service gets the switcher", async () => {
    renderLayout("user", { services: [httpService("svc1", "ollama")] });
    expect(await screen.findByRole("button", { name: "Workspace: Services" })).toBeInTheDocument();
  });

  it("a /gateway deep link without AI access keeps the Services shell and offers no way in", async () => {
    renderLayout("user", { path: "/gateway/providers", services: [httpService("svc1", "ollama", false)] });
    await waitFor(() => expect(screen.getByRole("link", { name: /^Your profile, alice/ })).toBeInTheDocument());
    await screen.findByRole("link", { name: "Services, 1" });
    expect(screen.getByRole("navigation", { name: "Services" })).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "AI Gateway" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Providers" })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Workspace:/ })).toBeNull();
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
      clients: [{ session_id: "sess_1", token_name: "office-box-1" }],
    });
    expect(await screen.findByRole("link", { name: "Services, 2" })).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Clients, 1 online" })).toBeInTheDocument();
  });

  it("does not ask for clients as a non-admin", async () => {
    renderLayout("user");
    await chip();
    await waitFor(() => expect(screen.getByRole("link", { name: /^Your profile, alice/ })).toBeInTheDocument());
    const urls = vi.mocked(globalThis.fetch).mock.calls.map((c) => String(c[0]));
    expect(urls.some((u) => u.includes("/api/v1/services"))).toBe(true);
    expect(urls.some((u) => u.includes("/api/v1/clients"))).toBe(false);
    expect(screen.getByRole("link", { name: "Clients" })).toBeInTheDocument();
  });

  it("the breadcrumb names a service instead of showing its id", async () => {
    renderLayout("admin", { path: "/gateway/requests/svc1/req9", services: [httpService("svc1", "ollama")] });
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(await within(crumbs).findByRole("link", { name: "ollama" })).toHaveAttribute("href", "/gateway/requests/svc1");
    expect(within(crumbs).getByText("req9")).toHaveAttribute("aria-current", "page");
    expect(within(screen.getByRole("navigation", { name: "AI Gateway" })).getByRole("link", { name: "Requests" }))
      .toHaveAttribute("aria-current", "page");
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
