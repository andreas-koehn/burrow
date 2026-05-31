/// <reference types="node" />
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect } from "vitest";

// Pin the "clean" invariants that keep the design system coherent across phases.
// Real assertions here must hold TODAY — they verify existing CSS contracts.
// Pending criteria (it.todo) are converted to real assertions as each phase lands.
//
// Playwright-level checks (no new color literals, screenshot-diff clean) live in
// web/e2e/usability.spec.ts.
const css = readFileSync(resolve(__dirname, "index.css"), "utf8");

describe("usability-clean — notice-inline is token-based (Cl-2 pre-condition)", () => {
  it(".notice-inline exists in index.css and uses var(-- tokens for all colour properties", () => {
    // The .notice-inline base rule must exist
    const rule = css.match(/\.notice-inline\s*\{[^}]*\}/)?.[0] ?? "";
    expect(rule, ".notice-inline rule not found in index.css").not.toBe("");

    // Every colour expression must be via a CSS custom-property token (var(--…))
    // or a color-mix() that itself references a token — never a bare hex/rgb/oklch literal.
    // This guarantees the future alerts strip can safely reuse the pattern.
    expect(rule).toContain("var(--");
    expect(rule).not.toMatch(/#[0-9a-fA-F]{3,6}\b/); // no bare hex
    expect(rule).not.toMatch(/\brgb\(/);              // no bare rgb()
  });
});

// --- Pending: converted to real assertions as each phase lands ---

describe("usability-clean — .home-explainer is token-based (Cl-1)", () => {
  it(".home-explainer exists in index.css and uses only var(-- tokens for colour properties", () => {
    // Extract the .home-explainer rule block
    const rule = css.match(/\.home-explainer\s*\{[^}]*\}/)?.[0] ?? "";
    expect(rule, ".home-explainer rule not found in index.css").not.toBe("");

    // Must reference at least one CSS custom-property token
    expect(rule).toContain("var(--");
    // Must not use bare hex colour literals
    expect(rule).not.toMatch(/#[0-9a-fA-F]{3,6}\b/);
    // Must not use bare rgb() colour literals
    expect(rule).not.toMatch(/\brgb\(/);
    // Must not use bare oklch() colour literals (tokens wrap them)
    expect(rule).not.toMatch(/\boklch\(/);
  });
});
it.todo("Cl-2: alerts strip uses .notice-inline tints, no new color literal (Phase 1)");
it.todo("Cl-3: ⌘K palette reuses .dialog idiom, role=dialog present (Phase 6)");

describe("usability-clean — .cmd-block.wrap variant exists (P2.1)", () => {
  it(".cmd-block.wrap rule exists in index.css with pre-wrap and break-all", () => {
    expect(css).toContain(".cmd-block.wrap");
    expect(css).toContain("pre-wrap");
    expect(css).toContain("break-all");
  });
});
