import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";

// Words that may not appear in what the user reads. Checked in string literals
// and JSX text only: comments and import lines are stripped first, so a
// component named ClientTokensPanel or a type ClientView does not count.
const RETIRED: [RegExp, string][] = [
  [/tunnell?ing agents?/i, "say client"],
  [/\bAI services?\b/, "say provider"],
  [/\bAI endpoints?\b|across endpoints\b|shared across endpoints/i, "say provider"],
  [/\bclient offline\b/i, "say Offline"],
  [/>\s*idle\s*</, "say Offline or Never connected"],
  [/Names clients ask for|What clients send as the model|clients send as the model/i, "say app"],
  [/OpenAI-compatible client|Anthropic-compatible client|in a client\b/i, "say app"],
  [/Rotate cache/, "removed"],
  [/edit this table in Settings/i, "no such page"],
];

// Mocks and tests quote the relay and the old words; the contract mirrors the wire.
const ALLOW = [/mocks\//, /\.test\.tsx?$/, /contract\.ts$/, /vocabulary\.test\.ts$/];

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((f) => {
    const p = join(dir, f);
    return statSync(p).isDirectory() ? walk(p) : [p];
  });
}

/**
 * The source without what the user never reads: comments and import lines.
 * Strings are kept and never looked into for comments, so the "//" of a URL
 * and the "/*" of a glob stay; where the scan is unsure it keeps the text, so
 * a mistake shows as a failure, not as a word that slipped through.
 */
function readable(src: string): string {
  let out = "";
  let quote = ""; // the delimiter of the string the scan is in
  for (let i = 0; i < src.length; i++) {
    const c = src[i]!;
    const next = src[i + 1] ?? "";
    if (quote) {
      out += c;
      if (c === "\\") { out += next; i++; }
      // Only a template literal runs over a line end (an apostrophe in JSX text does not open one).
      else if (c === quote || (c === "\n" && quote !== "`")) quote = "";
      continue;
    }
    if (c === "\\") { out += c + next; i++; continue; } // an escaped slash in a regex literal
    if (c === "/" && next === "*") {
      const end = src.indexOf("*/", i + 2);
      i = end < 0 ? src.length : end + 1;
      continue;
    }
    if (c === "/" && next === "/") {
      const end = src.indexOf("\n", i);
      i = end < 0 ? src.length : end - 1;
      continue;
    }
    if (c === '"' || c === "'" || c === "`") quote = c;
    out += c;
  }
  return out
    .replace(/\{\s*\}/g, "") // what a {/* JSX comment */} leaves behind
    .replace(/^\s*import\s+(?:type\s+)?[\w*{][^;]*?\bfrom\s+["'][^"']+["'];?/gm, "")
    .replace(/^\s*import\s+["'][^"']+["'];?/gm, "");
}

describe("readable", () => {
  it("drops comments and import lines and keeps what is shown", () => {
    const src = [
      'import { Idle } from "./AI service";',
      "import {",
      "  Thing,",
      '} from "./across endpoints";',
      "// an AI service in a line comment",
      "/* Rotate cache in a block comment */",
      "interface Props {",
      "  /** client offline, in a doc comment right after a brace */",
      "  a: string;",
      "}",
      "const a = <p>{/* client offline */}Shown</p>;",
      'const url = "https://example.com/x"; // tunneling agent',
      'const glob = "/api/*"; const kept = "after the glob"; /* Rotate cache */',
    ].join("\n");
    const out = readable(src);
    expect(out).toContain("<p>Shown</p>");
    expect(out).toContain('"https://example.com/x"');
    expect(out).toContain('"after the glob"');
    expect(out).toContain("a: string;");
    for (const [re] of RETIRED) expect(out).not.toMatch(re);
  });

  it("keeps string literals and JSX text, where the retired words would be read", () => {
    const out = readable([
      'const label = "Global (cache shared across endpoints)";',
      "const b = <Badge>idle</Badge>;",
      "const c = <p>It isn't there. Rotate cache</p>;",
    ].join("\n"));
    expect(out).toMatch(/shared across endpoints/);
    expect(out).toMatch(/>\s*idle\s*</);
    expect(out).toMatch(/Rotate cache/);
  });
});

describe("one vocabulary in the dashboard", () => {
  const root = join(__dirname);
  const files = walk(root)
    .filter((p) => /\.tsx?$/.test(p) && !ALLOW.some((re) => re.test(p)))
    .map((p) => relative(root, p));

  it("looks at the pages and components", () => {
    expect(files).toContain(join("pages", "Providers.tsx"));
    expect(files.length).toBeGreaterThan(50);
  });

  it.each(files)("%s", (file) => {
    const src = readable(readFileSync(join(root, file), "utf8"));
    const found = RETIRED.flatMap(([re, instead]) => {
      const m = src.match(re);
      return m ? [`"${m[0]}": ${instead}`] : [];
    });
    expect(found, `${file} uses retired words`).toEqual([]);
  });
});
