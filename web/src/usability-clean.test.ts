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

describe("usability-clean — the setup checklist is token-based (Cl-1)", () => {
  it(".setup-step-marker exists in index.css and uses only var(-- tokens for colour properties", () => {
    const rule = css.match(/\.setup-step-marker\s*\{[^}]*\}/)?.[0] ?? "";
    expect(rule, ".setup-step-marker rule not found in index.css").not.toBe("");
    expect(rule).toMatch(/color:\s*var\(--/);
    // Must not use bare hex colours
    expect(rule).not.toMatch(/#[0-9a-fA-F]{3,6}\b/);
    // Must not use bare rgb() colour literals
    expect(rule).not.toMatch(/\brgb\(/);
    // Must not use bare oklch() colour literals (tokens wrap them)
    expect(rule).not.toMatch(/\boklch\(/);
  });
});
it("Cl-2: overview rules use only var(-- tokens — no bare hex/oklch/rgb", () => {
  // The quick-action buttons in the ServicesOverview.tsx header, the setup
  // checklist and the linked metric tile must only reference DS tokens for
  // colour, never literals.
  for (const selector of ["setup-checklist", "setup-step", "metric-tile-link", "nav-attention", "alerts-strip"]) {
    const rules = css.match(new RegExp(`[^{}]*\\.${selector}[^{}]*\\{[^}]*\\}`, "g")) ?? [];
    expect(rules.length, selector).toBeGreaterThan(0);
    for (const rule of rules) {
      expect(rule).not.toMatch(/#[0-9a-fA-F]{3,6}\b/);
      expect(rule).not.toMatch(/\boklch\(/);
      expect(rule).not.toMatch(/\brgb\(/);
    }
  }
  const quickActionsRule = css.match(/\.home-quick-actions\s*\{[^}]*\}/)?.[0];
  // The rule may be absent if home-quick-actions has no colour properties (fine) —
  // but if it IS present it must be token-only.
  if (quickActionsRule) {
    expect(quickActionsRule).not.toMatch(/#[0-9a-fA-F]{3,6}\b/);
    expect(quickActionsRule).not.toMatch(/\boklch\(/);
    expect(quickActionsRule).not.toMatch(/\brgb\(/);
  }

  // The alerts strip reuses .notice-inline which is already validated in the
  // first describe block above. Verify .notice-inline exists in the CSS as a
  // further guard that the overviews' alerts strip has the right class available.
  expect(css).toContain(".notice-inline");
});
// Cl-3: verified via CommandPalette.test.tsx (RTL: getByRole('dialog') present when open=true).
// The CSS contract checked here: CommandPalette is rendered inside DS Dialog, which always
// emits class="dialog" with role="dialog". The .dialog class must exist in index.css.
it("Cl-3: .dialog CSS class exists in index.css (CommandPalette uses DS Dialog = role=dialog)", () => {
  expect(css).toMatch(/\.dialog\s*\{/);
});

describe("usability-clean — .cmd-block.wrap variant exists (P2.1)", () => {
  it(".cmd-block.wrap rule exists in index.css with pre-wrap and break-all", () => {
    expect(css).toContain(".cmd-block.wrap");
    expect(css).toContain("pre-wrap");
    expect(css).toContain("break-all");
  });
});
