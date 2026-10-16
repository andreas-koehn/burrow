import { test, expect, type APIRequestContext, type Page } from "@playwright/test";

// Management MCP: a person with a custom role sees exactly the pages and
// buttons their permissions open, and makes a read-only token for an MCP
// client from Settings → MCP (spec 2026-10-15, "Testing" → Playwright).

const ROLE = "operator-e2e";
const EMAIL = "operator-e2e@example.com";
const PASSWORD = "operator-e2e-password-123";
// The plan's list plus automation:tokens:manage:own: without it the relay refuses every
// automation-token request (routeAccess), so the role could not make the token of step 5.
const PERMISSIONS = ["ai:read:any", "ai:providers:manage", "ai:configure:any", "mcp:tools:read", "automation:tokens:manage:own"];

test.describe.configure({ mode: "serial" });

async function adminApi(page: Page): Promise<{ api: APIRequestContext; headers: Record<string, string> }> {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  return { api: page.request, headers: { "X-CSRF-Token": csrf, "Content-Type": "application/json" } };
}

/** Removes the user and the role of an earlier run against a reused server. */
async function cleanUp(page: Page): Promise<void> {
  const { api, headers } = await adminApi(page);
  const page_ = (await (await api.get(`/api/v1/users?q=${encodeURIComponent(EMAIL)}`)).json()) as { users: Array<{ id: string; email: string }> };
  for (const u of page_.users.filter((x) => x.email === EMAIL)) {
    const res = await api.delete(`/api/v1/users/${u.id}`, { headers });
    expect([200, 204]).toContain(res.status());
  }
  const role = await api.get(`/api/v1/roles/${ROLE}`);
  if (role.status() === 200) {
    const res = await api.delete(`/api/v1/roles/${ROLE}`, { headers });
    expect([200, 204]).toContain(res.status());
  }
}

test.describe("a custom role", () => {
  test.use({ storageState: "playwright-auth.json" });

  test.beforeAll(async ({ browser }) => {
    const ctx = await browser.newContext({ storageState: "playwright-auth.json" });
    const page = await ctx.newPage();
    await cleanUp(page);
    const { api, headers } = await adminApi(page);
    const role = await api.post("/api/v1/roles", {
      headers,
      data: { name: ROLE, description: "Runs the AI gateway; e2e", permissions: PERMISSIONS, default_for_new_users: false },
    });
    expect(role.status(), await role.text()).toBe(201);
    const user = await api.post("/api/v1/users", { headers, data: { email: EMAIL, password: PASSWORD, role: ROLE } });
    expect(user.status(), await user.text()).toBe(201);
    await ctx.close();
  });

  test.afterAll(async ({ browser }) => {
    const ctx = await browser.newContext({ storageState: "playwright-auth.json" });
    await cleanUp(await ctx.newPage());
    await ctx.close();
  });

  test("sees what the role opens, and makes a read-only token for an MCP client", async ({ browser, baseURL }) => {
    // A fresh context: no admin cookies.
    const ctx = await browser.newContext({ baseURL, storageState: { cookies: [], origins: [] } });
    const page = await ctx.newPage();
    const login = await page.request.post("/api/v1/auth/login", { data: { email: EMAIL, password: PASSWORD } });
    expect(login.ok(), await login.text()).toBe(true);

    // The AI Gateway is offered; Providers offers "New provider".
    await page.goto("/gateway/providers");
    const sidebar = page.locator("aside").first();
    await expect(sidebar.getByRole("navigation", { name: "Workspace" }).getByRole("link", { name: "AI Gateway", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "New provider" }).first()).toBeVisible();

    // Settings shows MCP and Automation tokens, not the pages the role does not open.
    await page.goto("/settings/mcp");
    const settings = page.getByRole("navigation", { name: "Settings" });
    await expect(settings.getByRole("link", { name: "MCP", exact: true })).toBeVisible();
    await expect(settings.getByRole("link", { name: "Automation tokens", exact: true })).toBeVisible();
    for (const hidden of ["General", "Users", "Roles", "Audit log", "Webhooks", "Backups"]) {
      await expect(settings.getByRole("link", { name: hidden, exact: true })).toHaveCount(0);
    }

    // A page the role does not open says which permission is missing.
    await page.goto("/settings/users");
    await expect(page.getByRole("heading", { name: "You cannot open this page" })).toBeVisible();
    await expect(page.getByText(/users:read/)).toBeVisible();
    await expect(page.getByText(/users:manage/)).toBeVisible();

    // The Services workspace offers neither Traffic (audit:read) nor Services (services:configure:*).
    await page.goto("/");
    const services = page.getByRole("navigation", { name: "Services" });
    await expect(services.getByRole("link", { name: "Overview", exact: true })).toBeVisible();
    for (const hidden of ["Traffic", "Services"]) {
      await expect(services.getByRole("link", { name: hidden, exact: true })).toHaveCount(0);
    }

    // Settings → MCP: the address and the Claude Code snippet.
    await page.goto("/settings/mcp");
    await expect(page.getByRole("heading", { name: "Address" })).toBeVisible();
    await expect(page.getByText(`${new URL(page.url()).origin}/mcp/burrow`, { exact: true })).toBeVisible();
    await expect(page.getByText(/claude mcp add --transport http burrow/)).toBeVisible();

    // "Create a read-only token" opens the token dialog with Read-only selected.
    await page.getByRole("link", { name: "Create a read-only token" }).click();
    const dlg = page.getByRole("dialog", { name: "New automation token" });
    await expect(dlg).toBeVisible();
    await expect(dlg.getByRole("radio", { name: "Read-only" })).toHaveAttribute("aria-checked", "true");
    await dlg.getByLabel("Name").fill(`mcp-e2e-${Date.now()}`);
    await dlg.getByRole("button", { name: "Create" }).click();

    // The token is shown once, with the way to an MCP client.
    const reveal = page.getByRole("dialog", { name: "New automation token" });
    await expect(reveal.getByText("Save this token now", { exact: false })).toBeVisible();
    await expect(reveal.locator("code").first()).toHaveText(/^bua_/);
    await expect(reveal.getByRole("link", { name: "Use it in an MCP client" })).toBeVisible();
    await reveal.getByRole("button", { name: "I've saved it" }).click();

    await ctx.close();
  });
});
