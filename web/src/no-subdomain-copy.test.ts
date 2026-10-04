import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";

// Files that legitimately talk about hostnames: custom domains (kept in the
// tree, unrouted) and client machine hostnames.
const ALLOW = [/CustomDomains/, /MtlsPanel/, /mocks\//, /\.test\.tsx?$/, /contract\.ts$/];

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((f) => {
    const p = join(dir, f);
    return statSync(p).isDirectory() ? walk(p) : [p];
  });
}

describe("no subdomain wording in the dashboard", () => {
  const files = walk(join(__dirname)).filter(
    (p) => /\.(tsx?|css)$/.test(p) && !ALLOW.some((re) => re.test(p)),
  );
  it.each(files)("%s", (file) => {
    const src = readFileSync(file, "utf8");
    expect(src).not.toMatch(/subdomain/i);
    expect(src).not.toMatch(/\.tunnels\.example\.com/);
    expect(src).not.toMatch(/<id>\.<domain>|<label>\.<domain>/);
  });
});
