/// <reference types="node" />
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect } from "vitest";
import { matchPath } from "react-router-dom";
import { OLD_ROUTES } from "./moved-routes";
import { NAVIGATIONS, FOOTER_ENTRIES, navigationFor, workspacesFor, activeEntry, breadcrumbFor, allEntries } from "./navigation";

const admin = { isAdmin: true, hasAiGateway: true };
const user = { isAdmin: false, hasAiGateway: false };
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
      ["Route", ["Providers"]],
      ["Control", ["Guardrails", "Prompt cache"]],
      ["Observe", ["Requests", "Cost & budgets"]],
    ]);
    expect(NAVIGATIONS.gateway.namespace).toBe("/ai/…");
  });
  it("Settings navigation for an admin", () => {
    expect(labels("settings")).toEqual([
      ["Relay", ["General", "Email", "Retention", "Database", "Backups"]],
      ["Access", ["Users", "Roles", "Audit log"]],
      ["Integrations", ["Webhooks", "API reference"]],
      ["Personal", ["Profile & password", "Sessions", "Automation tokens"]],
    ]);
  });
  it("a non-admin sees only the Personal group in Settings", () => {
    expect(labels("settings", user)).toEqual([["Personal", ["Profile & password", "Sessions", "Automation tokens"]]]);
  });
  // The spec's budget is seven entries per job. The AI Gateway wireframe reaches eight once
  // Models and Gateway keys land (plan gateway-models); nothing may go beyond that.
  it("keeps each workspace short", () => {
    const count = (ws: "services" | "gateway") => navigationFor(ws, admin).groups.reduce((a, g) => a + g.entries.length, 0);
    expect(count("services")).toBeLessThanOrEqual(7);
    expect(count("gateway")).toBeLessThanOrEqual(8);
  });
  it("the footer entries are for admins and point into Settings", () => {
    expect(FOOTER_ENTRIES.map((e) => [e.label, e.to, e.adminOnly])).toEqual([
      ["Users & roles", "/settings/users", true],
      ["Settings", "/settings/general", true],
    ]);
  });
  it("offers the AI Gateway workspace only when it is visible to the user", () => {
    expect(workspacesFor(admin).map((n) => n.workspace)).toEqual(["services", "gateway"]);
    expect(workspacesFor(user).map((n) => n.workspace)).toEqual(["services"]);
    expect(workspacesFor({ isAdmin: false, hasAiGateway: true }).map((n) => n.workspace)).toEqual(["services", "gateway"]);
  });
  it("every path is unique and none belongs to the relay's own prefixes", () => {
    const paths = allEntries(admin).map((x) => x.entry.to);
    expect(new Set(paths).size).toBe(paths.length);
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
    expect(breadcrumbFor("/services", admin)).toEqual([{ label: "Services", to: "/" }, { label: "Services" }]);
    expect(breadcrumbFor("/", admin)).toEqual([{ label: "Services", to: "/" }, { label: "Overview" }]);
    expect(breadcrumbFor("/settings/email", admin)).toEqual([{ label: "Settings", to: "/settings" }, { label: "Email" }]);
  });
  it("names a page under an entry that is not an object", () => {
    expect(breadcrumbFor("/clients/connect", admin)).toEqual([
      { label: "Services", to: "/" }, { label: "Clients", to: "/clients" }, { label: "Connect a client" },
    ]);
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

  // Admin-only settings pages are guarded where they are routed, exactly as the navigation marks them.
  it("wraps every admin-only settings route in RequireAdmin, and no other", () => {
    const element = (path: string) => app.match(new RegExp(`path="${path}" element=\\{(<[A-Za-z]+)`))?.[1];
    for (const e of NAVIGATIONS.settings.groups.flatMap((g) => g.entries)) {
      expect(element(e.to), e.to).toBeDefined();
      expect(element(e.to) === "<RequireAdmin", e.to).toBe(e.adminOnly === true);
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
