import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { ALL_PERMISSIONS, USER_PERMISSIONS, P, permissionsOf, holdsAny } from "./permissions";

const authzGo = readFileSync(resolve(__dirname, "../../../internal/authz/authz.go"), "utf8");

/** The quoted permission strings of a Go slice literal, in order. */
function goList(name: string): string[] {
  const body = authzGo.match(new RegExp(`${name}[^{]*\\{([\\s\\S]*?)\\n\\}`))?.[1] ?? "";
  const consts = Object.fromEntries([...authzGo.matchAll(/(Perm\w+)\s+Permission = "([^"]+)"/g)].map((m) => [m[1], m[2]]));
  return [...body.matchAll(/Perm\w+/g)].map((m) => consts[m[0]]).filter(Boolean);
}

describe("permission list", () => {
  it("is authz.AllPermissions(), in its order", () => {
    const go = goList("func AllPermissions\\(\\) \\[\\]Permission \\{\\s*return \\[\\]Permission");
    expect(go.length).toBeGreaterThan(30);
    expect(ALL_PERMISSIONS).toEqual(go);
  });
  it("USER_PERMISSIONS is the built-in user role", () => {
    const go = goList("var userPerms = \\[\\]Permission");
    expect(go.length).toBeGreaterThan(5);
    expect([...USER_PERMISSIONS].sort()).toEqual(go.sort());
  });
  it("names every permission once", () => {
    expect(Object.values(P).sort()).toEqual([...ALL_PERMISSIONS].sort());
    expect(P.aiProvidersManage).toBe("ai:providers:manage");
    expect(P.tunnelsReadAny).toBe("tunnels:read:any");
    expect(P.automationTokensManageOwn).toBe("automation:tokens:manage:own");
    expect(P.mcpToolsRead).toBe("mcp:tools:read");
  });
});

describe("permissionsOf", () => {
  it("takes what /me says", () => {
    expect([...permissionsOf({ id: "u", email: "e", role: "operator", permissions: ["audit:read"] })]).toEqual(["audit:read"]);
  });
  it("falls back to the role on a relay without permissions on /me", () => {
    expect(permissionsOf({ id: "u", email: "e", role: "admin" }).size).toBe(ALL_PERMISSIONS.length);
    expect([...permissionsOf({ id: "u", email: "e", role: "user" })].sort()).toEqual([...USER_PERMISSIONS].sort());
    expect(permissionsOf({ id: "u", email: "e", role: "custom-thing" }).size).toBe(USER_PERMISSIONS.length);
    expect(permissionsOf(undefined).size).toBe(0);
  });
  it("holdsAny is any-of, and true for no needs", () => {
    const held = new Set(["audit:read"]);
    expect(holdsAny(held, ["users:read", "audit:read"])).toBe(true);
    expect(holdsAny(held, ["users:read"])).toBe(false);
    expect(holdsAny(held, [])).toBe(true);
    expect(holdsAny(held)).toBe(true);
  });
});
