import { createHash, randomBytes } from "node:crypto";
import { test, expect, type Page } from "@playwright/test";
import { answerReauth, guardedRequest, ADMIN_PASSWORD } from "./reauth";

// MCP gateway (T14): the Tools pages, the tools section of a gateway key,
// the OAuth consent page and Connected apps, each driven through the
// dashboard against the real relay. The relay has no services domain, so the
// re-authentication dialog and the consent page's password field are part of
// the flows. Every object the spec creates is removed again at the end.

test.use({ storageState: "playwright-auth.json" });
test.describe.configure({ mode: "serial" });

const SOURCE = "e2e-mcp-local";
const KEY_NAME = "e2e-mcp-key";
const CLIENT_NAME = "e2e MCP client";
// A loopback redirect on a port nothing listens on: the browser's trip there
// is answered by page.route.
const REDIRECT = "http://127.0.0.1:9/cb";

let clientId = "";
let keyId = "";

function pkce() {
  const verifier = randomBytes(32).toString("base64url");
  const challenge = createHash("sha256").update(verifier).digest("base64url");
  return { verifier, challenge };
}

async function cleanup(page: Page) {
  const del = (url: string) => guardedRequest(page, "DELETE", url);
  const grants = await page.request.get("/api/v1/oauth/grants");
  if (grants.ok()) {
    for (const g of (await grants.json()) as { id: string; client_name: string }[]) {
      if (g.client_name === CLIENT_NAME) await del(`/api/v1/oauth/grants/${g.id}`);
    }
  }
  const clients = await page.request.get("/api/v1/oauth/clients");
  if (clients.ok()) {
    for (const c of (await clients.json()) as { client_id: string; name: string }[]) {
      if (c.name === CLIENT_NAME) await del(`/api/v1/oauth/clients/${c.client_id}`);
    }
  }
  const keys = await page.request.get("/api/v1/ai/keys");
  if (keys.ok()) {
    for (const k of (await keys.json()) as { id: string; name: string }[]) {
      if (k.name.startsWith(KEY_NAME)) await del(`/api/v1/ai/keys/${k.id}`);
    }
  }
  await del(`/api/v1/mcp/sources/${SOURCE}`);
  await del(`/api/v1/mcp/sources/e2e-mcp-remote`);
  await del(`/api/v1/services/${SOURCE}`);
}

test.beforeAll(async ({ browser }) => {
  const context = await browser.newContext({ storageState: "playwright-auth.json" });
  const page = await context.newPage();
  await cleanup(page);
  await context.close();
});

test.afterAll(async ({ browser }) => {
  const context = await browser.newContext({ storageState: "playwright-auth.json" });
  const page = await context.newPage();
  await cleanup(page);
  await context.close();
});

test("adds a local tool source and shows the burrow mcp command", async ({ page }) => {
  await page.goto("/gateway/tools");
  await expect(page.getByRole("heading", { name: "Tools", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Add local source" }).click();
  const dialog = page.getByRole("dialog", { name: "Add local source" });
  await dialog.getByLabel("Name").fill(SOURCE);
  await dialog.getByLabel("Command or address").fill("npx -y @modelcontextprotocol/server-filesystem /srv");
  await dialog.getByRole("button", { name: "Continue" }).click();
  await answerReauth(page, dialog.getByRole("heading", { name: "Run this on the machine" }));
  await expect(dialog.getByText(`burrow mcp ${SOURCE} -- npx -y @modelcontextprotocol/server-filesystem /srv`).first()).toBeVisible();
  await expect(dialog.getByText("Waiting for the client…")).toBeVisible();
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(page).toHaveURL(new RegExp(`/gateway/tools/${SOURCE}$`));

  await page.goto("/gateway/tools");
  const row = page.getByRole("table", { name: "Tool sources" }).getByRole("row", { name: new RegExp(SOURCE) });
  await expect(row).toBeVisible();
  await expect(row.getByText("On a machine")).toBeVisible();
});

test("source detail: address for agents and limits that are kept", async ({ page }) => {
  await page.goto(`/gateway/tools/${SOURCE}`);
  await expect(page.getByRole("heading", { name: `Tool source · ${SOURCE}` })).toBeVisible();
  const source = page.getByRole("region", { name: "Source" });
  await expect(source.getByText(new RegExp(`/mcp/${SOURCE}$`)).first()).toBeVisible();
  const limits = page.getByRole("region", { name: "Limits" });
  await limits.getByLabel("Calls per minute").fill("30");
  await limits.getByRole("button", { name: "Save limits" }).click();
  await answerReauth(page, limits.getByRole("button", { name: "Save limits" }));
  await expect.poll(async () => {
    const res = await page.request.get(`/api/v1/mcp/sources/${SOURCE}`);
    return ((await res.json()) as { rpm: number }).rpm;
  }).toBe(30);
  await page.reload();
  await expect(page.getByRole("region", { name: "Limits" }).getByLabel("Calls per minute")).toHaveValue("30");
});

test("a remote source must be https and is refused on a private address", async ({ page }) => {
  await page.goto("/gateway/tools");
  await page.getByRole("button", { name: "Add remote source" }).click();
  const dialog = page.getByRole("dialog", { name: "Add remote source" });
  await dialog.getByLabel("Name").fill("e2e-mcp-remote");
  await dialog.getByLabel("URL").fill("http://mcp.example.com/mcp");
  await dialog.getByLabel("URL").blur();
  await expect(dialog.getByText("The address must start with https://.")).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Add source" })).toBeDisabled();

  await dialog.getByLabel("URL").fill("https://127.0.0.1:1/mcp");
  await dialog.getByRole("button", { name: "Add source" }).click();
  await answerReauth(page, dialog.getByRole("alert").first());
  await expect(dialog).toBeVisible();
  expect((await page.request.get("/api/v1/mcp/sources/e2e-mcp-remote")).status()).toBe(404);
  await dialog.getByRole("button", { name: "Cancel" }).click();
});

test("a new gateway key gets the source's tools and read-only, then the edit dialog changes them", async ({ page }) => {
  await page.goto("/gateway/keys");
  await page.getByRole("button", { name: "New key" }).first().click();
  const dialog = page.getByRole("dialog", { name: "New gateway key" });
  await dialog.getByLabel("Name").fill(KEY_NAME);
  await dialog.getByRole("button", { name: "Tools" }).click();
  await dialog.getByRole("combobox", { name: "Tool source" }).click();
  await page.getByRole("option", { name: SOURCE, exact: true }).click();
  await dialog.getByRole("button", { name: "Add all tools of source" }).click();
  await expect(dialog.getByRole("list", { name: "Allowed tools" }).getByText(`${SOURCE}__*`)).toBeVisible();
  const ro = dialog.getByRole("switch", { name: "Read-only tools only" });
  if (!(await ro.isChecked())) await ro.click();
  await dialog.getByRole("button", { name: "Create key" }).click();
  await answerReauth(page, dialog.getByLabel("Your new key"));
  await expect(dialog.getByLabel("Your new key")).toHaveValue(/^bgw_/);
  await dialog.getByRole("button", { name: "Done" }).click();

  const keys = (await (await page.request.get("/api/v1/ai/keys")).json()) as { id: string; name: string; allowed_tools: string[]; tools_read_only: boolean }[];
  const key = keys.find((k) => k.name === KEY_NAME);
  expect(key).toBeTruthy();
  keyId = key!.id;
  expect(key!.allowed_tools).toEqual([`${SOURCE}__*`]);
  expect(key!.tools_read_only).toBe(true);

  await page.getByRole("button", { name: `Edit ${KEY_NAME}` }).click();
  const edit = page.getByRole("dialog", { name: `Edit ${KEY_NAME}` });
  await edit.getByRole("button", { name: `Remove ${SOURCE}__*` }).click();
  await edit.getByLabel("Tool pattern").fill(`${SOURCE}__echo`);
  await edit.getByRole("button", { name: "Add pattern" }).click();
  await edit.getByRole("switch", { name: "Read-only tools only" }).click();
  await edit.getByRole("button", { name: "Save changes" }).click();
  await answerReauth(page, page.getByRole("table", { name: "Gateway keys" }));
  await expect(edit).toBeHidden();
  await expect.poll(async () => {
    const all = (await (await page.request.get("/api/v1/ai/keys")).json()) as { id: string; allowed_tools: string[]; tools_read_only: boolean }[];
    const k = all.find((x) => x.id === keyId)!;
    return [k.allowed_tools, k.tools_read_only];
  }).toEqual([[`${SOURCE}__echo`], false]);
});

async function registerClient(page: Page) {
  if (clientId) return clientId;
  const res = await guardedRequest(page, "POST", "/api/v1/oauth/clients", { name: CLIENT_NAME, redirect_uris: [REDIRECT] });
  expect(res.status(), await res.text()).toBe(201);
  clientId = ((await res.json()) as { client_id: string }).client_id;
  return clientId;
}

/** Runs /oauth/authorize in the browser, approves on the consent page, and exchanges the code. */
async function connect(page: Page, resourcePath: string, onConsent: (page: Page) => Promise<void>) {
  const id = await registerClient(page);
  const origin = new URL(page.url() === "about:blank" ? "http://127.0.0.1:8723" : page.url()).origin;
  const resource = `${origin}${resourcePath}`;
  const { verifier, challenge } = pkce();
  const state = randomBytes(8).toString("hex");
  let landed = "";
  await page.route(`${REDIRECT}**`, async (route) => {
    landed = route.request().url();
    await route.fulfill({ status: 200, contentType: "text/html", body: "<title>back in the app</title><p>done</p>" });
  });
  const q = new URLSearchParams({
    response_type: "code", client_id: id, redirect_uri: REDIRECT, code_challenge: challenge,
    code_challenge_method: "S256", resource, state,
  });
  await page.goto(`/oauth/authorize?${q}`);
  await expect(page).toHaveURL(/\/oauth\/consent\?request=/);
  await expect(page.getByRole("heading", { name: `${CLIENT_NAME} wants to connect` })).toBeVisible();
  await expect(page.locator("strong.consent-host")).toHaveText("127.0.0.1");
  await expect(page.getByRole("note").filter({ hasText: "only this machine" })).toBeVisible();
  await onConsent(page);
  // No services domain: the password goes with the decision.
  const approve = page.getByRole("button", { name: "Approve" });
  await expect(approve).toBeDisabled();
  await page.getByLabel("Your password").fill(ADMIN_PASSWORD);
  await approve.click();
  await expect(page).toHaveTitle("back in the app");
  await page.unroute(`${REDIRECT}**`);
  const back = new URL(landed);
  expect(back.searchParams.get("state")).toBe(state);
  expect(back.searchParams.get("iss")).toBe(origin);
  const code = back.searchParams.get("code") ?? "";
  expect(code).not.toBe("");

  const token = await page.request.post("/oauth/token", {
    form: { grant_type: "authorization_code", code, redirect_uri: REDIRECT, client_id: id, code_verifier: verifier, resource },
  });
  expect(token.status(), await token.text()).toBe(200);
  const body = (await token.json()) as { access_token: string; refresh_token: string };
  expect(body.access_token).toMatch(/^boa_/);
  return { origin, access: body.access_token };
}

async function mcpInitialize(page: Page, path: string, access: string) {
  return page.request.post(path, {
    headers: { Authorization: `Bearer ${access}`, Accept: "application/json, text/event-stream", "Content-Type": "application/json" },
    data: { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "e2e", version: "1" } } },
  });
}

let burrowAccess = "";

test("consent for /mcp/burrow asks to read only, offers no write switch, and connects", async ({ page }) => {
  await page.goto("/");
  const { access } = await connect(page, "/mcp/burrow", async (p) => {
    await expect(p.getByText("Burrow management")).toBeVisible();
    await expect(p.getByRole("switch")).toHaveCount(0);
  });
  burrowAccess = access;
  expect((await mcpInitialize(page, "/mcp/burrow", access)).status()).toBe(200);
  // The token belongs to /mcp/burrow only.
  expect((await mcpInitialize(page, "/mcp", access)).status()).toBe(401);
});

test("consent for /mcp lists the source with read-only on, and Connected apps shows what was granted", async ({ page }) => {
  await page.goto("/");
  await connect(page, "/mcp", async (p) => {
    await expect(p.getByText("All tool sources")).toBeVisible();
    await expect(p.getByRole("checkbox", { name: new RegExp(SOURCE) })).toBeChecked();
    await expect(p.getByRole("switch", { name: "Read-only tools only" })).toBeChecked();
  });
  await page.goto("/settings/apps");
  const table = page.getByRole("table", { name: "Connected apps" });
  await expect(table.getByRole("row", { name: new RegExp(CLIENT_NAME) })).toHaveCount(2);
  await expect(table.getByText(`${SOURCE}__*, read-only tools only`)).toBeVisible();
  await expect(table.getByRole("cell", { name: "Read-only", exact: true })).toBeVisible();
});

test("revoking a connected app stops its token at once", async ({ page }) => {
  await page.goto("/settings/apps");
  const table = page.getByRole("table", { name: "Connected apps" });
  const row = table.getByRole("row").filter({ hasText: "Burrow management" }).filter({ hasText: CLIENT_NAME });
  await row.getByRole("button", { name: `Revoke ${CLIENT_NAME}` }).click();
  const dialog = page.getByRole("dialog", { name: `Revoke ${CLIENT_NAME}` });
  await dialog.getByRole("button", { name: "Revoke", exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(table.getByRole("row", { name: new RegExp(CLIENT_NAME) })).toHaveCount(1);
  const res = await mcpInitialize(page, "/mcp/burrow", burrowAccess);
  expect(res.status()).toBe(401);
  expect(res.headers()["www-authenticate"] ?? "").toContain('error="invalid_token"');
});

test("a fresh session is asked for the password before a key is minted", async ({ browser }, info) => {
  const ctx = await browser.newContext({ baseURL: info.project.use.baseURL, storageState: { cookies: [], origins: [] } });
  const login = await ctx.request.post("/api/v1/auth/login", { data: { email: "e2e@example.com", password: ADMIN_PASSWORD } });
  expect(login.status()).toBe(200);
  const page = await ctx.newPage();
  await page.goto("/gateway/keys");
  await page.getByRole("button", { name: "New key" }).first().click();
  const dialog = page.getByRole("dialog", { name: "New gateway key" });
  await dialog.getByLabel("Name").fill(`${KEY_NAME}-fresh`);
  await dialog.getByRole("button", { name: "Create key" }).click();
  const reauth = page.getByRole("dialog", { name: "Confirm your password" });
  await expect(reauth).toBeVisible();
  await expect(reauth).toContainText("asks for your password again");
  await reauth.getByLabel("Your password").fill(ADMIN_PASSWORD);
  await reauth.getByRole("button", { name: "Confirm" }).click();
  await expect(dialog.getByLabel("Your new key")).toHaveValue(/^bgw_/);
  await ctx.close();
});

test("deletes the tool source after a confirmation", async ({ page }) => {
  await page.goto(`/gateway/tools/${SOURCE}`);
  await page.getByRole("button", { name: "Delete source" }).click();
  const dialog = page.getByRole("dialog", { name: `Delete source · ${SOURCE}` });
  await expect(dialog).toContainText(`/mcp/${SOURCE} stops working at once`);
  await dialog.getByRole("button", { name: "Delete source" }).click();
  await answerReauth(page, page.getByRole("table", { name: "Tool sources" }).or(page.getByText("No tool sources yet")).first());
  expect((await page.request.get(`/api/v1/mcp/sources/${SOURCE}`)).status()).toBe(404);
});
