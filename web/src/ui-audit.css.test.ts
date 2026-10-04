/// <reference types="node" />
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect } from "vitest";

// Text-level pins for the CSS contracts introduced by the 2026-10 UI audit
// fixes. jsdom does not load index.css, so these read the stylesheet source.
const css = readFileSync(resolve(__dirname, "index.css"), "utf8");

describe("T01 — FormField width classes do not collide with Tailwind utilities", () => {
  it("scopes input max-width to .form-field.field-w-*", () => {
    expect(css).toContain(".form-field.field-w-sm");
    expect(css).toContain(".form-field.field-w-md");
    expect(css).toContain(".form-field.field-w-lg");
    expect(css).toContain(".form-field.field-w-full");
  });
  it("has no .form-field.w-* selectors left", () => {
    expect(css).not.toMatch(/\.form-field\.w-(sm|md|lg|full)\b/);
  });
});

describe("T02 — dialog sizing and overflow", () => {
  it("defines three dialog widths", () => {
    expect(css).toMatch(/\.dialog\.size-sm\s*\{[^}]*420px/);
    expect(css).toMatch(/\.dialog\.size-md\s*\{[^}]*560px/);
    expect(css).toMatch(/\.dialog\.size-lg\s*\{[^}]*720px/);
  });
  it("dialog body never scrolls sideways and wraps long strings", () => {
    const rule = css.match(/\.dialog-body\s*\{[^}]*\}/)?.[0] ?? "";
    expect(rule).toContain("overflow-x: hidden");
    expect(rule).toContain("overflow-wrap: anywhere");
  });
});

describe("T09 — Guardrails custom-rules heading row", () => {
  it("spreads the heading and its action and drops the h3's own margin", () => {
    expect(css).toMatch(/\.accordion-body \.subsection-head\s*\{[^}]*justify-content: space-between/);
    expect(css).toMatch(/\.accordion-body \.subsection-head h3\s*\{[^}]*margin: 0/);
  });
});
