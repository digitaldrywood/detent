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
for (const density of ["compact", "cozy", "comfy"]) {
  for (const theme of ["light", "dark"]) {
    test(`split footer contains long titles at every card width (${density}, ${theme})`, async ({ page }) => {
      await page.goto(`${fixtureUrl}?card`);
      await page.evaluate(({ density, theme }) => {
        document.documentElement.dataset.density = density;
        document.documentElement.dataset.theme = theme;
        document.documentElement.classList.toggle("dark", theme === "dark");
      }, { density, theme });
      const card = page.getByTestId("issue-card");
      const title = card.getByRole("button", { name: /^fix\(migrations\):/ });
      await expect(title).toBeVisible();
      await expect(card.getByText("Attempt 4", { exact: true })).toBeVisible();
      await expect(card.getByText("Updated 2h", { exact: true })).toBeVisible();
      const menu = card.getByRole("button", { name: /^Move / });
      for (const unbroken of [false, true]) {
        if (unbroken) await page.getByRole("button", { name: "Use unbroken title", exact: true }).click();
        for (const width of [240, 260, 320, 400]) {
          await page.getByTestId("card-fixture").evaluate((element, width) => { element.style.width = `${width}px`; }, width);
          const layout = await card.evaluate((element) => {
            const rect = element.getBoundingClientRect();
            const get = (id) => element.querySelector(`[data-testid="${id}"]`);
            const bounds = (node) => node.getBoundingClientRect();
            const status = get("issue-card-status-row");
            const metadata = get("issue-card-metadata-row");
            const labels = [...status.children, ...metadata.children];
            return {
              width: rect.width,
              contained: [get("issue-card-header"), get("lane-menu-trigger"), get("issue-card-open"),
                status, metadata, ...labels].every((node) => {
                const box = bounds(node);
                return box.left >= rect.left && box.right <= rect.right && box.top >= rect.top && box.bottom <= rect.bottom;
              }),
              overflows: element.scrollWidth > element.clientWidth,
              titleOverflows: get("issue-card-open").scrollWidth > get("issue-card-open").clientWidth,
              menuAboveTitle: bounds(get("lane-menu-trigger")).bottom <= bounds(get("issue-card-open")).top,
              metadataBelowStatus: bounds(metadata).top >= bounds(status).bottom,
              divider: getComputedStyle(metadata).borderTopWidth,
              sameStatusRow: Math.abs(bounds(status.firstElementChild).top - bounds(status.lastElementChild).top) < 1,
              sameMetadataRow: Math.abs(bounds(metadata.firstElementChild).top - bounds(metadata.lastElementChild).top) < 1,
              labelsIntact: labels.every((node) => getComputedStyle(node).whiteSpace === "nowrap"
                && node.scrollWidth <= node.clientWidth),
              statusOnLeft: bounds(status.firstElementChild).left < bounds(status.lastElementChild).left,
              updateOnRight: bounds(metadata.firstElementChild).right < bounds(metadata.lastElementChild).left,
            };
          });
          expect(layout.width).toBe(width);
          expect(layout.contained).toBe(true);
          expect(layout.overflows).toBe(false);
          expect(layout.titleOverflows).toBe(false);
          expect(layout.menuAboveTitle).toBe(true);
          expect(layout.metadataBelowStatus).toBe(true);
          expect(layout.divider).toBe("1px");
          expect(layout.labelsIntact).toBe(true);
          if (width >= 320) {
            expect(layout.sameStatusRow).toBe(true);
            expect(layout.sameMetadataRow).toBe(true);
            expect(layout.statusOnLeft).toBe(true);
            expect(layout.updateOnRight).toBe(true);
          }
        }
      }
      await menu.focus();
      await page.keyboard.press("Enter");
      await expect(page.getByRole("menu", { name: "Move to" })).toBeVisible();
      await page.getByRole("button", { name: "Refresh card", exact: true }).evaluate((button) => button.click());
      await expect(page.getByRole("menu", { name: "Move to" })).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(menu).toBeFocused();
      await menu.click();
      await page.getByRole("menuitem", { name: "Todo", exact: true }).click();
      await expect(page.getByLabel("Card action")).toHaveText("Todo");
      await card.getByTestId("issue-card-open").focus();
      await page.keyboard.press("Enter");
      await expect(page.getByLabel("Card action")).toHaveText("Opened");
      await page.getByRole("button", { name: "Use imported metadata", exact: true }).click();
      await page.getByTestId("card-fixture").evaluate((element) => { element.style.width = "240px"; });
      await expect(card.getByText("Attempt 444444", { exact: true })).toBeVisible();
      await expect(card.getByText("Native update 2h", { exact: true })).toBeVisible();
      const wrapped = await card.getByTestId("issue-card-metadata-row").evaluate((row) => {
        const attempt = row.firstElementChild;
        const update = row.lastElementChild;
        const rect = row.getBoundingClientRect();
        const boxes = [attempt, update].map((node) => node.getBoundingClientRect());
        return {
          wraps: boxes[1].top >= boxes[0].bottom,
          contained: boxes.every((box) => box.left >= rect.left && box.right <= rect.right),
          intact: [attempt, update].every((node) => node.scrollWidth <= node.clientWidth
            && getComputedStyle(node).whiteSpace === "nowrap"),
        };
      });
      expect(wrapped).toEqual({ wraps: true, contained: true, intact: true });
    });
  }
}
