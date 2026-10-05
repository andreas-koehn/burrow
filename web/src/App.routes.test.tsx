import { describe, it, expect, afterEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { render } from "@testing-library/react";
import { ThemeProvider } from "@/components/theme-provider";
import App from "@/App";
import { setCsrfCookie } from "@/mocks/test-utils";
import { db, resetDb } from "@/mocks/db";
import { OLD_ROUTES } from "@/lib/moved-routes";
import { NAVIGATIONS } from "@/lib/navigation";

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
  afterEach(() => resetDb());

  it("renders Clients at /clients", async () => {
    renderAt("/clients");
    expect(await screen.findByRole("heading", { name: /^Clients$/i })).toBeInTheDocument();
  });
  it("renders Connect at /clients/connect", async () => {
    renderAt("/clients/connect");
    expect(await screen.findByRole("heading", { name: /connect a client/i })).toBeInTheDocument();
  });
  it("shows the admin shortcuts and the Services entries for an admin", async () => {
    renderAt("/services");
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
    ["/settings/backups",              /^Backup & restore$/i],
  ])("resolves %s to its page heading", async (path, heading) => {
    renderAt(path);
    expect(await screen.findByRole("heading", { name: heading })).toBeInTheDocument();
  });

  // ---- Settings: every relay-wide and personal page has its own address and sidebar entry ----
  it.each([
    ["/settings/general",    /^General$/,            "General"],
    ["/settings/email",      /^Email$/,              "Email"],
    ["/settings/users",      /^Users$/,              "Users"],
    ["/settings/roles",      /^Roles$/,              "Roles"],
    ["/settings/audit",      /^Audit log$/,          "Audit log"],
    ["/settings/webhooks",   /^Webhooks$/,           "Webhooks"],
    ["/settings/api",        /^API reference$/,      "API reference"],
    ["/settings/profile",    /^Profile & password$/, "Profile & password"],
    ["/settings/sessions",   /^Sessions$/,           "Sessions"],
    ["/settings/automation", /^Automation tokens$/,  "Automation tokens"],
  ])("%s renders its page inside the Settings navigation", async (path, heading, entry) => {
    renderAt(path);
    expect(await screen.findByRole("heading", { name: heading, level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(path);
    const nav = within(screen.getByRole("navigation", { name: "Settings" }));
    expect(nav.getByRole("link", { name: entry })).toHaveAttribute("aria-current", "page");
  });

  it("/settings opens General for an admin", async () => {
    renderAt("/settings");
    expect(await screen.findByRole("heading", { name: "General", level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/settings\/general$/);
  });

  it("/settings keeps the query string and the hash", async () => {
    renderAt("/settings?from=mail#privacy");
    expect(await screen.findByRole("heading", { name: "General", level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent("/settings/general?from=mail#privacy");
  });

  it("/settings opens the profile for everyone else", async () => {
    db.me = { ...db.me, role: "user" };
    renderAt("/settings");
    expect(await screen.findByRole("heading", { name: "Profile & password", level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/settings\/profile$/);
  });

  const settingsEntries = NAVIGATIONS.settings.groups.flatMap((g) => g.entries);
  it.each(settingsEntries.filter((e) => e.adminOnly).map((e) => e.to))(
    "a non-admin opening %s is sent to their profile",
    async (path) => {
      db.me = { ...db.me, role: "user" };
      renderAt(path);
      expect(await screen.findByRole("heading", { name: "Profile & password", level: 1 })).toBeInTheDocument();
      expect(screen.getByTestId("path")).toHaveTextContent(/^\/settings\/profile$/);
      // Only the Personal group is offered.
      const nav = within(screen.getByRole("navigation", { name: "Settings" }));
      expect(nav.getAllByRole("link").map((a) => a.getAttribute("aria-label")))
        .toEqual(["Profile & password", "Sessions", "Automation tokens"]);
    },
  );

  it.each(settingsEntries.filter((e) => !e.adminOnly).map((e) => [e.to, e.label]))(
    "a non-admin may open %s",
    async (path, label) => {
      db.me = { ...db.me, role: "user" };
      renderAt(path);
      expect(await screen.findByRole("heading", { name: label, level: 1 })).toBeInTheDocument();
      expect(screen.getByTestId("path")).toHaveTextContent(path);
    },
  );

  // The two scopes of the log view: Traffic in Services, Requests in the AI Gateway.
  it.each([
    ["/traffic",          /^Traffic$/,  "Services",   "Traffic"],
    ["/gateway/requests", /^Requests$/, "AI Gateway", "Requests"],
  ])("%s renders its log view and stays put", async (path, heading, workspace, entry) => {
    renderAt(path);
    expect(await screen.findByRole("heading", { name: heading, level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(path);
    const nav = within(await screen.findByRole("navigation", { name: workspace }));
    expect(nav.getByRole("link", { name: entry })).toHaveAttribute("aria-current", "page");
  });

  // Old bookmarks: every moved path lands on its new page, params, query and hash intact.
  // `current` names the Settings entry the page must be marked under; without it the page is an AI Gateway one.
  // `services` names the Services entry instead (its accessible name may carry a count).
  const visit: Record<string, { url: string; lands: string; heading: RegExp; current?: string; services?: RegExp }> = {
    "/tunnels": { url: "/tunnels?q=web#row", lands: "/services?live=1&q=web#row", heading: /^Services$/, services: /^Services(,|$)/ },
    "/tokens": { url: "/tokens#list", lands: "/clients?tab=tokens#list", heading: /^Clients$/, services: /^Clients(,|$)/ },
    "/users": { url: "/users?q=bob#invite", lands: "/settings/users?q=bob#invite", heading: /^Users$/, current: "Users" },
    "/roles": { url: "/roles", lands: "/settings/roles", heading: /^Roles$/, current: "Roles" },
    "/audit": { url: "/audit?actor=x", lands: "/settings/audit?actor=x", heading: /^Audit log$/, current: "Audit log" },
    "/webhooks": { url: "/webhooks#deliveries", lands: "/settings/webhooks#deliveries", heading: /^Webhooks$/, current: "Webhooks" },
    "/openapi": { url: "/openapi", lands: "/settings/api", heading: /^API reference$/, current: "API reference" },
    "/account": { url: "/account", lands: "/settings/profile", heading: /^Profile & password$/, current: "Profile & password" },
    "/account/automation": {
      url: "/account/automation?new=1", lands: "/settings/automation?new=1", heading: /^Automation tokens$/, current: "Automation tokens",
    },
    "/settings/custom-domains": { url: "/settings/custom-domains", lands: "/settings/general", heading: /^General$/, current: "General" },
    "/cache": { url: "/cache", lands: "/gateway/cache", heading: /^Prompt cache$/i },
    "/guardrails": { url: "/guardrails#custom", lands: "/gateway/guardrails#custom", heading: /^Guardrails & redaction$/i },
    "/inspector": { url: "/inspector", lands: "/gateway/requests", heading: /^Requests$/ },
    "/connection-logs": {
      url: "/connection-logs?service=svc_web01#row", lands: "/traffic?service=svc_web01#row", heading: /^Traffic$/, services: /^Traffic$/,
    },
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
    expect(await screen.findByRole("heading", { name: c.heading, level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(c.lands);
    // The sidebar shows the workspace the page now belongs to.
    const nav = within(await screen.findByRole("navigation", { name: c.services ? "Services" : c.current ? "Settings" : "AI Gateway" }));
    const current = c.services ?? c.current;
    if (current) expect(nav.getByRole("link", { name: current })).toHaveAttribute("aria-current", "page");
  });

  it("/tunnels opens Services on Live, /tokens opens the Tokens tab of Clients", async () => {
    const first = renderAt("/tunnels");
    expect(await screen.findByRole("radio", { name: "Live" })).toBeChecked();
    first.unmount();
    renderAt("/tokens");
    expect(await screen.findByRole("tab", { name: "Tokens" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("table", { name: "Tokens" })).toBeInTheDocument();
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


  it("/provisioning is unreachable (backend pending)", async () => {
    renderAt("/provisioning");
    await waitFor(() => {
      expect(screen.queryByRole("heading", { name: /provisioning keys/i })).toBeNull();
    });
  });
});
