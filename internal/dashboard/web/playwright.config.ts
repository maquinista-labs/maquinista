import { defineConfig, devices } from "@playwright/test";

/**
 * Playwright config for the maquinista dashboard.
 *
 * Tests boot the real maquinista binary via the global-setup hook
 * in tests/e2e/support/global-setup.ts. The process is started once
 * per test run (the supervisor + Next cold-start is ~500 ms warm),
 * and each spec gets a fresh Postgres fixture via the dbtest
 * container in later phases.
 */
export default defineConfig({
  testDir: "tests/e2e",
  timeout: 60_000,
  expect: { timeout: 10_000 },
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: 1, // the Go binary is a shared listener; serialise for now
  reporter: process.env.CI
    ? [["line"], ["html", { open: "never" }]]
    : [["list"]],
  use: {
    baseURL: process.env.MAQUINISTA_DASHBOARD_URL ?? "http://127.0.0.1:3100",
    trace: "retain-on-failure",
    video: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  globalSetup: "./tests/e2e/support/global-setup.ts",
  globalTeardown: "./tests/e2e/support/global-teardown.ts",
  projects: [
    {
      name: "chromium-desktop",
      use: { ...devices["Desktop Chrome"] },
    },
    {
      name: "mobile-safari",
      // iPhone 14 Pro Max viewport — catches most mobile Safari
      // quirks that hit operators on phone hardware.
      use: { ...devices["iPhone 14 Pro Max"] },
    },
  ],
});
