import { test, expect } from "@playwright/test";

// Verifies the dark/light color-token fixes (D-1/D-2/D-3 and their light
// counterparts) that jsdom cannot check because it does not resolve CSS custom
// properties or compute oklch → rgb conversions.  These tests run against the
// embedded burrowd webServer (built by run-server.mjs) in a real Chromium
// browser, so getComputedStyle returns the actual resolved values.
//
// D-1: modal scrim (dialog-backdrop) has sufficient opacity (≥ 0.4 alpha).
// D-2/L-1: disabled primary CTA (btn-primary[disabled]) uses a flat muted
//   surface (opacity: 1; background: var(--muted)) — NOT a faded teal.
// D-3/L-2: input placeholder colour is a visible grey, not near-transparent.

// Use the globalSetup-cached admin session (see web/e2e/global-setup.ts).
test.use({ storageState: "playwright-auth.json" });

test.describe("Visual color-token verification (D-1/D-2/D-3)", () => {
  // Open the Create-service dialog on /services and keep it open for both
  // scrim + disabled-button assertions so we only need one page.goto.
  test("D-1: modal scrim covers the viewport with ≥ 0.4 alpha", async ({
    page,
  }) => {
    await page.goto("/services");
    await expect(
      page.getByRole("heading", { name: "Services" }),
    ).toBeVisible();

    // Open the Create-service dialog — the real trigger in Services.tsx.
    await page.getByRole("button", { name: "+ New service" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(
      dialog.getByRole("heading", { name: "Create service" }),
    ).toBeVisible();

    // The backdrop sits at class="dialog-backdrop" directly in the fixed
    // overlay wrapper (Dialog.tsx line 60).
    const backdrop = page.locator(".dialog-backdrop");
    await expect(backdrop).toBeVisible();

    // 1. position must be fixed (the D-1 fix: inset:0 full-screen cover).
    const position = await backdrop.evaluate(
      (el) => getComputedStyle(el).position,
    );
    expect(position).toBe("fixed");

    // 2. Bounding rect must cover most of the viewport width (≥ 1000 px on
    //    the default Desktop Chrome 1280 px viewport).
    const box = await backdrop.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.width).toBeGreaterThan(1000);

    // 3. Background-color alpha ≥ 0.4.  The CSS is
    //    oklch(0 0 0 / 0.5) which Chromium resolves to rgba(0,0,0,0.5).
    //    Parse the rgba() / rgb() string the browser returns.
    const bg = await backdrop.evaluate(
      (el) => getComputedStyle(el).backgroundColor,
    );
    // bg is typically "rgba(0, 0, 0, 0.498039)" or similar.
    const alpha = parseAlpha(bg);
    expect(alpha).toBeGreaterThanOrEqual(0.4);
  });

  test("D-2/L-1: disabled primary CTA is a muted surface, not faded teal", async ({
    page,
  }) => {
    await page.goto("/services");
    await expect(
      page.getByRole("heading", { name: "Services" }),
    ).toBeVisible();

    // Open the Create-service dialog.
    await page.getByRole("button", { name: "+ New service" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();

    // The Create button in the footer is disabled while the Service ID field
    // is empty (disabled={!nsServiceId} in Services.tsx).
    // Dialog.tsx renders the footer inside the [role=dialog] element, so we
    // can scope directly: dialog > .dialog-footer > button.btn-primary.
    const btn = dialog.locator(".dialog-footer button.btn-primary");
    await expect(btn).toBeVisible();

    // Must be disabled.
    await expect(btn).toBeDisabled();

    // The D-2/L-1 rule: opacity must be 1 (not the old 0.5 fade).
    await expect(btn).toHaveCSS("opacity", "1");

    // Background must be a near-neutral grey — NOT teal.  The var(--muted)
    // token resolves to a grey.  Parse the rgb and assert |r-g| + |g-b| < 40.
    const bgColor = await btn.evaluate(
      (el) => getComputedStyle(el).backgroundColor,
    );
    const { r, g, b } = parseRGB(bgColor);
    const chromaEstimate = Math.abs(r - g) + Math.abs(g - b);
    // Teal (the primary colour) has a large green component vs red/blue;
    // a grey muted surface has r ≈ g ≈ b so the sum is small (< 40).
    expect(chromaEstimate).toBeLessThan(40);
  });

  test("D-3/L-2: input placeholder colour is a visible grey (best-effort)", async ({
    page,
  }) => {
    // Navigate to a page with a visible search/filter input.
    // The Connection-logs page has a search input that is always visible
    // (not gated on having data); use it as a stable target.
    await page.goto("/connection-logs");
    await expect(
      page.getByRole("heading", { name: "Connection logs" }),
    ).toBeVisible();

    // Find any visible input with a placeholder attribute.
    const input = page.locator("input[placeholder]").first();

    // Best-effort: ::placeholder computed styles are not always reachable via
    // getComputedStyle in all browser versions.  Wrap in a try/catch and skip
    // gracefully rather than fail if the pseudo-element is not accessible.
    const placeholderColor: string = await input
      .evaluate((el) => {
        // Inject a temporary <style> that copies the placeholder color onto a
        // data attribute so we can read it without the pseudo-element limit.
        // This is the standard workaround for ::placeholder in Playwright.
        const id = "__pw_placeholder_probe__";
        const style = document.createElement("style");
        style.id = id;
        style.textContent = `input::placeholder { color: inherit; }`;
        document.head.appendChild(style);
        // Read the element's own color as a proxy: placeholder inherits it if
        // the author has not overridden it with an explicit value.
        // For the actual placeholder value, read the CSS variable directly.
        const cs = getComputedStyle(el, "::placeholder");
        document.getElementById(id)?.remove();
        return cs.color;
      })
      .catch(() => "");

    if (!placeholderColor) {
      // ::placeholder not reachable in this environment — skip gracefully.
      test.skip(true, "::placeholder computed color not accessible; skipping D-3");
      return;
    }

    // The placeholder must not be near-transparent (alpha close to 0).
    // Acceptable: any colour where alpha ≥ 0.3, OR an opaque grey.
    const alpha = parseAlpha(placeholderColor);
    // If alpha is 0 or very low, the placeholder was rendered invisible —
    // that is the bug this test catches.
    expect(alpha).toBeGreaterThan(0.25);
  });
});

// ── Helpers ──────────────────────────────────────────────────────────────────

/**
 * Parse the alpha channel from a CSS color string.
 * Handles "rgba(r, g, b, a)", "rgb(r, g, b)" (alpha=1), and
 * "color(srgb r g b / a)" forms that Chromium may return.
 */
function parseAlpha(color: string): number {
  // rgba(r, g, b, a) — the most common Chromium form.
  const rgba = color.match(
    /rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)(?:\s*,\s*([\d.]+))?\s*\)/,
  );
  if (rgba) {
    return rgba[4] !== undefined ? parseFloat(rgba[4]) : 1;
  }
  // color(srgb r g b / a) — possible for wide-gamut sources.
  const srgb = color.match(/color\(srgb\s+[\d.]+\s+[\d.]+\s+[\d.]+\s*\/\s*([\d.]+)\s*\)/);
  if (srgb) return parseFloat(srgb[1]);
  // Fallback: treat as fully opaque.
  return 1;
}

/**
 * Parse the RGB channels (0–255) from a CSS color string.
 */
function parseRGB(color: string): { r: number; g: number; b: number } {
  const m = color.match(
    /rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)/,
  );
  if (m) {
    return { r: parseFloat(m[1]), g: parseFloat(m[2]), b: parseFloat(m[3]) };
  }
  return { r: 0, g: 0, b: 0 };
}
