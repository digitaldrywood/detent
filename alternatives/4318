const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

let runtime;
test.beforeAll(async () => {
  runtime = await startDetentRuntime("sheet-model-effort", [
    "--demo",
    "screenshots",
  ]);
});
test.afterAll(async () => {
  await runtime?.stop();
});

for (const title of [
  "Add screenshot manifest smoke test",
  "Review deterministic chart colors",
]) {
  test(`non-running card shows stage selection through morph refresh: ${title}`, async ({ page }) => {
    await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": "fleet-healthy-parallel-work" });
    await page.goto(runtime.url, { waitUntil: "domcontentloaded" });
    await page.locator("article[data-work-representation=board]", { hasText: title }).click();
    const core = page.locator("#detail-sheet-core");
    const row = core.locator('[data-sheet-row="Model / Effort"]');
    await expect(row).toBeVisible();
    await expect(row.locator("[data-sheet-row-value]")).toHaveText("Build: demo-model · low (configured default)");
    await expect(core.locator('[data-sheet-row="Configured effort"]')).toHaveCount(0);
    expect(await core.locator("[data-sheet-row]").evaluateAll(rows => rows.slice(0, 2).map(row => row.dataset.sheetRow))).toEqual(["State", "Model / Effort"]);
    await row.evaluate(row => {
      window.originalSelectionRow = row;
    });
    const refresh = page.waitForResponse(response => response.url().includes("/core") && response.status() === 200);
    await core.evaluate(core => new Promise(resolve => {
      const settled = event => {
        if (event.target.id === core.id) {
          document.removeEventListener("htmx:afterSettle", settled);
          resolve();
        }
      };
      document.addEventListener("htmx:afterSettle", settled);
      window.htmx.trigger(core, "detailSheetRefresh");
    }));
    await refresh;
    expect(await row.evaluate(row => row === window.originalSelectionRow)).toBe(true);
    await expect(row.locator("[data-sheet-row-value]")).toHaveText("Build: demo-model · low (configured default)");
  });
}
