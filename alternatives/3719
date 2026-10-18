const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

let runtime;
test.beforeAll(async () => {
  runtime = await startDetentRuntime("operations", ["--demo", "screenshots"]);
});
test.afterAll(async () => { await runtime?.stop(); });

for (const width of [1440, 800]) {
  test(`operations sections and refresh preserve live region at ${width}px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 1100 });
    await page.goto(`${runtime.url}/operations`, { waitUntil: "domcontentloaded" });
    await expect(page.getByRole("heading", { name: "Operations", exact: true })).toBeVisible();
    for (const id of ["operations-stats", "operations-actions", "operations-decisions"]) {
      await expect(page.locator(`#${id}`)).toHaveCount(1);
    }
    await expect(page.locator("body > #help-tooltip")).toHaveCount(1);
    await expect(page.locator("#snapshot #help-tooltip")).toHaveCount(0);
    await expect(page.locator("#snapshot")).toHaveAttribute("hx-swap", "morph:innerHTML");
    await expect(page.locator("[data-detent-dashboard-stream]")).toHaveAttribute("sse-connect", "/events?nav=operations");
    await expect(page.locator('#app-sidebar-content a[href="/operations"]')).toHaveAttribute("aria-current", "page");
    const original = await page.locator("#snapshot").elementHandle();
    const refresh = page.getByRole("button", { name: "Refresh", exact: true });
    const cursor = await refresh.getAttribute("hx-get");
    await refresh.click();
    await expect(refresh).not.toHaveAttribute("hx-get", cursor);
    await expect(page.locator('#app-sidebar-content a[href="/operations"]')).toHaveAttribute("aria-current", "page");
    expect(await original.evaluate((node) => node === document.querySelector("#snapshot"))).toBe(true);
    await expect(page.locator("#operations-actions")).toContainText("No actions in this interval.");
    const response = await page.request.get(`${runtime.url}/api/v1/operations`);
    expect(response.ok()).toBe(true);
    const data = await response.json();
    expect(data.stats.map((window) => window.label)).toEqual(["24h", "7d"]);
    expect(data.instance).toBeTruthy();
    expect(data.merge_group_size).toBeNull();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath(`operations-${width}.png`), fullPage: true });
  });
}
