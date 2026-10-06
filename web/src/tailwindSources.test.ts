import { describe, it, expect } from "vitest";
import fs from "node:fs";
import path from "node:path";

// web/dist is committed and embedded into the relay, so a build must give the
// same CSS on every machine. Tailwind's automatic detection reads files
// outside the web app too (untracked notes of a working copy among them) and
// makes a rule for every class name it thinks it sees there. The stylesheet
// therefore turns the detection off and names its sources.
describe("Tailwind's content sources", () => {
  const css = fs.readFileSync(path.resolve(__dirname, "index.css"), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");

  it("turns the automatic detection off", () => {
    const imports = css.match(/@import\s+"tailwindcss"[^;]*;/g) ?? [];
    expect(imports).toEqual(['@import "tailwindcss" source(none);']);
  });

  it("reads index.html and src, and nothing else", () => {
    const sources = Array.from(css.matchAll(/@source\s+([^;]+);/g), (m) => m[1].trim());
    expect(sources.sort()).toEqual(['"."', '"../index.html"']);
    // Relative to src/index.css, each stays inside web/.
    const web = path.resolve(__dirname, "..");
    for (const s of sources) {
      const target = path.resolve(__dirname, s.replace(/"/g, ""));
      expect(target === path.join(web, "index.html") || target === path.join(web, "src")).toBe(true);
    }
  });
});
