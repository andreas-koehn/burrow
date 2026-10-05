import { describe, it, expect } from "vitest";
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
  it("decodes the object segment", () => {
    expect(breadcrumbFor("/services/my%20svc", admin).at(-1)).toEqual({ label: "my svc" });
  });
});
