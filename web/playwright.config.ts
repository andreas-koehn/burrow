import { defineConfig, devices } from "@playwright/test";
import * as os from "os";
import * as path from "path";
import * as fs from "fs";
import { fileURLToPath } from "url";

const _dirname = path.dirname(fileURLToPath(import.meta.url));

// Ephemeral directory for the Go binary, dev-certs, and the SQLite database.
// Created once when the config module is loaded; the same path is passed to
// the webServer child via E2E_TMPDIR so the binary lands there and dev-certs
// are written to <tmpDir>/certs (not into the repo working tree).
const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "burrow-e2e-"));

const HTTP_PORT = 8723;
const HTTP_LISTEN = `127.0.0.1:${HTTP_PORT}`;

export default defineConfig({
  testDir: "./e2e",
  // Run the single spec serially; no parallelism needed for a smoke test.
  workers: 1,
  // Generous timeout: the webServer command must finish a `go build` first.
  timeout: 120_000,
  expect: { timeout: 15_000 },

  // v0.4.0: globalSetup logs in ONCE (via the JSON API) and persists session
  // cookies to playwright-auth.json. Specs that use the seeded admin opt in
  // via test.use({ storageState: "playwright-auth.json" }) — collapsing
  // ~30 logins down to one and keeping the suite under the per-IP rate
  // limit (LoginRateLimitPerIP=10/min in internal/api/deps.go). Specs that
  // need to exercise the login UI (smoke, admin-v0.2.0) or anonymity (the
  // 401 contract assertions) deliberately skip the storageState opt-in.
  globalSetup: "./e2e/global-setup.ts",

  use: {
    baseURL: `http://${HTTP_LISTEN}`,
    // Capture traces on first retry to aid CI debugging.
    trace: "on-first-retry",
  },

  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],

  webServer: {
    // Cross-platform Node.js launcher: builds burrowd then boots the server.
    // Works on both Ubuntu CI and Windows dev machines.
    command: "node web/e2e/run-server.mjs",
    cwd: path.join(_dirname, ".."),
    url: `http://${HTTP_LISTEN}`,
    // In CI always start a fresh server; locally reuse if already running.
    reuseExistingServer: !process.env.CI,
    // Allow up to 90 s for `go build` + boot.
    timeout: 90_000,
    env: {
      BURROW_ADMIN_EMAIL: "e2e@example.com",
      BURROW_ADMIN_PASSWORD: "e2e-password-123",
      BURROW_HTTP_LISTEN: HTTP_LISTEN,
      // Sign-in, password confirmations (POST /auth/reauth, T02b) and the
      // password-bearing approvals of /link and the OAuth consent page share
      // the per-address sign-in limit (10/min by default). The suite signs in
      // and confirms more often than that within a minute.
      BURROW_LOGIN_RATE_LIMIT_PER_IP: "30",
      BURROW_DATABASE_PATH: path.join(tmpDir, "e2e.db"),
      E2E_TMPDIR: tmpDir,
    },
  },
});
