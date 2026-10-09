const { test, expect } = require("@playwright/test");
const { execFileSync } = require("node:child_process");
const { resolve } = require("node:path");

let fixtureUrl;

test.beforeAll(() => {
  fixtureUrl = execFileSync(process.execPath, ["tests/workPaginationBrowser.mjs"], {
    cwd: resolve(__dirname, "../../web/conversation"),
    encoding: "utf8",
  }).trim();
});

test("loads all 79 open cards without global pagination", async ({ page }) => {
  await page.goto(fixtureUrl);
  await expect(page.getByTestId("issue-card")).toHaveCount(79);
  await expect(page.getByText(/Load more/)).toHaveCount(0);
  const requests = await page.evaluate(() => window.workFixture.requests
    .filter(({ url }) => url.pathname.endsWith("/work-items"))
    .map(({ url }) => ({ project: url.pathname, states: url.searchParams.getAll("state"),
      open: url.searchParams.get("open"), limit: url.searchParams.get("limit"), include: url.searchParams.get("include") })));
  expect(requests).toHaveLength(6);
  for (const project of new Set(requests.map((request) => request.project))) {
    const totals = requests.filter((request) => request.project === project && request.include === "work");
    expect(totals).toEqual([{ project, states: [], open: null, limit: "1", include: "work" }]);
    const pages = requests.filter((request) => request.project === project && request.include === null);
    expect(pages).toHaveLength(2);
    expect(pages[0].states).not.toContain("Backlog");
    expect(pages[1].states).toEqual(["Backlog"]);
    expect(pages.every((request) => request.open === "true" && request.limit === "200")).toBe(true);
  }
});

test("continues Backlog inside its scroll body and retains the server count", async ({ page }) => {
  await page.goto(`${fixtureUrl}?backlog`);
  await expect(page.getByTestId("lane-count-Backlog")).toHaveText("405");
  await expect(page.getByRole("button", { name: "Show more", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Expand Backlog", exact: true }).click();
  const backlog = page.getByTestId("lane-body-Backlog");
  await expect(backlog.getByTestId("issue-card")).toHaveCount(200);
  const before = await page.evaluate(() => window.workFixture.requests.length);
  await backlog.getByRole("button", { name: "Show more", exact: true }).click();
  await expect(backlog.getByTestId("issue-card")).toHaveCount(400);
  await expect(page.getByTestId("lane-count-Backlog")).toHaveText("405");
  const pages = await page.evaluate((start) => window.workFixture.requests.slice(start)
    .filter(({ url }) => url.pathname.endsWith("/work-items"))
    .map(({ url }) => ({ states: url.searchParams.getAll("state"), cursor: url.searchParams.has("cursor"),
      include: url.searchParams.has("include") })), before);
  expect(pages).toEqual([{ states: ["Backlog"], cursor: true, include: false }]);
  await backlog.getByRole("button", { name: "Show more", exact: true }).click();
  await expect(backlog.getByTestId("issue-card")).toHaveCount(405);
  await expect(page.getByRole("button", { name: "Show more", exact: true })).toHaveCount(0);
  await expect(page.getByText(/Load more/)).toHaveCount(0);
  await expect(page.getByText("Observed later-page worker", { exact: true })).toBeVisible();
});


test("Work primary action stays pinned across every connection state", async ({ page }) => {
  await page.goto(`${fixtureUrl}?header`);
  const action = page.getByRole("button", { name: "New issue", exact: true });
  await expect(action).toBeVisible();
  const baseline = await action.boundingBox();
  for (const width of [1440, 900]) {
    await page.setViewportSize({ width, height: 844 });
    const box = await action.boundingBox();
    for (const label of ["Loading", "Live", "Updating", "Cached", "Reconnecting", "Not streaming"]) {
      for (const reload of [false, true]) {
        await page.evaluate((state) => window.setHeaderState(state), { label, reload });
        await expect(page.getByRole("status")).toContainText(label);
        await expect(page.getByRole("button", { name: "Reload", exact: true })).toHaveCount(reload ? 1 : 0);
        expect(await action.boundingBox()).toEqual(box);
        const chip = await page.getByRole("status").boundingBox();
        expect(chip.x + chip.width).toBeLessThan(box.x);
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      }
    }
  }
  expect(baseline).not.toBeNull();
  for (const width of [390, 320]) {
    await page.setViewportSize({ width, height: 844 });
    const ask = page.getByRole("button", { name: "Ask", exact: true });
    const menu = page.getByRole("button", { name: "More work actions", exact: true });
    await expect(ask).toBeVisible();
    for (const label of ["Loading", "Live", "Updating", "Cached", "Reconnecting", "Not streaming"]) {
      for (const reload of [false, true]) {
        await page.evaluate((state) => window.setHeaderState(state), { label, reload });
        await expect(page.getByRole("status")).toContainText(label);
        const chip = await page.getByRole("status").boundingBox();
        const askBox = await ask.boundingBox();
        const menuBox = await menu.boundingBox();
        expect(chip.x + chip.width).toBeLessThanOrEqual(askBox.x);
        expect(menuBox.x).toBeGreaterThan(askBox.x);
        expect(menuBox.x + menuBox.width).toBeLessThanOrEqual(width);
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      }
    }
  }
});
