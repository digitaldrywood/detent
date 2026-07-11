const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("local-time", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("capacity notices render once in each browser timezone after morphs", async ({
  browser,
}) => {
  const cases = [
    { timezoneId: "America/Chicago", expected: "12:44 PM", morphed: "1:44 PM" },
    { timezoneId: "America/Los_Angeles", expected: "10:44 AM", morphed: "11:44 AM" },
  ];

  for (const testCase of cases) {
    const context = await browser.newContext({
      locale: "en-US",
      timezoneId: testCase.timezoneId,
    });
    const page = await context.newPage();
    await page.setExtraHTTPHeaders({
      "X-Detent-Demo-Scenario": "backend-capacity-outage",
    });
    await page.goto(`${runtime.url}/health/ui`, { waitUntil: "domcontentloaded" });

    const banner = page.locator("#backend-capacity-outage");
    await expect(banner).toBeVisible();
    await expect(banner.getByText("Backend openai at usage limit")).toHaveCount(1);
    const resetTime = banner.locator("time[data-local-time]");
    await expect(resetTime).toHaveCount(1);
    await expect(resetTime).toContainText(testCase.expected);
    await expect(resetTime).not.toContainText("UTC");
    await expect(resetTime).toHaveAttribute("title", "2026-07-10T17:44:00.000Z");

    await resetTime.evaluate((element) => {
      element.setAttribute("datetime", "2026-07-10T18:44:00Z");
      element.textContent = "…";
      element.dispatchEvent(new CustomEvent("htmx:afterSettle", { bubbles: true }));
    });
    await expect(resetTime).toContainText(testCase.morphed);

    await page.setExtraHTTPHeaders({
      "X-Detent-Demo-Scenario": "diagnostics-rate-limit-pressure",
    });
    await page.goto(`${runtime.url}/projects/dogfood/diagnostics`, {
      waitUntil: "domcontentloaded",
    });
    await page.locator("#snapshot").waitFor({ state: "visible" });
    await expect(page.locator("#snapshot time[data-local-time]").first()).toBeVisible();
    await expect(page.locator("#snapshot")).not.toContainText("{{detent-time:");
    await expect(page.locator("#snapshot")).not.toContainText("UTC");
    await context.close();
  }
});
