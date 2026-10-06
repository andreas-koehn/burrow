import { describe, it, expect } from "vitest";
import { destinationsFor } from "./destinations";
import { allEntries } from "./navigation";

const admin = { isAdmin: true, hasAiGateway: true };
const plain = { isAdmin: false, hasAiGateway: false };
const byGroup = (ctx: typeof admin, group: string) => destinationsFor(ctx).filter((d) => d.group === group).map((d) => d.label);

describe("destinationsFor — the navigation description, flattened", () => {
  it("lists every entry of the three navigations under its workspace label", () => {
    expect(byGroup(admin, "Services")).toEqual(["Overview", "Services", "Clients", "Traffic"]);
    expect(byGroup(admin, "AI Gateway")).toEqual(["Overview", "Providers", "Models", "Gateway keys", "Guardrails", "Prompt cache", "Requests", "Cost & budgets"]);
    expect(byGroup(admin, "Settings")).toEqual([
      "General", "Email", "Retention", "Database", "Backups", "Users", "Roles", "Audit log",
      "Webhooks", "API reference", "Profile & password", "Sessions", "Automation tokens",
    ]);
    expect(new Set(destinationsFor(admin).map((d) => d.group))).toEqual(new Set(["Services", "AI Gateway", "Settings"]));
  });

  it("adds nothing to the navigation description", () => {
    expect(destinationsFor(admin).map((d) => d.path)).toEqual(allEntries(admin).map((x) => x.entry.to));
  });

  it("all paths are unique (the palette keys its rows by path)", () => {
    const paths = destinationsFor(admin).map((d) => d.path);
    expect(new Set(paths).size).toBe(paths.length);
  });

  it("every destination carries an icon", () => {
    for (const d of destinationsFor(admin)) expect(d.icon, d.label).toBeTruthy();
  });

  it("points the gateway entries at their /gateway/ paths", () => {
    const path = (label: string) => destinationsFor(admin).find((d) => d.label === label)?.path;
    expect(path("Requests")).toBe("/gateway/requests");
    expect(path("Cost & budgets")).toBe("/gateway/cost");
    expect(path("Prompt cache")).toBe("/gateway/cache");
    expect(path("Guardrails")).toBe("/gateway/guardrails");
  });

  it("a non-admin without AI access sees Services and the Personal settings only", () => {
    expect(byGroup(plain, "Services")).toEqual(["Overview", "Services", "Clients", "Traffic"]);
    expect(byGroup(plain, "AI Gateway")).toEqual([]);
    expect(byGroup(plain, "Settings")).toEqual(["Profile & password", "Sessions", "Automation tokens"]);
  });

  it("a non-admin with AI access sees the gateway entries, Requests included", () => {
    expect(byGroup({ isAdmin: false, hasAiGateway: true }, "AI Gateway")).toContain("Requests");
  });
});
