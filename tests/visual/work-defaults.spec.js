const path = require("node:path");
const { test: base, expect } = require("@playwright/test");
const { startHostedHub, resetSavedWorkViews, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

const test = base.extend({
  hub: [async ({}, use) => {
    const hub = await startHostedHub("work-defaults", {
      env: { DETENT_HOSTED_BROWSER_BOARD_DEFAULTS: "1" },
    });
    try {
      await use(hub);
    } finally {
      await hub.stop();
    }
  }, { timeout: STARTUP_TIMEOUT_MS }],
});

async function openWork(page, route, instance) {
  await page.goto(instance.fixture.accounts.owner);
  await resetSavedWorkViews(page);
  await page.goto(new URL(route, instance.fixture.url).toString());
  await expect(page.getByTestId("work-board")).toBeVisible();
  await expect(page.getByRole("button", { name: /shipped ·/ })).toBeVisible();
}

const active = ["Triage", "Backlog", "Todo", "In Progress", "Rework", "Merging", "Blocked", "Human Review"];

for (const scope of ["all", "project"]) {
  test(`${scope} board shows only active lanes with a persisted Backlog rail`, async ({ page, hub }) => {
    const route = scope === "all" ? "/work" : `/work/p/${hub.fixture.project_id}`;
    await openWork(page, route, hub);
    await expect(page.getByTestId("board-lane")).toHaveCount(8);
    expect(await page.getByTestId("board-lane").evaluateAll((lanes) => lanes.map((lane) => lane.dataset.lane))).toEqual(active);
    await expect(page.getByTestId("lane-count-Merging")).toHaveText("0");
    await expect(page.getByTestId("lane-body-Merging")).toContainText("Nothing is in merging.");
    const copies = scope === "all" ? 2 : 1;
    await expect(page.getByTestId("work-stats")).toHaveCount(0);
    for (const counter of ["running", "waiting", "need-attention", "backlog"]) {
      await expect(page.getByTestId(`stat-${counter}`)).toHaveCount(0);
    }
    const heading = page.getByRole("heading", { level: 1 });
    const completed = page.getByRole("button", { name: /shipped ·/ });
    expect(await heading.evaluate((title) => title.closest("header")?.textContent)).not.toMatch(/\d+ (projects|issues)/);
    expect(await completed.evaluate((counter) => Boolean(counter.closest("header")?.querySelector("h1")?.compareDocumentPosition(counter) & Node.DOCUMENT_POSITION_FOLLOWING))).toBe(true);
    const titleBox = await heading.boundingBox();
    const liveBox = await page.getByTestId("stat-live").boundingBox();
    const counterBox = await completed.boundingBox();
    expect(liveBox.x).toBeGreaterThan(titleBox.x + titleBox.width);
    expect(liveBox.x - titleBox.x - titleBox.width).toBeLessThanOrEqual(16);
    expect(counterBox.x).toBeGreaterThan(liveBox.x + liveBox.width);
    expect(counterBox.x - liveBox.x - liveBox.width).toBeLessThanOrEqual(16);
    expect(counterBox.y).toBeLessThan(titleBox.y + titleBox.height);
    await expect(page.getByTestId("work-toolbar").getByText(/Completed/)).toHaveCount(0);
    const backlog = page.getByRole("region", { name: "Backlog", exact: true });
    expect(await backlog.evaluate((lane) => lane.getBoundingClientRect().width)).toBe(44);
    await expect(page.getByTestId("lane-body-Backlog")).toHaveCount(0);
    await expect(page.getByTestId("lane-count-Backlog")).toHaveText(String(130 * copies));
    await expect(page.getByTestId("lane-collapse-Backlog")).toHaveAttribute("aria-expanded", "false");
    for (const lane of active.filter((lane) => lane !== "Backlog")) {
      await expect(page.getByTestId(`lane-collapse-${lane}`)).toHaveAttribute("aria-expanded", "true");
    }
    await expect(page.locator('[data-testid="board-lane"][data-terminal="true"]')).toHaveCount(0);
    for (const theme of ["light", "dark"]) {
      await page.emulateMedia({ colorScheme: theme });
      await page.evaluate((theme) => {
        document.documentElement.dataset.theme = theme;
        document.documentElement.classList.toggle("dark", theme === "dark");
      }, theme);
      await test.info().attach(`active-board-${scope}-${theme}.png`, {
        body: await page.getByTestId("work-board").screenshot({ animations: "disabled", caret: "hide", mask: [page.getByTestId("issue-age")] }),
        contentType: "image/png",
      });
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

  test(`${scope} shipped window reads the Hub totals and survives reload and remembered navigation`, async ({ page, hub }) => {
    const route = scope === "all" ? "/work" : `/work/p/${hub.fixture.project_id}`;
    await openWork(page, route, hub);
    const copies = scope === "all" ? 2 : 1;
    const counter = page.getByTestId("stat-completed");
    await expect(counter).toHaveText(`${2 * copies} shipped · 24h`);
    await expect(counter).toHaveRole("button");
    await counter.click();
    await expect(counter).toHaveAttribute("aria-expanded", "true");
    await expect(page.getByRole("menu").getByText("Shipped in the last", { exact: true })).toBeVisible();
    await expect(page.getByRole("menuitemradio")).toHaveText([`1 hour${copies}`, `24 hours${2 * copies}`, `7 days${3 * copies}`]);
    await expect(page.getByRole("menuitemradio", { name: /^24 hours/ })).toHaveAttribute("aria-checked", "true");
    await expect(page.getByTestId("shipped-cancelled")).toHaveText(`Plus ${copies} cancelled in 24 hours`);
    await expect(page.getByRole("menuitem", { name: /Open in Reports/ })).toHaveAttribute("href", /\/reports\?range=24h$/);
    const anchor = await counter.boundingBox();
    const menu = await page.getByRole("menu").boundingBox();
    expect(Math.abs(menu.x - anchor.x)).toBeLessThan(2);
    await page.keyboard.press("Escape");
    for (const [window, count, label, range] of [["7d", 3, "7 days", "7d"], ["1h", 1, "1 hour", "24h"]]) {
      const saved = page.waitForResponse((response) => response.url().endsWith("/work-view-preference") && response.request().method() === "PUT" && new URLSearchParams(response.request().postDataJSON().query).get("completed") === window);
      const request = page.waitForRequest((request) => new URL(request.url()).searchParams.get("completed_window") === window);
      await counter.click();
      await page.getByRole("menuitemradio", { name: new RegExp(`^${label}`) }).click();
      await request;
      await expect(counter).toHaveText(`${count * copies} shipped · ${window}`);
      await expect(page).toHaveURL(new RegExp(`completed=${window}`));
      await saved;
      await counter.click();
      await expect(page.getByRole("menuitem", { name: /Open in Reports/ })).toHaveAttribute("href", new RegExp(`/reports\\?range=${range}$`));
      await page.keyboard.press("Escape");
    }
    await page.reload();
    await expect(counter).toHaveText(`${copies} shipped · 1h`);
    await page.goto(new URL(route, hub.fixture.url).toString());
    await expect(page).toHaveURL(/completed=1h/);
    await expect(counter).toHaveText(`${copies} shipped · 1h`);
    await counter.click();
    await page.getByRole("menuitemradio", { name: /^24 hours/ }).click();
    await expect(counter).toHaveText(`${2 * copies} shipped · 24h`);
    expect(new URL(page.url()).searchParams.has("completed")).toBe(false);
    await counter.click();
    await page.getByRole("menuitem", { name: /Open in Reports/ }).click();
    await expect(page).toHaveURL(scope === "all" ? /\/reports\?project=all&range=24h$/ : /\/reports\?range=24h$/);
    await expect(page.getByRole("tab", { name: "Overview", exact: true })).toHaveAttribute("aria-selected", "true");
    await expect(page.getByRole("group", { name: "Report range" }).getByRole("button", { name: "24h", exact: true })).toHaveAttribute("aria-pressed", "true");
  });

  test(`${scope} saved 48h, 14d and all windows fall back to 24 hours`, async ({ page, hub }) => {
    const route = scope === "all" ? "/work" : `/work/p/${hub.fixture.project_id}`;
    const copies = scope === "all" ? 2 : 1;
    await openWork(page, route, hub);
    for (const legacy of ["48h", "14d", "all"]) {
      const stored = await page.evaluate(async ({ project, query }) => {
        const bootstrap = await (await fetch("/chat/bootstrap")).json();
        const target = project === null ? `${bootstrap.api_base}/work-view-preference` : `${bootstrap.api_base}/projects/${project}/work-view-preference`;
        const response = await fetch(target, {
          method: "PUT",
          headers: { "Content-Type": "application/json", "X-CSRF-Token": bootstrap.csrf_token },
          body: JSON.stringify({ query }),
        });
        return response.status;
      }, { project: scope === "all" ? null : hub.fixture.project_id, query: `completed=${legacy}` });
      expect(stored).toBe(200);
      await page.goto(new URL(route, hub.fixture.url).toString());
      await expect(page.getByTestId("stat-completed")).toHaveText(`${2 * copies} shipped · 24h`);
    }
  });

  for (const theme of ["light", "dark"]) {
    test(`${scope} open shipped menu in ${theme}`, async ({ page, hub }) => {
      const route = scope === "all" ? "/work" : `/work/p/${hub.fixture.project_id}`;
      await openWork(page, route, hub);
      await page.evaluate((value) => { document.documentElement.classList.add("no-transitions"); document.documentElement.classList.toggle("dark", value === "dark"); document.documentElement.dataset.theme = value; }, theme);
      await page.getByTestId("stat-completed").click();
      await expect(page.getByTestId("shipped-cancelled")).toBeVisible();
      await page.screenshot({ path: path.join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `work-shipped-menu-${scope}-${theme}.png`) });
    });
  }
}

test("terminal URL and stored lane names never produce board lanes", async ({ page, hub }) => {
  const route = `/work/p/${hub.fixture.project_id}`;
  await openWork(page, `${route}?lanes=Todo,Done`, hub);
  await expect(page.getByTestId("board-lane")).toHaveCount(1);
  await expect(page.getByTestId("lane-body-Todo")).toBeVisible();
  await expect(page.getByTestId("lane-body-Done")).toHaveCount(0);
  const stored = await page.evaluate(async () => {
    const bootstrap = await (await fetch("/chat/bootstrap")).json();
    const response = await fetch(`${bootstrap.api_base}/work-view-preference`, {
      method: "PUT",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": bootstrap.csrf_token },
      body: JSON.stringify({ query: "lanes=Backlog,Cancelled" }),
    });
    return response.status;
  });
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
    const completed = page.getByRole("button", { name: /shipped ·/ });
    await expect(completed).toHaveText("0 shipped · 24h");
    const title = "Renew the lease before the handoff completes";
    const saved = page.waitForResponse((response) =>
      response.url().endsWith(`/work-items/${instance.fixture.work_item}/workflow`) && response.request().method() === "POST",
    );
    await page.getByRole("button", { name: `Move ${title}`, exact: true }).click();
    await page.getByRole("menuitem", { name: "Done", exact: true }).click();
    expect((await saved).ok()).toBe(true);
    await expect(page.getByRole("button", { name: title, exact: true })).toHaveCount(0);
    await expect(completed).toHaveText("1 shipped · 24h");
    await page.reload();
    await expect(completed).toHaveText("1 shipped · 24h");
    await expect(page.getByRole("button", { name: title, exact: true })).toHaveCount(0);
    await expect(page.getByRole("region", { name: "Done", exact: true })).toHaveCount(0);
    await expect(page.getByRole("region", { name: "Cancelled", exact: true })).toHaveCount(0);
  } finally {
    await instance.stop();
  }
});
