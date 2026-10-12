/// <reference types="node" />
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative, resolve } from "node:path";
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
  it("the setup checklist draws its own step markers instead of list numbers", () => {
    expect(css).toMatch(/\.setup-checklist ol\s*\{[^}]*list-style:\s*none/);
    expect(css).not.toContain(".home-explainer");
  });
});

describe("T16 — back links", () => {
  it("styles the page back link inside the page header", () => {
    // At 390 px two header actions used to overlap the title; they wrap below it now.
    expect(css).toMatch(/\.page-header\s*\{[^}]*flex-wrap:\s*wrap/);
    expect(css).toMatch(/\.page-header \.left\s*\{[^}]*flex:\s*1 1 200px/);
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
  // The settings pages have a container of their own (see below): a form there starts at the left edge.
  it.each(["ConnectClient"])("%s is a narrow page", (name) => {
    const w = wrappers(name);
    expect(w.length).toBeGreaterThan(0);
    for (const c of w) expect(c.split(" ")).toContain("page-narrow");
  });
  it.each(["Services", "Users", "settings/Sessions", "BackupRestore", "CustomDomainsOverview", "ServiceDetail", "ServicesOverview", "GatewayOverview"])(
    "%s keeps the full column (it shows a table, tiles or tabs)",
    (name) => {
      expect(page(name)).not.toContain("page-narrow");
    },
  );
});

describe("settings pages share one container", () => {
  const app = readFileSync(resolve(__dirname, "App.tsx"), "utf8");
  // Every page routed under /settings, as the file it is imported from.
  const components = [...app.matchAll(/<Route path="\/settings\/[^"]+" element=\{(?:<RequireAdmin>)?<(\w+) \/>/g)].map((m) => m[1]!);
  const files = components.map((c) => app.match(new RegExp(`import ${c} from "@/pages/([^"]+)"`))?.[1] ?? `no import of ${c}`);
  const page = (name: string) => readFileSync(resolve(__dirname, "pages", `${name}.tsx`), "utf8");
  // className of every element that directly wraps a <PageHeader> in a page: its root, in each of its states.
  const roots = (name: string) => [...page(name).matchAll(/<div className="([^"]*)">\s*<PageHeader\b/g)].map((m) => m[1]!);

  it("looks at every settings page", () => {
    expect(files).toEqual(expect.arrayContaining([
      "settings/GeneralSettings", "settings/EmailSettings", "Retention", "DatabaseBackend", "BackupRestore",
      "Users", "Roles", "AuditLog", "Webhooks", "OpenApiViewer", "settings/Profile", "settings/Sessions", "AutomationTokens",
    ]));
  });
  // The pages that hold forms only. Every other settings page shows a table (or the API viewer).
  const FORM_PAGES = ["settings/GeneralSettings", "settings/EmailSettings", "Retention", "DatabaseBackend", "settings/Profile"];
  const formPages = files.filter((f) => FORM_PAGES.includes(f));
  const tablePages = files.filter((f) => !FORM_PAGES.includes(f));

  it.each(files)("%s carries the class on its root, in every state it renders", (name) => {
    const found = roots(name);
    expect(found.length).toBeGreaterThan(0);
    for (const c of found) expect(c.split(" ")).toContain("settings-page");
    // Not centred as well: the left edge is the same on every settings page.
    expect(page(name)).not.toContain("page-narrow");
  });
  it("sorts every settings page into one of the two kinds by what it shows", () => {
    expect(formPages).toHaveLength(FORM_PAGES.length);
    for (const name of formPages) expect(page(name), name).not.toMatch(/<table\b|<iframe\b/);
    for (const name of tablePages) expect(page(name), name).toMatch(/<table\b|<iframe\b/);
  });
  it.each(FORM_PAGES)("%s, a page of forms, says so on its root", (name) => {
    for (const c of roots(name)) expect(c.split(" ")).toEqual(expect.arrayContaining(["settings-page", "settings-form"]));
  });
  // The width is the page's, written in its source: it cannot change while the
  // table is loading, is empty or gave way to an error.
  it.each(tablePages)("%s, a page with a table, keeps the whole column in every state", (name) => {
    expect(roots(name).length).toBeGreaterThan(0);
    expect(page(name)).not.toContain("settings-form");
  });
  it("gives the page the whole column and caps a page of forms, at the same left edge", () => {
    // Without the comments: the one above the rules names the classes too.
    const plain = css.replace(/\/\*[\s\S]*?\*\//g, "");
    const rules = (plain.match(/[^{}]*\.settings-(?:page|form)[^{]*\{[^}]*\}/g) ?? []).map((r) => r.trim());
    expect(rules.map((r) => r.slice(0, r.indexOf("{")).trim())).toEqual([".settings-page", ".settings-page.settings-form"]);
    expect(rules[0]).toMatch(/max-width:\s*none/);
    expect(rules[1]).toMatch(/max-width:\s*var\(--page-w-narrow\)/);
    // Never centred: no auto margin moves either away from the left edge.
    for (const r of rules) expect(r).not.toMatch(/margin[^;]*auto/);
    // The width never depends on what the page holds at the moment.
    expect(plain).not.toMatch(/settings-[a-z-]+[^{]*:has\(/);
  });
});

describe("type size", () => {
  // Every rule that sets a size, as [selector, size in px]. Comments are dropped first.
  const plain = css.replace(/\/\*[\s\S]*?\*\//g, "");
  const sizes: [string, number][] = [];
  const unread: string[] = [];
  for (const m of plain.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const selector = m[1]!.trim().replace(/\s+/g, " ");
    for (const d of m[2]!.matchAll(/(?:^|[;\s])font(-size)?\s*:\s*([^;]+)/g)) {
      const value = d[2]!.trim();
      if (value === "inherit") continue;
      const px = d[1] ? value.match(/^(\d+(?:\.\d+)?)(px|rem)$/) : null;
      if (!px) { unread.push(`${selector} { ${d[0].trim()} }`); continue; }
      sizes.push([selector, px[2] === "px" ? Number(px[1]) : Number(px[1]) * 16]);
    }
  }
  const below = (min: number, skip: readonly string[] = []) =>
    sizes.filter(([selector, px]) => px < min && !skip.includes(selector)).map(([selector, px]) => `${selector}: ${px}px`);

  // What may stay at 11px: uppercase labels set with letter spacing, and keyboard hints.
  const UPPERCASE_LABELS = [
    "table.data thead th",   // column headers
    ".section-label",        // group titles
    ".nav-group-title",
  ];
  const KEY_HINTS = [".kbd-token", ".menu-item .shortcut"];
  const SMALL_LABELS = [...UPPERCASE_LABELS, ...KEY_HINTS];

  it("reads every size the stylesheet sets", () => {
    // A size in another unit, or in the font shorthand, would pass unseen.
    expect(unread).toEqual([]);
    expect(sizes.length).toBeGreaterThan(100);
  });
  it("sets no size below 11px anywhere", () => {
    expect(below(11)).toEqual([]);
  });
  it("sets none below 12px outside headers, group titles and keyboard hints", () => {
    expect(below(12, SMALL_LABELS)).toEqual([]);
    // The list names rules that exist, and nothing that needs no exception.
    for (const selector of SMALL_LABELS) {
      const px = sizes.find(([s]) => s === selector)?.[1];
      expect(px, selector).toBeGreaterThanOrEqual(11);
      expect(px, selector).toBeLessThan(12);
    }
  });
  it("keeps the small labels uppercase with letter spacing, or in the mono face of a key", () => {
    const rule = (selector: string) =>
      plain.match(new RegExp(`(?:^|\\n)${selector.replace(/[.[\]"=]/g, "\\$&")}\\s*\\{[^}]*\\}`))?.[0] ?? "";
    for (const selector of UPPERCASE_LABELS) {
      expect(rule(selector), selector).toMatch(/text-transform:\s*uppercase/);
      expect(rule(selector), selector).toMatch(/letter-spacing:/);
    }
    for (const selector of KEY_HINTS) expect(rule(selector), selector).toMatch(/font-family:\s*var\(--font-mono\)/);
  });
  it("has no micro step in the type scale: nothing used it", () => {
    expect(plain).not.toMatch(/\.t-micro\b/);
  });
  it("sets body text and table cells at 13px or more", () => {
    const of = (selector: string) => sizes.find(([s]) => s === selector)?.[1];
    expect(of("body")).toBeGreaterThanOrEqual(13);
    expect(of("table.data")).toBeGreaterThanOrEqual(13);
    // No rule makes a cell smaller than the table it is in.
    const cells = sizes.filter(([selector]) => /\btd\b/.test(selector));
    expect(cells.length).toBeGreaterThan(0);
    expect(cells.filter(([, px]) => px < 13).map(([selector, px]) => `${selector}: ${px}px`)).toEqual([]);
  });

  // The stylesheet cannot see a class put on a cell: .small is 12px, on whatever carries it.
  describe("no table cell is made small in the source", () => {
    const walk = (dir: string): string[] => readdirSync(dir).flatMap((f) => {
      const p = join(dir, f);
      return statSync(p).isDirectory() ? walk(p) : [p];
    });
    /** Every <td whose className, a literal or an expression, names the class small. */
    const smallCells = (src: string) =>
      [...src.matchAll(/<td\b[^>]*?\bclassName=(?:"([^"]*)"|\{([^}]*)\})/g)]
        .filter((m) => /(?:^|[\s"'`])small(?:$|[\s"'`])/.test(m[1] ?? m[2] ?? ""))
        .map((m) => m[0]);
    const sources = walk(__dirname).filter((p) => /\.tsx$/.test(p) && !/\.test\.tsx$/.test(p));

    it("looks at the pages and components", () => {
      expect(sources.length).toBeGreaterThan(50);
      expect(sources.some((p) => p.endsWith(join("pages", "AuditLog.tsx")))).toBe(true);
    });
    it("catches the class on a cell, and only there", () => {
      expect(smallCells('<td className="mono small">x</td>')).toHaveLength(1);
      expect(smallCells('<td className="small">x</td>')).toHaveLength(1);
      expect(smallCells('<td\n  className="col-traffic small"\n>x</td>')).toHaveLength(1);
      expect(smallCells('<td className={open ? "small" : "mono"}>x</td>')).toHaveLength(1);
      expect(smallCells('<td className={cx("mono", dim && "small")}>x</td>')).toHaveLength(1);
      // In a cell, on something else: help text under a value may be small.
      expect(smallCells('<td className="mono"><span className="muted small">x</span></td>')).toEqual([]);
      expect(smallCells('<td className="col-smallest">x</td><p className="small">y</p>')).toEqual([]);
    });
    it.each(sources.map((p) => relative(__dirname, p)))("%s", (file) => {
      expect(smallCells(readFileSync(join(__dirname, file), "utf8"))).toEqual([]);
    });
  });
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
  it("draws the workspace tabs with existing tokens, in one row of equal widths", () => {
    expect(rule(".workspace-tabs")).toMatch(/display:\s*flex/);
    expect(rule(".workspace-tab")).toMatch(/flex:\s*1 1 0/);
    expect(rule(".workspace-tab")).toMatch(/color:\s*var\(--muted-foreground\)/);
    expect(rule(".workspace-tab")).toMatch(/font-family:\s*var\(--font-sans\)/);
    expect(rule(".workspace-tab:focus-visible")).toMatch(/outline:\s*2px solid var\(--ring\)/);
  });
  it("marks the current tab as the current navigation entry is marked", () => {
    const declared = (r: string, property: string) => r.match(new RegExp(`(?:^|[\\s;{])${property}:\\s*([^;]+);`))?.[1];
    for (const property of ["color", "font-weight", "background"]) {
      expect(declared(rule(".workspace-tab.is-active"), property), property).toBe(declared(rule(".nav-item.is-active"), property));
    }
    expect(declared(rule(".workspace-tab.is-active::before"), "background")).toBe(declared(rule(".nav-item.is-active::before"), "background"));
    expect(declared(rule(".dark .workspace-tab.is-active"), "background")).toBe(declared(rule(".dark .nav-item.is-active"), "background"));
    // Collapsed, the bar is on the left edge, exactly where an entry has it.
    expect(rule(".sidebar.is-collapsed .workspace-tab.is-active::before")).toMatch(/left:\s*0;[^}]*width:\s*3px/);
  });
  it("leaves nothing of the switcher behind", () => {
    expect(css).not.toMatch(/workspace-switcher|\.workspace-text|\.workspace-name|\.namespace\b|\.menu-check/);
  });
  it("narrows the collapsed sidebar to its icons without hiding named text", () => {
    expect(rule(".sidebar.is-collapsed")).toMatch(/width:\s*56px/);
    expect(rule(".sidebar.is-collapsed .nav-item")).toMatch(/justify-content:\s*center/);
    // Names come from aria-label; no collapsed rule may switch an element off.
    expect(css).not.toMatch(/\.sidebar\.is-collapsed[^{]*\{[^}]*display:\s*none/);
  });
  it("adds no colour of its own: every colour in the shell rules is a custom property", () => {
    const shell = css.match(/\.(?:topbar|workspace-tabs?|workspace-brand|sidebar-workspaces|sidebar\.is-collapsed|sidebar-back|shell-column|nav-search|sidebar-footer-nav)[^{]*\{[^}]*\}/g) ?? [];
    expect(shell.length).toBeGreaterThan(10);
    for (const r of shell) expect(r).not.toMatch(/#[0-9a-f]{3,8}\b|oklch\(|rgba?\(|hsla?\(/i);
  });
  it("drops the wordmark rule with the brand row it belonged to", () => {
    expect(css).not.toContain(".sidebar-brand .wordmark");
  });
});

describe("X08 — request inspector stays inside a narrow screen", () => {
  it("gives the single column no minimum of its content's width", () => {
    const narrow = css.match(/@media \(max-width: 900px\) \{[^@]*?\.inspector-grid\s*\{[^}]*\}/)?.[0] ?? "";
    expect(narrow).toContain("grid-template-columns: minmax(0, 1fr)");
    expect(css).toMatch(/\.detail-pane\s*\{[^}]*min-width: 0/);
  });
  it("wraps the pane's tabs and scrolls a wide table inside its panel", () => {
    expect(css).toMatch(/\.detail-pane \.tabs-list\s*\{[^}]*flex-wrap: wrap/);
    const panel = css.match(/\.detail-pane \.tab-panel\s*\{[^}]*\}/)?.[0] ?? "";
    expect(panel).toContain("min-width: 0");
    expect(panel).toContain("overflow-x: auto");
  });
  it("gives the translate switch and its label a target of at least 24px", () => {
    expect(css).toMatch(/\.model-translate > label\s*\{[^}]*min-height: 24px/);
  });
  it("scrolls a captured body inside the detail pane", () => {
    const rule = css.match(/\.detail-pane pre\s*\{[^}]*\}/)?.[0] ?? "";
    expect(rule).toContain("overflow-x: auto");
    expect(rule).toContain("max-width: 100%");
  });
});
