const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

const maxNavigationMilliseconds = 1_000;

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("board-navigation", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("board-to-board navigation does not stall on server rendering", async ({ page }) => {
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  await expect(page.locator("#board-lanes")).toBeVisible();

  await expectFastNavigation(
    page,
    page.locator('[data-sidebar-project="demo-project"]'),
    /\/projects\/demo-project\/kanban$/,
    "#board-lanes",
  );
  await expectFastNavigation(
    page,
    page.getByRole("navigation", { name: "Project views" }).getByRole("link", { name: "Overview" }),
    /\/projects\/demo-project$/,
    "#project-figures",
  );
  await expectFastNavigation(
    page,
    page.locator('#app-sidebar-content a[href="/fleet"]'),
    /\/fleet$/,
    "#fleet-figures",
  );
});

async function expectFastNavigation(page, link, url, readySelector) {
  const startedAt = Date.now();
  await Promise.all([
    page.waitForURL(url, { waitUntil: "domcontentloaded" }),
    link.click(),
  ]);
  await expect(page.locator(readySelector)).toBeVisible();
  expect(Date.now() - startedAt).toBeLessThan(maxNavigationMilliseconds);
}
