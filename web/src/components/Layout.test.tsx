import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, act, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { ThemeProvider } from "./theme-provider";
import { Layout } from "./Layout";

function renderLayout(meRole?: "admin" | "user" | null) {
  // Mock fetch: /api/v1/me returns a user with the given role, or 401 if null.
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
    if (String(url).includes("/api/v1/me")) {
      if (meRole == null) {
        return new Response(JSON.stringify({ error: "unauthorized" }), { status: 401 }) as Response;
      }
      return new Response(
        JSON.stringify({ id: "u1", email: "alice@example.com", role: meRole }),
        { status: 200 }
      ) as Response;
    }
    return new Response("{}", { status: 200 }) as Response;
  });

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/tunnels"]}>
          <Routes>
            <Route element={<Layout />}>
              <Route path="/tunnels" element={<div>TUNNELS</div>} />
            </Route>
            <Route path="/login" element={<div>LOGIN</div>} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>
  );
}

describe("Layout theme toggle", () => {
  beforeEach(() => {
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
  });

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

  it("Log out button is still present alongside toggle", () => {
    renderLayout();
    expect(screen.getByRole("button", { name: /log out/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /switch to/i })).toBeInTheDocument();
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

describe("Layout nav role-gating", () => {
  beforeEach(() => {
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
  });

  it("shows Users nav link when role is admin", async () => {
    renderLayout("admin");
    // findByRole waits for the link to appear (after useAuth resolves)
    expect(await screen.findByRole("link", { name: /^users$/i })).toBeInTheDocument();
  });

  it("hides Users nav link when role is user", async () => {
    renderLayout("user");
    // Wait for auth to settle: the Account link must appear (role-neutral)
    await screen.findByRole("link", { name: /^account$/i });
    // Users link must NOT be present for non-admin
    await waitFor(() => {
      expect(screen.queryByRole("link", { name: /^users$/i })).not.toBeInTheDocument();
    });
  });

  it("Tunnels, Tokens, and Account links are present for both roles", async () => {
    renderLayout("user");
    await screen.findByRole("link", { name: /^account$/i });
    expect(screen.getByRole("link", { name: /^tunnels$/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^tokens$/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^account$/i })).toBeInTheDocument();
  });

  it("Tunnels, Tokens, Account, and Users links are present for admin", async () => {
    renderLayout("admin");
    // Wait for the Users link which only appears after useAuth resolves with admin role
    expect(await screen.findByRole("link", { name: /^users$/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^tunnels$/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^tokens$/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^account$/i })).toBeInTheDocument();
  });

  it("nav links use the design-system .nav-item class", () => {
    renderLayout();
    expect(screen.getByRole("link", { name: /^tunnels$/i }).className).toContain("nav-item");
  });

  it("Connection logs is reachable from /settings, not the top-level sidebar (P2-10)", async () => {
    renderLayout("admin");
    // Wait for admin links to appear (Users only shows for admin).
    await screen.findByRole("link", { name: /^users$/i });
    // Sidebar no longer carries Connection logs — it lives on the /settings page.
    expect(screen.queryByRole("link", { name: /connection logs/i })).toBeNull();
  });

  it("OpenAPI moved off the sidebar and is reached via /openapi (P1-14 + P2-10)", async () => {
    renderLayout("admin");
    await screen.findByRole("link", { name: /^users$/i });
    // No top-level OpenAPI link in the sidebar.
    expect(screen.queryByRole("link", { name: /openapi/i })).toBeNull();
  });

  it("does NOT render Provisioning nav link (backend pending — issue tracked in BACKLOG_1.0.0.md)", async () => {
    renderLayout("admin");
    // Wait for admin role to be active (Users link only appears for admin)
    await screen.findByRole("link", { name: /^users$/i });
    // Now the Access control group is rendered — assert Provisioning link is absent
    await waitFor(() => {
      expect(screen.queryByRole("link", { name: "Provisioning" })).toBeNull();
    });
  });

  it("Home nav link is present with href '/' and is the first nav link inside the sidebar (task 1.1)", async () => {
    renderLayout("user");
    // Wait for auth to settle
    await screen.findByRole("link", { name: /^account$/i });
    const homeLink = screen.getByRole("link", { name: /^home$/i });
    expect(homeLink).toBeInTheDocument();
    expect(homeLink).toHaveAttribute("href", "/");
    // Home must be the first link inside the <nav> sidebar (skip-to-content is outside nav)
    const sidebar = screen.getByRole("navigation", { name: "Main" });
    const navLinks = Array.from(sidebar.querySelectorAll("a"));
    expect(navLinks[0]).toBe(homeLink);
  });
});

describe("Layout ⌘K command palette (P6A.3)", () => {
  beforeEach(() => {
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
  });

  it("Ctrl+K on window opens the command palette (role=dialog visible)", async () => {
    renderLayout("admin");
    // Wait for layout to settle
    await screen.findByRole("link", { name: /^account$/i });
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
    await screen.findByRole("link", { name: /^account$/i });
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
    await screen.findByRole("link", { name: /^account$/i });
    const searchBtn = screen.getByRole("button", { name: /search/i });
    await act(async () => { searchBtn.click(); });
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("palette opened as user excludes admin-only 'Users' destination", async () => {
    renderLayout("user");
    // Wait for auth to settle (Account link is role-neutral)
    await screen.findByRole("link", { name: /^account$/i });
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
    await screen.findByRole("link", { name: /^users$/i });
    await act(async () => {
      fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    });
    await screen.findByRole("dialog");
    // Option accessible name includes the group label "Access control" so match broadly.
    expect(await screen.findByRole("option", { name: /users/i })).toBeInTheDocument();
  });

  it("⌘K reopen resets palette query (stale-query bug fix)", async () => {
    renderLayout("admin");
    await screen.findByRole("link", { name: /^account$/i });

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
