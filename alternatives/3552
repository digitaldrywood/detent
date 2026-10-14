const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");
let runtime;
test.beforeAll(async () => { runtime = await startDetentRuntime("card-status", ["--demo", "screenshots"]); });
test.afterAll(async () => { await runtime?.stop(); });

for (const density of ["compact", "cozy", "comfy"]) {
  test(`INV-13 status stays on one line in ${density}`, async ({ page }) => {
    await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": "board-card-identity-maximal" });
    await page.goto(runtime.url, { waitUntil: "domcontentloaded" });
    await page.locator(`[data-density-choice="${density}"]`).click();
    const cards = page.locator('article[data-work-representation="board"]');
    expect(await cards.count()).toBeGreaterThan(0);
    for (const card of await cards.all()) {
      await expect(card.locator("[data-board-card-facts], [data-board-card-age-footer], [data-board-dispatch-evidence], [data-board-tracker-observation], [data-board-card-expanded]")).toHaveCount(0);
      const statuses = card.locator("[data-board-card-signal]");
      expect(await statuses.count()).toBeLessThanOrEqual(1);
      if (await statuses.count() && await statuses.isVisible()) {
        const geometry = await statuses.evaluate(el => ({
          characters: [...el.textContent].length,
          height: el.getBoundingClientRect().height,
          lineHeight: parseFloat(getComputedStyle(el).lineHeight),
          whiteSpace: getComputedStyle(el).whiteSpace,
        }));
        expect(geometry.characters).toBeLessThanOrEqual(48);
        expect(geometry.height).toBeLessThanOrEqual(geometry.lineHeight + 1);
        expect(geometry.whiteSpace).toBe("nowrap");
      }
    }
  });
}
