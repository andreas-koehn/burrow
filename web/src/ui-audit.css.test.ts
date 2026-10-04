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

describe("T09 — long redaction patterns wrap inside the rules table", () => {
  it("overrides the table's nowrap for the pattern cell", () => {
    const rule = css.match(/table\.data tbody td\.cell-wrap\s*\{[^}]*\}/)?.[0] ?? "";
    expect(rule).toContain("white-space: normal");
    expect(rule).toContain("overflow-wrap: anywhere");
  });
});

describe("T10 — type hierarchy and rhythm", () => {
  it("page title is larger than section headings", () => {
    expect(css).toMatch(/\.page-header \.left h1\s*\{[^}]*font-size:\s*20px/);
    expect(css).toMatch(/\.section-head \.left h2\s*\{[^}]*font-size:\s*15px/);
    expect(css).toMatch(/\.section-head \.left h3\s*\{[^}]*font-size:\s*13px/);
  });
  it("defines the shared rhythm helpers", () => {
    expect(css).toMatch(/\.stack-md\s*\{[^}]*gap:\s*var\(--space-md\)/);
    expect(css).toMatch(/\.page-intro\s*\{/);
    expect(css).toMatch(/\.section-head\.sub\s*\{[^}]*margin-top:\s*var\(--space-xl\)/);
    expect(css).toMatch(/\.card-title\s*\{[^}]*margin:\s*0 0 var\(--space-md\)/);
  });
  it("adjacency margins do not double the gap of flex/grid parents", () => {
    const reset = ".stack-md > .field,\n.stack-md > .row,\n.form-grid > .field,\n.pw-form > .field,\n.users-form > .field { margin-top: 0; }";
    expect(css).toContain(reset);
    // equal specificity: the reset only wins by coming after the adjacency rule
    expect(css.indexOf(reset)).toBeGreaterThan(css.indexOf(".field + .row"));
    expect(css).toContain(":where(:not(.form-grid, .stack-md, .form-field-group, .pw-form, .users-form)) > .notice-inline + * {");
    expect(css).toMatch(/\.section-head \.left\s*\{[^}]*gap:\s*0 var\(--space-sm\)/);
  });
});

describe("T11 — shell", () => {
  it("content area may grow to 1440px", () => {
    expect(css).toMatch(/\.shell-content\s*\{[^}]*max-width:\s*1440px/);
  });
  it("sidebar nav uses a thin scrollbar", () => {
    expect(css).toMatch(/\.sidebar-nav\s*\{[^}]*scrollbar-width:\s*thin/);
  });
});

describe("T12 — notice colours do not drift in hue", () => {
  it("mixes notice backgrounds in oklab, not oklch", () => {
    const block = css.slice(css.indexOf(".notice-inline {"), css.indexOf(".notice-inline .icon"));
    expect(block).not.toContain("color-mix(in oklch");
    expect(block.match(/color-mix\(in oklab/g)?.length ?? 0).toBeGreaterThanOrEqual(6);
  });
});

describe("T14 — table empty state", () => {
  it("centres the empty cell and styles title and hint with tokens", () => {
    expect(css).toMatch(/table\.data td\.table-empty\s*\{[^}]*text-align:\s*center/);
    expect(css).toMatch(/\.table-empty-title\s*\{[^}]*var\(--foreground\)/);
    expect(css).toMatch(/\.table-empty-hint\s*\{[^}]*var\(--muted-foreground\)/);
  });
});

describe("T14 — table empty row wraps and ignores row hover", () => {
  it("lets the empty cell wrap", () => {
    expect(css).toMatch(/table\.data td\.table-empty\s*\{[^}]*white-space:\s*normal/);
  });
  it("does not paint the hover background on the empty row", () => {
    expect(css).toMatch(/table\.data tbody tr:hover td\.table-empty\s*\{[^}]*background:\s*transparent/);
  });
});
