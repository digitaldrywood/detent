const { defineConfig } = require("@playwright/test");

const compareSnapshots = process.platform === "linux" || process.env.DETENT_VISUAL_STRICT === "1";

module.exports = defineConfig({
  testDir: "./tests/visual",
  outputDir: "./tmp/playwright-results",
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  timeout: 60_000,
  ignoreSnapshots: !compareSnapshots,
  updateSnapshots: process.env.CI ? "none" : "missing",
  snapshotPathTemplate:
    "{testDir}/__screenshots__{/projectName}/{testFilePath}/{arg}{ext}",
  reporter: process.env.CI
    ? [
        ["github"],
        ["list"],
        ["html", { outputFolder: "tmp/playwright-report", open: "never" }],
      ]
    : [["list"], ["html", { outputFolder: "tmp/playwright-report", open: "never" }]],
  expect: {
    timeout: 10_000,
    toHaveScreenshot: {
      animations: "disabled",
      caret: "hide",
      maxDiffPixelRatio: 0.01,
      threshold: 0.2,
    },
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
  projects: [
    {
      name: "chromium",
    },
  ],
});
