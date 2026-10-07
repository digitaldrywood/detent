const { test, expect } = require("@playwright/test");
const { startHostedHub, resetSavedWorkViews, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("work-defaults", {
    env: { DETENT_HOSTED_BROWSER_BOARD_DEFAULTS: "1" },
  });
});

test.afterAll(async () => { await hub?.stop(); });

async function openWork(page, route, instance = hub) {
  await page.goto(instance.fixture.accounts.owner);
  await resetSavedWorkViews(page);
  await page.goto(new URL(route, instance.fixture.url).toString());
  await expect(page.getByTestId("work-board")).toBeVisible();
  await expect(page.getByTestId("work-stats")).toHaveAttribute("aria-busy", "false");
}

const active = ["Backlog", "Todo", "In Progress", "Rework", "Merging", "Blocked", "Human Review", "Triage"];

for (const scope of ["all", "project"]) {
  test(`${scope} board shows only active lanes with a persisted Backlog rail`, async ({ page }) => {
    const route = scope === "all" ? "/work" : `/work/p/${hub.fixture.project_id}`;
    await openWork(page, route);
    await expect(page.getByTestId("board-lane")).toHaveCount(8);
    expect(await page.getByTestId("board-lane").evaluateAll((lanes) => lanes.map((lane) => lane.dataset.lane))).toEqual(active);
    await expect(page.getByTestId("lane-count-Merging")).toHaveText("0");
    await expect(page.getByTestId("lane-body-Merging")).toContainText("Nothing is in merging.");
    const copies = scope === "all" ? 2 : 1;
    const totals = await page.evaluate(async (projectId) => {
      const bootstrap = await (await fetch("/chat/bootstrap")).json();
      const projects = projectId === null ? bootstrap.projects.map((project) => project.id) : [projectId];
      const counts = await Promise.all(projects.map(async (id) => {
        const base = `${bootstrap.api_base}/projects/${id}`;
        const [project, page] = await Promise.all([fetch(base).then((response) => response.json()), fetch(`${base}/work-items?include=work`).then((response) => response.json())]);
        return page.work.lanes.reduce((counts, lane) => {
          const state = project.states.find((state) => state.name === lane.state);
          if (state?.terminal) return counts;
          counts.open += lane.total;
          counts.running += lane.running;
          if (lane.state === "Backlog") counts.backlog += lane.total - lane.running;
          else if (state?.dispatchable) counts.waiting += lane.total - lane.running;
          else counts.attention += lane.total - lane.running;
          return counts;
        }, { running: 0, waiting: 0, attention: 0, backlog: 0, open: 0 });
      }));
      return counts.reduce((totals, count) => Object.fromEntries(Object.keys(totals).map((key) => [key, totals[key] + count[key]])), { running: 0, waiting: 0, attention: 0, backlog: 0, open: 0 });
    }, scope === "all" ? null : hub.fixture.project_id);
    expect(totals.running + totals.waiting + totals.attention + totals.backlog).toBe(totals.open);
    expect(totals.backlog).toBe(130 * copies);
    await expect(page.getByTestId("stat-running")).toHaveText(`${totals.running} running`);
    await expect(page.getByTestId("stat-waiting")).toHaveText(`${totals.waiting} waiting`);
    await expect(page.getByTestId("stat-need-attention")).toHaveText(`${totals.attention} need attention`);
    await expect(page.getByTestId("stat-backlog")).toHaveText(`${totals.backlog} backlog`);
    expect(await page.getByTestId("work-stats").locator('[data-testid^="stat-"]').evaluateAll((counters) => counters.map((counter) => counter.dataset.testid))).toEqual(["stat-running", "stat-waiting", "stat-need-attention", "stat-backlog", "stat-completed"]);
    await expect(page.getByTestId("work-toolbar").getByText(/Completed/)).toHaveCount(0);
    const waiting = await page.getByTestId("stat-waiting").innerText();
    const backlog = page.getByRole("region", { name: "Backlog", exact: true });
    expect(await backlog.evaluate((lane) => lane.getBoundingClientRect().width)).toBe(44);
    await expect(page.getByTestId("lane-body-Backlog")).toHaveCount(0);
    await expect(page.getByTestId("lane-count-Backlog")).toHaveText(String(130 * copies));
    await expect(page.getByTestId("lane-collapse-Backlog")).toHaveAttribute("aria-expanded", "false");
    for (const lane of active.slice(1)) {
      await expect(page.getByTestId(`lane-collapse-${lane}`)).toHaveAttribute("aria-expanded", "true");
    }
    await expect(page.locator('[data-testid="board-lane"][data-terminal="true"]')).toHaveCount(0);
    for (const theme of ["light", "dark"]) {
      await page.emulateMedia({ colorScheme: theme });
      await page.evaluate((theme) => {
        document.documentElement.dataset.theme = theme;
        document.documentElement.classList.toggle("dark", theme === "dark");
      }, theme);
      await expect(page.getByTestId("work-stats")).toHaveScreenshot(`stats-row-${scope}-${theme}.png`);
      await expect(page.getByTestId("work-board")).toHaveScreenshot(`active-board-${scope}-${theme}.png`, { mask: [page.getByTestId("issue-age")] });
    }
    await page.getByTestId("lanes-trigger").click();
    await expect(page.getByTestId("lanes-trigger")).toHaveText("Lanes8/8");
    await expect(page.getByTestId("lane-toggle-Backlog")).toHaveAttribute("aria-checked", "true");
    await expect(page.getByTestId("lane-toggle-Backlog")).toContainText(String(130 * copies));
    await expect(page.getByTestId("lane-toggle-Done")).toHaveCount(0);
    await expect(page.getByTestId("lane-toggle-Cancelled")).toHaveCount(0);
    await page.keyboard.press("Escape");
    const collapsedSaved = page.waitForResponse((response) => response.url().endsWith("/work-view-preference") && response.request().method() === "PUT" && response.request().postDataJSON().query.includes("collapsed="));
    await page.getByTestId("lane-collapse-Backlog").focus();
    await page.keyboard.press("Enter");
    await expect(page.getByTestId("lane-body-Backlog")).toBeVisible();
    await expect(page.getByTestId("lane-collapse-Backlog")).toHaveAttribute("aria-label", "Collapse Backlog");
    expect(await backlog.evaluate((lane) => lane.getBoundingClientRect().width)).toBe(300);
    await expect(page).toHaveURL(/collapsed=/);
    await expect(page.getByTestId("stat-waiting")).toHaveText(waiting);
    await collapsedSaved;
    await page.reload();
    await expect(page.getByTestId("lane-body-Backlog")).toBeVisible();
    await page.goto(new URL(route, hub.fixture.url).toString());
    await expect(page.getByTestId("lane-body-Backlog")).toBeVisible();
    await page.getByTestId("lane-collapse-Backlog").focus();
    await page.keyboard.press("Space");
    await expect(page.getByTestId("lane-body-Backlog")).toHaveCount(0);
    await page.getByTestId("lane-collapse-Blocked").click();
    await expect(page.getByTestId("lane-body-Blocked")).toHaveCount(0);
    await expect(page).toHaveURL(/collapsed=Backlog%2CBlocked/);
    await page.reload();
    await expect(page.getByTestId("lane-body-Blocked")).toHaveCount(0);
    await page.getByTestId("lane-collapse-Blocked").click();
    expect(new URL(page.url()).searchParams.has("collapsed")).toBe(false);
    await page.getByTestId("lanes-trigger").click();
    await page.getByTestId("lane-toggle-Backlog").click();
    await page.keyboard.press("Escape");
    await expect(page.getByTestId("board-lane")).toHaveCount(7);
    await page.getByTestId("lanes-trigger").click();
    await page.getByTestId("lane-toggle-Backlog").click();
    await page.keyboard.press("Escape");
    await expect(page.getByTestId("board-lane")).toHaveCount(8);
    expect(new URL(page.url()).searchParams.has("lanes")).toBe(false);
    expect(await page.evaluate((key) => localStorage.getItem(`detent.work.view:${key}`), scope === "all" ? "" : hub.fixture.project_id)).toBeNull();
    await page.getByTestId("view-list").click();
    await expect(page.getByTestId("work-list")).not.toContainText("Backlog");
    await page.getByTestId("list-tab-backlog").click();
    await expect(page.getByTestId("work-list-row")).toHaveCount(130 * copies);
  });

  test(`${scope} completed window reads the Hub total and survives reload and remembered navigation`, async ({ page }) => {
    const route = scope === "all" ? "/work" : `/work/p/${hub.fixture.project_id}`;
    await openWork(page, route);
    const copies = scope === "all" ? 2 : 1;
    await expect(page.getByTestId("stat-completed")).toHaveText(`${2 * copies} completed · 48h`);
    for (const [window, count] of [["7d", 3], ["14d", 3], ["all", 4]]) {
      const saved = page.waitForResponse((response) => response.url().endsWith("/work-view-preference") && response.request().method() === "PUT" && new URLSearchParams(response.request().postDataJSON().query).get("completed") === window);
      const counter = page.getByTestId("stat-completed");
      await expect(counter).toHaveRole("button");
      const request = page.waitForRequest((request) => new URL(request.url()).searchParams.get("completed_window") === window);
      await counter.click();
      await expect(counter).toHaveAttribute("aria-expanded", "true");
      await expect(page.getByRole("menuitemradio")).toHaveText(["48 hours", "7 days", "14 days", "All time"]);
      const anchor = await counter.boundingBox();
      const menu = await page.getByRole("menu").boundingBox();
      expect(Math.abs(menu.x - anchor.x)).toBeLessThan(2);
      await page.getByRole("menuitemradio", { name: window === "all" ? "All time" : window === "7d" ? "7 days" : "14 days", exact: true }).click();
      await request;
      await page.keyboard.press("Escape");
      await expect(page.getByTestId("stat-completed")).toHaveText(`${count * copies} completed · ${window === "all" ? "All time" : window}`);
      await expect(page).toHaveURL(new RegExp(`completed=${window}`));
      await saved;
    }
    await page.reload();
    await expect(page.getByTestId("stat-completed")).toHaveText(`${4 * copies} completed · All time`);
    await page.goto(new URL(route, hub.fixture.url).toString());
    await expect(page).toHaveURL(/completed=all/);
    await expect(page.getByTestId("stat-completed")).toHaveText(`${4 * copies} completed · All time`);
    await page.getByTestId("stat-completed").click();
    await page.getByRole("menuitemradio", { name: "48 hours", exact: true }).click();
    await expect(page.getByTestId("stat-completed")).toHaveText(`${2 * copies} completed · 48h`);
    expect(new URL(page.url()).searchParams.has("completed")).toBe(false);
  });
}

test("terminal URL and stored lane names never produce board lanes", async ({ page }) => {
  const route = `/work/p/${hub.fixture.project_id}`;
  await openWork(page, `${route}?lanes=Todo,Done`);
  await expect(page.getByTestId("board-lane")).toHaveCount(1);
  await expect(page.getByTestId("lane-body-Todo")).toBeVisible();
  await expect(page.getByTestId("lane-body-Done")).toHaveCount(0);
  const stored = await page.evaluate(async (project) => {
    const bootstrap = await (await fetch("/chat/bootstrap")).json();
    const response = await fetch(`${bootstrap.api_base}/projects/${project}/work-view-preference`, {
      method: "PUT",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": bootstrap.csrf_token },
      body: JSON.stringify({ query: "lanes=Backlog,Cancelled" }),
    });
    return response.status;
  }, hub.fixture.project_id);
  expect(stored).toBe(200);
  await page.goto(new URL(route, hub.fixture.url).toString());
  await expect(page.getByTestId("board-lane")).toHaveCount(1);
  await expect(page.getByTestId("lane-collapse-Backlog")).toBeVisible();
  await expect(page.getByTestId("lane-body-Cancelled")).toHaveCount(0);
  await page.goto(new URL(`${route}?lanes=Done`, hub.fixture.url).toString());
  await expect(page.getByTestId("board-lane")).toHaveCount(0);
  await expect(page.getByTestId("work-board")).toContainText("Every lane is hidden.");
  await page.getByTestId("lanes-trigger").click();
  await expect(page.getByTestId("lanes-trigger")).toHaveText("Lanes0/8");
  await expect(page.getByTestId("lane-toggle-Done")).toHaveCount(0);
});

test("a terminal move removes the card and refreshes the completed count", async ({ page }) => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  const instance = await startHostedHub("work-terminal-move");
  try {
    await openWork(page, `/work/p/${instance.fixture.project_id}?collapsed=Backlog`, instance);
    const completed = Number((await page.getByTestId("stat-completed").innerText()).split(" ")[0]);
    const title = "Renew the lease before the handoff completes";
    const saved = page.waitForResponse((response) =>
      response.url().endsWith(`/work-items/${instance.fixture.work_item}/workflow`) && response.request().method() === "POST",
    );
    await page.getByRole("button", { name: `Move ${title}`, exact: true }).click();
    await page.getByRole("menuitem", { name: "Done", exact: true }).click();
    expect((await saved).ok()).toBe(true);
    await expect(page.getByRole("button", { name: title, exact: true })).toHaveCount(0);
    await page.reload();
    await expect(page.getByTestId("stat-completed")).toHaveText(`${completed + 1} completed · 48h`);
    await expect(page.getByRole("button", { name: title, exact: true })).toHaveCount(0);
    await expect(page.locator('[data-testid="board-lane"][data-terminal="true"]')).toHaveCount(0);
  } finally {
    await instance.stop();
  }
});
