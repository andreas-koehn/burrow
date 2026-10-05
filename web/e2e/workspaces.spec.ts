import { test, expect, type Page } from "@playwright/test";

// The workspace shell end to end: two workspaces behind one switcher, Settings as a
// place of its own, old bookmarks, and what survives a reload. Runs against the real
// burrowd as the seeded admin (who always has the AI Gateway workspace).

test.use({ storageState: "playwright-auth.json" });

const sidebar = (page: Page) => page.locator(".sidebar");
const switcher = (page: Page, workspace: string) => page.getByRole("button", { name: `Workspace: ${workspace}` });
const pathOf = (page: Page) => { const u = new URL(page.url()); return u.pathname + u.search + u.hash; };

async function switchTo(page: Page, from: string, to: RegExp) {
  await switcher(page, from).click();
  await page.getByRole("menu", { name: "Workspace" }).getByRole("menuitemradio", { name: to }).click();
}

test("the switcher moves between the two workspaces and the sidebar follows", async ({ page }) => {
  await page.goto("/");
  await expect(switcher(page, "Services")).toContainText("/svc/…");
  await expect(page.getByRole("navigation", { name: "Services" }).getByRole("link", { name: "Traffic" })).toBeVisible();

  await switchTo(page, "Services", /AI Gateway/);
  await expect(page).toHaveURL(/\/gateway$/);
  await expect(switcher(page, "AI Gateway")).toContainText("/ai/…");
  const gateway = page.getByRole("navigation", { name: "AI Gateway" });
  await expect(gateway.getByRole("link", { name: "Providers" })).toBeVisible();
  await expect(gateway.getByRole("link", { name: "Traffic" })).toHaveCount(0);

  await switchTo(page, "AI Gateway", /Services/);
  expect(pathOf(page)).toBe("/");
  await expect(page.getByRole("navigation", { name: "Services" })).toBeVisible();
});

test("the switcher works from the keyboard and gives the focus back", async ({ page }) => {
  await page.goto("/");
  const button = switcher(page, "Services");
  await button.focus();

  await page.keyboard.press("ArrowDown");
  const items = page.getByRole("menuitemradio");
  await expect(items.nth(0)).toBeFocused();
  await expect(items.nth(0)).toHaveAttribute("aria-checked", "true");
  await page.keyboard.press("ArrowDown");
  await expect(items.nth(1)).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("menu")).toHaveCount(0);
  await expect(button).toBeFocused();

  // Tab closes the menu and carries on from the button, not from the end of the document
  // (where the menu is portalled): the next control is the sidebar's Search.
  await page.keyboard.press("Enter");
  await expect(items.nth(0)).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("menu")).toHaveCount(0);
  await expect(sidebar(page).getByRole("button", { name: "Search" })).toBeFocused();

  // Space opens it too; Enter on the other workspace goes there.
  await button.focus();
  await page.keyboard.press("Space");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/gateway$/);
});

test("Settings opens from the footer and 'Back to …' returns to the workspace you came from", async ({ page }) => {
  await page.goto("/gateway/providers");
  await sidebar(page).getByRole("link", { name: /^Settings(,|$)/ }).click();
  await expect(page).toHaveURL(/\/settings\/(general|email)$/);
  await expect(page.getByRole("navigation", { name: "Settings" })).toBeVisible();
  await sidebar(page).getByRole("link", { name: "Back to AI Gateway" }).click();
  expect(pathOf(page)).toBe("/gateway");

  await page.goto("/services");
  await sidebar(page).getByRole("link", { name: /^Settings(,|$)/ }).click();
  await expect(page.getByRole("navigation", { name: "Settings" })).toBeVisible();
  await sidebar(page).getByRole("link", { name: "Back to Services" }).click();
  expect(pathOf(page)).toBe("/");
});

test("old bookmarks land on their new page with their detail intact", async ({ page }) => {
  await page.goto("/inspector?range=7d&q=chat#top");
  await expect(page.getByRole("heading", { name: "Requests", level: 1 })).toBeVisible();
  expect(pathOf(page)).toBe("/gateway/requests?range=7d&q=chat#top");
  await expect(page.getByRole("navigation", { name: "AI Gateway" }).getByRole("link", { name: "Requests" }))
    .toHaveAttribute("aria-current", "page");

  await page.goto("/tunnels");
  await expect(page.getByRole("heading", { name: "Services", level: 1 })).toBeVisible();
  expect(pathOf(page)).toBe("/services?live=1");
  await expect(page.getByRole("radio", { name: "Live" })).toBeChecked();
  await expect(page.getByRole("navigation", { name: "Services" }).getByRole("link", { name: /^Services/ }))
    .toHaveAttribute("aria-current", "page");

  await page.goto("/account/automation?from=mail");
  await expect(page.getByRole("heading", { name: "Automation tokens", level: 1 })).toBeVisible();
  expect(pathOf(page)).toBe("/settings/automation?from=mail");
  await expect(page.getByRole("navigation", { name: "Settings" }).getByRole("link", { name: "Automation tokens" }))
    .toHaveAttribute("aria-current", "page");
});

test("a reload keeps the workspace of the URL; the back button returns to the previous one", async ({ page }) => {
  await page.goto("/services");
  await switchTo(page, "Services", /AI Gateway/);
  await page.getByRole("navigation", { name: "AI Gateway" }).getByRole("link", { name: "Providers" }).click();
  await expect(page).toHaveURL(/\/gateway\/providers$/);

  await page.reload();
  await expect(switcher(page, "AI Gateway")).toBeVisible();
  await expect(page.getByRole("navigation", { name: "AI Gateway" }).getByRole("link", { name: "Providers" }))
    .toHaveAttribute("aria-current", "page");

  await page.goBack(); // /gateway
  await expect(switcher(page, "AI Gateway")).toBeVisible();
  await page.goBack(); // /services
  await expect(page).toHaveURL(/\/services$/);
  await expect(switcher(page, "Services")).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Services" })).toBeVisible();
});

test("the collapsed sidebar survives a reload and keeps a name on every icon", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("button", { name: "Collapse sidebar" }).click();
  await expect(sidebar(page)).toHaveClass(/is-collapsed/);

  await page.reload();
  await expect(sidebar(page)).toHaveClass(/is-collapsed/);
  await expect(page.getByRole("button", { name: "Expand sidebar" })).toHaveAttribute("aria-expanded", "false");
  await expect(sidebar(page).locator(".nav-label")).toHaveCount(0);
  // Icons only, each still named and with a tooltip.
  for (const link of await sidebar(page).getByRole("link").all()) {
    const name = await link.getAttribute("aria-label");
    expect(name).toBeTruthy();
    await expect(link).toHaveAttribute("title", name!);
  }

  await page.getByRole("button", { name: "Expand sidebar" }).click();
  await page.reload();
  await expect(sidebar(page)).not.toHaveClass(/is-collapsed/);
  await expect(sidebar(page).locator(".nav-label").first()).toBeVisible();
});

// Traffic and Requests share one filter bar. Its search text lives in the URL, which the
// router updates a moment after the key: the box must not lose the caret or a key to that.
for (const path of ["/traffic", "/gateway/requests"]) {
  test(`the Filter box on ${path} keeps the caret where you type and every key typed fast`, async ({ page }) => {
    await page.goto(`${path}?q=abcdef`);
    const box = page.getByRole("searchbox", { name: "Filter" });
    await expect(box).toHaveValue("abcdef");
    await box.click();
    for (const key of ["Home", "ArrowRight", "ArrowRight"]) await page.keyboard.press(key);
    await page.keyboard.type("XYZ", { delay: 40 });
    await expect(box).toHaveValue("abXYZcdef");
    await expect(page).toHaveURL(/[?&]q=abXYZcdef/);

    await box.fill("");
    await page.keyboard.type("hello world again"); // no delay between keys
    await expect(box).toHaveValue("hello world again");
    await expect(page).toHaveURL(/[?&]q=hello\+world\+again/);
  });
}

test("Traffic: filters changed one right after the other all reach the URL and survive a reload", async ({ page }) => {
  await page.goto("/traffic");
  await page.getByRole("radio", { name: "7 days" }).click();
  await page.getByRole("searchbox", { name: "Filter" }).fill("closed");
  await page.getByRole("combobox", { name: "Protocol" }).selectOption("http_proxy");
  await expect(page).toHaveURL(/\/traffic\?range=7d&q=closed&kind=http_proxy$/);

  await page.reload();
  await expect(page.getByRole("radio", { name: "7 days" })).toBeChecked();
  await expect(page.getByRole("searchbox", { name: "Filter" })).toHaveValue("closed");
  await expect(page.getByRole("combobox", { name: "Protocol" })).toHaveValue("http_proxy");
});
