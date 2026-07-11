const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("reports-digest", [
    "--demo",
    "screenshots",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("daily digest renders local days, deltas, and runtime metrics", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "reports-normal-window",
  });
  await page.goto(`${runtime.url}/reports`, { waitUntil: "domcontentloaded" });

  const digest = page.locator("#reports-daily-digest");
  await expect(digest).toBeVisible();
  await expect(page).toHaveURL(/(?:\?|&)tz=UTC(?:&|$)/);
  await expect(digest.locator("[data-digest-day]")).toHaveCount(7);
  await expect(digest.locator("[data-digest-day]").last()).toBeVisible();

  const today = digest.locator("[data-digest-day]").first();
  await expect(today).toContainText("Today");
  await expect(today.locator('[data-digest-metric="sessions"]')).toContainText(
    "Sessions",
  );
  await expect(today.locator('[data-digest-metric="tokens"]')).toContainText(
    "Tokens",
  );
  await expect(today.locator('[data-digest-metric="cache"]')).toContainText(
    "Cached share",
  );
  await expect(today.locator('[data-digest-metric="cost"]')).toContainText(
    "Estimated cost",
  );
  await expect(today.locator('[data-digest-metric="failures"]')).toContainText(
    "Failed sessions",
  );
  await expect(today.locator("[data-digest-trend]").first()).toContainText(
    /7d/,
  );
});
