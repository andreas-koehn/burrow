import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect } from "vitest";

// These tests pin the intended values of the centralized design tokens in
// index.css. jsdom cannot resolve oklch()/var()/color-mix(), so rendered-color
// correctness is verified separately by the Playwright computed-style spec added
// in Task 22 (web/e2e/visual-tokens.spec.ts); this file guards the SOURCE
// declarations so a regression is caught instantly in unit CI.
const css = readFileSync(resolve(__dirname, "index.css"), "utf8");

describe("design tokens — placeholder contrast (D-3/L-3)", () => {
  it("input placeholder uses the full muted-foreground token, not a faded 55% mix", () => {
    const rule = css.match(/\.input::placeholder\s*\{[^}]*\}/)?.[0] ?? "";
    expect(rule).toContain("var(--muted-foreground)");
    expect(rule).not.toContain("55%");
  });
});
