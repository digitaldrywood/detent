const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });
test.skip(({ isMobile }) => !isMobile, "mobile project only");

const routes = [
  { name: "board", path: "/", scenario: "fleet-kanban-multiproject" },
  { name: "fleet", path: "/fleet", scenario: "fleet-healthy-parallel-work" },
  { name: "library", path: "/library" },
  { name: "reports", path: "/reports", scenario: "reports-normal-window" },
  { name: "analytics", path: "/analytics", scenario: "diagnostics-healthy" },
  { name: "health", path: "/health/ui", scenario: "github-api-healthy" },
  { name: "api-keys", path: "/api-keys" },
  { name: "settings", path: "/settings", scenario: "settings-loaded-fleet" },
  {
    name: "project-kanban",
    path: "/projects/detent/kanban",
    scenario: "kanban-full-integration",
  },
  {
    name: "project-overview",
    path: "/projects/billing-api",
    scenario: "project-hot-path",
  },
  {
    name: "project-runs",
    path: "/projects/dogfood/runs",
    scenario: "runs-long-content",
  },
  {
    name: "project-diagnostics",
    path: "/projects/dogfood/diagnostics",
    scenario: "diagnostics-rate-limit-pressure",
  },
  {
    name: "project-configuration",
    path: "/projects/dogfood/configuration",
    scenario: "settings-project-context",
  },
];

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("mobile-shell", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

for (const route of routes) {
  test(`${route.name} uses the mobile app shell`, async ({ page }) => {
    await page.setExtraHTTPHeaders(
      route.scenario ? { "X-Detent-Demo-Scenario": route.scenario } : {},
    );
    await page.goto(`${runtime.url}${route.path}`, {
      waitUntil: "domcontentloaded",
    });
    const sidebar = page.locator("#app-sidebar");
    const scrim = page.locator("[data-mobile-sidebar-scrim]");
    const toggle = page.getByRole("button", { name: "Open navigation" });
    const topbarToggle = page.getByRole("button", {
      name: "More topbar controls",
    });
    const topbarControls = page.locator("[data-mobile-topbar-controls]");
    await toggle.waitFor({ state: "visible" });
    await page.evaluate(() => document.fonts?.ready);

    await expect(sidebar).toBeHidden();
    await expect(toggle).toBeVisible();
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    const toggleBox = await toggle.boundingBox();
    expect(toggleBox?.width).toBeGreaterThanOrEqual(44);
    expect(toggleBox?.height).toBeGreaterThanOrEqual(44);
    await expect(topbarToggle).toBeVisible();
    const topbarToggleBox = await topbarToggle.boundingBox();
    expect(topbarToggleBox?.width).toBeGreaterThanOrEqual(44);
    expect(topbarToggleBox?.height).toBeGreaterThanOrEqual(44);
    await expect(page).toHaveScreenshot(`${route.name}.png`);
    await expectNoHorizontalScroll(page);

    await topbarToggle.click();
    await expect(topbarControls).toBeVisible();
    await expect(page.locator("#live-indicator")).toBeVisible();
    await expect(page.getByRole("group", { name: "Density" })).toBeVisible();
    await topbarToggle.click();
    await expect(topbarControls).toBeHidden();

    await toggle.click();
    await expect(sidebar).toBeVisible();
    await expect(scrim).toBeVisible();
    await expect(toggle).toHaveAttribute("aria-expanded", "true");
    await scrim.click({ position: { x: 380, y: 422 } });
    await expect(sidebar).toBeHidden();
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
  });
}

test("drawer closes with Escape and a navigation tap", async ({ page }) => {
  await page
    .context()
    .addCookies([{ name: "sidebar_state", value: "false", url: runtime.url }]);
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-kanban-multiproject",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });

  const sidebar = page.locator("#app-sidebar");
  const toggle = page.getByRole("button", { name: "Open navigation" });
  await toggle.click();
  const sidebarBox = await sidebar.boundingBox();
  expect(sidebarBox?.width).toBe(208);
  await expect(
    sidebar.locator("[data-sidebar-nav-label]").first(),
  ).toBeVisible();
  await expect(
    sidebar.getByRole("button", { name: "Toggle sidebar" }),
  ).toBeHidden();
  await page.keyboard.press("Escape");
  await expect(sidebar).toBeHidden();

  await toggle.click();
  await page
    .locator("#app-sidebar a")
    .first()
    .evaluate((link) => {
      link.addEventListener("click", (event) => event.preventDefault(), {
        once: true,
      });
    });
  await page.locator("#app-sidebar a").first().click();
  await expect(sidebar).toBeHidden();
});

test("onboarding has a touch-safe mobile baseline", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "onboarding-project-selection",
  });
  await page.goto(`${runtime.url}/onboarding`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator("#onboarding-step").waitFor({ state: "visible" });
  await page.evaluate(() => document.fonts?.ready);

  for (const control of await page
    .locator(
      'a, button, input:not([type="hidden"]):not([type="radio"]):not([type="checkbox"]), select, textarea, summary',
    )
    .all()) {
    if (!(await control.isVisible())) {
      continue;
    }
    const box = await control.boundingBox();
    expect(box?.height).toBeGreaterThanOrEqual(44);
  }
  await expectNoHorizontalScroll(page);
  await expect(page).toHaveScreenshot("onboarding.png");
});

test("shared figures use a compact mobile grid", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-kanban-multiproject",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });

  const figures = page.locator("#board-figures > div");
  await expect(figures).toBeVisible();
  await expect(figures).toHaveCSS("display", "grid");
  const columns = await figures.evaluate(
    (element) =>
      getComputedStyle(element).gridTemplateColumns.split(" ").length,
  );
  expect(columns).toBe(3);
});

test("project tabs remain reachable by touch", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "project-hot-path",
  });
  await page.goto(`${runtime.url}/projects/billing-api`, {
    waitUntil: "domcontentloaded",
  });

  const tabs = page.locator("nav[aria-label='Project views']");
  const diagnostics = tabs.getByRole("link", { name: "Diagnostics" });
  await expect(tabs).toHaveCSS("overflow-x", "auto");
  expect(
    await tabs.evaluate((element) => element.scrollWidth > element.clientWidth),
  ).toBe(true);
  await diagnostics.scrollIntoViewIfNeeded();
  const tabBox = await diagnostics.boundingBox();
  expect(tabBox?.x).toBeGreaterThanOrEqual(0);
  expect((tabBox?.x || 0) + (tabBox?.width || 0)).toBeLessThanOrEqual(391);
});

test("issue detail is a touch-safe full-screen sheet", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-healthy-parallel-work",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });

  const tooltip = page.locator("body > #help-tooltip");
  const runtimeBadge = page
    .locator("[data-board-runtime-badge][data-help-trigger]")
    .first();
  const card = runtimeBadge.locator("xpath=ancestor::article");
  const cardRequest = await card.getAttribute("hx-get");
  await card.evaluate((element) => element.removeAttribute("hx-get"));
  await runtimeBadge.tap();
  await expect(tooltip).toBeVisible();
  await runtimeBadge.tap();
  await expect(tooltip).toBeHidden();
  await runtimeBadge.tap();
  await expect(tooltip).toBeVisible();
  await page.locator("h1").first().tap();
  await expect(tooltip).toBeHidden();
  await card.evaluate(
    (element, request) => element.setAttribute("hx-get", request),
    cardRequest,
  );
  await card.tap({ position: { x: 20, y: 20 } });

  const sheet = page.locator("[data-detail-sheet]");
  const dialog = sheet.getByRole("dialog");
  await expect(dialog).toBeVisible();
  const dialogBox = await dialog.boundingBox();
  expect(dialogBox).not.toBeNull();
  expect(dialogBox.x).toBe(0);
  expect(dialogBox.y).toBe(0);
  expect(dialogBox.width).toBe(390);
  expect(dialogBox.height).toBe(844);

  for (const label of ["Provider", "Model", "Session"]) {
    const row = dialog.locator(`[data-sheet-row="${label}"]`);
    const value = row.locator("[data-sheet-row-value]");
    await expect(value).toBeVisible();
    const dimensions = await value.evaluate((element) => ({
      clientWidth: element.clientWidth,
      scrollWidth: element.scrollWidth,
      whiteSpace: getComputedStyle(element).whiteSpace,
    }));
    expect(dimensions.scrollWidth).toBeLessThanOrEqual(dimensions.clientWidth);
    expect(dimensions.whiteSpace).toBe("normal");
  }

  const commentBodies = dialog.locator("[aria-label='Conversation'] article p");
  if ((await commentBodies.count()) > 0) {
    await expect(commentBodies.first()).toHaveCSS("overflow-wrap", "anywhere");
  }

  for (const control of [
    dialog.getByRole("button", { name: "Close details" }),
    dialog.getByRole("tab", { name: "Timeline" }),
    dialog.getByRole("tab", { name: "Live session" }),
  ]) {
    const box = await control.boundingBox();
    expect(box).not.toBeNull();
    expect(box.height).toBeGreaterThanOrEqual(44);
  }
  await expectNoHorizontalScroll(page);

  await page.evaluate(
    () =>
      new Promise((resolve) => {
        document.addEventListener("htmx:afterRequest", function settled(event) {
          if (event.detail?.target?.id !== "detail-sheet-host") return;
          document.removeEventListener("htmx:afterRequest", settled);
          resolve();
        });
        const snapshot = document.querySelector("#snapshot");
        if (snapshot.getAttribute("hx-swap") !== "morph:innerHTML") {
          throw new Error("snapshot must use an in-place morph swap");
        }
        snapshot.dispatchEvent(
          new CustomEvent("htmx:afterSettle", {
            bubbles: true,
            detail: { target: snapshot },
          }),
        );
      }),
  );
  await expect(dialog).toBeVisible();
  await expectNoHorizontalScroll(page);
  await expect(page).toHaveScreenshot("issue-detail-sheet.png");

  await dialog.getByRole("button", { name: "Close details" }).tap();
  await expect(sheet).toHaveCount(0);
  await expect(tooltip).toBeHidden();
});

test("settings exposes complete values and touch copy actions", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "settings-loaded-fleet",
  });
  await page.goto(`${runtime.url}/settings`, {
    waitUntil: "domcontentloaded",
  });

  const pathValue = page
    .locator("#settings-global [title]")
    .filter({ hasText: "/" })
    .first();
  await expect(pathValue).toBeVisible();
  await expect(pathValue).toHaveCSS("white-space", "normal");
  const copy = page.locator("#settings-global [data-copy]").first();
  await expect(copy).toBeVisible();
  const copyBox = await copy.boundingBox();
  expect(copyBox?.width).toBeGreaterThanOrEqual(44);
  expect(copyBox?.height).toBeGreaterThanOrEqual(44);
  await copy.click();
  await expect(copy.locator("[data-copy-label]")).toHaveText("Copied!");
  await expectNoHorizontalScroll(page);
});

test("health keeps its morph target and contains table overflow", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "github-api-healthy",
  });
  await page.goto(`${runtime.url}/health/ui`, {
    waitUntil: "domcontentloaded",
  });

  await expect(page.locator("#snapshot")).toHaveAttribute(
    "hx-swap",
    "morph:innerHTML",
  );
  const table = page.locator("[data-health-table-scroll]");
  await expect(table).toHaveCSS("overflow-x", "auto");
  const dimensions = await table.evaluate((element) => ({
    clientWidth: element.clientWidth,
    scrollWidth: element.scrollWidth,
  }));
  expect(dimensions.scrollWidth).toBeGreaterThan(dimensions.clientWidth);
  await expect(page.locator("#health-verdict")).toHaveCSS("flex-wrap", "wrap");
  await expectNoHorizontalScroll(page);
});

test("library filters stack with touch-sized controls", async ({ page }) => {
  await page.goto(`${runtime.url}/library`, { waitUntil: "domcontentloaded" });

  const form = page.locator('form[action="/library"]');
  await expect(form).toHaveCSS("flex-direction", "column");
  for (const control of await form.locator("select, input, button").all()) {
    const box = await control.boundingBox();
    expect(box?.width).toBeLessThanOrEqual(326);
    expect(box?.height).toBeGreaterThanOrEqual(44);
  }
  await expect(page.locator("[data-library-table-scroll]")).toHaveCSS(
    "overflow-x",
    "auto",
  );
  await expectNoHorizontalScroll(page);
});

test("API key controls are touch-sized and the create dialog fits", async ({
  page,
}) => {
  await page.goto(`${runtime.url}/api-keys`, {
    waitUntil: "domcontentloaded",
  });

  const create = page.locator("[data-api-key-open-create]").first();
  const createBox = await create.boundingBox();
  expect(createBox?.height).toBeGreaterThanOrEqual(44);
  await create.click();
  const dialog = page.locator("#api-key-modal");
  await expect(dialog).toBeVisible();
  const dialogBox = await dialog.boundingBox();
  expect(dialogBox?.x).toBeGreaterThanOrEqual(0);
  expect((dialogBox?.x || 0) + (dialogBox?.width || 0)).toBeLessThanOrEqual(390);
  expect(dialogBox?.height).toBeLessThanOrEqual(844);
  await expectNoHorizontalScroll(page);
});

test("reports charts and analytics log stay usable on mobile", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "reports-normal-window",
  });
  await page.goto(`${runtime.url}/reports`, {
    waitUntil: "domcontentloaded",
  });

  const reportGrids = [
    page.locator("#reports-spend").locator(".."),
    page.locator("#reports-top-issues").locator(".."),
    page.locator("#reports-budget").locator(".."),
  ];
  for (const grid of reportGrids) {
    await expect(grid).toBeVisible();
    const columns = await grid.evaluate(
      (element) =>
        getComputedStyle(element).gridTemplateColumns.split(" ").length,
    );
    expect(columns).toBe(1);
  }

  const kpis = page.locator("#reports-kpis");
  await expect(kpis).toHaveCSS("display", "grid");
  expect(
    await kpis.evaluate(
      (element) =>
        getComputedStyle(element).gridTemplateColumns.split(" ").length,
    ),
  ).toBe(2);

  for (const chart of await page
    .locator("#reports-spend svg, #reports-tokens svg")
    .all()) {
    const box = await chart.boundingBox();
    expect(box).not.toBeNull();
    expect(box.width).toBeGreaterThan(300);
    expect(box.width / box.height).toBeGreaterThan(2.5);
    expect(box.width / box.height).toBeLessThan(3.5);
  }
  await expectNoHorizontalScroll(page);

  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "diagnostics-healthy",
  });
  await page.goto(`${runtime.url}/analytics`, {
    waitUntil: "domcontentloaded",
  });

  const tableScroll = page.locator("[data-analytics-table-scroll]");
  await expect(tableScroll).toHaveCSS("overflow-x", "auto");
  const dimensions = await tableScroll.evaluate((element) => ({
    clientWidth: element.clientWidth,
    scrollWidth: element.scrollWidth,
  }));
  expect(dimensions.scrollWidth).toBeGreaterThan(dimensions.clientWidth);
  await expect(
    page.locator("#analytics-log [id^='event-'] > span").nth(1),
  ).toHaveCSS("white-space", "normal");
  await expectNoHorizontalScroll(page);
});
for (const board of [
  { name: "global board", path: "/", scenario: "fleet-kanban-multiproject" },
  {
    name: "project kanban",
    path: "/projects/detent/kanban",
    scenario: "kanban-full-integration",
  },
]) {
  test(`${board.name} uses full-width swipe lanes`, async ({ page }) => {
    await page.setExtraHTTPHeaders({
      "X-Detent-Demo-Scenario": board.scenario,
    });
    await page.goto(`${runtime.url}${board.path}`, {
      waitUntil: "domcontentloaded",
    });

    const lanes = page.locator("#board-lanes");
    const visibleLanes = lanes.locator(
      '[data-board-lane][data-lane-hidden="false"]',
    );
    const indicator = page.locator("[data-board-lane-position]");
    await expect(lanes).toBeVisible();
    await expect(indicator).toBeVisible();

    const layout = await lanes.evaluate((root) => {
      const style = getComputedStyle(root);
      const visible = Array.from(
        root.querySelectorAll('[data-board-lane][data-lane-hidden="false"]'),
      );
      const first = visible[0];
      const second = visible[1];
      const card = first?.querySelector("article");
      const rootRect = root.getBoundingClientRect();
      return {
        snapType: style.scrollSnapType,
        contentWidth:
          root.clientWidth -
          Number.parseFloat(style.paddingLeft) -
          Number.parseFloat(style.paddingRight),
        laneWidth: first?.getBoundingClientRect().width || 0,
        cardFits:
          !card ||
          card.getBoundingClientRect().right <=
            first.getBoundingClientRect().right + 1,
        nextLaneOutsideViewport:
          !second || second.getBoundingClientRect().left >= rootRect.right - 1,
        visibleCount: visible.length,
      };
    });
    expect(layout.snapType).toContain("x mandatory");
    expect(layout.visibleCount).toBeGreaterThan(1);
    expect(
      Math.abs(layout.laneWidth - layout.contentWidth),
    ).toBeLessThanOrEqual(1);
    expect(layout.cardFits).toBe(true);
    expect(layout.nextLaneOutsideViewport).toBe(true);
    await expectNoHorizontalScroll(page);

    const firstPosition = await indicator.getAttribute("aria-label");
    expect(firstPosition).toBe(`Lane 1 of ${layout.visibleCount}`);
    const secondLane = visibleLanes.nth(1);
    const secondLaneID = await secondLane.getAttribute("data-board-lane");
    await secondLane.evaluate((lane) =>
      lane.scrollIntoView({
        behavior: "instant",
        block: "nearest",
        inline: "start",
      }),
    );
    await expect(indicator).toHaveAttribute(
      "aria-label",
      `Lane 2 of ${layout.visibleCount}`,
    );
    expect(await lanes.evaluate((root) => root.scrollLeft)).toBeGreaterThan(0);

    const picker = page.locator("#board-lane-picker");
    const pickerSummary = picker.locator("summary");
    const summaryBox = await pickerSummary.boundingBox();
    expect(summaryBox?.height).toBeGreaterThanOrEqual(44);
    await pickerSummary.click();
    await expect(picker).toHaveAttribute("open", "");
    const pickerPanel = picker.locator(":scope > div");
    const panelBox = await pickerPanel.boundingBox();
    expect(panelBox?.x).toBeGreaterThanOrEqual(0);
    expect((panelBox?.x || 0) + (panelBox?.width || 0)).toBeLessThanOrEqual(
      390,
    );

    const hideLaneID = await visibleLanes.evaluateAll(
      (laneNodes, activeID) =>
        laneNodes
          .map((lane) => lane.getAttribute("data-board-lane"))
          .reverse()
          .find((laneID) => laneID && laneID !== activeID),
      secondLaneID,
    );
    const visibility = page.locator(
      `[data-board-lane-visibility="${hideLaneID}"]`,
    );
    const visibilityBox = await visibility.boundingBox();
    expect(visibilityBox?.height).toBeGreaterThanOrEqual(44);
    await visibility.selectOption("hide");
    await expect(
      page.locator(`[data-board-lane="${hideLaneID}"]`),
    ).toBeHidden();
    await expect(indicator).toHaveAttribute(
      "aria-label",
      `Lane 2 of ${layout.visibleCount - 1}`,
    );

    const persisted = await lanes.evaluate((root, laneID) => {
      const key = `detent.ui.board.lanes.v2.${root.dataset.boardKey}`;
      return JSON.parse(localStorage.getItem(key) || "{}").hide?.includes(
        laneID,
      );
    }, hideLaneID);
    expect(persisted).toBe(true);

    const beforeMorph = await lanes.evaluate((root) => ({
      left: root.scrollLeft,
      html: document.querySelector("#snapshot")?.innerHTML || "",
    }));
    await page.evaluate(
      (incomingSnapshot) =>
        new Promise((resolve) => {
          document.addEventListener(
            "htmx:afterSettle",
            () => requestAnimationFrame(() => requestAnimationFrame(resolve)),
            { once: true },
          );
          const target = document.querySelector("#snapshot");
          const event = new CustomEvent("htmx:sseBeforeMessage", {
            bubbles: true,
            cancelable: true,
            detail: { elt: target, data: incomingSnapshot },
          });
          target.dispatchEvent(event);
          if (!event.defaultPrevented) {
            window.htmx.swap(
              target,
              incomingSnapshot,
              { swapStyle: target.getAttribute("hx-swap") || "innerHTML" },
              { contextElement: target },
            );
          }
        }),
      beforeMorph.html,
    );
    expect(
      Math.abs(
        (await lanes.evaluate((root) => root.scrollLeft)) - beforeMorph.left,
      ),
    ).toBeLessThanOrEqual(1);
    await expect(
      page.locator(`[data-board-lane="${hideLaneID}"]`),
    ).toBeHidden();
    await expect(indicator).toHaveAttribute(
      "aria-label",
      `Lane 2 of ${layout.visibleCount - 1}`,
    );

    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(
      page.locator(`[data-board-lane="${hideLaneID}"]`),
    ).toBeHidden();
    await expect(
      page.locator(`[data-board-lane-visibility="${hideLaneID}"]`),
    ).toHaveValue("hide");
    await expectNoHorizontalScroll(page);
  });
}

test("board lanes scroll horizontally from card touches in both densities", async ({
  page,
}) => {
  const kanbanRuntime = await startDetentRuntime("mobile-kanban-card-swipe", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
  let session;
  try {
    await page.goto(`${kanbanRuntime.url}/projects/demo-project/kanban`, {
      waitUntil: "domcontentloaded",
    });
    session = await page.context().newCDPSession(page);
    for (const density of ["compact", "cozy"]) {
      await page
        .locator(`[data-density-choice="${density}"]`)
        .click({ force: true });

      const lanes = page.locator("#board-lanes");
      const card = lanes
        .locator('[data-kanban-card][data-kanban-action="move"]')
        .first();
      await expect(card).toBeVisible();
      await expect(lanes).toHaveCSS("touch-action", "pan-x");
      await expect(card).toHaveCSS("touch-action", "pan-x");
      await lanes.evaluate((root) => {
        root.scrollLeft = 0;
      });

      const cardBox = await card.boundingBox();
      if (!cardBox) {
        throw new Error(`${density} swipe source has no bounding box`);
      }
      const startX = cardBox.x + cardBox.width - 24;
      const swipeY = cardBox.y + cardBox.height / 2;
      await dispatchTouch(session, "touchStart", startX, swipeY);
      for (const distance of [50, 110, 170, 230, 290]) {
        await dispatchTouch(
          session,
          "touchMove",
          Math.max(24, startX - distance),
          swipeY,
        );
      }
      const scrolledDuringSwipe = await lanes.evaluate(
        (root) => root.scrollLeft,
      );
      await dispatchTouch(session, "touchEnd", 24, swipeY);

      expect(scrolledDuringSwipe).toBeGreaterThan(0);
      await expect(
        page.locator("body > [data-kanban-card][aria-hidden='true']"),
      ).toHaveCount(0);
    }
  } finally {
    await session?.detach();
    await kanbanRuntime.stop();
  }
});

test("fleet content stays readable and morph-safe", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-healthy-parallel-work",
  });
  await page.goto(`${runtime.url}/fleet`, { waitUntil: "domcontentloaded" });

  const snapshot = page.locator("#snapshot");
  await expect(snapshot).toHaveAttribute("hx-swap", "morph:innerHTML");

  const agents = page.locator("[data-fleet-agent-row]");
  expect(await agents.count()).toBeGreaterThan(0);
  for (const agent of await agents.all()) {
    await expect(agent.locator("[data-agent-repo]")).toBeVisible();
    await expect(agent.locator("[data-agent-issue]")).toBeVisible();
    await expect(agent.locator("[data-agent-issue]")).toContainText(/^#\d+$/);
    await expect(agent.locator("[data-agent-stage]")).toBeVisible();
    await expect(agent.locator("[data-agent-elapsed]")).toBeVisible();
    await expect(agent.locator("[data-agent-telemetry]")).toBeVisible();
    await expectContentToFit(agent.locator("[data-agent-repo]"));
    await expectContentToFit(agent.locator("[data-agent-issue]"));
    await expectContentToFit(agent.locator("[data-agent-stage]"));
  }

  const lanes = page.locator("#fleet-pr-pipeline > div > section");
  await expect(lanes).toHaveCount(3);
  const laneBoxes = await lanes.evaluateAll((elements) =>
    elements.map((element) => {
      const box = element.getBoundingClientRect();
      return { left: box.left, top: box.top, width: box.width };
    }),
  );
  expect(laneBoxes.every((box) => box.width >= 300)).toBeTruthy();
  expect(laneBoxes[1].top).toBeGreaterThan(laneBoxes[0].top);
  expect(laneBoxes[2].top).toBeGreaterThan(laneBoxes[1].top);
  expect(laneBoxes.every((box) => box.left === laneBoxes[0].left)).toBeTruthy();

  const originalSnapshot = await snapshot.elementHandle();
  const incoming = await snapshot.evaluate((element) => element.innerHTML);
  await page.route("**/__detent-test-fleet-refresh", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "text/html; charset=utf-8",
      body: incoming,
    });
  });
  await page.evaluate(
    () =>
      new Promise((resolve) => {
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
        window.htmx.ajax("GET", "/__detent-test-fleet-refresh", {
          target: "#snapshot",
          swap: "morph:innerHTML",
        });
      }),
  );
  expect(await originalSnapshot?.evaluate((element) => element.isConnected)).toBe(
    true,
  );
  await expectNoHorizontalScroll(page);
});

async function expectContentToFit(locator) {
  const fits = await locator.evaluate(
    (element) =>
      element.scrollWidth <= element.clientWidth + 1 &&
      element.scrollHeight <= element.clientHeight + 1,
  );
  expect(fits).toBeTruthy();
}

test("project kanban supports long-press touch status moves", async ({
  page,
}) => {
  const kanbanRuntime = await startDetentRuntime("mobile-kanban-touch-drag", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
  let session;
  let touchActive = false;
  try {
    await page.goto(`${kanbanRuntime.url}/projects/demo-project/kanban`, {
      waitUntil: "domcontentloaded",
    });
    const lanes = page.locator("#board-lanes");
    const card = page.locator(
      '#board-lanes [data-kanban-card][data-kanban-current-state="Backlog"]',
      { hasText: "Kanban demo backlog intake" },
    );
    const sourceLane = page.locator('[data-kanban-drop-state="Backlog"]');
    const targetLane = page.locator('[data-kanban-drop-state="Todo"]');
    const ghost = page.locator("body > [data-kanban-card][aria-hidden='true']");
    await expect(card).toHaveAttribute("data-kanban-action", "move");

    const cardBox = await card.boundingBox();
    if (!cardBox) {
      throw new Error("Touch drag source has no bounding box");
    }
    const startX = cardBox.x + cardBox.width / 2;
    const startY = cardBox.y + cardBox.height / 2;

    await page.touchscreen.tap(startX, startY);
    await expect(page.locator("[data-detail-sheet]")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.locator("[data-detail-sheet]")).toHaveCount(0);

    session = await page.context().newCDPSession(page);
    const swipeY = cardBox.y + cardBox.height / 2;
    await dispatchTouch(
      session,
      "touchStart",
      cardBox.x + cardBox.width - 20,
      swipeY,
    );
    touchActive = true;
    for (const x of [260, 200, 140, 80, 30]) {
      await dispatchTouch(session, "touchMove", x, swipeY);
    }
    await dispatchTouch(session, "touchEnd", 30, swipeY);
    touchActive = false;
    await expect(ghost).toHaveCount(0);
    await expect(page.locator("[data-kanban-drop-allowed]")).toHaveCount(0);
    await expect(page.locator("[data-detail-sheet]")).toHaveCount(0);

    const sourceHeader = sourceLane.locator("header");
    const headerBox = await sourceHeader.boundingBox();
    if (!headerBox) {
      throw new Error("Touch swipe source has no bounding box");
    }
    const headerY = headerBox.y + headerBox.height / 2;
    await dispatchTouch(session, "touchStart", 330, headerY);
    touchActive = true;
    for (const x of [280, 220, 160, 100, 50]) {
      await dispatchTouch(session, "touchMove", x, headerY);
    }
    await dispatchTouch(session, "touchEnd", 50, headerY);
    touchActive = false;
    await expect
      .poll(() => lanes.evaluate((root) => root.scrollLeft))
      .toBeGreaterThan(0);
    await page.waitForTimeout(500);
    await sourceLane.evaluate((lane) =>
      lane.scrollIntoView({
        behavior: "instant",
        block: "nearest",
        inline: "start",
      }),
    );
    await expect
      .poll(async () => {
        const box = await sourceLane.boundingBox();
        return box && box.x >= 0 && box.x < 390;
      })
      .toBe(true);

    const incomingSnapshot = await page
      .locator("#snapshot")
      .evaluate((snapshot) => snapshot.innerHTML);
    const moveRequest = page.waitForRequest((request) => {
      if (
        request.method() !== "POST" ||
        !request.url().endsWith("/api/v1/kanban/move")
      ) {
        return false;
      }
      return (
        new URLSearchParams(request.postData() || "").get("kanban_drag") ===
        "true"
      );
    });

    const dragBox = await card.boundingBox();
    if (!dragBox) {
      throw new Error("Touch drag source disappeared after swipe");
    }
    const dragX = dragBox.x + dragBox.width / 2;
    const dragY = dragBox.y + dragBox.height / 2;
    await dispatchTouch(session, "touchStart", dragX, dragY);
    touchActive = true;
    await page.waitForTimeout(500);
    await expect(ghost).toHaveCount(1);
    await expect(ghost).toContainText("From Backlog");
    await expect(card).toHaveAttribute("data-kanban-dragging", "true");
    await expect(sourceLane).toHaveAttribute("data-kanban-drop-source", "true");
    await expect(targetLane).toHaveAttribute(
      "data-kanban-drop-allowed",
      "true",
    );
    await expect(card).toHaveCSS("touch-action", "none");
    await expect(lanes).toHaveCSS("touch-action", "none");

    await page.evaluate(
      (incoming) =>
        new Promise((resolve) => {
          document.addEventListener("htmx:afterSettle", resolve, {
            once: true,
          });
          const target = document.querySelector("#snapshot");
          const event = new CustomEvent("htmx:sseBeforeMessage", {
            bubbles: true,
            cancelable: true,
            detail: { elt: target, data: incoming },
          });
          target.dispatchEvent(event);
          if (!event.defaultPrevented) {
            window.htmx.swap(
              target,
              incoming,
              { swapStyle: target.getAttribute("hx-swap") || "innerHTML" },
              { contextElement: target },
            );
          }
        }),
      incomingSnapshot,
    );
    await expect(ghost).toHaveCount(1);
    await expect(card).toHaveAttribute("data-kanban-dragging", "true");
    await expect(sourceLane).toHaveAttribute("data-kanban-drop-source", "true");
    await expect(targetLane).toHaveAttribute(
      "data-kanban-drop-allowed",
      "true",
    );
    await expect(card).toHaveCSS("touch-action", "none");
    await expect(lanes).toHaveCSS("touch-action", "none");

    await page.evaluate(() => {
      const target = document.elementFromPoint(20, 20) || document.body;
      const remaining = new Touch({
        identifier: 998,
        target,
        clientX: 20,
        clientY: 20,
      });
      const released = new Touch({
        identifier: 999,
        target,
        clientX: 20,
        clientY: 20,
      });
      document.dispatchEvent(
        new TouchEvent("touchend", {
          bubbles: true,
          cancelable: true,
          touches: [remaining],
          targetTouches: [remaining],
          changedTouches: [released],
        }),
      );
    });
    await expect(ghost).toHaveCount(1);
    await expect(card).toHaveAttribute("data-kanban-dragging", "true");

    await dispatchTouch(session, "touchMove", 340, dragY);
    await expect
      .poll(async () => {
        const box = await targetLane.boundingBox();
        return Boolean(box && box.x <= 340 && box.x + box.width > 340);
      })
      .toBe(true);
    await dispatchTouch(session, "touchEnd", 340, dragY);
    touchActive = false;

    const request = await moveRequest;
    const form = new URLSearchParams(request.postData() || "");
    expect(form.get("kanban_drag")).toBe("true");
    expect(form.get("target_state")).toBe("Todo");
    await expect(
      targetLane.locator("[data-kanban-card]", {
        hasText: "Kanban demo backlog intake",
      }),
    ).toBeVisible();
    await expect(
      sourceLane.locator("[data-kanban-card]", {
        hasText: "Kanban demo backlog intake",
      }),
    ).toHaveCount(0);
    await expect(ghost).toHaveCount(0);
    await expect(lanes).not.toHaveCSS("touch-action", "none");
    await expect(page.locator("[data-detail-sheet]")).toHaveCount(0);
  } finally {
    if (session && touchActive) {
      await dispatchTouch(session, "touchCancel", 0, 0).catch(() => {});
    }
    await session?.detach().catch(() => {});
    await kanbanRuntime.stop();
  }
});

async function dispatchTouch(session, type, x, y) {
  await session.send("Input.dispatchTouchEvent", {
    type,
    touchPoints:
      type === "touchEnd" || type === "touchCancel"
        ? []
        : [{ x, y, radiusX: 2, radiusY: 2, force: 1 }],
  });
}

async function expectNoHorizontalScroll(page) {
  const dimensions = await page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    viewportWidth: window.innerWidth,
  }));
  expect(dimensions.scrollWidth).toBeLessThanOrEqual(dimensions.viewportWidth);
}
