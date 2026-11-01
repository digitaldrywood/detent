const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("board-navigation", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
  await expect
    .poll(async () => (await fetch(`${runtime.url}/health`)).status, { timeout: 30_000 })
    .toBe(200);
  await expect
    .poll(async () => (await (await fetch(`${runtime.url}/projects/demo-project/kanban`)).text()).includes('data-kanban-action="move"'), { timeout: 30_000 })
    .toBe(true);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("board-to-board navigation does not stall on server rendering", async ({ page }) => {
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  await expect(page.locator("#board-lanes")).toBeVisible();

  await expectNavigation(
    page,
    page.locator('[data-sidebar-project="demo-project"]'),
    /\/projects\/demo-project\/kanban$/,
    "#board-lanes",
  );
  await expectNavigation(
    page,
    page.getByRole("navigation", { name: "Project views" }).getByRole("link", { name: "Overview" }),
    /\/projects\/demo-project$/,
    "#project-figures",
  );
  await expectNavigation(
    page,
    page.locator('#app-sidebar-content a[href="/fleet"]'),
    /\/fleet$/,
    "#fleet-figures",
  );
});

async function expectNavigation(page, link, url, readySelector) {
  await Promise.all([
    page.waitForURL(url, { waitUntil: "domcontentloaded" }),
    link.click(),
  ]);
  await expect(page.locator(readySelector)).toBeVisible();
}
