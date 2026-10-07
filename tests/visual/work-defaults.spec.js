const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("work-defaults", {
    env: { DETENT_HOSTED_BROWSER_BOARD_DEFAULTS: "1" },
  });
});

test.afterAll(async () => { await hub?.stop(); });

async function openWork(page, route) {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL(route, hub.fixture.url).toString());
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
    const queuedTotal = await page.evaluate(async (projectId) => {
      const bootstrap = await (await fetch("/chat/bootstrap")).json();
      const projects = projectId === null ? bootstrap.projects.map((project) => project.id) : [projectId];
      const counts = await Promise.all(projects.map(async (id) => {
        const base = `${bootstrap.api_base}/projects/${id}`;
        const [project, page] = await Promise.all([fetch(base).then((response) => response.json()), fetch(`${base}/work-items?include=work`).then((response) => response.json())]);
        return page.work.lanes.reduce((total, lane) => total + (!project.states.find((state) => state.name === lane.state)?.terminal && (lane.state === "Backlog" || project.states.find((state) => state.name === lane.state)?.dispatchable) ? lane.total - lane.running : 0), 0);
      }));
      return counts.reduce((total, count) => total + count, 0);
    }, scope === "all" ? null : hub.fixture.project_id);
    expect(queuedTotal).toBeGreaterThanOrEqual(130 * copies);
    await expect(page.getByTestId("stat-queued")).toHaveText(`${queuedTotal} queued inventory`);
    const queued = await page.getByTestId("stat-queued").innerText();
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
      await expect(page.getByTestId("work-board")).toHaveScreenshot(`active-board-${scope}-${theme}.png`, { mask: [page.getByTestId("issue-age")] });
    }
    await page.getByTestId("lanes-trigger").click();
    await expect(page.getByTestId("lanes-trigger")).toHaveText("Lanes8/8");
    await expect(page.getByTestId("lane-toggle-Backlog")).toHaveAttribute("aria-checked", "true");
    await expect(page.getByTestId("lane-toggle-Backlog")).toContainText(String(130 * copies));
    await expect(page.getByTestId("lane-toggle-Done")).toHaveCount(0);
    await expect(page.getByTestId("lane-toggle-Cancelled")).toHaveCount(0);
    await page.keyboard.press("Escape");
    await page.getByTestId("lane-collapse-Backlog").focus();
    await page.keyboard.press("Enter");
    await expect(page.getByTestId("lane-body-Backlog")).toBeVisible();
    await expect(page.getByTestId("lane-collapse-Backlog")).toHaveAttribute("aria-label", "Collapse Backlog");
    expect(await backlog.evaluate((lane) => lane.getBoundingClientRect().width)).toBe(300);
    await expect(page).toHaveURL(/collapsed=/);
    await expect(page.getByTestId("stat-queued")).toHaveText(queued);
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
      await page.getByTestId("completed-window-trigger").click();
      await page.getByRole("menuitemradio", { name: window === "all" ? "All time" : window, exact: true }).click();
      await page.keyboard.press("Escape");
      await expect(page.getByTestId("stat-completed")).toHaveText(`${count * copies} completed · ${window}`);
      await expect(page).toHaveURL(new RegExp(`completed=${window}`));
    }
    await page.reload();
    await expect(page.getByTestId("stat-completed")).toHaveText(`${4 * copies} completed · all`);
    await page.goto(new URL(route, hub.fixture.url).toString());
    await expect(page).toHaveURL(/completed=all/);
    await expect(page.getByTestId("stat-completed")).toHaveText(`${4 * copies} completed · all`);
    await page.getByTestId("completed-window-trigger").click();
    await page.getByRole("menuitemradio", { name: "48h", exact: true }).click();
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
  await page.evaluate((project) => localStorage.setItem(`detent.work.view:${project}`, "lanes=Backlog,Cancelled"), hub.fixture.project_id);
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
  await openWork(page, `/work/p/${hub.fixture.project_id}?collapsed=Backlog`);
  const completed = Number((await page.getByTestId("stat-completed").innerText()).split(" ")[0]);
  const title = "Review the invitation flow";
  await page.getByRole("button", { name: `Move ${title}`, exact: true }).click();
  await page.getByRole("menuitem", { name: "Done", exact: true }).click();
  await expect(page.getByRole("button", { name: title, exact: true })).toHaveCount(0);
  await page.reload();
  await expect(page.getByTestId("stat-completed")).toHaveText(`${completed + 1} completed · 48h`);
  await expect(page.getByRole("button", { name: title, exact: true })).toHaveCount(0);
  await expect(page.locator('[data-testid="board-lane"][data-terminal="true"]')).toHaveCount(0);
});
