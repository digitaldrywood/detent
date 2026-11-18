const { defineConfig } = require("@playwright/test");

module.exports = defineConfig({
  testDir: "./tests/visual",
  outputDir: "./tmp/playwright-results",
  fullyParallel: true,
  workers: 4,
  maxFailures: process.env.CI ? 10 : 0,
  retries: process.env.CI ? 1 : 0,
  failOnFlakyTests: !!process.env.CI,
  timeout: 60_000,
  reporter: process.env.CI
    ? [
        ["github"],
        ["list"],
        ["html", { outputFolder: "tmp/playwright-report", open: "never" }],
      ]
    : [["list"], ["html", { outputFolder: "tmp/playwright-report", open: "never" }]],
  expect: {
    timeout: 10_000,
  },
  use: {
    browserName: "chromium",
    colorScheme: "light",
    deviceScaleFactor: 1,
    headless: true,
    locale: "en-US",
    timezoneId: "UTC",
    trace: "retain-on-failure",
    video: "retain-on-failure",
    viewport: { width: 1440, height: 1100 },
    reducedMotion: "reduce",
  },
  projects: [{ name: "chromium" }],
});
