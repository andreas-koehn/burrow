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

it.todo("Cl-1: Home uses .card/.metric-tile DS primitives, no one-off styles (Phase 1)");
it.todo("Cl-2: alerts strip uses .notice-inline tints, no new color literal (Phase 1)");
it.todo("Cl-3: ⌘K palette reuses .dialog idiom, role=dialog present (Phase 6)");
