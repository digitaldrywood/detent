const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("reports-cost-outcomes", [
    "--demo",
    "screenshots",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("renders per-project cost summaries and trend charts", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "reports-normal-window",
  });
  await page.goto(`${runtime.url}/reports?tz=UTC`, {
    waitUntil: "domcontentloaded",
  });

  const report = page.locator("#reports-cost-outcomes");
  await report.scrollIntoViewIfNeeded();
  await expect(report).toBeVisible();
  await expect(report.locator("[data-outcome-project]")).toHaveCount(2);

  const billing = report.locator('[data-outcome-project="billing-api"]');
  await expect(
    billing.locator('[data-outcome-metric="tokens-merged-pr"]'),
  ).toContainText("800.0K");
  await expect(
    billing.locator('[data-outcome-metric="spend-merged-pr"]'),
  ).toContainText("$6.00");
  await expect(
    billing.locator('[data-outcome-metric="tokens-closed-issue"]'),
  ).toContainText("600.0K");
  await expect(
    billing.locator('[data-outcome-metric="spend-closed-issue"]'),
  ).toContainText("$4.50");
  await expect(
    billing.locator('svg[aria-label="billing-api tokens per outcome trend"]'),
  ).toBeVisible();
  await expect(
    billing.locator(
      'svg[aria-label="billing-api notional USD per outcome trend"]',
    ),
  ).toBeVisible();
});

test("selects the 24 hour outcome window", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "reports-normal-window",
  });
  await page.goto(`${runtime.url}/reports?tz=UTC`, {
    waitUntil: "domcontentloaded",
  });

  await page.locator('[data-outcome-window="24h"]').click();
  await expect(page).toHaveURL(/outcome_window=24h/);
  await expect(page.locator('[data-outcome-window="24h"]')).toHaveAttribute(
    "aria-current",
    "true",
  );
  await expect(
    page.locator('[data-outcome-project="dogfood"] svg').first(),
  ).toBeVisible();
});
