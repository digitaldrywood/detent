const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("admission-proposals", [
    "--demo",
    "screenshots",
    "--demo-clock",
    "play",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("zero open proposals render no review queue condition", async ({ page }) => {
  await openScenario(page, "board-admission-proposals-zero", "/");
  await expect(page.locator("#board-alerts")).toHaveCount(0);

  await page.goto(`${runtime.url}/diagnostics`, { waitUntil: "domcontentloaded" });
  await expect(page.locator('[id^="diagnostics-condition-admission-"]')).toHaveCount(0);
});

test("one open proposal stays off the banner and remains linked in Diagnostics", async ({ page }) => {
  await openScenario(page, "board-admission-proposals-one", "/");
  await expect(page.locator("#board-alerts")).toHaveCount(0);

  await page.goto(`${runtime.url}/diagnostics`, { waitUntil: "domcontentloaded" });
  const condition = page.locator('[id^="diagnostics-condition-admission-"]');
  await expect(condition).toHaveCount(1);
  await expect(condition).toHaveAttribute("data-diagnostics-condition-class", "review_queue");
  await expect(condition).toContainText("Admission decision");
  await expect(condition.getByRole("link", { name: "digitaldrywood/detent#1586" })).toHaveAttribute(
    "href",
    "https://github.com/digitaldrywood/detent/issues/1586",
  );
});

test("several proposals stay off the banner and appear per project in Diagnostics", async ({ page }) => {
  await openScenario(page, "board-admission-proposals-several", "/");
  await expect(page.locator("#board-alerts")).toHaveCount(0);

  await page.goto(`${runtime.url}/diagnostics`, { waitUntil: "domcontentloaded" });
  const conditions = page.locator('[id^="diagnostics-condition-admission-"]');
  await expect(conditions).toHaveCount(3);
  await expect(conditions.filter({ hasText: "dogfood" })).toHaveCount(2);
  await expect(conditions.filter({ hasText: "docs-site" })).toHaveCount(1);
  for (const identifier of [
    "digitaldrywood/detent#1586",
    "digitaldrywood/detent#1594",
    "digitaldrywood/docs-site#241",
  ]) {
    await expect(conditions.filter({ hasText: identifier })).toHaveCount(1);
  }
});

async function openScenario(page, scenario, route) {
  await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": scenario });
  await page.goto(`${runtime.url}${route}`, { waitUntil: "domcontentloaded" });
  await page.locator("#snapshot").waitFor({ state: "visible" });
}
