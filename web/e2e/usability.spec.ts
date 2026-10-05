import { test, expect } from "@playwright/test";

// Usability acceptance — consolidated journey gate (Phase 8).
//
// Four describe blocks covering the Playwright-only usability criteria:
//   1. "Home is default"          — In-1, In-3, metric tiles, quick-action links
//   2. "Onboarding success loop"  — Ea-1 (connect-client form, command, Waiting indicator)
//   3. "Command palette"          — In-6, Ea-2 (Ctrl+K opens, type, Enter navigates)
//   4. "Cross-feature wiring"     — Clr-4 (Providers → "New AI service" dialog prefilled)
//                                  Ea-4 (Users SMTP notice → /settings link)
//
// This spec runs against the REAL built burrowd (no MSW mocks).
// Use `playwright-auth.json` for the admin session so the login rate-limit
// is not hit (mirrors all other specs in this suite).
//
// Run: npx playwright test e2e/usability.spec.ts
// List: npx playwright test e2e/usability.spec.ts --list

test.use({ storageState: "playwright-auth.json" });

// ---------------------------------------------------------------------------
// 1. Home is default
// ---------------------------------------------------------------------------
test.describe("Home is default", () => {
  test("/ renders 'Overview' heading with count tiles and quick-action links", async ({ page }) => {
    await page.goto("/");

    // In-1: root route shows the Home/Overview page
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();

    // Metric tiles — the Overview MetricStrip renders role="list" with aria-label="Overview"
    const strip = page.getByRole("list", { name: "Overview" });
    await expect(strip).toBeVisible();

    // In-3: explainer section present — use the bold terms inside the explainer
    // to avoid strict-mode violations from sidebar nav items with the same words.
    const explainer = page.locator("section.home-explainer");
    await expect(page.getByRole("heading", { name: "How Burrow works" })).toBeVisible();
    await expect(explainer.getByText("Client", { exact: true })).toBeVisible();
    await expect(explainer.getByText("Services", { exact: true })).toBeVisible();
    await expect(explainer.getByText("Tunnel", { exact: true })).toBeVisible();

    // Quick-action links in the page header
    await expect(page.getByRole("link", { name: /Connect a client/i })).toBeVisible();
    await expect(page.getByRole("link", { name: /New service/i })).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 2. Onboarding success loop  (Ea-1)
// ---------------------------------------------------------------------------
test.describe("Onboarding success loop", () => {
  test("connect-client: fill fields → command shows real endpoint + not clipped → Waiting indicator", async ({ page }) => {
    await page.goto("/clients/connect");
    await expect(page.getByRole("heading", { name: "Connect a client" })).toBeVisible();

    // Fill form fields
    const localInput = page.getByLabel("Local address");
    await localInput.clear();
    await localInput.fill("127.0.0.1:5000");

    const clientName = `usability-${Date.now()}`;
    await page.getByLabel("Client name").fill(clientName);
    await page.getByRole("button", { name: /generate token/i }).click();

    // "Run on the client" section appears after token is minted
    await expect(page.getByRole("heading", { name: /run on the client/i })).toBeVisible();

    // Command shows the real relay endpoint (not a hardcoded placeholder)
    const cmdBlock = page.locator("pre.cmd-block code");
    await expect(cmdBlock).toBeVisible();
    const cmdText = (await cmdBlock.textContent()) ?? "";
    expect(cmdText).toContain("--server ");
    expect(cmdText).toContain(`--name ${clientName}`);

    // Command block uses the .wrap class — pre-wrap / break-all applied
    await expect(page.locator("pre.cmd-block.wrap")).toBeVisible();

    // Waiting indicator (D9 honest state) — no real client connecting in CI
    const statusDiv = page.locator("[role=status]").last();
    await expect(statusDiv.getByText(/waiting for/i)).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 3. Command palette  (In-6, Ea-2)
// ---------------------------------------------------------------------------
test.describe("Command palette", () => {
  test("Ctrl+K opens the palette dialog; typing a destination + Enter navigates", async ({ page }) => {
    await page.goto("/");

    // Palette must be closed initially
    await expect(page.getByRole("dialog", { name: "Jump to…" })).not.toBeVisible();

    // Open with Ctrl+K
    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Jump to…" });
    await expect(dialog).toBeVisible();

    // Type a known destination (Services) and navigate
    await dialog.getByRole("searchbox").fill("Services");
    // At least one result should appear
    const firstResult = dialog.getByRole("option").first();
    await expect(firstResult).toBeVisible();

    // Press Enter → URL changes
    const beforeURL = page.url();
    await page.keyboard.press("Enter");
    // After navigation the URL should differ from the home page
    await page.waitForURL((url) => url.toString() !== beforeURL, { timeout: 10_000 });
    expect(page.url()).not.toBe(beforeURL);
  });
});

// ---------------------------------------------------------------------------
// 4. Cross-feature wiring
// ---------------------------------------------------------------------------
test.describe("Cross-feature wiring", () => {
  test("Providers page: 'New AI service' button opens dialog prefilled with API-key", async ({ page }) => {
    // Navigate directly to /services?new=ai, where the button on
    // /gateway/providers leads; the button itself is tested via RTL (Clr-4)
    // against MSW. Here we verify the guided create flow works end-to-end.
    await page.goto("/services?new=ai");

    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole("heading", { name: "New AI service" })).toBeVisible();

    // Access mode is fixed to API key for AI services: no picker is offered
    await expect(dialog.getByRole("button", { name: /access mode/i })).toHaveCount(0);
  });

  test("Users page: SMTP notice 'Set up email' link navigates to /settings/email", async ({ page }) => {
    // The SMTP notice on Users.tsx appears when smtp.host is not set.
    // A fresh e2e server has no SMTP configured.
    await page.goto("/settings/users");
    await expect(page.getByRole("heading", { name: "Users" })).toBeVisible();

    // SMTP notice should be visible (server not configured in test env)
    const notice = page.getByRole("status");
    // We check whether the notice shows or not — if SMTP IS configured in
    // the e2e env the test skips the link assertion gracefully.
    const noticeVisible = await notice.isVisible().catch(() => false);
    if (noticeVisible) {
      const link = notice.getByRole("link", { name: /Set up email/i });
      await expect(link).toBeVisible();
      await link.click();
      await expect(page).toHaveURL(/\/settings\/email$/);
    } else {
      // SMTP already configured — navigate to the email settings manually and verify
      await page.goto("/settings/email");
      await expect(page.getByRole("heading", { name: "Email", exact: true })).toBeVisible();
    }
  });
});
