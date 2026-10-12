import { test, expect } from "@playwright/test";

// P2.7 — Connect a client (real-stack Playwright spec).
//
// Exercises the ConnectClient page against the live burrowd:
//   1. Navigate to /clients/connect
//   2. Assert the three lines, none of which holds a token
//   3. Assert the "Waiting…" indicator appears in role=status
//   4. Assert "Other ways to connect" holds the downloads, the burrow.yaml
//      example and the way to a token, and no form
//   5. Assert /clients/connect?service=<id> shows the command that serves that service

// Use the globalSetup-cached admin session (web/e2e/global-setup.ts).
test.use({ storageState: "playwright-auth.json" });

test("connect-client: three lines, the waiting indicator, and other ways without a form", async ({ page }) => {
  // ── 1. Navigate ──────────────────────────────────────────────────────────
  await page.goto("/clients/connect");
  await expect(page.getByRole("heading", { name: "Connect a client" })).toBeVisible();

  // ── 2. Page-level explainer must be visible ──────────────────────────────
  await expect(page.getByText(/machine running/i)).toBeVisible();

  // ── 2a. The three lines come first; none of them holds a token ───────────
  for (const name of ["Copy install command", "Copy sign-in command", "Copy run command"]) {
    await expect(page.getByRole("button", { name })).toBeVisible();
  }
  await expect(page.locator(".install-line pre code")).toHaveText([
    /\/install\.(sh \| sh|ps1 \| iex)$/, /^burrow login \S+$/, "burrow http 3000",
  ]);

  // ── 3. Waiting indicator ─────────────────────────────────────────────────
  // No burrow client connects in the e2e test environment.
  await expect(page.locator("[role=status]").filter({ hasText: "Waiting for your client…" })).toBeVisible();

  // ── 4. Other ways to connect: downloads, burrow.yaml, the way to a token ──
  const other = page.getByRole("button", { name: "Other ways to connect" });
  await expect(other).toHaveAttribute("aria-expanded", "false");
  await other.click();
  await expect(other).toHaveAttribute("aria-expanded", "true");
  await expect(page.getByRole("heading", { name: "Download by hand" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "burrow.yaml" })).toBeVisible();

  // The form that minted a token for a `burrow connect` line is gone.
  for (const label of ["Local address", "Public port", "Client name"]) {
    await expect(page.getByLabel(label)).toHaveCount(0);
  }
  await expect(page.getByRole("button", { name: /generate token/i })).toHaveCount(0);
  await expect(page.locator("pre#connect-command")).toHaveCount(0);
  await expect(page.getByText(/bur_/)).toHaveCount(0);

  // A token is made under Clients → Tokens.
  await expect(page.getByText(/Need a token for a machine without a browser\?/)).toBeVisible();
  await page.getByRole("link", { name: "Clients → Tokens" }).click();
  await expect(page).toHaveURL(/\/clients\?tab=tokens$/);
  await expect(page.getByRole("tab", { name: "Tokens", selected: true })).toBeVisible();
});

test("connect-client: ?service=<id> shows the command that serves that service", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  // A client finds its service by name: the title is what the command names.
  const svcId = `pw-connect-${Date.now()}`;
  const created = await page.request.post("/api/v1/services", {
    headers,
    data: { service_id: svcId, title: svcId, access_mode: "api_key" },
  });
  expect([201, 409]).toContain(created.status());

  await page.goto(`/clients/connect?service=${svcId}`);
  await expect(page.getByText(`Serve ${svcId} from this machine.`)).toBeVisible();
  // Never served, so the port is the example one; the name is the service's.
  await expect(page.locator(".install-line pre code").last()).toHaveText(`burrow http 3000 --name ${svcId}`);

  // A service that is gone leaves the plain page, and says so.
  await page.goto("/clients/connect?service=pw-connect-gone");
  await expect(page.getByText("That service no longer exists.")).toBeVisible();
  await expect(page.locator(".install-line pre code").last()).toHaveText("burrow http 3000");
});

test("connect-client: on a phone the three lines are readable and nothing leaves the page", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/clients/connect");
  await expect(page.getByRole("heading", { name: "Connect a client" })).toBeVisible();

  // Every operating system can be chosen: none of the three is cut off.
  for (const name of ["Linux", "macOS", "Windows"]) {
    const box = await page.getByRole("radio", { name }).boundingBox();
    expect(box, name).not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
  }
  await page.getByRole("radio", { name: "Windows" }).click();
  await expect(page.locator(".install-line pre code").first()).toHaveText(/install\.ps1 \| iex$/);
  await page.getByRole("radio", { name: "Linux" }).click();

  // Each command is one line in its box; a long one scrolls inside the box.
  const boxes = await page.locator(".install-line pre").evaluateAll((els) => els.map((el) => {
    const r = el.getBoundingClientRect();
    const row = el.closest(".install-line")!.getBoundingClientRect();
    return { left: r.left, right: r.right, width: r.width, rowWidth: row.width, height: r.height, scrolls: el.scrollWidth > el.clientWidth, overflowX: getComputedStyle(el).overflowX };
  }));
  expect(boxes).toHaveLength(3);
  for (const b of boxes) {
    expect(b.right).toBeLessThanOrEqual(390);
    expect(b.height).toBeLessThan(60); // one line of text, not a column of letters
    // The box has the whole width the page has there (the sidebar keeps its
    // own); the copy button is on the line below, not beside it.
    expect(b.width).toBeGreaterThanOrEqual(b.rowWidth - 1);
    expect(b.overflowX).toBe("auto");
  }
  expect(boxes[0]!.scrolls).toBe(true); // the install line is longer than a phone is wide

  // The copy buttons stay on the page too.
  for (const name of ["Copy install command", "Copy sign-in command", "Copy run command"]) {
    const box = await page.getByRole("button", { name }).boundingBox();
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
  }
  // And the page itself does not scroll sideways.
  expect(await page.evaluate(() => document.scrollingElement!.scrollWidth <= window.innerWidth)).toBe(true);
});
