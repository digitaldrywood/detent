const fs = require("node:fs");
const path = require("node:path");
const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

const desktopViewport = { width: 1440, height: 1100 };
const narrowViewport = { width: 390, height: 844 };

async function expectProjectKanbanVisibilityStorage(page, key, expected) {
  const raw = await page.evaluate((storageKey) => {
    return window.localStorage.getItem(`detent.ui.projectKanban.visibleLanes.${storageKey}`);
  }, key);
  if (expected === null) {
    expect(raw).toBeNull();
    return;
  }
  expect(raw).not.toBeNull();
  const parsed = JSON.parse(raw);
  expect(parsed.v).toBe(4);
  expect([...parsed.show].sort()).toEqual([...expected.show].sort());
  expect([...parsed.hide].sort()).toEqual([...expected.hide].sort());
}

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

test.afterEach(async ({ page }, testInfo) => {
  if (testInfo.status === testInfo.expectedStatus) {
    return;
  }
  await attachFailureEvidence(page, testInfo);
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
      "kanban-startup-loading",
      "kanban-read-only",
      "kanban-dense-overflow",
      "github-api-healthy",
      "github-api-warning",
      "github-api-secondary-backoff",
      "github-api-primary-exhausted",
      "settings-project-context",
      "onboarding-project-selection",
    ]),
  );
});

test("GitHub API health chrome covers key rate-limit states", async ({ page }, testInfo) => {
  const scenarios = [
    ["github-api-healthy", "GitHub API healthy"],
    ["github-api-warning", "GitHub API warning"],
    ["github-api-secondary-backoff", "GitHub API backoff"],
    ["github-api-primary-exhausted", "GitHub API exhausted"],
  ];

  for (const [scenario, label] of scenarios) {
    await openScenario(page, {
      runtime: screenshotsRuntime,
      scenario,
      viewport: scenario === "github-api-secondary-backoff" ? narrowViewport : desktopViewport,
    });

    const indicator = page.locator("#github-api-health");
    await expect(indicator).toBeVisible();
    await expect(indicator).toContainText(label);
    await assertElementFitsViewport(page, "#github-api-health");
    await captureClipAndAttach(page, "#github-api-health", `${scenario}.png`, testInfo, {
      maxHeight: 280,
    });
  }
});

test("GitHub API health disclosure stays open across live refreshes", async ({ page }) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "github-api-secondary-backoff",
    viewport: desktopViewport,
  });

  const indicator = page.locator("#github-api-health");
  await expect(indicator).toBeVisible();
  await expect(indicator).toHaveAttribute("data-preserve-details", "github-api-health");

  await indicator.locator("summary").click();
  await expect(indicator).toHaveJSProperty("open", true);

  await page.evaluate(() => {
    const target = document.getElementById("github-api-health");
    document.dispatchEvent(new CustomEvent("htmx:beforeSwap", { detail: { target } }));
    if (target) {
      const replacement = target.cloneNode(true);
      if (replacement instanceof HTMLDetailsElement) {
        replacement.open = false;
      }
      target.replaceWith(replacement);
      document.dispatchEvent(new CustomEvent("htmx:afterSettle", { detail: { target: replacement } }));
    }
  });
  await expect(indicator).toHaveJSProperty("open", true);

  await indicator.locator("summary").click();
  await expect(indicator).toHaveJSProperty("open", false);

  await page.evaluate(() => {
    const target = document.getElementById("github-api-health");
    document.dispatchEvent(new CustomEvent("htmx:beforeSwap", { detail: { target } }));
    if (target) {
      const replacement = target.cloneNode(true);
      if (replacement instanceof HTMLDetailsElement) {
        replacement.open = true;
      }
      target.replaceWith(replacement);
      document.dispatchEvent(new CustomEvent("htmx:afterSettle", { detail: { target: replacement } }));
    }
  });
  await expect(indicator).toHaveJSProperty("open", false);
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

test("project runs issue and PR actions stay inside activity rows", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "runs-tracker-refresh-gap",
    viewport: desktopViewport,
  });

  const activity = page.locator('section[aria-label="Agent activity timeline"]');
  await expect(activity).toBeVisible();
  await expect(activity.locator('a[aria-label="Open issue digitaldrywood/detent-core#5294"]:visible')).toHaveCount(1);
  await expect(activity.locator('a[aria-label="Open PR #5294"]:visible')).toHaveCount(1);
  await assertAgentActivityActionsFit(page);
  await assertNoDocumentOverflow(page);
  await captureClipAndAttach(page, 'section[aria-label="Agent activity timeline"]', "project-runs-actions-desktop.png", testInfo, {
    maxHeight: 430,
  });

  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "runs-tracker-refresh-gap",
    viewport: narrowViewport,
  });

  await expect(activity).toBeVisible();
  await expect(activity.locator('a[aria-label="Open issue digitaldrywood/detent-core#5294"]:visible')).toHaveCount(1);
  await expect(activity.locator('a[aria-label="Open PR #5294"]:visible')).toHaveCount(1);
  await assertAgentActivityActionsFit(page);
  await assertElementFitsViewport(page, 'section[aria-label="Agent activity timeline"]');
  await captureClipAndAttach(page, 'section[aria-label="Agent activity timeline"]', "project-runs-actions-narrow.png", testInfo, {
    maxHeight: 520,
  });
});

test("project runs keeps active identities readable without column overlap", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "runs-long-content",
    viewport: desktopViewport,
  });

  const activeIdentity = "digitaldrywood/creswoodcorners-phone #66 · PR #75";
  const pipelineIdentity = "digitaldrywood/detent-core #5290 · PR #5290";
  const running = page.locator("#running-issues");
  const activity = page.locator('section[aria-label="Agent activity timeline"]');
  const pipeline = page.locator('section[aria-label="Pull request pipeline"]');

  await expect(running.locator(`[data-issue-identity="${activeIdentity}"]:visible`)).toHaveCount(1);
  await expect(activity.locator(`[data-agent-identity="${activeIdentity}"]:visible`)).toHaveCount(1);
  await expect(pipeline.locator(`[data-pr-pipeline-identity="${pipelineIdentity}"]:visible`)).toHaveCount(1);
  await assertPrimaryIdentitiesReadable(page, "#running-issues");
  await assertPrimaryIdentitiesReadable(page, 'section[aria-label="Agent activity timeline"]');
  await assertPrimaryIdentitiesReadable(page, 'section[aria-label="Pull request pipeline"]');
  await assertRunningTableColumnsDoNotOverlap(page);
  await assertElementsDoNotOverlap(page, "#github-api-health", ".dashboard-topbar");
  await assertNoDocumentOverflow(page);
  await running.scrollIntoViewIfNeeded();
  await captureClipAndAttach(page, "#running-issues", "project-runs-identities-desktop.png", testInfo, {
    maxHeight: 520,
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

test("project Kanban lane visibility overrides reset to defaults", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "kanban-terminal-states",
    viewport: desktopViewport,
  });

  const board = page.locator("#project-kanban");
  const todoLane = board.locator("[data-kanban-drop-state='Todo']");
  const cancelledLane = board.locator("[data-project-kanban-lane-title='Cancelled']");
  await expect(todoLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-empty", "false");
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(cancelledLane).toBeHidden();
  await expect(board.locator("[data-kanban-issue-id='demo-cancelled']")).toBeHidden();

  await board.locator("[data-project-kanban-visibility-menu] summary").click();
  await expect(board.locator("[data-project-kanban-visibility-count]")).toHaveText("6/9");
  const defaultHiddenLaneIDs = await board.locator("[data-project-kanban-lane-default-visible='false']").evaluateAll((lanes) => {
    return lanes.map((lane) => lane.dataset.projectKanbanLane).filter(Boolean).sort();
  });
  const todoCheckbox = board.locator("[data-project-kanban-visibility-checkbox][value='todo']");
  const cancelledCheckbox = board.locator("[data-project-kanban-visibility-checkbox][value='cancelled']");
  const todoReset = board.getByRole("button", { name: "Reset Todo lane to default visibility" });
  const cancelledReset = board.getByRole("button", { name: "Reset Cancelled lane to default visibility" });
  await expect(todoCheckbox).toBeChecked();
  await expect(cancelledCheckbox).not.toBeChecked();
  await expect(cancelledCheckbox).toBeEnabled();
  await expect(cancelledCheckbox.locator("xpath=ancestor::label")).toContainText("1");
  await expect(todoCheckbox.locator("xpath=ancestor::*[@data-project-kanban-visibility-row]")).toContainText("Default visible");
  await expect(cancelledCheckbox.locator("xpath=ancestor::*[@data-project-kanban-visibility-row]")).toContainText("Default hidden");

  await todoCheckbox.uncheck();
  await expect(todoLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(todoLane).toBeHidden();
  await expect(todoCheckbox.locator("xpath=ancestor::*[@data-project-kanban-visibility-row]")).toContainText("Forced hidden");
  await expectProjectKanbanVisibilityStorage(page, "project:dogfood", { show: [], hide: ["todo"] });

  await todoReset.click();
  await expect(todoLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(todoLane).toBeVisible();
  await expect(todoCheckbox).toBeChecked();
  await expect(todoCheckbox.locator("xpath=ancestor::*[@data-project-kanban-visibility-row]")).toContainText("Default visible");
  await expectProjectKanbanVisibilityStorage(page, "project:dogfood", null);

  await cancelledCheckbox.check();
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(cancelledLane).toBeVisible();
  await expect(cancelledCheckbox.locator("xpath=ancestor::*[@data-project-kanban-visibility-row]")).toContainText("Forced visible");
  await expectProjectKanbanVisibilityStorage(page, "project:dogfood", { show: ["cancelled"], hide: [] });

  await cancelledReset.click();
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(cancelledLane).toBeHidden();
  await expect(cancelledCheckbox).not.toBeChecked();
  await expect(cancelledCheckbox.locator("xpath=ancestor::*[@data-project-kanban-visibility-row]")).toContainText("Default hidden");
  await expectProjectKanbanVisibilityStorage(page, "project:dogfood", null);

  await todoCheckbox.uncheck();
  await cancelledCheckbox.check();
  await expect(todoLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expectProjectKanbanVisibilityStorage(page, "project:dogfood", { show: ["cancelled"], hide: ["todo"] });

  await page.reload({ waitUntil: "domcontentloaded" });
  await expect(board).toBeVisible();
  await expect(todoLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(cancelledLane).toBeVisible();
  await board.locator("[data-project-kanban-visibility-menu] summary").click();
  await expect(todoCheckbox).not.toBeChecked();
  await expect(cancelledCheckbox).toBeChecked();

  await board.getByRole("button", { name: "Reset to defaults" }).click();
  await expect(todoLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(cancelledLane).toBeHidden();
  await board.getByRole("button", { name: "All lanes" }).click();
  await expect(todoLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(cancelledLane).toBeVisible();
  await expectProjectKanbanVisibilityStorage(page, "project:dogfood", { show: defaultHiddenLaneIDs, hide: [] });
  await page.keyboard.press("Escape");
  await cancelledLane.locator("[data-project-kanban-pin-toggle]").click();
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(cancelledLane).toBeHidden();
  await expectProjectKanbanVisibilityStorage(page, "project:dogfood", {
    show: defaultHiddenLaneIDs.filter((id) => id !== "cancelled"),
    hide: [],
  });
  await board.locator("[data-project-kanban-visibility-menu] summary").click();
  await board.getByRole("button", { name: "Reset to defaults" }).click();
  await expect(todoLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(cancelledLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(cancelledLane).toBeHidden();
  await expectProjectKanbanVisibilityStorage(page, "project:dogfood", null);
  await captureClipAndAttach(page, "#project-kanban", "project-kanban-reset-defaults-desktop.png", testInfo, {
    maxHeight: 430,
  });
});

test("saved lane visibility keeps populated active lanes visible", async ({ page }) => {
  await page.setViewportSize(desktopViewport);
  await page.addInitScript(() => {
    window.localStorage.setItem(
      "detent.ui.projectKanban.visibleLanes.project:release-train",
      JSON.stringify({ v: 3, lanes: ["backlog"] }),
    );
  });
  await page.goto(`${kanbanRuntime.url}/projects/release-train/kanban`, { waitUntil: "domcontentloaded" });
  await page.request.post(`${kanbanRuntime.url}/api/v1/refresh`);

  const board = page.locator("#project-kanban");
  const backlogLane = board.locator("[data-kanban-drop-state='Backlog']");
  const mergingLane = board.locator("[data-kanban-drop-state='Merging']");
  const doneLane = board.locator("[data-kanban-drop-state='Done']");
  await expect(board).toBeVisible();
  await expect(backlogLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(mergingLane).toHaveAttribute("data-project-kanban-lane-empty", "false");
  await expect(mergingLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(mergingLane).toBeVisible();
  await expect(doneLane).toHaveAttribute("data-project-kanban-lane-empty", "false");
  await expect(doneLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(doneLane).toBeHidden();

  await board.locator("[data-project-kanban-visibility-menu] summary").click();
  await expect(board.locator("[data-project-kanban-visibility-count]")).toHaveText("2/10");
  await expect(board.locator("[data-project-kanban-visibility-checkbox][value='backlog']")).toBeChecked();
  await expect(board.locator("[data-project-kanban-visibility-checkbox][value='merging']")).toBeChecked();
  await expect(board.locator("[data-project-kanban-visibility-checkbox][value='done']")).not.toBeChecked();
  await expectProjectKanbanVisibilityStorage(page, "project:release-train", { show: ["backlog"], hide: [] });

  await page.evaluate(() => {
    document.dispatchEvent(new Event("htmx:afterSwap"));
    document.dispatchEvent(new Event("htmx:afterSettle"));
  });
  await expect(mergingLane).toHaveAttribute("data-project-kanban-lane-visible", "true");
  await expect(mergingLane).toBeVisible();
  await expect(doneLane).toHaveAttribute("data-project-kanban-lane-visible", "false");
  await expect(doneLane).toBeHidden();
  await expect(board.locator("[data-project-kanban-visibility-count]")).toHaveText("2/10");
});

test("project Kanban startup loading hides empty states and actions", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "kanban-startup-loading",
    viewport: desktopViewport,
  });

  const board = page.locator("#project-kanban");
  await expect(board).toBeVisible();
  await expect(board.getByText("Loading tracker state...")).toBeVisible();
  await expect(board.getByText("No issues in this state.")).toHaveCount(0);
  await expect(board.locator("[data-kanban-action]")).toHaveCount(0);
  await expect(board.locator("[data-kanban-drop-state]")).toHaveCount(0);
  await assertProjectKanbanLayout(page, "#project-kanban", { minLanes: 6 });
  await assertElementFitsViewport(page, "#project-kanban");
  await compareClipAndAttach(page, "#project-kanban", "project-kanban-startup-loading-desktop.png", testInfo, {
    maxHeight: 520,
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

test("direct Kanban blocked drag stays client-side", async ({ page }, testInfo) => {
  await page.setViewportSize(desktopViewport);
  await page.goto(`${kanbanRuntime.url}/projects/demo-project/kanban`, { waitUntil: "domcontentloaded" });
  await page.request.post(`${kanbanRuntime.url}/api/v1/refresh`);
  await expect(page.locator("#project-kanban")).toBeVisible();
  await expect(page.locator("[data-kanban-issue-id='kanban-demo-backlog']")).toBeVisible();
  await page.waitForFunction(() => Boolean(window.htmx && window.__detentKanbanDragHandlersRegistered));
  await page.locator("[data-kanban-drop-state='Merging']").evaluate((lane) => {
    lane.dataset.projectKanbanLaneVisible = "true";
  });
  await expect(page.locator("[data-kanban-drop-state='Merging']")).toBeVisible();

  const moveRequests = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname === "/api/v1/kanban/move" && request.method() === "POST") {
      moveRequests.push(request.url());
    }
  });

  const accepted = await dragKanbanCardToLane(page, "kanban-demo-backlog", "Merging");
  expect(accepted).toBe(false);
  await expect(page.locator("#kanban-feedback")).toContainText("Move blocked by transition policy.");
  expect(moveRequests).toHaveLength(0);
  await expect(page.locator("[data-kanban-drop-state='Backlog'] [data-kanban-issue-id='kanban-demo-backlog']")).toHaveCount(1);
  await expect(page.locator("[data-kanban-drop-state='Merging'] [data-kanban-issue-id='kanban-demo-backlog']")).toHaveCount(0);
  await captureClipAndAttach(page, "#project-kanban", "project-kanban-drag-blocked-client-side.png", testInfo, {
    maxHeight: 430,
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
  await expect(page.locator("#kanban-action-dialog")).not.toHaveAttribute("data-tui-dialog-open", "true");
  await expect(page.locator("#kanban-dialog-content")).toHaveText("");

  await page.locator("[data-kanban-card][data-kanban-issue-id='kanban-demo-backlog'] [aria-label^='Move ']").click();
  await expect(page.locator("#kanban-action-dialog")).toHaveAttribute("data-tui-dialog-open", "true");
  await expect(page.locator("#kanban-dialog-content")).toContainText("Move card");
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

async function attachFailureEvidence(page, testInfo) {
  const evidenceDir = path.join(process.cwd(), "tmp", "playwright-evidence", testInfo.project.name);
  fs.mkdirSync(evidenceDir, { recursive: true });
  const baseName = artifactName(testInfo.title);
  const htmlPath = path.join(evidenceDir, `${baseName}.html`);
  const screenshotPath = path.join(evidenceDir, `${baseName}.png`);

  try {
    fs.writeFileSync(htmlPath, await page.content());
    await testInfo.attach(`${baseName}.html`, { path: htmlPath, contentType: "text/html" });
  } catch (error) {
    await testInfo.attach(`${baseName}-html-error.txt`, { body: String(error), contentType: "text/plain" });
  }

  try {
    await page.screenshot({ path: screenshotPath, animations: "disabled", caret: "hide" });
    await testInfo.attach(`${baseName}.png`, { path: screenshotPath, contentType: "image/png" });
  } catch (error) {
    await testInfo.attach(`${baseName}-screenshot-error.txt`, { body: String(error), contentType: "text/plain" });
  }
}

function artifactName(title) {
  return title.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "") || "failure";
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

async function assertPrimaryIdentitiesReadable(page, selector) {
  const failures = await page.locator(selector).evaluate((root) => {
    const out = [];
    for (const identity of root.querySelectorAll("[data-primary-identity]")) {
      const identityRect = identity.getBoundingClientRect();
      const identityStyle = window.getComputedStyle(identity);
      if (
        identityStyle.display === "none" ||
        identityStyle.visibility === "hidden" ||
        identityRect.width === 0 ||
        identityRect.height === 0
      ) {
        continue;
      }
      const label = identity.getAttribute("data-primary-identity") || identity.textContent || "identity";
      const container = identity.closest("[data-issue-identity], [data-agent-identity], [data-pr-pipeline-identity]");
      const containerRect = container?.getBoundingClientRect() || identityRect;
      for (const part of identity.querySelectorAll("[data-identity-repository], [data-identity-issue], [data-identity-pr]")) {
        const style = window.getComputedStyle(part);
        const rect = part.getBoundingClientRect();
        if (style.display === "none" || style.visibility === "hidden" || rect.width === 0 || rect.height === 0) {
          out.push(`${label} has hidden identity part`);
          continue;
        }
        if (style.textOverflow === "ellipsis") {
          out.push(`${label} identity part still uses ellipsis`);
        }
        if (rect.left < containerRect.left - 1 || rect.right > containerRect.right + 1) {
          out.push(`${label} identity part escapes container bounds`);
        }
      }
    }
    return out;
  });

  expect(failures).toEqual([]);
}

async function assertRunningTableColumnsDoNotOverlap(page) {
  const failures = await page.locator("#running-issues table").evaluate((table) => {
    const out = [];
    for (const row of table.querySelectorAll("tbody tr")) {
      const activityCell = row.querySelector("[data-running-activity-cell]");
      const diffCell = row.querySelector("[data-running-diff-cell]");
      const tokensCell = row.querySelector("[data-running-tokens-cell]");
      if (!activityCell || !diffCell || !tokensCell) {
        out.push("running row is missing measured cells");
        continue;
      }
      const activityRect = activityCell.getBoundingClientRect();
      const diffRect = diffCell.getBoundingClientRect();
      const tokensRect = tokensCell.getBoundingClientRect();
      if (activityRect.right > diffRect.left + 1) {
        out.push("Codex update cell overlaps Diff cell");
      }
      if (diffRect.right > tokensRect.left + 1) {
        out.push("Diff cell overlaps Tokens cell");
      }
      for (const child of activityCell.querySelectorAll("[data-running-activity-trigger], [data-running-activity-trigger] *")) {
        const style = window.getComputedStyle(child);
        const rect = child.getBoundingClientRect();
        if (style.display === "none" || style.visibility === "hidden" || rect.width === 0 || rect.height === 0) {
          continue;
        }
        if (rect.left < activityRect.left - 1 || rect.right > activityRect.right + 1) {
          out.push("Codex update content escapes its column");
        }
      }
    }
    return out;
  });

  expect(failures).toEqual([]);
}

async function assertElementsDoNotOverlap(page, firstSelector, secondSelector) {
  const overlap = await page.evaluate(
    ({ firstSelector, secondSelector }) => {
      const first = document.querySelector(firstSelector);
      const second = document.querySelector(secondSelector);
      if (!first || !second) {
        return { missing: true, overlaps: false };
      }
      const firstRect = first.getBoundingClientRect();
      const secondRect = second.getBoundingClientRect();
      return {
        missing: false,
        overlaps:
          firstRect.left < secondRect.right - 1 &&
          firstRect.right > secondRect.left + 1 &&
          firstRect.top < secondRect.bottom - 1 &&
          firstRect.bottom > secondRect.top + 1,
      };
    },
    { firstSelector, secondSelector },
  );

  expect(overlap.missing).toBeFalsy();
  expect(overlap.overlaps).toBeFalsy();
}

async function assertAgentActivityActionsFit(page) {
  const metrics = await page.locator('section[aria-label="Agent activity timeline"]').evaluate((section) => {
    const visibleActions = [];
    const leaks = [];
    for (const action of section.querySelectorAll('a[aria-label^="Open "]')) {
      const style = window.getComputedStyle(action);
      const rect = action.getBoundingClientRect();
      if (style.display === "none" || style.visibility === "hidden" || rect.width === 0 || rect.height === 0) {
        continue;
      }
      visibleActions.push(action.getAttribute("aria-label") || "");
      const container = action.closest(".agent-activity-table-row, article");
      const containerRect = container?.getBoundingClientRect();
      if (!containerRect) {
        leaks.push(`${action.getAttribute("aria-label")} has no row container`);
        continue;
      }
      if (rect.left < containerRect.left - 1 || rect.right > containerRect.right + 1) {
        leaks.push(`${action.getAttribute("aria-label")} escapes row bounds`);
      }
    }
    return { visibleActions, leaks };
  });

  expect(metrics.visibleActions.length).toBeGreaterThanOrEqual(2);
  expect(metrics.leaks).toEqual([]);
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
