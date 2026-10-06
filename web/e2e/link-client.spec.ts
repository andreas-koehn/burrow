import { test, expect, type APIRequestContext } from "@playwright/test";

// /link — where `burrow login` sends the browser (real-stack Playwright spec).
//
// The sign-in requests are started the way the client starts them: an
// unauthenticated POST to /api/v1/client/login/start. The page is then driven
// in the browser, and the device code is polled as the client would poll it.

const E2E_EMAIL = "e2e@example.com";
const E2E_PASSWORD = "e2e-password-123";

interface Started { device_code: string; user_code: string; verification_url: string }

async function startSignIn(request: APIRequestContext, hostname: string, tokenName = ""): Promise<Started> {
  const res = await request.post("/api/v1/client/login/start", {
    data: { hostname, os: "linux", arch: "amd64", client_version: "0.0.0-e2e", token_name: tokenName },
  });
  expect(res.status(), await res.text()).toBe(200);
  return (await res.json()) as Started;
}

// The relay answers a poll at most every two seconds per device code.
async function poll(request: APIRequestContext, deviceCode: string) {
  return request.post("/api/v1/client/login/poll", { data: { device_code: deviceCode } });
}

test.describe("signed in", () => {
  // Use the globalSetup-cached admin session (web/e2e/global-setup.ts).
  test.use({ storageState: "playwright-auth.json" });

  test("link: a pending request shows the code and the machine, and Approve hands the client a token", async ({ page, request }) => {
    const hostile = `<img src=x onerror="window.__pwned=1"><b>box</b>`;
    const started = await startSignIn(request, hostile, "e2e-link-approve");
    expect(started.user_code).toMatch(/^[A-Z2-9]{4}-[A-Z2-9]{4}$/);
    expect(new URL(started.verification_url).pathname).toBe("/link");

    await page.goto(`/link?code=${started.user_code}`);
    await expect(page.getByRole("heading", { name: "Sign in a machine" })).toBeVisible();

    // The code is there to be compared with the terminal: large, and exactly the code.
    const code = page.locator(".linkpage-code");
    await expect(code).toHaveText(started.user_code);
    expect(await code.evaluate((el) => parseFloat(getComputedStyle(el).fontSize))).toBeGreaterThanOrEqual(28);

    // What the machine says about itself is text, whatever it holds.
    const reported = page.getByRole("group", { name: "Reported by the client" });
    await expect(reported).toContainText(hostile);
    await expect(reported).toContainText("linux");
    await expect(reported).toContainText("0.0.0-e2e");
    expect(await reported.locator("img, b, script").count()).toBe(0);
    expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();
    await expect(page.getByRole("group", { name: "Seen by the relay" })).toContainText("127.0.0.1");

    // Not a workspace page: no sidebar, no top bar.
    await expect(page.locator(".sidebar")).toHaveCount(0);
    await expect(page.getByRole("navigation")).toHaveCount(0);

    // Nothing was decided by opening the page.
    expect((await poll(request, started.device_code)).status()).toBe(202);

    await expect(page.getByLabel("Token name")).toHaveValue("e2e-link-approve");
    await page.getByRole("button", { name: "Approve" }).click();
    const result = page.getByRole("status").filter({ hasText: "Approved." });
    await expect(result).toBeVisible();
    await expect(result).toBeFocused();
    await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);

    // The client collects its token once; the code is then used up.
    await page.waitForTimeout(2100);
    const collected = await poll(request, started.device_code);
    expect(collected.status()).toBe(200);
    const body = (await collected.json()) as { token: string; token_name: string; email: string };
    expect(body.token).toMatch(/^bur_/);
    expect(body.token_name).toBe("e2e-link-approve");
    expect(body.email).toBe(E2E_EMAIL);
    await page.waitForTimeout(2100);
    expect((await poll(request, started.device_code)).status()).toBe(410);

    // The token is listed under Clients, tab Tokens, by its name.
    await page.getByRole("link", { name: "See your clients" }).click();
    await page.getByRole("tab", { name: "Tokens" }).click();
    await expect(page.getByText("e2e-link-approve")).toBeVisible();
  });

  test("link: Deny discards the request and the client is told", async ({ page, request }) => {
    const started = await startSignIn(request, "deny-me.example");
    await page.goto(`/link?code=${started.user_code}`);
    await expect(page.locator(".linkpage-code")).toHaveText(started.user_code);
    await page.getByRole("button", { name: "Deny" }).click();
    const result = page.getByRole("status").filter({ hasText: "Denied." });
    await expect(result).toBeVisible();
    await expect(result).toBeFocused();
    expect((await poll(request, started.device_code)).status()).toBe(403);
    // A decided code is like one that never existed.
    await page.goto(`/link?code=${started.user_code}`);
    await expect(page.getByRole("alert")).toContainText("This code is not valid or has expired");
    await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
  });

  test("link: a made-up code says so and offers no form; without a code the page asks for one", async ({ page }) => {
    await page.goto("/link?code=ABCD-EFGH");
    await expect(page.getByRole("alert")).toContainText("This code is not valid or has expired. Run burrow login again.");
    await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Deny" })).toHaveCount(0);
    await expect(page.getByRole("textbox")).toHaveCount(0);

    // Not even the shape of a code: nothing is asked of the relay.
    const asked: string[] = [];
    page.on("request", (r) => { if (r.url().includes("/client/login/requests/")) asked.push(r.url()); });
    await page.goto("/link?code=<script>1</script>");
    await expect(page.getByRole("alert")).toContainText("This code is not valid or has expired");
    expect(asked).toEqual([]);

    await page.getByRole("link", { name: "Enter another code" }).click();
    await expect(page).toHaveURL(/\/link$/);
    const field = page.getByLabel("Enter the code from your terminal");
    await expect(field).toBeFocused();
    await expect(page.getByRole("button", { name: "Continue" })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
  });

  test("link: keyboard only — the code field, Deny and Approve are reached and operated", async ({ page, request }) => {
    const started = await startSignIn(request, "keyboard.example", "e2e-link-keyboard");
    await page.goto("/link");
    // The page puts the focus in the field by itself; nothing is clicked.
    await expect(page.getByLabel("Enter the code from your terminal")).toBeFocused();
    // Small letters and no dash, as a person types it.
    await page.keyboard.type(started.user_code.replace("-", "").toLowerCase());
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/link\\?code=${started.user_code}$`));
    await expect(page.locator(".linkpage-code")).toHaveText(started.user_code);
    // The focus is on the request, not on a button: Enter decides nothing.
    await expect(page.getByRole("heading", { name: "Sign in a machine" })).toBeFocused();
    await page.keyboard.press("Enter");
    expect((await poll(request, started.device_code)).status()).toBe(202);

    await page.keyboard.press("Tab");
    await expect(page.getByLabel("Token name")).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(page.getByRole("button", { name: "Deny" })).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(page.getByRole("button", { name: "Approve" })).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(page.getByRole("status").filter({ hasText: "Approved." })).toBeFocused();
  });
});

test("link: signed out, the page leads through the login and back to the same code", async ({ page, request }) => {
  const started = await startSignIn(request, "after-login.example");
  await page.goto(`/link?code=${started.user_code}`);
  await expect(page).toHaveURL(/\/login/);
  await page.getByLabel("Email").fill(E2E_EMAIL);
  await page.getByLabel("Password").fill(E2E_PASSWORD);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/link\\?code=${started.user_code}$`));
  await expect(page.locator(".linkpage-code")).toHaveText(started.user_code);
  await expect(page.getByRole("group", { name: "Reported by the client" })).toContainText("after-login.example");
  await expect(page.getByRole("button", { name: "Approve" })).toBeEnabled();
  // Left pending: opening the page decided nothing.
  expect((await poll(request, started.device_code)).status()).toBe(202);
});
