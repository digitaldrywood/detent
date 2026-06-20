const fs = require("node:fs");
const path = require("node:path");
const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

const desktopViewport = { width: 1440, height: 1100 };
const narrowViewport = { width: 390, height: 844 };

let screenshotsRuntime;
let kanbanRuntime;
let screenshotManifest;

test.beforeAll(async () => {
  screenshotsRuntime = await startDetentRuntime("screenshots", ["--demo", "screenshots"]);
  const manifestResponse = await fetch(`${screenshotsRuntime.url}/api/v1/demo/scenarios`);
  if (!manifestResponse.ok) {
    throw new Error(`Failed to load screenshots manifest: ${manifestResponse.status}`);
  }
  screenshotManifest = await manifestResponse.json();
  kanbanRuntime = await startDetentRuntime("kanban", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
});

test.afterAll(async () => {
  await Promise.all([screenshotsRuntime?.stop(), kanbanRuntime?.stop()]);
});

test("screenshots manifest includes visual gate scenarios", async ({ request }) => {
  const response = await request.get(`${screenshotsRuntime.url}/api/v1/demo/scenarios`);
  expect(response.ok()).toBeTruthy();
  const payload = await response.json();
  expect(payload).toEqual(screenshotManifest);
  const scenarioIDs = payload.scenarios.map((scenario) => scenario.id);

  expect(scenarioIDs).toEqual(
    expect.arrayContaining([
      "fleet-kanban-multiproject",
      "kanban-full-integration",
      "kanban-read-only",
      "kanban-dense-overflow",
      "settings-project-context",
      "onboarding-project-selection",
    ]),
  );
});

test("fleet Kanban lanes stay fixed width across multiple projects", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-kanban-multiproject",
    viewport: desktopViewport,
  });

  const board = page.locator("#fleet-kanban");
  await expect(board).toBeVisible();
  await assertProjectKanbanLayout(page, "#fleet-kanban", { minLanes: 4 });
  await assertElementFitsViewport(page, "#fleet-kanban");
  await assertSidebarActive(page, "[data-dashboard-static-nav='kanban']");
  await compareClipAndAttach(page, "#fleet-kanban", "fleet-kanban-multiproject-desktop.png", testInfo, {
    maxHeight: 700,
  });
});

test("project Kanban keeps action controls inside compact cards", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "kanban-full-integration",
    viewport: desktopViewport,
  });

  const board = page.locator("#project-kanban");
  await expect(board).toBeVisible();
  await assertProjectKanbanLayout(page, "#project-kanban", { minLanes: 6 });
  await assertElementFitsViewport(page, "#project-kanban");
  await assertSidebarActive(page, "[data-dashboard-view='kanban']");
  await compareClipAndAttach(page, "#project-kanban", "project-kanban-full-integration-desktop.png", testInfo, {
    maxHeight: 430,
  });
});

test("project read-only Kanban explains integration setup", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "kanban-read-only",
    viewport: desktopViewport,
  });

  const board = page.locator("#project-kanban");
  await expect(board).toBeVisible();
  await expect(board.getByText("This board is currently read-only.")).toBeVisible();
  await expect(board.getByText("To move cards from Detent, enable Kanban integration in WORKFLOW.md.")).toBeVisible();
  await expect(board.getByLabel("Kanban integration config snippet")).toHaveValue(
    "server:\n  kanban:\n    mode: integration",
  );
  await expect(board.locator("[data-kanban-action]")).toHaveCount(0);
  await assertProjectKanbanLayout(page, "#project-kanban", { minLanes: 6 });
  await assertElementFitsViewport(page, "#project-kanban");
  await assertSidebarActive(page, "[data-dashboard-view='kanban']");
  await compareClipAndAttach(page, "#project-kanban", "project-kanban-read-only-guidance-desktop.png", testInfo, {
    maxHeight: 580,
  });
});

test("long Kanban issue titles do not inflate lanes", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "kanban-dense-overflow",
    viewport: desktopViewport,
  });

  const board = page.locator("#project-kanban");
  await expect(board.locator("[data-project-kanban-card]").first()).toBeVisible();
  await assertProjectKanbanLayout(page, "#project-kanban", { minLanes: 6 });
  await assertElementFitsViewport(page, "#project-kanban");
  await compareClipAndAttach(page, "#project-kanban", "project-kanban-dense-overflow-desktop.png", testInfo, {
    maxHeight: 760,
  });
});

test("Kanban boards remain contained on narrow screens", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-kanban-multiproject",
    viewport: narrowViewport,
  });

  const board = page.locator("#fleet-kanban");
  await expect(board).toBeVisible();
  await assertProjectKanbanLayout(page, "#fleet-kanban", { minLanes: 4 });
  await assertElementFitsViewport(page, "#fleet-kanban");
  await compareClipAndAttach(page, "#fleet-kanban", "fleet-kanban-multiproject-narrow.png", testInfo, {
    maxHeight: 740,
  });
});

test("sidebar active state survives Kanban refreshes", async ({ page }, testInfo) => {
  await page.setViewportSize(desktopViewport);
  await page.goto(`${kanbanRuntime.url}/kanban`, { waitUntil: "domcontentloaded" });
  await page.request.post(`${kanbanRuntime.url}/api/v1/refresh`);
  await expect(page.locator("#fleet-kanban")).toBeVisible();
  await assertSidebarActive(page, "[data-dashboard-static-nav='kanban']");

  await page.request.post(`${kanbanRuntime.url}/api/v1/refresh`);
  await page.waitForTimeout(300);
  await assertSidebarActive(page, "[data-dashboard-static-nav='kanban']");
  await compareClipAndAttach(page, "#fleet-kanban", "kanban-demo-refresh-active-state.png", testInfo, {
    maxHeight: 340,
  });
});

test("project configuration page preserves project color layout", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "settings-project-context",
    viewport: desktopViewport,
  });

  const settingsProjects = page.locator("#settings-projects");
  await expect(settingsProjects).toBeVisible();
  await assertNoDocumentOverflow(page);
  await assertSidebarActive(page, "[data-dashboard-view='configuration']");
  await compareClipAndAttach(page, "#settings-projects", "project-configuration-colors-desktop.png", testInfo, {
    maxHeight: 900,
  });
});

test("onboarding project selection remains readable on narrow screens", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "onboarding-project-selection",
    viewport: narrowViewport,
  });

  const step = page.locator("#onboarding-step");
  await expect(step).toBeVisible();
  await assertNoDocumentOverflow(page);
  await compareAndAttach(step, "onboarding-project-selection-narrow.png", testInfo);
});

async function openScenario(page, options) {
  const scenario = screenshotManifest.scenarios.find((item) => item.id === options.scenario);
  if (!scenario) {
    throw new Error(`Unknown screenshots scenario: ${options.scenario}`);
  }
  const route = options.route || scenario.route;
  const waitSelector = options.waitSelector || scenario.wait_selector;
  await page.setViewportSize(options.viewport);
  await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": options.scenario });
  await page.goto(`${options.runtime.url}${route}`, { waitUntil: "domcontentloaded" });
  await page.locator(waitSelector).waitFor({ state: "visible" });
  await page.evaluate(() => document.fonts?.ready);
}

async function compareAndAttach(locator, name, testInfo) {
  await expect(locator).toHaveScreenshot(name);
  const evidenceDir = path.join(process.cwd(), "tmp", "playwright-evidence", testInfo.project.name);
  fs.mkdirSync(evidenceDir, { recursive: true });
  const evidencePath = path.join(evidenceDir, name);
  await locator.screenshot({ path: evidencePath, animations: "disabled", caret: "hide" });
  await testInfo.attach(name, { path: evidencePath, contentType: "image/png" });
}

async function compareClipAndAttach(page, selector, name, testInfo, options) {
  const clip = await page.locator(selector).evaluate((element, maxHeight) => {
    const box = element.getBoundingClientRect();
    const x = Math.max(0, box.left);
    const y = Math.max(0, box.top);
    return {
      x,
      y,
      width: Math.min(box.width, window.innerWidth - x),
      height: Math.min(maxHeight, window.innerHeight - y),
    };
  }, options.maxHeight);
  await expect(page).toHaveScreenshot(name, { clip });
  const evidenceDir = path.join(process.cwd(), "tmp", "playwright-evidence", testInfo.project.name);
  fs.mkdirSync(evidenceDir, { recursive: true });
  const evidencePath = path.join(evidenceDir, name);
  await page.screenshot({ path: evidencePath, clip, animations: "disabled", caret: "hide" });
  await testInfo.attach(name, { path: evidencePath, contentType: "image/png" });
}

async function assertSidebarActive(page, selector) {
  const active = page.locator(selector);
  await expect(active).toHaveAttribute("aria-current", "page");
  await expect(active).toHaveAttribute("data-tui-sidebar-active", "true");
}

async function assertNoDocumentOverflow(page) {
  const overflow = await page.evaluate(() => {
    const root = document.documentElement;
    return root.scrollWidth - root.clientWidth;
  });
  expect(overflow).toBeLessThanOrEqual(1);
}

async function assertElementFitsViewport(page, selector) {
  const rect = await page.locator(selector).evaluate((element) => {
    const box = element.getBoundingClientRect();
    return {
      left: box.left,
      right: box.right,
      viewportWidth: window.innerWidth,
    };
  });
  expect(rect.left).toBeGreaterThanOrEqual(-1);
  expect(rect.right).toBeLessThanOrEqual(rect.viewportWidth + 1);
}

async function assertProjectKanbanLayout(page, boardSelector, options) {
  const metrics = await page.locator(boardSelector).evaluate((board) => {
    const lanes = Array.from(board.querySelectorAll("[data-project-kanban-lane]")).filter((lane) => {
      const style = window.getComputedStyle(lane);
      return style.display !== "none" && lane.getBoundingClientRect().width > 0;
    });
    const laneMetrics = lanes.map((lane) => {
      const rect = lane.getBoundingClientRect();
      return {
        title: lane.getAttribute("data-project-kanban-lane-title") || "",
        width: rect.width,
      };
    });
    const visibleLeaks = [];
    for (const card of board.querySelectorAll("[data-project-kanban-card]")) {
      const cardRect = card.getBoundingClientRect();
      const lane = card.closest("[data-project-kanban-lane]");
      const laneRect = lane?.getBoundingClientRect();
      if (!laneRect || cardRect.width === 0) {
        continue;
      }
      if (cardRect.left < laneRect.left - 1 || cardRect.right > laneRect.right + 1) {
        visibleLeaks.push(`${card.getAttribute("data-project-kanban-card")} escapes lane bounds`);
      }
      for (const child of card.querySelectorAll("*")) {
        const style = window.getComputedStyle(child);
        if (style.display === "none" || style.visibility === "hidden" || style.position === "absolute") {
          continue;
        }
        const childRect = child.getBoundingClientRect();
        if (childRect.width === 0 || childRect.height === 0) {
          continue;
        }
        if (childRect.left < cardRect.left - 1 || childRect.right > cardRect.right + 1) {
          visibleLeaks.push(`${card.getAttribute("data-project-kanban-card")} child escapes card bounds`);
        }
      }
    }
    return { lanes: laneMetrics, visibleLeaks };
  });

  expect(metrics.lanes.length).toBeGreaterThanOrEqual(options.minLanes);
  for (const lane of metrics.lanes) {
    expect(lane.width, `${lane.title} lane width`).toBeGreaterThanOrEqual(287);
    expect(lane.width, `${lane.title} lane width`).toBeLessThanOrEqual(289);
  }
  expect(metrics.visibleLeaks).toEqual([]);
}
