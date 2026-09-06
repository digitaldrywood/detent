const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

const desktopViewport = { width: 1440, height: 1100 };
const tabletViewport = { width: 900, height: 900 };
const scenarios = {
  healthy: {
    id: "kanban-full-integration",
    route: "/projects/dogfood/kanban",
  },
  dense: {
    id: "kanban-dense-overflow",
    route: "/projects/dogfood/kanban",
  },
  empty: {
    id: "kanban-empty-lanes",
    route: "/projects/agent-lab/kanban",
  },
  loading: {
    id: "kanban-startup-loading",
    route: "/projects/dogfood/kanban",
  },
  error: {
    id: "kanban-work-error",
    route: "/projects/dogfood/kanban",
  },
};

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("work-views", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("Board and List share pagination after filtering and refresh", async ({ page }) => {
  await openWorkScenario(page, scenarios.healthy, desktopViewport);
  await page.evaluate(() => {
    const templates = ["board", "list"].map((representation) => document.querySelector(`[data-work-representation="${representation}"]`));
    templates.forEach((template) => {
      for (let index = 0; index < 120; index += 1) {
        const row = template.cloneNode(true);
        row.id = `${template.id}-page-${index}`;
        row.dataset.workKey = `pagination-${index}`;
        row.dataset.workIdentity = `pagination-${index}`;
        row.dataset.workSearch = `pagination fixture ${index}`;
        template.parentNode.appendChild(row);
      }
    });
    document.body.dispatchEvent(new CustomEvent("htmx:afterSettle", { detail: { target: document.getElementById("snapshot") } }));
  });
  await page.locator("[data-work-search-input]").fill("pagination fixture");
  await expect(page.locator("[data-work-page-summary]")).toHaveText("Page 1 of 3");
  await page.getByRole("button", { name: "Next work page" }).click();
  await expect(page.locator("[data-work-page-summary]")).toHaveText("Page 2 of 3");
  expect(new URL(page.url()).searchParams.get("page")).toBe("2");
  const keys = await unhiddenKeys(page, "board");
  expect(keys).toHaveLength(50);
  await page.locator('[data-work-view="list"]').click();
  expect(await unhiddenKeys(page, "list")).toEqual(keys);
  await page.getByRole("button", { name: "Next work page" }).click();
  expect(await unhiddenKeys(page, "list")).toHaveLength(20);
  await expect(page.getByRole("button", { name: "Next work page" })).toBeDisabled();
  await page.locator("[data-work-search-input]").fill("pagination fixture 119");
  await expect(page.locator("[data-work-page-summary]")).toHaveText("Page 1 of 1");
  expect(await unhiddenKeys(page, "list")).toHaveLength(1);
  expect(await unhiddenKeys(page, "board")).toEqual(await unhiddenKeys(page, "list"));
});

test("Board and List share query, selection, detail, and density state", async ({
  page,
}) => {
  await openWorkScenario(page, scenarios.healthy, desktopViewport);

  const boardItems = page.locator('[data-work-representation="board"]');
  const listItems = page.locator('[data-work-representation="list"]');
  expect(await boardItems.count()).toBeGreaterThan(0);
  expect(await listItems.count()).toBe(await boardItems.count());
  await expect(page.locator('[data-work-view-panel="board"]')).toBeVisible();
  await expect(page.locator('[data-work-view-panel="list"]')).toBeHidden();
  await expect(page.locator("[data-work-health]")).toHaveAttribute(
    "href",
    "/health/ui",
  );

  const search = page.locator("[data-work-search-input]");
  await search.fill("deterministic chart colors");
  await expect(page.locator("[data-work-result-count]")).toHaveText("1 issue");
  expect(await unhiddenKeys(page, "board")).toEqual(
    await unhiddenKeys(page, "list"),
  );

  await page.locator('[data-work-view="list"]').click();
  await expect(page.locator('[data-work-view-panel="list"]')).toBeVisible();
  await expect(page.locator('[data-work-view-panel="board"]')).toBeHidden();
  expect(new URL(page.url()).searchParams.get("view")).toBe("list");
  expect(new URL(page.url()).searchParams.get("q")).toBe(
    "deterministic chart colors",
  );

  const row = page.locator('[data-work-representation="list"]:not([hidden])');
  await row.locator("[data-board-card-title]").click();
  const sheet = page.locator("[data-detail-sheet]");
  await expect(sheet).toBeVisible();
  await expect(sheet).toContainText("Review deterministic chart colors");
  expect(new URL(page.url()).searchParams.get("issue")).toBe(
    await row.getAttribute("data-work-identity"),
  );
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.locator("[data-work-toolbar]").waitFor({ state: "visible" });
  await expect(page.locator('[data-work-view-panel="list"]')).toBeVisible();
  await expect(sheet).toBeVisible();
  await expect(sheet).toContainText("Review deterministic chart colors");
  await page.keyboard.press("Escape");
  await expect(sheet).toHaveCount(0);
  expect(new URL(page.url()).searchParams.has("issue")).toBeFalsy();

  await page.locator("[data-work-search-input]").fill("");
  await page.locator("[data-work-filters] summary").click();
  const stateFilter = page.locator(
    '[data-work-filter="state"][value="in-progress"]',
  );
  await stateFilter.check();
  expect(await unhiddenKeys(page, "board")).toEqual(
    await unhiddenKeys(page, "list"),
  );
  expect(new URL(page.url()).searchParams.getAll("state")).toEqual([
    "in-progress",
  ]);

  const firstRow = page.locator('[data-work-representation="list"]:not([hidden])').first();
  await firstRow.focus();
  const focusedKey = await firstRow.getAttribute("data-work-key");
  await page.locator('[data-work-view="board"]').click();
  await expect(page.locator(`[data-work-representation="board"][data-work-key="${focusedKey}"]`)).toBeFocused();

  await page.locator('[data-work-view="list"]').click();
  await page.locator('[data-density-choice="compact"]').click();
  await expectRowHeight(page, 36);
  await page.locator('[data-density-choice="cozy"]').click();
  await expectRowHeight(page, 44);

  expect(new URL(page.url()).searchParams.get("view")).toBe("list");
  await page.reload({ waitUntil: "domcontentloaded" });
  expect(new URL(page.url()).searchParams.get("view")).toBe("list");
  await page.locator("[data-work-toolbar]").waitFor({ state: "visible" });
  await expect(page.locator('[data-work-view-panel="list"]')).toBeVisible();
  await expect(page.locator('[data-work-filter="state"][value="in-progress"]')).toBeChecked();
  await expect(page.locator("html")).toHaveAttribute("data-density", "cozy");
  expect(await unhiddenKeys(page, "board")).toEqual(
    await unhiddenKeys(page, "list"),
  );
});

test("List stays contained and keyboard position survives a tablet view switch", async ({
  page,
}) => {
  await openWorkScenario(page, scenarios.dense, tabletViewport, "?view=list&sort=identifier");

  const listScroll = page.locator("#work-list-scroll");
  await expect(listScroll).toBeVisible();
  expect(
    await listScroll.evaluate(
      (element) => element.scrollWidth > element.clientWidth,
    ),
  ).toBeTruthy();
  expect(
    await page.locator("#snapshot").evaluate(
      (element) => element.scrollWidth <= element.clientWidth,
    ),
  ).toBeTruthy();

  const rows = page.locator('[data-work-representation="list"]:not([hidden])');
  expect(await rows.count()).toBeGreaterThan(2);
  await rows.first().focus();
  await page.keyboard.press("ArrowDown");
  const focusedKey = await page.evaluate(
    () => document.activeElement?.dataset.workKey,
  );
  expect(focusedKey).toBe(await rows.nth(1).getAttribute("data-work-key"));
  expect(new URL(page.url()).searchParams.get("focus")).toBe(focusedKey);

  await listScroll.evaluate((element) => {
    element.scrollLeft = 240;
    element.scrollTop = 120;
  });
  const before = await listScroll.evaluate((element) => ({
    left: element.scrollLeft,
    top: element.scrollTop,
  }));
  await page.locator('[data-work-view="board"]').click();
  await expect(page.locator(`[data-work-representation="board"][data-work-key="${focusedKey}"]`)).toBeFocused();
  await page.locator('[data-work-view="list"]').click();
  await expect(page.locator(`[data-work-representation="list"][data-work-key="${focusedKey}"]`)).toBeFocused();
  expect(await listScroll.evaluate((element) => element.scrollLeft)).toBe(
    before.left,
  );
  expect(await listScroll.evaluate((element) => element.scrollTop)).toBe(
    before.top,
  );

  await page.keyboard.press("Enter");
  await expect(page.locator("[data-detail-sheet]")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator("[data-detail-sheet]")).toHaveCount(0);

  const issueLink = rows.first().locator("a").first();
  await issueLink.evaluate((link) => {
    link.addEventListener(
      "click",
      (event) => {
        event.preventDefault();
        document.body.dataset.workNestedLinkActivated = "true";
      },
      { once: true },
    );
  });
  await issueLink.focus();
  await issueLink.press("Enter");
  await expect(page.locator("body")).toHaveAttribute(
    "data-work-nested-link-activated",
    "true",
  );
  await expect(page.locator("[data-detail-sheet]")).toHaveCount(0);
});

for (const state of ["loading", "empty", "error"]) {
  test(`List renders the ${state} state without a page-wide sync banner`, async ({
    page,
  }) => {
    await openWorkScenario(
      page,
      scenarios[state],
      state === "loading" ? desktopViewport : tabletViewport,
      "?view=list",
    );

    await expect(page.locator('[data-work-view-panel="list"]')).toBeVisible();
    await expect(page.locator("[data-work-toolbar]")).toBeVisible();
    await expect(page.locator("#board-alerts")).toHaveCount(0);
    if (state === "loading") {
      await expect(page.locator("#work-list")).toHaveAttribute("aria-busy", "true");
    } else if (state === "empty") {
      await expect(page.locator("[data-work-list-empty]")).toBeVisible();
    } else {
      await expect(page.locator("#work-list")).toContainText(
        "Work data is unavailable",
      );
      await expect(page.locator("[data-work-health]")).toContainText(
        "Refresh failed",
      );
    }
  });
}

async function openWorkScenario(page, scenario, viewport, search = "") {
  await page.setViewportSize(viewport);
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": scenario.id,
  });
  await page.goto(`${runtime.url}${scenario.route}${search}`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator("[data-work-toolbar]").waitFor({ state: "visible" });
  await page.evaluate(() => document.fonts?.ready);
}

async function unhiddenKeys(page, representation) {
  return page
    .locator(`[data-work-representation="${representation}"]`)
    .evaluateAll((items) =>
      items
        .filter((item) => !item.hidden)
        .map((item) => item.dataset.workKey)
        .sort(),
    );
}

async function expectRowHeight(page, expected) {
  const height = await page
    .locator('[data-work-representation="list"]:not([hidden])')
    .first()
    .evaluate((row) => row.getBoundingClientRect().height);
  expect(height).toBe(expected);
}
