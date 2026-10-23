import { test, expect, type Page } from "@playwright/test";
import { execSync, spawn, type ChildProcess } from "node:child_process";
import * as fs from "node:fs";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { answerReauth, guardedRequest } from "./reauth";

// The tcp service page against a real client: a `burrow tcp` client serves a
// local echo server in mode ip. The spec picks a mode, allows its own address
// for an hour and sees a visitor in the live connections. The relay of the
// suite listens for clients on :7000 with a dev certificate, so the client
// runs with --insecure (tcp services allow it; forwards would not).

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const NAME = "e2e-tcp-echo";
const TOKEN_NAME = "e2e-tcp-client";

test.describe("tcp service page", () => {
  test.use({ storageState: "playwright-auth.json" });
  test.describe.configure({ timeout: 240_000 });

  let dir = "";
  let echo: net.Server | null = null;
  let client: ChildProcess | null = null;
  let clientLog = "";
  let visitor: net.Socket | null = null;

  test.beforeAll(() => {
    dir = fs.mkdtempSync(path.join(os.tmpdir(), "burrow-e2e-tcp-"));
    // The suite's relay is built the same way (run-server.mjs).
    execSync(`go build -o "${path.join(dir, "burrow")}" ./cmd/client`, { cwd: repoRoot, stdio: "inherit" });
  });

  test.afterAll(async () => {
    visitor?.destroy();
    client?.kill("SIGTERM");
    await new Promise<void>((r) => (echo ? echo.close(() => r()) : r()));
    if (dir) fs.rmSync(dir, { recursive: true, force: true });
  });

  async function cleanup(page: Page) {
    const services = (await (await page.request.get("/api/v1/services")).json()) as { id: string; name: string; type: string }[];
    for (const s of services.filter((x) => x.name === NAME && x.type === "tcp")) {
      await guardedRequest(page, "DELETE", `/api/v1/services/${s.id}`);
    }
    const tokens = (await (await page.request.get("/api/v1/tokens")).json()) as { id: string; name: string }[];
    for (const t of tokens.filter((x) => x.name === TOKEN_NAME)) {
      await guardedRequest(page, "DELETE", `/api/v1/tokens/${t.id}`);
    }
  }

  test("mode picker, allow my IP and a live visitor", async ({ page }) => {
    await page.goto("/services");
    await cleanup(page);

    // A local echo server stands in for the service at home.
    echo = net.createServer((s) => s.pipe(s));
    await new Promise<void>((r) => echo!.listen(0, "127.0.0.1", () => r()));
    const echoPort = (echo.address() as net.AddressInfo).port;

    const created = await guardedRequest(page, "POST", "/api/v1/tokens", { name: TOKEN_NAME });
    expect(created.status(), await created.text()).toBe(201);
    const { token } = (await created.json()) as { token: string };
    const cfg = path.join(dir, "config.yaml");
    fs.writeFileSync(cfg, `relay: https://127.0.0.1:8723\ncontrol: 127.0.0.1:7000\ntoken: ${token}\ntoken_name: ${TOKEN_NAME}\n`, { mode: 0o600 });

    client = spawn(path.join(dir, "burrow"), [
      "--config", cfg, "--insecure", "--log", "text",
      "tcp", `127.0.0.1:${echoPort}`, "--name", NAME, "--access", "ip",
    ], { stdio: ["ignore", "pipe", "pipe"] });
    client.stdout?.on("data", (b: Buffer) => { clientLog += b.toString(); });
    client.stderr?.on("data", (b: Buffer) => { clientLog += b.toString(); });

    // The client registers the service in mode ip on a public port.
    let svc: { id: string; remote_port: number; access_mode: string; connected: boolean } | undefined;
    await expect.poll(async () => {
      const list = (await (await page.request.get("/api/v1/services")).json()) as (typeof svc & { name: string; type: string })[];
      svc = list.find((s) => s!.name === NAME && s!.type === "tcp");
      return svc?.connected && svc.remote_port > 0 ? svc.access_mode : `not yet: ${clientLog.slice(-300)}`;
    }, { timeout: 30_000 }).toBe("ip");

    await page.goto(`/services/${svc!.id}`);

    // Mode picker: three modes, IP allowlist chosen; Open warns in red.
    const modes = page.getByRole("radiogroup", { name: "Access mode" });
    await expect(modes.getByRole("radio")).toHaveCount(3);
    await expect(modes.getByRole("radio", { name: /IP allowlist/ })).toHaveAttribute("aria-checked", "true");
    await modes.getByRole("radio", { name: /^Open/ }).click();
    await expect(page.getByText(/Anyone on the internet can connect to this port/)).toBeVisible();
    await expect(page.getByText(/needs the client to be restarted with --access open/)).toBeVisible();
    await modes.getByRole("radio", { name: /IP allowlist/ }).click();
    await expect(page.getByRole("button", { name: "Save mode" })).toBeDisabled();

    // The connect card names the public port.
    const connect = page.getByRole("region", { name: "Connect", exact: true });
    await expect(connect).toContainText(`:${svc!.remote_port}`);

    // Allow my IP for 1 h: the browser's address is 127.0.0.1.
    const grants = page.getByRole("region", { name: "Allow my address", exact: true });
    await grants.getByRole("button", { name: "Allow my IP for 1 h" }).click();
    const active = grants.getByRole("list", { name: "Active grants" });
    await answerReauth(page, active.getByText("127.0.0.1/32"));

    // A visitor from 127.0.0.1 gets through to the echo server and stays connected.
    visitor = net.connect(svc!.remote_port, "127.0.0.1");
    const echoed = await new Promise<string>((resolve, reject) => {
      visitor!.once("error", reject);
      visitor!.once("data", (b) => resolve(b.toString()));
      visitor!.write("hello burrow");
    });
    expect(echoed).toBe("hello burrow");

    const live = page.getByRole("region", { name: "Live connections", exact: true });
    const row = live.getByRole("row").filter({ hasText: "127.0.0.1" });
    await expect(row).toContainText("Visitor", { timeout: 20_000 });

    // Disconnect it from the page: the visitor's socket ends.
    const ended = new Promise<void>((r) => visitor!.once("close", () => r()));
    await row.getByRole("button", { name: /^Disconnect/ }).click();
    const dialog = page.getByRole("dialog", { name: /^Disconnect/ });
    await dialog.getByRole("button", { name: "Disconnect" }).click();
    await answerReauth(page, live.getByText("No connection is open right now."));
    await ended;

    // Revoke the grant again, then a stricter mode: the client reconnects in it.
    await active.getByRole("button", { name: "Revoke 127.0.0.1/32" }).click();
    await page.getByRole("dialog").getByRole("button", { name: /^Revoke/ }).click();
    await answerReauth(page, grants.getByText(/No grants right now/));

    await modes.getByRole("radio", { name: /^Private/ }).click();
    await expect(page.getByText(/A stricter mode applies when the client reconnects/)).toBeVisible();
    await page.getByRole("button", { name: "Save mode" }).click();
    await answerReauth(page, connect.getByText(/no public port/));
    await expect(connect).toContainText("burrow forward");
    await expect.poll(async () => {
      const s = (await (await page.request.get(`/api/v1/services/${svc!.id}`)).json()) as { connected: boolean; remote_port: number; access_mode: string };
      return `${s.access_mode} ${s.connected} ${s.remote_port}`;
    }, { timeout: 30_000 }).toBe("private true 0");

    client.kill("SIGTERM");
    client = null;
    await cleanup(page);
  });
});
