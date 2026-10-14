// test-only — never deploy this shape.
import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./spec",
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  timeout: 90_000,
  expect: { timeout: 15_000 },
  reporter: process.env.CI ? "github" : "list",
  use: {
    // Kept in step with fixtures/env.ts (DASHBOARD_URL).
    baseURL: `http://localhost:${process.env.BURROW_E2E_DASHBOARD_PORT || "8080"}`,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
    ignoreHTTPSErrors: true,
  },
  projects: [
    {
      name: "mock",
      use: { ...devices["Desktop Chrome"] },
    },
    {
      name: "postgres",
      use: { ...devices["Desktop Chrome"] },
      // Run ONLY the Postgres-parity spec. NOTE: `grep` matches the
      // project-prefixed test title, so a `/postgres/` grep matched the
      // project NAME "postgres" on every test and ran the whole suite.
      // `testMatch` filters by file PATH, which is immune to that.
      testMatch: /19-postgres-swap\.spec\.ts$/,
    },
  ],
  outputDir: "./test-results",
});
