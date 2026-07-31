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

test("zero open proposals render no admission attention state", async ({ page }) => {
  await openScenario(page, "board-admission-proposals-zero", "/");
  await expect(page.locator('[data-board-alert="admission-proposal"]')).toHaveCount(0);

  await page.goto(`${runtime.url}/health/ui`, { waitUntil: "domcontentloaded" });
  await expect(page.locator('[id^="health-admission-proposals-"]')).toHaveCount(0);
});

test("one open proposal renders a linked compact indicator", async ({ page }) => {
  await openScenario(page, "board-admission-proposals-one", "/");

  const indicator = page.locator("#board-alerts");
  await expect(indicator).toContainText("1 admission proposal awaiting decision");
  await indicator.getByRole("button").click();
  const overlay = page.locator("#board-alerts-overlay");
  await expect(overlay).toBeVisible();
  await expect(overlay.getByRole("link", { name: "digitaldrywood/detent#1586" })).toHaveAttribute(
    "href",
    "https://github.com/digitaldrywood/detent/issues/1586",
  );
});

test("several proposals survive snapshot morphs and appear per project in Health", async ({ page }) => {
  await openScenario(page, "board-admission-proposals-several", "/");

  const indicator = page.locator("#board-alerts");
  await expect(indicator).toContainText("3 admission proposals awaiting decision");
  await expect(indicator).toHaveScreenshot("board-admission-proposals-several.png");
  await indicator.getByRole("button").click();
  await expect(page.locator("#board-alerts-overlay")).toBeVisible();

  const before = await page.evaluate(() => {
    window.__admissionSnapshot = document.getElementById("snapshot");
    return window.__detentSSEMetrics?.snapshot()?.snapshot?.swaps || 0;
  });
  await expect
    .poll(
      () => page.evaluate(() => window.__detentSSEMetrics?.snapshot()?.snapshot?.swaps || 0),
      { timeout: 15_000 },
    )
    .toBeGreaterThan(before);
  expect(await page.evaluate(() => document.getElementById("snapshot") === window.__admissionSnapshot)).toBe(true);
  await expect(indicator.getByRole("button")).toHaveAttribute("aria-expanded", "true");
  await expect(page.locator("#board-alerts-overlay")).toBeVisible();

  await page.goto(`${runtime.url}/health/ui`, { waitUntil: "domcontentloaded" });
  const detent = page.locator("#health-admission-proposals-dogfood");
  const docs = page.locator("#health-admission-proposals-docs-site");
  await expect(detent).toContainText("2 awaiting decisions");
  await expect(detent).toContainText("digitaldrywood/detent#1586");
  await expect(detent).toContainText("confidence");
  await expect(detent).toContainText("expires in");
  await expect(docs).toContainText("1 awaiting decision");
  await expect(docs).toContainText("digitaldrywood/docs-site#241");
});

async function openScenario(page, scenario, route) {
  await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": scenario });
  await page.goto(`${runtime.url}${route}`, { waitUntil: "domcontentloaded" });
  await page.locator("#snapshot").waitFor({ state: "visible" });
}
