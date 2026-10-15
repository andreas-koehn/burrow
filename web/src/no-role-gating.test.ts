import { describe, it, expect } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name);
    return statSync(p).isDirectory() ? walk(p) : [p];
  });
}

// What the dashboard shows depends on what the caller may do (useCan), never on
// the role's name. The mock relay (src/mocks) imitates the server, and src/auth
// holds the one fallback for relays without permissions on /me.
const GATING = [
  /\bisAdmin\b/,
  /\brole\s*[!=]==?\s*["'`]admin["'`]/,
  /["'`]admin["'`]\s*[!=]==?\s*[\w.?]*\brole\b/,
];
const gates = (src: string) => GATING.some((re) => re.test(src));

describe("no gating by role", () => {
  it("no source file outside src/mocks and src/auth compares the role or reads isAdmin", () => {
    const offenders = walk(__dirname)
      .filter((p) => /\.(ts|tsx)$/.test(p) && !/\.test\.(ts|tsx)$/.test(p))
      .filter((p) => !relative(__dirname, p).startsWith("mocks") && !relative(__dirname, p).startsWith("auth"))
      .filter((p) => gates(readFileSync(p, "utf8")))
      .map((p) => relative(__dirname, p));
    expect(offenders).toEqual([]);
  });

  it("the patterns catch the usual spellings", () => {
    for (const src of [
      "const a = user?.role === \"admin\";",
      "if (me.role !== 'admin') return;",
      "if (\"admin\" === user.role) {}",
      "if ('admin' !== me?.role) {}",
      "x.role == `admin`",
      "role != \"admin\"",
      "const isAdmin = true;",
    ]) expect(gates(src), src).toBe(true);
  });

  it("and leave alone what is not gating", () => {
    for (const src of [
      "const options = [{ value: \"admin\", label: \"Admin\" }];",
      "setRole(user.role);",
      "if (role !== user.role) patch.role = role;",
      "const ROLE_KIND = { admin: \"role-admin-teal\" };",
    ]) expect(gates(src), src).toBe(false);
  });
});
