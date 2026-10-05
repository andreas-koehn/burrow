import { describe, it, expect } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { render } from "@testing-library/react";
import { ThemeProvider } from "@/components/theme-provider";
import App from "@/App";
import { setCsrfCookie } from "@/mocks/test-utils";
import { OLD_ROUTES } from "@/lib/moved-routes";

function PathProbe() {
  const { pathname, search, hash } = useLocation();
  return <div data-testid="path">{pathname + search + hash}</div>;
}

function renderAt(route: string) {
  setCsrfCookie();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <ThemeProvider><QueryClientProvider client={qc}><MemoryRouter initialEntries={[route]}><App /><PathProbe /></MemoryRouter></QueryClientProvider></ThemeProvider>,
  );
}

describe("App routes", () => {
  it("renders Roles at /roles", async () => {
    renderAt("/roles");
    expect(await screen.findByRole("heading", { name: /^Roles$/i })).toBeInTheDocument();
  });
  it("renders Settings at /settings", async () => {
    renderAt("/settings");
    expect(await screen.findByRole("heading", { name: /^Settings$/i })).toBeInTheDocument();
  });
  it("renders Clients at /clients", async () => {
    renderAt("/clients");
    expect(await screen.findByRole("heading", { name: /^Clients$/i })).toBeInTheDocument();
  });
  it("renders Connect at /clients/connect", async () => {
    renderAt("/clients/connect");
    expect(await screen.findByRole("heading", { name: /connect a client/i })).toBeInTheDocument();
  });
  it("shows the admin shortcuts and the Services entries for an admin", async () => {
    renderAt("/account");
    expect(await screen.findByRole("link", { name: "Users & roles" })).toHaveAttribute("href", "/settings/users");
    expect(screen.getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings/general");
    expect(screen.getByRole("link", { name: /^Clients(,|$)/ })).toHaveAttribute("href", "/clients");
  });

  // ---- v0.4.0 routes; the AI pages live under /gateway/ since the workspace shell ----
  it.each([
    ["/gateway",                       /^AI Gateway$/i],
    ["/gateway/providers",             /^Providers$/i],
    ["/gateway/providers/ollama",      /^Provider · /i],
    ["/gateway/cache",                 /^Prompt cache$/i],
    ["/gateway/guardrails",            /^Guardrails & redaction$/i],
    ["/gateway/requests/svc_ai001",    /^Request inspector$/i],
    ["/gateway/cost",                  /^Cost & budgets$/i],
    ["/audit",                         /^Audit log$/i],
    ["/webhooks",                      /^Webhooks$/i],
    ["/account/automation",            /^Automation tokens$/i],
    ["/settings/backups",              /^Backup & restore$/i],
  ])("resolves %s to its page heading", async (path, heading) => {
    renderAt(path);
    expect(await screen.findByRole("heading", { name: heading })).toBeInTheDocument();
  });

  it("/gateway/requests picks the first http service", async () => {
    renderAt("/gateway/requests");
    expect(await screen.findByRole("heading", { name: /^Request inspector$/i })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId("path").textContent).toMatch(/^\/gateway\/requests\/[^/]+$/));
  });

  // Old bookmarks: every moved path lands on its new page, params, query and hash intact.
  const visit: Record<string, { url: string; lands: string; heading: RegExp }> = {
    "/cache": { url: "/cache", lands: "/gateway/cache", heading: /^Prompt cache$/i },
    "/guardrails": { url: "/guardrails#custom", lands: "/gateway/guardrails#custom", heading: /^Guardrails & redaction$/i },
    "/inspector": { url: "/inspector", lands: "/gateway/requests/", heading: /^Request inspector$/i },
    "/inspector/:serviceId/:requestId?": {
      url: "/inspector/svc_ai001/req9?tab=diff#body", lands: "/gateway/requests/svc_ai001/req9?tab=diff#body", heading: /^Request inspector$/i,
    },
    "/cost": { url: "/cost?window=week", lands: "/gateway/cost?window=week", heading: /^Cost & budgets$/i },
  };
  it("has a bookmark case for every moved route", () => {
    expect(Object.keys(visit).sort()).toEqual(OLD_ROUTES.map((r) => r.from).sort());
  });
  it.each(OLD_ROUTES.map((r) => r.from))("old path %s lands on its new page", async (from) => {
    const c = visit[from];
    renderAt(c.url);
    expect(await screen.findByRole("heading", { name: c.heading })).toBeInTheDocument();
    // "/inspector" goes on to the first http service, so only its prefix is fixed.
    const path = screen.getByTestId("path").textContent ?? "";
    if (c.lands.endsWith("/")) expect(path.startsWith(c.lands)).toBe(true);
    else expect(path).toBe(c.lands);
    // The sidebar shows the workspace the page now belongs to.
    expect(await screen.findByRole("navigation", { name: "AI Gateway" })).toBeInTheDocument();
  });

  it("an old path with one param keeps it", async () => {
    renderAt("/inspector/svc_ai001");
    expect(await screen.findByRole("heading", { name: /^Request inspector$/i })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/gateway\/requests\/svc_ai001$/);
  });

  it("shows the AI Gateway sidebar on a gateway page and marks the parent entry on a detail page", async () => {
    renderAt("/gateway/providers/ollama");
    const nav = within(await screen.findByRole("navigation", { name: "AI Gateway" }));
    const providers = nav.getByRole("link", { name: "Providers" });
    expect(providers).toHaveAttribute("href", "/gateway/providers");
    expect(providers).toHaveAttribute("aria-current", "page");
    expect(nav.getByRole("link", { name: "Cost & budgets" })).toHaveAttribute("href", "/gateway/cost");
    expect(nav.getByRole("link", { name: "Prompt cache" })).toHaveAttribute("href", "/gateway/cache");
    expect(nav.getByRole("link", { name: "Guardrails" })).toHaveAttribute("href", "/gateway/guardrails");
    expect(nav.getByRole("link", { name: "Requests" })).toHaveAttribute("href", "/gateway/requests");
  });

  // Until W03 and W05 move the pages, a navigation entry leads to the page's current address.
  it.each([
    ["/settings/users",      "/users",              /^Users$/i],
    ["/settings/general",    "/settings",           /^Settings$/i],
    ["/settings/profile",    "/account",            /^Account$/i],
    ["/settings/automation", "/account/automation", /^Automation tokens$/i],
    ["/traffic",             "/connection-logs",    /^Connection logs$/i],
  ])("%s leads to the page that has not moved yet (%s)", async (from, lands, heading) => {
    renderAt(from);
    expect(await screen.findByRole("heading", { name: heading, level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(lands);
  });

  it("/provisioning is unreachable (backend pending)", async () => {
    renderAt("/provisioning");
    await waitFor(() => {
      expect(screen.queryByRole("heading", { name: /provisioning keys/i })).toBeNull();
    });
  });
});
