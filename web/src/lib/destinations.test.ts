import { describe, it, expect } from "vitest";
import { destinationsFor } from "./destinations";
import { allEntries } from "./navigation";
import { holdsAny, ALL_PERMISSIONS, USER_PERMISSIONS } from "@/auth/permissions";
const canOf = (held: readonly string[]) => (needs?: readonly string[]) => holdsAny(new Set(held), needs);

const admin = { can: canOf(ALL_PERMISSIONS), hasAiGateway: true };
const plain = { can: canOf(USER_PERMISSIONS), hasAiGateway: false };
const byGroup = (ctx: typeof admin, group: string) => destinationsFor(ctx).filter((d) => d.group === group).map((d) => d.label);

describe("destinationsFor — the navigation description, flattened", () => {
  it("lists every entry of the three navigations under its workspace label", () => {
    expect(byGroup(admin, "Services")).toEqual(["Services overview", "Services", "Clients", "Traffic"]);
    expect(byGroup(admin, "AI Gateway")).toEqual(["AI Gateway overview", "Providers", "Models", "Gateway keys", "Guardrails", "Prompt cache", "Requests", "Cost & budgets"]);
    expect(byGroup(admin, "Settings")).toEqual([
      "General", "Email", "Retention", "Database", "Backups", "Users", "Roles", "Audit log",
      "Webhooks", "API reference", "Profile & password", "Sessions", "Automation tokens",
    ]);
    expect(new Set(destinationsFor(admin).map((d) => d.group))).toEqual(new Set(["Services", "AI Gateway", "Settings"]));
  });

  it("names the two Overview pages apart and keeps what the sidebar calls them", () => {
    const overviews = destinationsFor(admin).filter((d) => d.entryLabel === "Overview");
    expect(overviews.map((d) => [d.path, d.label])).toEqual([["/", "Services overview"], ["/gateway", "AI Gateway overview"]]);
    // Every other page is called in the palette what the sidebar calls it.
    for (const d of destinationsFor(admin)) if (d.entryLabel !== "Overview") expect(d.label, d.path).toBe(d.entryLabel);
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

  it("a non-admin without AI access sees Services (no Clients, which needs tunnels:read:any) and the Personal settings only", () => {
    expect(byGroup(plain, "Services")).toEqual(["Services overview", "Services", "Traffic"]);
    expect(byGroup(plain, "AI Gateway")).toEqual([]);
    expect(byGroup(plain, "Settings")).toEqual(["Profile & password", "Sessions", "Automation tokens"]);
  });

  it("a non-admin with AI access sees the gateway entries, Requests included", () => {
    expect(byGroup({ ...plain, hasAiGateway: true }, "AI Gateway")).toContain("Requests");
  });
});
