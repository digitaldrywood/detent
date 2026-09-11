const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

let runtime;
test.beforeAll(async () => {
  runtime = await startDetentRuntime("board-counts", ["--demo", "screenshots"]);
});
test.afterAll(async () => { await runtime?.stop(); });

for (const state of [
  { name: "live", count: "3", note: "", waiting: "8" },
  { name: "partial", count: "2", note: " (1 project unknown)", waiting: "8+" },
  { name: "unknown", count: "unknown", note: " (3 projects unknown)", waiting: "8+" },
]) {
  test(`board header with ${state.name} projects`, async ({ page }) => {
    await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": `board-counts-${state.name}` });
    await page.goto(runtime.url, { waitUntil: "domcontentloaded" });
    await expect(page.locator("#fig-ready")).toHaveText(`${state.count} ready${state.note}`);
    await expect(page.locator("#fig-blocked")).toHaveText(`${state.count} blocked${state.note}`);
    await expect(page.locator("#fig-waiting")).toHaveText(`${state.waiting} waiting`);
    if (state.name === "partial") {
      const project = page.locator('[data-help-term="sidebar-project-parable"]');
      await expect(project).toContainText("unknown");
      await project.hover();
      const tooltip = page.locator("#help-tooltip");
      await expect(tooltip).toBeVisible();
      await expect(tooltip).toContainText("Authorization selector declined: issues are authorized for other hosts.");
      expect(await tooltip.evaluate(node => node.closest("#snapshot") === null)).toBe(true);
      await page.evaluate(() => document.dispatchEvent(new CustomEvent("htmx:afterSettle", { detail: { target: document.querySelector("#snapshot") } })));
      await expect(tooltip).toBeVisible();
      await expect(tooltip).toContainText("Authorization selector declined");
    }
    await page.setViewportSize({ width: 390, height: 844 });
    for (const id of ["fig-ready", "fig-blocked"]) {
      const figure = page.locator(`#${id}`);
      expect(await figure.evaluate(node => node.scrollWidth <= Math.ceil(node.getBoundingClientRect().width))).toBe(true);
    }
  });
}
