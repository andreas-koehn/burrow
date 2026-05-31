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

describe("design tokens — border/input edges (D-4/L-2)", () => {
  it("light borders are darker than the old 0.900 hairline", () => {
    const light = css.slice(0, css.indexOf(".dark {"));
    expect(light).toContain("--border:               oklch(0.865 0.010 72);");
    expect(light).toContain("--input:                oklch(0.845 0.010 72);");
    expect(light).toContain("--border-strong:        oklch(0.800 0.012 72);");
  });
  it("dark borders are raised above the old 0.10 alpha", () => {
    const dark = css.slice(css.indexOf(".dark {"));
    expect(dark).toContain("--border:               oklch(1 0 0 / 0.15);");
    expect(dark).toContain("--input:                oklch(1 0 0 / 0.18);");
    expect(dark).toContain("--border-strong:        oklch(1 0 0 / 0.24);");
  });
});

describe("design tokens — disabled filled buttons (D-2/L-1)", () => {
  it("filled primary/destructive-solid disabled state uses a flat muted surface, not faded teal", () => {
    expect(css).toMatch(/\.btn-primary\[disabled\][^{]*\{[^}]*background:\s*var\(--muted\)/);
    expect(css).toMatch(/\.btn-primary\[disabled\][^{]*\{[^}]*opacity:\s*1/);
  });
});

describe("design tokens — notice banner tints (L-4)", () => {
  it("base notice has a faint destructive tint and warn/error/ok have explicit fills", () => {
    const base = css.match(/\.notice-inline\s*\{[^}]*\}/)?.[0] ?? "";
    expect(base).toContain("color-mix(in oklch, var(--destructive)");
    expect(base).not.toMatch(/background:\s*var\(--card\)\s*;/);
    expect(css).toMatch(/\.notice-inline\.warn\s*\{[^}]*color-mix\(in oklch, var\(--warning\)/);
    expect(css).toMatch(/\.notice-inline\.error\s*\{/);
    expect(css).toMatch(/\.notice-inline\.ok\s*\{/);
  });
});
