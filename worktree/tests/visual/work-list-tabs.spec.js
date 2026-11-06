const { test, expect } = require("@playwright/test");
const path = require("node:path");
const { startHostedHub, resetSavedWorkViews, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });
let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("work-list-tabs", {
    env: { DETENT_HOSTED_BROWSER_BOARD_DEFAULTS: "1" },
  });
});

test.afterAll(async () => { await hub?.stop(); });

test.beforeEach(async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await resetSavedWorkViews(page);
});

for (const scope of ["project", "all"]) {
  test(`${scope} list tabs partition server totals and ignore board lanes`, async ({ page }) => {
    const route = scope === "project" ? `/work/p/${hub.fixture.project_id}` : "/work";
    const copies = scope === "project" ? 1 : 2;
    await page.goto(new URL(`${route}?view=list&lanes=Todo`, hub.fixture.url).toString());
    await expect(page.getByTestId("work-list")).toBeVisible();
    const tabs = page.getByRole("group", { name: "Issue list tabs" });
    await expect(tabs).toBeVisible();
    await expect(page.getByTestId("list-tab-backlog")).toHaveText(`Backlog ${130 * copies}`);
    await expect(page.getByTestId("list-tab-closed")).toHaveText(`Closed ${4 * copies}`);
    const seededActiveIssues = 2;
    const open = 130 * copies + seededActiveIssues;
    await expect(page.getByTestId("list-tab-active")).toHaveText(`Active ${open - 130 * copies}`);
    await expect(page.getByTestId("list-tab-all")).toHaveText(`All ${open + 4 * copies}`);
    await expect(page.getByTestId("lanes-trigger")).toHaveCount(0);
    await expect(page.getByTestId("work-list").getByText("Done", { exact: true })).toHaveCount(0);
    await expect(page.getByTestId("work-list").getByText("Cancelled", { exact: true })).toHaveCount(0);
    await expect(page.getByTestId("work-list-more")).toHaveCount(0);

    await page.getByTestId("list-tab-closed").click();
    await expect(page.getByTestId("work-list-row")).toHaveCount(4 * copies);
    await expect(page.getByTestId("work-list").getByText("Done", { exact: true })).toHaveCount(3 * copies);
    await expect(page.getByTestId("work-list").getByText("Cancelled", { exact: true })).toHaveCount(copies);
    await expect(page.getByRole("columnheader", { name: "Closed", exact: true })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: "Change", exact: true })).toBeVisible();
    const closedTimes = await page.getByTestId("work-list-row").evaluateAll((rows) => rows.map((row) => Date.parse(row.querySelector("td[title]").title)));
    expect(closedTimes).toEqual([...closedTimes].sort((a, b) => b - a));
    await page.reload();
    await expect(page.getByTestId("list-tab-closed")).toHaveAttribute("aria-pressed", "true");
    await page.goto(new URL(route, hub.fixture.url).toString());
    await expect(page).toHaveURL(/tab=closed/);

    await page.getByTestId("list-tab-backlog").click();
    await expect(page.getByTestId("work-list-row")).toHaveCount(130 * copies);
    await expect(page.getByTestId("work-list").getByText("Backlog", { exact: true })).toHaveCount(130 * copies);
    await expect(page.getByTestId("work-list-more")).toHaveCount(0);
    await expect(page.getByRole("columnheader", { name: "Closed", exact: true })).toHaveCount(0);

    await page.getByTestId("list-tab-all").click();
    await expect(page.getByTestId("work-list-more")).toBeVisible();
    const before = await page.getByTestId("work-list-row").count();
    await page.getByTestId("work-list-more").click();
    await expect(page.getByTestId("work-list-row")).toHaveCount(open + 4 * copies);
    expect(await page.getByTestId("work-list-row").count()).toBeGreaterThan(before);
    await page.getByTestId("view-board").click();
    await expect(page.getByTestId("board-lane")).toHaveCount(1);
    await expect(page.getByTestId("lane-body-Todo")).toBeVisible();
    await expect(tabs).toHaveCount(0);
  });
}

for (const theme of ["light", "dark"]) {
  test(`Closed list remains contained in ${theme} at desktop and narrow widths`, async ({ page }) => {
    await page.goto(new URL(`/work/p/${hub.fixture.project_id}?view=list&tab=closed`, hub.fixture.url).toString());
    await expect(page.getByTestId("work-list-row")).toHaveCount(4);
    await page.evaluate((value) => { document.documentElement.classList.add("no-transitions"); document.documentElement.classList.toggle("dark", value === "dark"); document.documentElement.dataset.theme = value; }, theme);
    await page.screenshot({ path: path.join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `work-list-closed-${theme}.png`) });
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(page.getByTestId("list-tab-closed")).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.getByRole("button", { name: "Show archived", exact: true }).click();
    await expect(page).toHaveURL(/archived=true/);
    await expect(page.getByTestId("work-list-empty")).toBeVisible();
  });
}
