import { test, expect } from "@playwright/test";

// Verifies the dark/light color-token fixes (D-1, D-2/L-1) that jsdom cannot
// check because it does not resolve CSS custom properties or compute oklch → rgb
// conversions.  These tests run against the embedded burrowd webServer (built by
// run-server.mjs) in a real Chromium browser, so getComputedStyle returns the
// actual resolved values.
//
// D-1: modal scrim (dialog-backdrop) is a fixed full-viewport layer with
//   sufficient opacity (≥ 0.4 alpha) so it visibly dims the page.
// D-2/L-1: disabled primary CTA (btn-primary[disabled]) uses a flat muted
//   surface (opacity: 1; background: var(--muted)) — NOT a faded teal.
//
// Note: D-3/L-3 (input placeholder contrast) is guarded by the deterministic
// CSS-source pin in web/src/theme-tokens.test.ts (asserts the `.input::placeholder`
// rule no longer uses the faded 55% mix). A ::placeholder computed-style probe is
// unreliable across browser versions, so it is intentionally NOT duplicated here.

// Use the globalSetup-cached admin session (see web/e2e/global-setup.ts).
test.use({ storageState: "playwright-auth.json" });

test.describe("Visual color-token verification (D-1/D-2)", () => {
  test("D-1: modal scrim covers the viewport with ≥ 0.4 alpha", async ({
    page,
  }) => {
    await page.goto("/services");
    await expect(
      page.getByRole("heading", { name: "Services" }),
    ).toBeVisible();

    // Open the Create-service dialog — the real trigger in Services.tsx.
    await page.getByRole("button", { name: "New service", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(
      dialog.getByRole("heading", { name: "New service" }),
    ).toBeVisible();

    // The backdrop sits at class="dialog-backdrop" directly in the fixed
    // overlay wrapper (Dialog.tsx).
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

    // 3. Background-color alpha ≥ 0.4.  The CSS is oklch(0 0 0 / 0.5) which
    //    Chromium resolves to rgba(0, 0, 0, 0.498039) or similar.
    const bg = await backdrop.evaluate(
      (el) => getComputedStyle(el).backgroundColor,
    );
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
    await page.getByRole("button", { name: "New service", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();

    // The Create button in the footer is disabled while the Service ID field
    // is empty (disabled={!nsServiceId} in Services.tsx). Dialog.tsx renders
    // the footer inside the [role=dialog] element.
    const btn = dialog.locator(".dialog-footer button.btn-primary");
    await expect(btn).toBeVisible();

    // Must be disabled.
    await expect(btn).toBeDisabled();

    // The D-2/L-1 rule: opacity must be 1 (not the old 0.5 fade).
    await expect(btn).toHaveCSS("opacity", "1");

    // Background must be a near-neutral grey — NOT teal. The var(--muted)
    // token resolves to a grey; teal has a large green component vs red/blue,
    // so a grey muted surface has r ≈ g ≈ b and the chroma estimate is small.
    const bgColor = await btn.evaluate(
      (el) => getComputedStyle(el).backgroundColor,
    );
    const { r, g, b } = parseRGB(bgColor);
    const chromaEstimate = Math.abs(r - g) + Math.abs(g - b);
    expect(chromaEstimate).toBeLessThan(40);
  });
});

// ── Helpers ──────────────────────────────────────────────────────────────────

/**
 * Parse the alpha channel from a CSS color string.
 * Handles "rgba(r, g, b, a)", "rgb(r, g, b)" (alpha=1), and
 * "color(srgb r g b / a)" forms that Chromium may return.
 */
function parseAlpha(color: string): number {
  const rgba = color.match(
    /rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)(?:\s*,\s*([\d.]+))?\s*\)/,
  );
  if (rgba) {
    return rgba[4] !== undefined ? parseFloat(rgba[4]) : 1;
  }
  const srgb = color.match(/color\(srgb\s+[\d.]+\s+[\d.]+\s+[\d.]+\s*\/\s*([\d.]+)\s*\)/);
  if (srgb) return parseFloat(srgb[1]);
  return 1;
}

/**
 * Parse the RGB channels (0–255) from a CSS color string.
 */
function parseRGB(color: string): { r: number; g: number; b: number } {
  const m = color.match(/rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)/);
  if (m) {
    return { r: parseFloat(m[1]), g: parseFloat(m[2]), b: parseFloat(m[3]) };
  }
  return { r: 0, g: 0, b: 0 };
}
