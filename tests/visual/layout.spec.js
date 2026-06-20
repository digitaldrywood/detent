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

test("active-only Kanban view hides populated terminal lanes", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "kanban-terminal-states",
    viewport: desktopViewport,
  });

  const board = page.locator("#project-kanban");
  const cancelledLane = board.locator("[data-project-kanban-lane-title='Cancelled']");
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-empty", "false");
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(cancelledLane).toBeHidden();
  await expect(board.locator("[data-kanban-issue-id='demo-cancelled']")).toBeHidden();

  await board.locator("[data-project-kanban-visibility-menu] summary").click();
  await expect(board.locator("[data-project-kanban-visibility-count]")).toHaveText("6/9");
  const cancelledCheckbox = board.locator("[data-project-kanban-visibility-checkbox][value='cancelled']");
  await expect(cancelledCheckbox).not.toBeChecked();
  await expect(cancelledCheckbox).toBeEnabled();
  await expect(cancelledCheckbox.locator("xpath=ancestor::label")).toContainText("1");

  await cancelledCheckbox.check();
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(cancelledLane).toBeVisible();
  await page.reload({ waitUntil: "domcontentloaded" });
  await expect(board).toBeVisible();
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(cancelledLane).toBeVisible();
  await board.locator("[data-project-kanban-visibility-menu] summary").click();
  await expect(cancelledCheckbox).toBeChecked();

  await board.getByRole("button", { name: "Active only" }).click();
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(cancelledLane).toBeHidden();
  await board.getByRole("button", { name: "All lanes" }).click();
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(cancelledLane).toBeVisible();
  await board.getByRole("button", { name: "Active only" }).click();
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(cancelledLane).toBeHidden();
  await captureClipAndAttach(page, "#project-kanban", "project-kanban-active-only-terminal-hidden-desktop.png", testInfo, {
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
  await expect(
    board.getByText("To move cards from Detent, enable Kanban integration in WORKFLOW.md under the existing server block."),
  ).toBeVisible();
  await expect(board.getByLabel("Kanban integration config snippet")).toHaveValue(
    "kanban:\n  mode: integration",
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

test("direct Kanban drag success does not shift board layout", async ({ page }, testInfo) => {
  await page.setViewportSize(desktopViewport);
  await page.goto(`${kanbanRuntime.url}/projects/demo-project/kanban`, { waitUntil: "domcontentloaded" });
  await page.request.post(`${kanbanRuntime.url}/api/v1/refresh`);
  await expect(page.locator("#project-kanban")).toBeVisible();
  await expect(page.locator("[data-kanban-issue-id='kanban-demo-backlog']")).toBeVisible();
  await page.waitForFunction(() => Boolean(window.htmx && window.__detentKanbanDragHandlersRegistered));
  await page.locator("[data-kanban-drop-state='Todo']").evaluate((lane) => {
    lane.dataset.projectKanbanLaneVisible = "true";
  });
  await expect(page.locator("[data-kanban-drop-state='Todo']")).toBeVisible();

  const before = await kanbanDragSuccessMetrics(page);
  const moveResponse = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return url.pathname === "/api/v1/kanban/move" && response.request().method() === "POST";
  });
  const accepted = await dragKanbanCardToLane(page, "kanban-demo-backlog", "Todo");
  expect(accepted).toBe(true);
  await moveResponse;

  await expect(page.locator("[data-kanban-drop-state='Todo'] [data-kanban-issue-id='kanban-demo-backlog']")).toHaveCount(1);
  await expect(page.locator("[data-kanban-drop-state='Backlog'] [data-kanban-issue-id='kanban-demo-backlog']")).toHaveCount(0);
  await expect(page.locator("#kanban-feedback")).not.toContainText("Moved card to Todo.");

  const after = await kanbanDragSuccessMetrics(page);
  assertNoTopShift("project Kanban board", before.boardTop, after.boardTop);
  assertNoTopShift("project Kanban lanes", before.lanesTop, after.lanesTop);
  assertNoTopShift("Todo lane", before.todoTop, after.todoTop);
  await captureClipAndAttach(page, "#project-kanban", "project-kanban-drag-success-no-flash.png", testInfo, {
    maxHeight: 430,
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

async function captureClipAndAttach(page, selector, name, testInfo, options) {
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

async function dragKanbanCardToLane(page, issueID, targetState) {
  return page.evaluate(
    ({ issueID, targetState }) => {
      const card = document.querySelector(`[data-kanban-issue-id="${CSS.escape(issueID)}"]`);
      const lane = document.querySelector(`[data-kanban-drop-state="${CSS.escape(targetState)}"]`);
      if (!card) {
        throw new Error(`missing card ${issueID}`);
      }
      if (!lane) {
        throw new Error(`missing lane ${targetState}`);
      }
      card.scrollIntoView({ block: "center", inline: "center" });
      lane.scrollIntoView({ block: "center", inline: "center" });
      card.dataset.kanbanDragging = "true";
      const cardRect = card.getBoundingClientRect();
      const laneRect = lane.getBoundingClientRect();
      const data = new DataTransfer();
      data.effectAllowed = "move";
      data.setData("text/plain", issueID);
      function fire(type, target, rect) {
        const event = new DragEvent(type, {
          bubbles: true,
          cancelable: true,
          dataTransfer: data,
          clientX: rect.left + Math.min(rect.width / 2, 160),
          clientY: rect.top + Math.min(rect.height / 2, 160),
        });
        target.dispatchEvent(event);
        return event.defaultPrevented;
      }
      fire("dragstart", card, cardRect);
      const accepted = fire("dragover", lane, laneRect);
      if (accepted) {
        fire("drop", lane, laneRect);
      }
      fire("dragend", card, cardRect);
      delete card.dataset.kanbanDragging;
      return accepted;
    },
    { issueID, targetState },
  );
}

async function kanbanDragSuccessMetrics(page) {
  return page.evaluate(() => {
    function documentTop(selector) {
      const element = document.querySelector(selector);
      if (!element) {
        throw new Error(`missing element ${selector}`);
      }
      const rect = element.getBoundingClientRect();
      return Math.round(rect.top + window.scrollY);
    }
    return {
      boardTop: documentTop("#project-kanban"),
      lanesTop: documentTop("#project-kanban .project-kanban-lanes"),
      todoTop: documentTop("[data-kanban-drop-state='Todo']"),
    };
  });
}

function assertNoTopShift(name, before, after) {
  expect(Math.abs(after - before), name).toBeLessThanOrEqual(1);
}
