// test-only — never deploy this shape.
//
// Spec 33 — a UI-minted token connects a REAL client end-to-end.
// Mints a client token via the Connect-a-client wizard UI, docker-runs a
// throwaway burrow client with that exact token tunneling to mockoai:8081,
// then proves the tunnel registers connected and a proxied request reaches
// the upstream. Cleans up the container in finally.
import { test, expect } from "@playwright/test";
import { spawnSync } from "node:child_process";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";
import { HTTPS_INGRESS } from "../../fixtures/env";

test.use({ storageState: AUTH_STORAGE_PATH });
test.slow();

const NETWORK = "burrow-e2e-full_e2e";
const CLIENT_IMAGE = "burrow-e2e-client-ai:dev";
const EPHEMERAL = `burrow-e2e-ephemeral-${Date.now().toString().slice(-6)}`;
const TUNNEL_NAME = `ephem-${Date.now().toString().slice(-6)}`;

test("33-token-connect: UI-minted token connects a real client + traffic flows", async ({ page, request }) => {
  // 1. Mint a token via the Connect-a-client wizard UI (reuse spec 22 selectors).
  await page.goto("/clients/connect");

  // Fill the client name and generate a token.
  await page.getByLabel("Client name").fill(TUNNEL_NAME);
  await page.getByRole("button", { name: /generate token/i }).click();

  // Click Reveal to unmask the token (default is masked as bur_••••••••).
  await page.getByRole("button", { name: /reveal token/i }).click();

  // Find the code element with the plaintext bur_ token (not the command block).
  const tokenCode = page.locator("code.mono", { hasText: /^bur_[A-Za-z0-9_-]{20,}$/ });
  await expect(tokenCode).toBeVisible({ timeout: 10_000 });
  const token = (await tokenCode.textContent()) ?? "";
  expect(token).toMatch(/^bur_/);

  let started = false;
  try {
    // 2. docker run the ephemeral client with that exact token.
    // The wizard token is generic (POST /tokens with just a name) — not service-scoped.
    // The wizard example command uses --local 127.0.0.1:3000 --remote 9000 as
    // placeholders; we override --local to mockoai:8081 (reachable in-network)
    // and --remote 0 (auto-assign) so the relay creates the tunnel entry with
    // the TUNNEL_NAME we can discover via /api/v1/services.
    // Override the entrypoint (which normally waits for a token file) so we can
    // pass the token directly on the command line.
    const run = spawnSync(
      "docker",
      [
        "run", "-d", "--rm", "--name", EPHEMERAL, "--network", NETWORK,
        "--entrypoint", "/usr/local/bin/burrow",
        CLIENT_IMAGE,
        "connect",
        "--server", "relay.test.local:7000",
        "--token", token,
        "--local", "mockoai:8081",
        "--remote", "0",
        "--name", TUNNEL_NAME,
        "--type", "http",
        "--insecure",
      ],
      { encoding: "utf8" },
    );
    if (run.status !== 0) {
      throw new Error(`docker run failed: ${run.stdout}\n${run.stderr}`);
    }
    started = true;

    // 3. Poll the API until the ephemeral tunnel registers connected; capture its subdomain.
    // The relay assigns a random subdomain when the http-type client connects.
    // /api/v1/services returns {name, slug, connected, ...} per tunnel/service.
    let subdomain = "";
    for (let i = 0; i < 30; i++) {
      const r = await request.get("/api/v1/services", { headers: adminHeaders() });
      const svcs = (await r.json()) as {
        name: string;
        slug: string;
        connected: boolean;
      }[];
      const mine = svcs.find((s) => s.name === TUNNEL_NAME && s.connected && s.slug);
      if (mine) {
        subdomain = mine.slug;
        break;
      }
      await new Promise((res) => setTimeout(res, 1000));
    }
    expect(subdomain, "ephemeral tunnel should register connected with a subdomain").not.toBe("");

    // 4. Traffic through the HTTPS proxy to the ephemeral subdomain reaches mockoai.
    // HTTP tunnels are host-routed on :8443 — send the Host header to target the tunnel.
    const resp = await request.get(`${HTTPS_INGRESS}/healthz`, {
      headers: { host: `${subdomain}.test.local:8443` },
      ignoreHTTPSErrors: true,
    });
    expect(resp.status()).toBe(200);
  } finally {
    if (started) {
      spawnSync("docker", ["rm", "-f", EPHEMERAL], { encoding: "utf8" });
    }
  }
});
