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
  it("centres the capped content column in the main area", () => {
    const rule = css.match(/\.shell-content\s*\{[^}]*\}/)?.[0] ?? "";
    expect(rule).toMatch(/margin-inline:\s*auto/);
    // No later margin / margin-left / margin-right in the same rule may undo
    // the centring (the old rule left-anchored with margin: 0 + margin-right).
    expect(rule).not.toMatch(/margin(-left|-right)?:\s*0\b/);
    expect(rule).not.toMatch(/margin-(left|right):/);
    expect(rule).toMatch(/box-sizing:\s*border-box/);
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

describe("T15 — controls", () => {
  it("inputs and default buttons share one height", () => {
    expect(css).toMatch(/\.input, \.select-trigger\s*\{[^}]*height:\s*32px/);
    expect(css).toMatch(/\.btn\s*\{[^}]*height:\s*32px/);
  });
  it("textareas are not forced to single-line height", () => {
    expect(css).toMatch(/textarea\.input\s*\{[^}]*height:\s*auto/);
  });
  it("small controls get a larger hit area", () => {
    expect(css).toMatch(/\.switch::before\s*\{[^}]*inset:\s*-7px/);
    expect(css).toMatch(/button\.sort-header\s*\{[^}]*min-height:\s*28px/);
  });
  it("restores list numbers in the Home explainer", () => {
    expect(css).toMatch(/\.home-explainer ol\s*\{[^}]*list-style:\s*decimal/);
  });
});

describe("T16 — back links", () => {
  it("styles the page back link inside the page header", () => {
    expect(css).toMatch(/\.page-header \.page-back\s*\{[^}]*display:\s*inline-flex/);
    expect(css).toMatch(/\.page-header \.page-back:focus-visible\s*\{[^}]*outline:/);
  });
});

describe("final review — page header actions and dialog field widths", () => {
  it("resets the page-scoped .actions margin inside the page header", () => {
    const scoped = css.indexOf(".inspector-page     .actions {");
    const reset = css.search(/\.page-header \.actions\s*\{\s*margin-top:\s*0;?\s*\}/);
    expect(scoped).toBeGreaterThan(-1);
    // Equal specificity: the reset only wins if it comes later in the file.
    expect(reset).toBeGreaterThan(scoped);
    // The account/tokens/automation block sets the same margin further down.
    expect(reset).toBeGreaterThan(css.indexOf(".automation-page .actions {"));
  });
  it("lets md and lg fields fill the dialog body, after the capped rules", () => {
    const rule = css.match(
      /\.dialog-body \.form-field\.field-w-md\s+\.input,\s*\.dialog-body \.form-field\.field-w-md\s+\.select-trigger,\s*\.dialog-body \.form-field\.field-w-lg\s+\.input,\s*\.dialog-body \.form-field\.field-w-lg\s+\.select-trigger\s*\{\s*max-width:\s*none;?\s*\}/,
    );
    expect(rule).not.toBeNull();
    const capped = css.search(/\n\.form-field\.field-w-lg\s+\.select-trigger\s*\{\s*max-width:\s*var\(--input-w-lg\)/);
    expect(capped).toBeGreaterThan(-1);
    expect(rule!.index!).toBeGreaterThan(capped);
  });
  it("keeps sm fields narrow inside dialogs", () => {
    expect(css).not.toMatch(/\.dialog-body \.form-field\.field-w-sm/);
  });
});

describe("access-mode detail — field width and keys panel gap", () => {
  it("caps detail fields at the card width, out-specifying the dialog fill rule", () => {
    const rule = css.match(
      /\.dialog-body \.mode-detail \.form-field\.field-w-md \.input,\s*\.dialog-body \.mode-detail \.form-field\.field-w-md \.select-trigger,\s*\.dialog-body \.mode-detail \.form-field\.field-w-lg \.input,\s*\.dialog-body \.mode-detail \.form-field\.field-w-lg \.select-trigger\s*\{\s*max-width:\s*var\(--input-w-lg\);?\s*\}/,
    );
    expect(rule).not.toBeNull();
    // The cards the fields line up with carry the same cap.
    expect(css).toMatch(/\.access-mode-card\s*\{[^}]*max-width:\s*var\(--input-w-lg\)/);
  });
  it("separates the keys panel from the field above it", () => {
    expect(css).toMatch(/\.mode-detail > \.form-field \+ \.api-keys-panel\s*\{\s*margin-top:\s*var\(--space-md\);?\s*\}/);
  });
  it("lets the Select list follow its trigger width and scroll inside the viewport", () => {
    const rule = css.match(/\.menu\.select-list\s*\{[^}]*\}/)?.[0] ?? "";
    expect(rule).toMatch(/min-width:\s*0/);
    expect(rule).toMatch(/max-height:\s*min\(320px, calc\(100vh - 16px\)\)/);
    expect(rule).toMatch(/overflow-y:\s*auto/);
    // Equal specificity is not enough: it must out-specify .menu's 200px.
    expect(css).toMatch(/\.menu\s*\{[^}]*min-width:\s*200px/);
  });
  it("does not turn the detail area into a gap container the margin would stack on", () => {
    expect(css).not.toMatch(/\.mode-detail\s*\{[^}]*gap:/);
  });
});

describe("narrow page column — form-only pages", () => {
  const page = (name: string) => readFileSync(resolve(__dirname, "pages", `${name}.tsx`), "utf8");
  // className of every element that directly wraps a <PageHeader> in a page.
  const wrappers = (name: string) =>
    [...page(name).matchAll(/<div className="([^"]*)">\s*(?:<div className="page-header-row">\s*)?<PageHeader\b/g)].map((m) => m[1]);

  it("defines the cap as a token next to the input widths", () => {
    expect(css).toMatch(/--input-w-lg:\s*560px;\s*--page-w-narrow:\s*880px;/);
  });
  it("caps and centres the page wrapper with one modifier", () => {
    const rules = css.match(/\.page-narrow\s*\{[^}]*\}/g) ?? [];
    expect(rules).toHaveLength(1);
    expect(rules[0]).toMatch(/max-width:\s*var\(--page-w-narrow\)/);
    expect(rules[0]).toMatch(/margin-inline:\s*auto/);
    // The cap sits on the page wrapper, so the header shares the column; the
    // shell column itself keeps its own cap.
    expect(css).toMatch(/\.shell-content\s*\{[^}]*max-width:\s*1440px/);
    expect(css).not.toMatch(/\.page-narrow\s+\.page-header/);
  });
  it("is narrower than the shell column and wider than the widest field", () => {
    const px = (name: string) => Number(css.match(new RegExp(`${name}:\\s*(\\d+)px`))?.[1]);
    expect(px("--page-w-narrow")).toBeGreaterThan(px("--input-w-lg"));
    expect(px("--page-w-narrow")).toBeLessThan(1440);
  });
  it.each(["settings/GeneralSettings", "settings/EmailSettings", "settings/Profile", "Retention", "DatabaseBackend", "ConnectClient"])("%s is a narrow page", (name) => {
    const w = wrappers(name);
    expect(w.length).toBeGreaterThan(0);
    for (const c of w) expect(c.split(" ")).toContain("page-narrow");
  });
  it.each(["Services", "Users", "settings/Sessions", "BackupRestore", "CustomDomainsOverview", "ServiceDetail", "Home"])(
    "%s keeps the full column (it shows a table, tiles or tabs)",
    (name) => {
      expect(page(name)).not.toContain("page-narrow");
    },
  );
});

describe("workspace shell", () => {
  const rule = (selector: string) =>
    css.match(new RegExp(`(?:^|\\n)${selector.replace(/[.[\]"=]/g, "\\$&")}\\s*\\{[^}]*\\}`))?.[0] ?? "";

  it("gives the top bar a border and the page background from tokens", () => {
    expect(rule(".topbar")).toMatch(/border-bottom:\s*1px solid var\(--border\)/);
    expect(rule(".topbar")).toMatch(/background:\s*var\(--background\)/);
  });
  it("styles the breadcrumb as a plain inline list in the muted colour", () => {
    expect(rule(".topbar .crumbs")).toMatch(/list-style:\s*none/);
    expect(rule(".topbar .crumbs")).toMatch(/color:\s*var\(--muted-foreground\)/);
    expect(rule('.topbar .crumbs [aria-current="page"]')).toMatch(/color:\s*var\(--foreground\)/);
  });
  it("draws the switcher with existing tokens and the namespace in the mono face", () => {
    expect(rule(".workspace-switcher")).toMatch(/color:\s*var\(--foreground\)/);
    expect(rule(".workspace-switcher")).toMatch(/font-family:\s*var\(--font-sans\)/);
    expect(rule(".workspace-switcher .namespace")).toMatch(/font-family:\s*var\(--font-mono\)/);
    expect(rule(".workspace-switcher .namespace")).toMatch(/color:\s*var\(--muted-foreground\)/);
    expect(rule("button.workspace-switcher:focus-visible")).toMatch(/outline:\s*2px solid var\(--ring\)/);
  });
  it("narrows the collapsed sidebar to its icons without hiding named text", () => {
    expect(rule(".sidebar.is-collapsed")).toMatch(/width:\s*56px/);
    expect(rule(".sidebar.is-collapsed .nav-item")).toMatch(/justify-content:\s*center/);
    // Names come from aria-label; no collapsed rule may switch an element off.
    expect(css).not.toMatch(/\.sidebar\.is-collapsed[^{]*\{[^}]*display:\s*none/);
  });
  it("adds no colour of its own: every colour in the shell rules is a custom property", () => {
    const shell = css.match(/\.(?:topbar|workspace-switcher|sidebar\.is-collapsed|sidebar-back|shell-column|nav-search|sidebar-footer-nav)[^{]*\{[^}]*\}/g) ?? [];
    expect(shell.length).toBeGreaterThan(10);
    for (const r of shell) expect(r).not.toMatch(/#[0-9a-f]{3,8}\b|oklch\(|rgba?\(|hsla?\(/i);
  });
  it("drops the wordmark rule with the brand row it belonged to", () => {
    expect(css).not.toContain(".sidebar-brand .wordmark");
  });
});
