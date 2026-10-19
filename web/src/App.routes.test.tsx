import { describe, it, expect, afterEach, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { render } from "@testing-library/react";
import { ThemeProvider } from "@/components/theme-provider";
import App from "@/App";
import { setCsrfCookie } from "@/mocks/test-utils";
import { db, resetDb } from "@/mocks/db";
import { OLD_ROUTES } from "@/lib/moved-routes";
import { NAVIGATIONS } from "@/lib/navigation";
import { USER_PERMISSIONS } from "@/auth/permissions";

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
  // Email is set up, so no entry carries the attention mark ("Email, needs attention")
  // and the exact names below do not depend on when the settings arrive.
  beforeEach(() => { db.settings["smtp.host"] = "smtp.example.com"; });
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
    ["/gateway/tools",                 /^Tools$/i],
    ["/gateway/tools/github",          /^Tool source · /i],
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
    ["/settings/apps",       /^Connected apps$/,     "Connected apps"],
    ["/settings/oauth-clients", /^OAuth clients$/,   "OAuth clients"],
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
    db.me = { ...db.me, role: "user", permissions: [...USER_PERMISSIONS] };
    renderAt("/settings");
    expect(await screen.findByRole("heading", { name: "Profile & password", level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/settings\/profile$/);
  });

  it("opening /traffic without audit:read says which permission is missing", async () => {
    db.me = { ...db.me, role: "user", permissions: [...USER_PERMISSIONS] };
    renderAt("/traffic");
    expect(await screen.findByRole("heading", { name: "You cannot open this page", level: 1 })).toBeInTheDocument();
    expect(screen.getByText(/audit:read/)).toBeInTheDocument();
  });

  const settingsEntries = NAVIGATIONS.settings.groups.flatMap((g) => g.entries);
  it.each(settingsEntries.filter((e) => e.needs).map((e) => [e.to, e.needs!, e.label] as const))(
    "someone without the permissions of %s is told what is missing",
    async (path, needs, label) => {
      db.me = { ...db.me, role: "user", permissions: USER_PERMISSIONS.filter((p) => !needs.includes(p)) };
      renderAt(path);
      expect(await screen.findByRole("heading", { name: "You cannot open this page", level: 1 })).toBeInTheDocument();
      expect(screen.getByTestId("path")).toHaveTextContent(new RegExp(`^${path}$`));
      expect(screen.getByText(new RegExp(`${needs[0]}`))).toBeInTheDocument();
      expect(screen.getByRole("link", { name: "Back to your profile" })).toHaveAttribute("href", "/settings/profile");
      // Only the Personal group is offered, without the page itself.
      const nav = within(screen.getByRole("navigation", { name: "Settings" }));
      expect(nav.getAllByRole("link").map((a) => a.getAttribute("aria-label")))
        .toEqual(["Profile & password", "Sessions", "Automation tokens", "Connected apps"].filter((l) => l !== label));
    },
  );

  it("a custom role with audit:read opens the audit log and not the webhooks", async () => {
    db.me = { ...db.me, role: "auditor", permissions: ["audit:read"] };
    renderAt("/settings/audit");
    expect(await screen.findByRole("heading", { name: "Audit log", level: 1 })).toBeInTheDocument();
    const nav = within(screen.getByRole("navigation", { name: "Settings" }));
    expect(nav.getByRole("link", { name: "Audit log" })).toBeInTheDocument();
    expect(nav.queryByRole("link", { name: "Webhooks" })).toBeNull();
  });

  it("/settings opens General for a custom role with settings:manage", async () => {
    db.me = { ...db.me, role: "operator", permissions: ["settings:manage"] };
    renderAt("/settings");
    expect(await screen.findByTestId("path")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent(/^\/settings\/general$/));
  });

  it.each(settingsEntries.filter((e) => !e.needs).map((e) => [e.to, e.label]))(
    "a non-admin may open %s",
    async (path, label) => {
      db.me = { ...db.me, role: "user", permissions: [...USER_PERMISSIONS] };
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

  it("/tunnels opens Services on Online, /tokens opens the Tokens tab of Clients", async () => {
    const first = renderAt("/tunnels");
    expect(await screen.findByRole("radio", { name: "Online" })).toBeChecked();
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

// RequireAuth hands the address a signed-out visitor asked for to the login page.
describe("return after signing in", () => {
  afterEach(() => resetDb());

  // GET /me answers 401 until somebody signs in, and again after a logout.
  function session(signedIn: boolean) {
    const s = { signedIn };
    server.use(
      http.get("/api/v1/me", () => (s.signedIn ? HttpResponse.json(db.me) : HttpResponse.json({ error: "unauthorized" }, { status: 401 }))),
      http.post("/api/v1/auth/login", () => { s.signedIn = true; return HttpResponse.json({}); }),
      http.post("/api/v1/auth/logout", () => { s.signedIn = false; return new HttpResponse(null, { status: 204 }); }),
    );
    return s;
  }

  async function signIn() {
    expect(await screen.findByRole("heading", { name: "Sign in to Burrow" })).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Email"), "alice@acme.io");
    await userEvent.type(screen.getByLabelText("Password"), "password123");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
  }

  it("a signed-out deep link comes back with its query and hash", async () => {
    session(false);
    renderAt("/services?live=1#x");
    await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent("/login"));
    await signIn();
    await waitFor(() => expect(screen.getByTestId("path").textContent).toBe("/services?live=1#x"));
    expect(await screen.findByRole("heading", { name: /^Services$/i })).toBeInTheDocument();
  });

  it("logging out from a deep page and signing in again starts on the overview", async () => {
    session(true);
    renderAt("/settings/sessions?x=1#y");
    await userEvent.click(await screen.findByRole("button", { name: "Log out" }));
    await waitFor(() => expect(screen.getByTestId("path").textContent).toBe("/login"));
    await signIn();
    await waitFor(() => expect(screen.getByTestId("path").textContent).toBe("/"));
  });
});
