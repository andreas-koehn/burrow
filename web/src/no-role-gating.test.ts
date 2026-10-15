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
describe("no gating by role", () => {
  it("no source file outside src/mocks and src/auth compares the role or reads isAdmin", () => {
    const offenders = walk(__dirname)
      .filter((p) => /\.(ts|tsx)$/.test(p) && !/\.test\.(ts|tsx)$/.test(p))
      .filter((p) => !relative(__dirname, p).startsWith("mocks") && !relative(__dirname, p).startsWith("auth"))
      .filter((p) => /\bisAdmin\b|role\s*[!=]==\s*["']admin["']|["']admin["']\s*[!=]==\s*\w*role/.test(readFileSync(p, "utf8")))
      .map((p) => relative(__dirname, p));
    expect(offenders).toEqual([]);
  });
});
