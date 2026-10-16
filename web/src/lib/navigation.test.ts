/// <reference types="node" />
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect, beforeEach } from "vitest";
import { matchPath } from "react-router-dom";
import { OLD_ROUTES } from "./moved-routes";
import { holdsAny, ALL_PERMISSIONS, USER_PERMISSIONS } from "@/auth/permissions";
import { NAVIGATIONS, FOOTER_ENTRIES, navigationFor, workspacesFor, activeEntry, attentionTarget, breadcrumbFor, allEntries } from "./navigation";

// What is remembered decides where a relay-level page is shown: start every test without it.
beforeEach(() => localStorage.clear());

const canOf = (held: readonly string[]) => (needs?: readonly string[]) => holdsAny(new Set(held), needs);
const admin = { can: canOf(ALL_PERMISSIONS), hasAiGateway: true };
const user = { can: canOf(USER_PERMISSIONS), hasAiGateway: false };
const auditor = { can: canOf(["audit:read"]), hasAiGateway: false };
const labels = (ws: "services" | "gateway" | "settings", ctx = admin) =>
  navigationFor(ws, ctx).groups.map((g) => [g.title ?? "", g.entries.map((e) => e.label)]);

describe("navigation description", () => {
  it("Services workspace", () => {
    expect(labels("services")).toEqual([
      ["", ["Overview"]],
      ["Connect", ["Services", "Clients"]],
      ["Observe", ["Traffic"]],
    ]);
    expect(NAVIGATIONS.services.namespace).toBe("/svc/…");
  });
  it("AI Gateway workspace", () => {
    expect(labels("gateway")).toEqual([
      ["", ["Overview"]],
      ["Connect", ["Clients"]],
      ["Route", ["Providers", "Models", "Gateway keys"]],
      ["Control", ["Guardrails", "Prompt cache"]],
      ["Observe", ["Requests", "Cost & budgets"]],
    ]);
    expect(NAVIGATIONS.gateway.namespace).toBe("/ai/…");
    const route = NAVIGATIONS.gateway.groups.find((g) => g.title === "Route")!;
    expect(route.entries.map((e) => e.to)).toEqual(["/gateway/providers", "/gateway/models", "/gateway/keys"]);
  });
  it("Settings navigation for an admin", () => {
    expect(labels("settings")).toEqual([
      ["Relay", ["General", "Email", "Retention", "Database", "Backups"]],
      ["Access", ["Users", "Roles", "Audit log"]],
      ["Integrations", ["Webhooks", "MCP", "API reference"]],
      ["Personal", ["Profile & password", "Sessions", "Automation tokens"]],
    ]);
  });
  it("a non-admin sees only the Personal group in Settings", () => {
    expect(labels("settings", user)).toEqual([["Personal", ["Profile & password", "Sessions", "Automation tokens"]]]);
  });
  it("someone who may only read the audit log sees that page beside the profile", () => {
    expect(labels("settings", auditor)).toEqual([
      ["Access", ["Audit log"]],
      ["Personal", ["Profile & password"]],
    ]);
  });
  it("MCP sits in Integrations after Webhooks and needs mcp:tools:read", () => {
    const g = NAVIGATIONS.settings.groups.find((x) => x.title === "Integrations")!;
    const ids = g.entries.map((e) => e.id);
    expect(ids.indexOf("mcp")).toBe(ids.indexOf("webhooks") + 1);
    expect(g.entries.find((e) => e.id === "mcp")).toMatchObject({ to: "/settings/mcp", needs: ["mcp:tools:read"] });
  });
  it("no entry is marked by role", () => {
    const entries = [...(["services", "gateway", "settings"] as const).flatMap((ws) => NAVIGATIONS[ws].groups.flatMap((g) => g.entries)), ...FOOTER_ENTRIES];
    for (const e of entries) expect(e, e.id).not.toHaveProperty("adminOnly");
  });
  it("each settings entry declares the permissions of its API", () => {
    const needs = Object.fromEntries(NAVIGATIONS.settings.groups.flatMap((g) => g.entries).map((e) => [e.id, e.needs]));
    expect(needs).toMatchObject({
      general: ["settings:manage"], email: ["settings:manage"], retention: ["settings:manage"],
      database: ["metrics:read"], backups: ["backup:run"],
      users: ["users:read", "users:manage"], roles: ["roles:read", "roles:manage"], audit: ["audit:read"],
      webhooks: ["webhooks:manage"], api: ["metrics:read"],
      sessions: ["sessions:manage:own", "sessions:manage:any"],
      automation: ["automation:tokens:manage:own", "automation:tokens:manage:any"],
    });
    expect(needs.profile).toBeUndefined();
  });
  it("Clients, in both workspaces, is open to whoever lists clients or manages tokens, the user role included", () => {
    for (const id of ["clients", "gateway-clients"]) {
      const e = [...NAVIGATIONS.services.groups, ...NAVIGATIONS.gateway.groups].flatMap((g) => g.entries).find((x) => x.id === id);
      expect(e?.needs, id).toEqual(["tunnels:read:any", "tokens:manage:own", "tokens:manage:any"]);
      expect(user.can(e?.needs), id).toBe(true);
      expect(auditor.can(e?.needs), id).toBe(false);
    }
  });
  // The spec's budget is seven entries per job. The AI Gateway wireframe reaches eight with
  // Models and Gateway keys (plan gateway-models) and nine with Clients, which belongs to the
  // relay and is listed in both workspaces; nothing may go beyond that.
  it("keeps each workspace short", () => {
    const count = (ws: "services" | "gateway") => navigationFor(ws, admin).groups.reduce((a, g) => a + g.entries.length, 0);
    expect(count("services")).toBeLessThanOrEqual(7);
    expect(count("gateway")).toBeLessThanOrEqual(9);
  });
  it("both workspaces open the one Clients page, with the same count", () => {
    for (const ws of ["services", "gateway"] as const) {
      const all = navigationFor(ws, admin).groups.flatMap((g) => g.entries);
      const clients = all.filter((e) => e.to === "/clients");
      expect(clients.map((e) => [e.label, e.count]), ws).toEqual([["Clients", "clientsOnline"]]);
      // In the first titled group, directly below Overview.
      expect(navigationFor(ws, admin).groups[1].entries, ws).toContain(clients[0]);
    }
    const ids = (["services", "gateway"] as const).flatMap((ws) => NAVIGATIONS[ws].groups.flatMap((g) => g.entries.map((e) => e.id)));
    expect(new Set(ids).size).toBe(ids.length);
  });
  // Two pages are called "Overview"; a list that shows both at once needs to tell them apart.
  it("the two Overview entries carry a longer name for the palette, and only they", () => {
    const named = (["services", "gateway", "settings"] as const)
      .flatMap((ws) => NAVIGATIONS[ws].groups.flatMap((g) => g.entries))
      .filter((e) => e.paletteLabel !== undefined)
      .map((e) => [e.to, e.label, e.paletteLabel]);
    expect(named).toEqual([["/", "Overview", "Services overview"], ["/gateway", "Overview", "AI Gateway overview"]]);
    expect(FOOTER_ENTRIES.every((e) => e.paletteLabel === undefined)).toBe(true);
  });
  it("the footer entries point into Settings and follow the permissions of the page", () => {
    expect(FOOTER_ENTRIES.map((e) => [e.label, e.to, e.needs])).toEqual([
      ["Users & roles", "/settings/users", ["users:read", "users:manage"]],
      ["Settings", "/settings/general", ["settings:manage"]],
    ]);
    expect(FOOTER_ENTRIES.filter((e) => user.can(e.needs))).toEqual([]);
    expect(FOOTER_ENTRIES.filter((e) => admin.can(e.needs))).toHaveLength(2);
  });
  it("offers the AI Gateway workspace only when it is visible to the user", () => {
    expect(workspacesFor(admin).map((n) => n.workspace)).toEqual(["services", "gateway"]);
    expect(workspacesFor(user).map((n) => n.workspace)).toEqual(["services"]);
    expect(workspacesFor({ ...user, hasAiGateway: true }).map((n) => n.workspace)).toEqual(["services", "gateway"]);
  });
  it("every path is unique and none belongs to the relay's own prefixes", () => {
    const paths = allEntries(admin).map((x) => x.entry.to);
    expect(new Set(paths).size).toBe(paths.length);
    // Clients is in two sidebars and still one destination: the first one listed.
    expect(allEntries(admin).filter((x) => x.entry.to === "/clients").map((x) => [x.entry.id, x.workspace]))
      .toEqual([["clients", "services"]]);
    for (const p of paths) expect(p).not.toMatch(/^\/(ai|svc|openai|anthropic|api|__burrow)(\/|$)/);
  });
});

describe("activeEntry", () => {
  const gw = navigationFor("gateway", admin);
  const svc = navigationFor("services", admin);
  it("marks the parent on a detail page", () => {
    expect(activeEntry("/gateway/providers/zai", gw)?.label).toBe("Providers");
    expect(activeEntry("/gateway/requests/svc1/req9", gw)?.label).toBe("Requests");
    expect(activeEntry("/services/abc", svc)?.label).toBe("Services");
    expect(activeEntry("/clients/connect", svc)?.label).toBe("Clients");
  });
  it("marks Clients in the AI Gateway too", () => {
    expect(activeEntry("/clients/abc", gw)?.id).toBe("gateway-clients");
    expect(activeEntry("/clients", gw)?.id).toBe("gateway-clients");
    expect(activeEntry("/clients/abc", svc)?.id).toBe("clients");
  });
  it("marks Overview only on the workspace's home", () => {
    expect(activeEntry("/gateway", gw)?.label).toBe("Overview");
    expect(activeEntry("/", svc)?.label).toBe("Overview");
    expect(activeEntry("/services", svc)?.label).toBe("Services");
  });
  it("returns nothing for a path the navigation does not cover", () => {
    expect(activeEntry("/nowhere", svc)).toBeUndefined();
  });
});

describe("breadcrumbFor", () => {
  it("names the workspace, the entry and leaves room for the object", () => {
    expect(breadcrumbFor("/gateway/providers/zai", admin)).toEqual([
      { label: "AI Gateway", to: "/gateway" }, { label: "Providers", to: "/gateway/providers" }, { label: "zai" },
    ]);
    expect(breadcrumbFor("/", admin)).toEqual([{ label: "Services", to: "/" }, { label: "Overview" }]);
    expect(breadcrumbFor("/settings/email", admin)).toEqual([{ label: "Settings", to: "/settings" }, { label: "Email" }]);
    expect(breadcrumbFor("/gateway/models", admin)).toEqual([{ label: "AI Gateway", to: "/gateway" }, { label: "Models" }]);
    expect(breadcrumbFor("/gateway/keys", admin)).toEqual([{ label: "AI Gateway", to: "/gateway" }, { label: "Gateway keys" }]);
  });
  it("says a word once where the workspace and its entry share it", () => {
    // Not "Services / Services / ollama": the entry stands for both, and leads to the list.
    expect(breadcrumbFor("/services", admin)).toEqual([{ label: "Services" }]);
    expect(breadcrumbFor("/services/svc1", admin, { svc1: "ollama" })).toEqual([
      { label: "Services", to: "/services" }, { label: "ollama" },
    ]);
    // Every other page of the workspace keeps both crumbs.
    expect(breadcrumbFor("/traffic", admin)).toEqual([{ label: "Services", to: "/" }, { label: "Traffic" }]);
  });
  it("names a page under an entry that is not an object", () => {
    expect(breadcrumbFor("/clients/connect", admin)).toEqual([
      { label: "Services", to: "/" }, { label: "Clients", to: "/clients" }, { label: "Connect a client" },
    ]);
  });
  it("a relay-level page starts with the workspace used last", () => {
    expect(breadcrumbFor("/clients", admin, {}, "gateway")).toEqual([{ label: "AI Gateway", to: "/gateway" }, { label: "Clients" }]);
    expect(breadcrumbFor("/clients", admin, {}, "services")).toEqual([{ label: "Services", to: "/" }, { label: "Clients" }]);
    expect(breadcrumbFor("/clients/connect", admin, {}, "gateway")).toEqual([
      { label: "AI Gateway", to: "/gateway" }, { label: "Clients", to: "/clients" }, { label: "Connect a client" },
    ]);
    expect(breadcrumbFor("/clients/sess_1", admin, { sess_1: "office-box-1" }, "gateway").map((c) => c.label))
      .toEqual(["AI Gateway", "Clients", "office-box-1"]);
    // Without the argument it reads what is remembered.
    localStorage.setItem("burrow.lastWorkspace", "gateway");
    expect(breadcrumbFor("/clients", admin)[0]).toEqual({ label: "AI Gateway", to: "/gateway" });
    // Every other page follows its URL.
    expect(breadcrumbFor("/traffic", admin, {}, "gateway")[0]).toEqual({ label: "Services", to: "/" });
    expect(breadcrumbFor("/gateway/models", admin, {}, "services")[0]).toEqual({ label: "AI Gateway", to: "/gateway" });
  });
  it("links the service and ends on the request", () => {
    expect(breadcrumbFor("/gateway/requests/svc1/req9", admin)).toEqual([
      { label: "AI Gateway", to: "/gateway" }, { label: "Requests", to: "/gateway/requests" },
      { label: "svc1", to: "/gateway/requests/svc1" }, { label: "req9" },
    ]);
    expect(breadcrumbFor("/gateway/requests/svc1", admin).at(-1)).toEqual({ label: "svc1" });
  });
  it("shows an object's name when the caller knows it", () => {
    const names = { svc1: "ollama", sess_1: "office-box-1" };
    expect(breadcrumbFor("/services/svc1", admin, names).at(-1)).toEqual({ label: "ollama" });
    expect(breadcrumbFor("/clients/sess_1", admin, names).at(-1)).toEqual({ label: "office-box-1" });
    expect(breadcrumbFor("/gateway/requests/svc1/req9", admin, names).map((c) => c.label))
      .toEqual(["AI Gateway", "Requests", "ollama", "req9"]);
    expect(breadcrumbFor("/services/other", admin, names).at(-1)).toEqual({ label: "other" });
  });
  it("decodes the object segment", () => {
    expect(breadcrumbFor("/services/my%20svc", admin).at(-1)).toEqual({ label: "my svc" });
  });
});

// Review Focus 2 — no orphan page, in both directions. App.tsx is read as text: a route
// counts as declared when it appears there as a literal path="…".
describe("reachability", () => {
  const app = readFileSync(resolve(__dirname, "..", "App.tsx"), "utf8");
  const routes = [...app.matchAll(/path="([^"]+)"/g)].map((m) => m[1]);
  const entries = allEntries(admin).map((x) => x.entry);

  it("reads the route list from App.tsx", () => {
    expect(routes).toContain("/gateway/requests/:serviceId/:requestId?");
    expect(routes.length).toBeGreaterThan(25);
  });

  it("routes the Models and Gateway keys pages", () => {
    expect(routes).toContain("/gateway/models");
    expect(routes).toContain("/gateway/keys");
  });

  // /openai/ and /anthropic/ are the gateway's endpoints, /ai/ and /svc/ the relay's other
  // doors: the relay answers them itself, so a dashboard route there could never be opened.
  it("no dashboard route starts with a prefix of the relay's data plane", () => {
    for (const r of routes) expect(r, r).not.toMatch(/^\/(ai|svc|openai|anthropic|api)(\/|$)/);
  });

  it("every navigation entry has a route", () => {
    const unrouted = [...entries, ...FOOTER_ENTRIES].map((e) => e.to).filter((to) => !routes.includes(to));
    expect(unrouted).toEqual([]);
  });

  it("every moved path redirects to a declared route", () => {
    for (const r of OLD_ROUTES) {
      expect(routes, r.from).not.toContain(r.from);
      // A target may name a filter or tab of its page (/services?live=1); a "?" that ends a segment is an optional param.
      expect(routes, r.to).toContain(r.to.split(/\?(?!\/|$)/)[0]);
    }
  });

  it("every route is reachable from an entry, a tab or detail link below one, or a known way in", () => {
    // A route below an entry's path is that entry's detail page, sub-page or tab.
    const underEntry = (route: string) =>
      (["services", "gateway", "settings"] as const).some((ws) => activeEntry(route, navigationFor(ws, admin)) !== undefined);
    const waysIn: Record<string, string> = {
      "/login": "outside the shell: where a signed-out visitor is sent",
      // Reached from a terminal: `burrow login` prints and opens /link?code=…; no entry leads there.
      "/link": "outside the shell: where burrow login sends the browser to approve a sign-in",
      "*": "catch-all, sends unknown paths to the overview",
      "/settings": "the Settings index: sends an admin to General, everyone else to their profile",
    };
    const orphans = routes.filter((r) => !underEntry(r) && !(r in waysIn));
    expect(orphans).toEqual([]);
    // No stale excuse: every way in names a route that exists and is not an entry's own page.
    for (const r of Object.keys(waysIn)) {
      expect(routes, r).toContain(r);
      expect(underEntry(r), r).toBe(false);
    }
  });

  // Settings pages with a need are guarded where they are routed, and the route reads the need from the entry.
  it("wraps every settings route that has a need in RequirePermission, and no other", () => {
    const element = (path: string) => app.match(new RegExp(`path="${path}" element=\\{(<[A-Za-z]+)`))?.[1];
    for (const e of NAVIGATIONS.settings.groups.flatMap((g) => g.entries)) {
      expect(element(e.to), e.to).toBeDefined();
      expect(element(e.to) === "<RequirePermission", e.to).toBe(e.needs !== undefined);
    }
  });

  it("every breadcrumb of a settings page leads to a declared route", () => {
    for (const e of NAVIGATIONS.settings.groups.flatMap((g) => g.entries)) {
      for (const crumb of breadcrumbFor(e.to, admin)) {
        if (crumb.to) expect(routes, `${e.to} → ${crumb.to}`).toContain(crumb.to);
      }
    }
  });

  it("every object crumb leads to a declared route", () => {
    for (const path of ["/gateway/requests/svc1/req9", "/gateway/providers/zai", "/services/abc", "/clients/sess_1", "/clients/connect"]) {
      for (const crumb of breadcrumbFor(path, admin)) {
        if (!crumb.to) continue;
        expect(routes.some((r) => matchPath(r, crumb.to!) !== null), `${path} → ${crumb.to}`).toBe(true);
      }
    }
  });
});

describe("attentionTarget", () => {
  const all = [...allEntries(admin).map((e) => e.entry), ...FOOTER_ENTRIES];
  const entry = (id: string) => all.find((e) => e.id === id)!;

  it("marks the entry of the page a notice is resolved on", () => {
    expect(attentionTarget(entry("email"), ["/settings/email"])).toBe("/settings/email");
    expect(attentionTarget(entry("general"), ["/settings/email"])).toBeUndefined();
  });

  it("the Settings shortcut stands for all of them and leads to the first", () => {
    expect(attentionTarget(entry("footer-settings"), ["/settings/email", "/settings/backups"])).toBe("/settings/email");
    expect(attentionTarget(entry("footer-users"), ["/settings/email"])).toBeUndefined();
  });

  it("marks nothing without an open notice", () => {
    for (const e of all) expect(attentionTarget(e, [])).toBeUndefined();
  });
});
