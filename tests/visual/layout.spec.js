const fs = require("node:fs");
const path = require("node:path");
const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

const desktopViewport = { width: 1440, height: 1100 };
const narrowViewport = { width: 390, height: 844 };
const groupedSidebarSequence = [
  "nav:board",
  "section:projects",
  "section:monitor",
  "nav:fleet",
  "nav:health",
  "section:insights",
  "nav:reports",
  "nav:library",
  "section:system",
  "nav:analytics",
  "nav:api-keys",
  "nav:settings",
];

let screenshotsRuntime;
let kanbanRuntime;
let singleProjectRuntime;
let sidebarRuntime;
let screenshotManifest;

test.beforeAll(async () => {
  screenshotsRuntime = await startDetentRuntime("screenshots", [
    "--demo",
    "screenshots",
  ]);
  const manifestResponse = await fetch(
    `${screenshotsRuntime.url}/api/v1/demo/scenarios`,
  );
  if (!manifestResponse.ok) {
    throw new Error(
      `Failed to load screenshots manifest: ${manifestResponse.status}`,
    );
  }
  screenshotManifest = await manifestResponse.json();
  kanbanRuntime = await startDetentRuntime("kanban", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
  singleProjectRuntime = await startDetentRuntime("single-project", []);
  sidebarRuntime = await startDetentRuntime("sidebar-live", [
    "--demo",
    "screenshots",
    "--demo-clock",
    "play",
  ]);
});

test.afterAll(async () => {
  await Promise.all([
    screenshotsRuntime?.stop(),
    kanbanRuntime?.stop(),
    singleProjectRuntime?.stop(),
    sidebarRuntime?.stop(),
  ]);
});

test.afterEach(async ({ page }, testInfo) => {
  if (testInfo.status === testInfo.expectedStatus) {
    return;
  }
  await attachFailureEvidence(page, testInfo);
});

test("screenshots manifest includes visual gate scenarios", async ({
  request,
}) => {
  const response = await request.get(
    `${screenshotsRuntime.url}/api/v1/demo/scenarios`,
  );
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
      "diagnostics-healthy",
      "settings-project-context",
      "onboarding-project-selection",
      "fleet-kanban-blocked-alerts",
    ]),
  );
});

test("sidebar groups global navigation and hides a single project", async ({
  page,
}) => {
  await openScenario(page, {
    runtime: sidebarRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  const sidebar = page.locator("#app-sidebar");
  const content = sidebar.locator("#app-sidebar-content");
  await expect(content).toHaveAttribute("sse-swap", "sidebar-v2");
  await expect(content).toHaveAttribute("hx-swap", "morph:innerHTML");
  await expect(content.locator('[data-sidebar-section="projects"]')).toBeVisible();
  expect(await content.locator("[data-sidebar-project]").count()).toBeGreaterThan(1);
  await expect
    .poll(() => sidebarSequence(content))
    .toEqual(groupedSidebarSequence);
  expect(await waitForSidebarMorph(page)).toEqual({
    preserved: true,
    swap: "morph:innerHTML",
  });
  await expect
    .poll(() => sidebarSequence(content))
    .toEqual(groupedSidebarSequence);

  await sidebar.getByRole("button", { name: "Toggle sidebar" }).click();
  await expect(sidebar).toHaveAttribute("data-rail", "true");
  for (const heading of await content.locator("[data-sidebar-section]").all()) {
    await expect(heading).toBeHidden();
  }
  for (const label of await content.locator("[data-sidebar-nav-label]").all()) {
    await expect(label).toBeHidden();
  }
  for (const icon of await content.locator("[data-sidebar-nav-icon]").all()) {
    await expect(icon).toBeVisible();
  }
  for (const label of await content.locator("[data-sidebar-project-label]").all()) {
    await expect(label).toBeHidden();
  }
  for (const project of await content.locator("[data-sidebar-project]").all()) {
    await expect(project).toBeVisible();
    await expect(project.locator('span[aria-hidden="true"]').first()).toBeVisible();
  }

  await page.setExtraHTTPHeaders({});
  await page.goto(`${singleProjectRuntime.url}/`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator("#board-lanes").waitFor({ state: "visible" });
  const singleProjectContent = page.locator("#app-sidebar-content");
  await expect(singleProjectContent.locator('[data-sidebar-section="projects"]')).toHaveCount(0);
  await expect(singleProjectContent.locator("[data-sidebar-project]")).toHaveCount(0);
});

test("sidebar project badges keep load, activity, blocked tint, and breakdown distinct", async ({
  page,
}) => {
  await openScenario(page, {
    runtime: sidebarRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  const detent = page.locator('[data-sidebar-project="dogfood"]');
  const detentBadge = detent.locator("[data-sidebar-project-badge]");
  await expect(detentBadge).toHaveText("4");
  await expect(detentBadge).toHaveAttribute(
    "data-sidebar-project-blocked",
    "true",
  );
  await expect(detentBadge).toHaveClass(/bg-err\/15/);
  await expect(detent.locator('[data-sidebar-project-activity="true"] .dt-pulse')).toHaveClass(/bg-ok/);
  await expect(detent).toHaveAttribute(
    "data-help-description",
    "1 todo · 3 active · 0 waiting · 1 blocked",
  );

  const docs = page.locator('[data-sidebar-project="docs-site"]');
  await expect(docs.locator("[data-sidebar-project-badge]")).toHaveText("3");
  await expect(docs.locator("[data-sidebar-project-badge]")).toHaveAttribute(
    "data-sidebar-project-blocked",
    "false",
  );
  await expect(docs.locator('[data-sidebar-project-activity="true"] .dt-pulse')).toHaveClass(/bg-ok/);

  for (const density of ["compact", "cozy"]) {
    await page.locator(`[data-density-choice="${density}"]`).click();
    for (const theme of ["dark", "light"]) {
      await page.evaluate((value) => {
        if (value === "light") document.documentElement.setAttribute("data-theme", "light");
        else document.documentElement.removeAttribute("data-theme");
      }, theme);
      await expect(detentBadge).toHaveText("4");
      await expect(detentBadge).toHaveAttribute(
        "data-sidebar-project-blocked",
        "true",
      );
      await expect(detent.locator('[data-sidebar-project-activity="true"] .dt-pulse')).toBeVisible();
    }
  }
  await page.locator('[data-density-choice="compact"]').click();
  await page.evaluate(() => document.documentElement.removeAttribute("data-theme"));

  await detent.hover();
  const tooltip = page.locator("body > #help-tooltip");
  await expect(tooltip).toBeVisible();
  await expect(tooltip).toContainText(
    "1 todo · 3 active · 0 waiting · 1 blocked",
  );
  await waitForSidebarMorph(page);
  await expect(tooltip).toBeVisible();
  await expect(tooltip).toContainText(
    "1 todo · 3 active · 0 waiting · 1 blocked",
  );

  await expect(page.locator("#fig-waiting")).toContainText("1 waiting");
  await expect(page.locator("#fig-blocked")).toContainText("1 blocked");
});

test("board home renders lanes without page overflow", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  await expect(page.locator("#board-lanes")).toBeVisible();
  await expect(page.locator("#board-figures")).toBeVisible();
  await expect(page.locator("[data-board-lane]").first()).toBeVisible();
  const ageFooter = page.locator("[data-board-card-age-footer]").first();
  await expect(ageFooter).toBeVisible();
  await expect(ageFooter).toContainText("In lane");
  await assertNoDocumentOverflow(page);
  await capturePageAndAttach(page, "board-home.png", testInfo);
});

test("board keeps dependency waits on cards without global alerts", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-kanban-multiproject",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  const exceptions = page.locator("#board-exceptions");
  const blockedFigure = page.locator("#fig-blocked");
  await expect(blockedFigure).toBeVisible();
  await expect(exceptions).toHaveCount(0);
  const waitingCard = page.locator("#board-lanes article", {
    hasText: "Dependency issue waiting on ledger migration",
  });
  await expect(waitingCard).toBeVisible();
  await expect(waitingCard).toContainText("waiting -");
  await capturePageAndAttach(page, "board-dependency-waits.png", testInfo);
});

test("boosted card explains its direct downstream count", async ({ page }) => {
  await page.setViewportSize(desktopViewport);
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-kanban-unblocker-boost",
  });
  await page.goto(`${screenshotsRuntime.url}/`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  const card = page.locator("article", {
    hasText: "Add screenshot manifest smoke test",
  });
  const badge = card.locator('[data-board-priority]', {
    hasText: "unblocker",
  });
  await expect(badge).toHaveAttribute(
    "data-help-description",
    "Unblocks 2 issues.",
  );
  await badge.hover();
  await badge.dispatchEvent("pointerover", { pointerType: "mouse" });
  const tooltip = page.locator("body > #help-tooltip");
  await expect(tooltip).toBeVisible();
  await expect(tooltip).toContainText("Unblocks 2 issues");
});

test("board elevated blockers render one compact opt-in alert", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-kanban-blocked-alerts",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  const exceptions = page.locator("#board-exceptions [id^='exception-']");
  await expect(exceptions).toHaveCount(1);
  await expect(exceptions.first()).toContainText("Needs review");
  await expect(exceptions.first()).toContainText("after_create hook exited 2");
  await expect(page.locator("#board-exceptions")).not.toContainText(
    "Dependency waiting",
  );
  await capturePageAndAttach(page, "board-blocked-alerts.png", testInfo);
});

test("board hides informational recovery and overload notices", async ({
  page,
}) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "board-ramp-active-recoveries",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  await expect(page.locator("#dispatch-recovery-status")).toHaveCount(0);
  await expect(page.locator("#backend-overload-retries")).toHaveCount(0);
  await expect(page.locator("#snapshot > :visible").first()).toHaveAttribute(
    "id",
    "board-figures",
  );
});

test("board hides scheduled pacing while health retains its signal", async ({
  page,
}) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "board-scheduled-pacing",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  await expect(page.locator("#dispatch-recovery-status")).toHaveCount(0);
  await expect(page.locator("#backend-capacity-outage")).toHaveCount(0);
  await expect(page.locator("#snapshot > :visible").first()).toHaveAttribute(
    "id",
    "board-figures",
  );
  await expect(
    page.locator('[data-sidebar-nav-item="health"] .bg-warn'),
  ).toBeVisible();
});

test("board shows only health states needing attention", async ({ page }) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "board-degraded-health-banners",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  const failure = page.locator("#project-failure-breaker");
  const recovery = page.locator("#dispatch-recovery-status");
  const capacity = page.locator("#backend-capacity-outage");
  await expect(failure).toHaveCount(1);
  await expect(recovery).toHaveCount(1);
  await expect(capacity).toHaveCount(0);
  await expect(failure).toHaveClass(/border-warn/);
  await expect(recovery).toHaveClass(/border-warn/);
  await expect(failure.locator("p")).toHaveCount(1);
  await expect(recovery.locator("p")).toHaveCount(1);
  await expect(failure).toContainText("2 projects");
  await expect(recovery).toContainText(
    "Dispatch retry overdue for GitHub REST capacity — 1 project",
  );
  await expect(page.locator("#backend-overload-retries")).toHaveCount(0);
  await expect(page.locator("#snapshot")).not.toContainText(
    "Dispatch recovery ramp active",
  );
});

test("board lane picker hides and restores lanes", async ({ page }) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  const firstLane = page
    .locator(
      '[data-board-lane][data-lane-hidden="false"][data-board-lane-card-count]:not([data-board-lane-card-count="0"])',
    )
    .first();
  const laneID = await firstLane.getAttribute("data-board-lane");
  const lane = page.locator(`[data-board-lane="${laneID}"]`);
  const visibility = page.locator(`[data-board-lane-visibility="${laneID}"]`);
  const reset = page.locator(`[data-board-lane-reset="${laneID}"]`);
  const hiddenBadge = page.locator("[data-board-hidden-card-count]");

  await page.locator("#board-lane-picker summary").click();
  await visibility.selectOption("hide");
  await expect(lane).toBeHidden();
  await expect(visibility).toHaveValue("hide");
  await expect(hiddenBadge).toBeVisible();
  await expect(hiddenBadge).toContainText("hidden");
  await reset.click();
  await expect(lane).toBeVisible();
  await expect(visibility).toHaveValue("auto");
});

test("board applies persisted todo visibility before snapshot morphs", async ({
  page,
}) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-kanban-multiproject",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  const laneID = "todo";
  const lane = page.locator(`[data-board-lane="${laneID}"]`);
  const visibility = page.locator(`[data-board-lane-visibility="${laneID}"]`);
  const count = page.locator("[data-board-lane-count]");
  const boardKey = await page
    .locator("[data-board-lanes]")
    .getAttribute("data-board-key");

  await page.evaluate(
    ({ boardKey, laneID }) => {
      localStorage.setItem(
        `detent.ui.board.lanes.v2.${boardKey}`,
        JSON.stringify({ v: 1, show: [laneID] }),
      );
      document.dispatchEvent(new Event("htmx:afterSettle"));
    },
    { boardKey, laneID },
  );

  await expect(lane).toBeVisible();
  await expect(visibility).toHaveValue("show");
  await expect(count).toHaveText("8/9");

  const incomingSnapshot = await page.evaluate((laneID) => {
    const template = document.createElement("template");
    const snapshot = document.querySelector("#snapshot");
    template.innerHTML = snapshot ? snapshot.innerHTML : "";
    const lane = template.content.querySelector(
      `[data-board-lane="${laneID}"]`,
    );
    if (lane) {
      lane.setAttribute("data-board-lane-default", "false");
      lane.setAttribute("data-lane-hidden", "true");
    }
    const visibility = template.content.querySelector(
      `[data-board-lane-visibility="${laneID}"]`,
    );
    if (visibility) {
      visibility.value = "auto";
      visibility.setAttribute("data-board-lane-visibility-state", "auto");
      visibility.setAttribute("data-board-lane-visibility-effective", "false");
    }
    const lanes = Array.from(
      template.content.querySelectorAll("[data-board-lane]"),
    );
    const visible = lanes.filter(
      (lane) => lane.getAttribute("data-lane-hidden") !== "true",
    ).length;
    const count = template.content.querySelector("[data-board-lane-count]");
    if (count) {
      count.textContent = `${visible}/${lanes.length}`;
    }
    return template.innerHTML;
  }, laneID);

  await page.route("**/__detent-test-board-refresh", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "text/html; charset=utf-8",
      body: incomingSnapshot,
    });
  });

  await startLaneHiddenRecorder(page, laneID);
  await page.evaluate(
    () =>
      new Promise((resolve) => {
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
        window.htmx.ajax("GET", "/__detent-test-board-refresh", {
          target: "#snapshot",
          swap: "morph:innerHTML",
        });
      }),
  );

  const hiddenValues = await laneHiddenValues(page);
  expect(hiddenValues).not.toContain("true");
  await expect(lane).toBeVisible();
  await expect(visibility).toHaveValue("show");
  await expect(count).toHaveText("8/9");

  await startLaneHiddenRecorder(page, laneID);
  await page.evaluate(
    (incomingSnapshot) =>
      new Promise((resolve) => {
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
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
    incomingSnapshot,
  );

  const sseHiddenValues = await laneHiddenValues(page);
  expect(sseHiddenValues).not.toContain("true");
  await expect(lane).toBeVisible();
  await expect(visibility).toHaveValue("show");
  await expect(count).toHaveText("8/9");
});

test("board card opens the detail sheet", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  await page.locator("#board-lanes article[id^='card-']").first().click();
  const sheet = page.locator("[data-detail-sheet]");
  await expect(sheet).toBeVisible();
  await expect(sheet.getByText("State", { exact: true })).toBeVisible();
  await expect(sheet.locator("#board-activity-stream")).toBeVisible();
  await expect(sheet.getByText("Orchestration activity")).toBeVisible();
  await capturePageAndAttach(page, "board-detail-sheet.png", testInfo);

  await page.keyboard.press("Escape");
  await expect(sheet).toHaveCount(0);
});

test("long activity history stays contained across display modes", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  await page.locator("#board-lanes article[id^='card-']").first().click();
  const sheet = page.locator("[data-detail-sheet]");
  const activity = sheet.locator("[data-sheet-activity-tabs]");
  const activityPanel = sheet.locator('[data-sheet-panel="activity"]');
  const activityScroll = sheet.locator("[data-activity-list-scroll]");
  const labels = sheet.getByText("Labels", { exact: true });
  const conversation = sheet.getByText("Conversation", { exact: true });
  await seedLongActivityHistory(activityPanel, 140);

  for (const density of ["compact", "cozy"]) {
    await page.evaluate((value) => {
      if (value === "cozy") document.documentElement.setAttribute("data-density", "cozy");
      else document.documentElement.removeAttribute("data-density");
    }, density);
    for (const theme of ["dark", "light"]) {
      await page.evaluate((value) => {
        if (value === "light") document.documentElement.setAttribute("data-theme", "light");
        else document.documentElement.removeAttribute("data-theme");
      }, theme);

      await expect(activityScroll).toHaveCSS("overflow-y", "auto");
      expect(
        await activityScroll.evaluate(
          (element) => element.scrollHeight > element.clientHeight,
        ),
      ).toBeTruthy();
      const [activityBox, activityScrollBox, labelsBox, conversationBox] =
        await Promise.all([
          activity.boundingBox(),
          activityScroll.boundingBox(),
          labels.boundingBox(),
          conversation.boundingBox(),
        ]);
      expect(activityBox).not.toBeNull();
      expect(activityScrollBox).not.toBeNull();
      expect(labelsBox).not.toBeNull();
      expect(conversationBox).not.toBeNull();
      expect(
        activityScrollBox.y + activityScrollBox.height,
      ).toBeLessThanOrEqual(activityBox.y + activityBox.height);
      expect(activityBox.y + activityBox.height).toBeLessThanOrEqual(
        labelsBox.y,
      );
      expect(labelsBox.y + labelsBox.height).toBeLessThanOrEqual(
        conversationBox.y,
      );
      await capturePageAndAttach(
        page,
        `board-detail-long-activity-${density}-${theme}.png`,
        testInfo,
      );
    }
  }

  await sheet.getByRole("tab", { name: "Live session" }).click();
  await expect(sheet.getByText("No active worker session")).toBeVisible();
});

test("live session logs stay left aligned in sheet and full-page views", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  const runningBadge = page.locator("[data-board-runtime-badge]", {
    hasText: "agent working",
  }).first();
  await runningBadge.locator("xpath=ancestor::article").click();

  const sheet = page.locator("[data-detail-sheet]");
  await sheet.getByRole("tab", { name: "Live session" }).click();
  const sheetSession = sheet.locator("[data-board-live-session]");
  const popOut = sheetSession.getByRole("link", { name: "Open full-page view" });
  await expect(popOut).toHaveAttribute("target", "_blank");
  const [fullPage] = await Promise.all([
    page.waitForEvent("popup"),
    popOut.click(),
  ]);
  await fullPage.locator("[data-live-session-page]").waitFor({ state: "visible" });

  await seedMixedSessionLog(sheetSession);
  await assertSessionLogStartsAtColumnZero(sheetSession);
  await attachScreenshotEvidence(page, "board-live-session-sheet.png", testInfo);

  const fullPageSession = fullPage.locator("[data-board-live-session]");
  await seedMixedSessionLog(fullPageSession);
  await assertSessionLogStartsAtColumnZero(fullPageSession);
  await attachScreenshotEvidence(
    fullPage,
    "board-live-session-full-page.png",
    testInfo,
  );
});

test("detail sheet activity tabs survive morphs across display modes", async ({
  page,
}) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  await page.locator('[data-density-choice="cozy"]').click();

  const runningBadge = page.locator('[data-board-runtime-badge]', {
    hasText: "agent working",
  }).first();
  await runningBadge.locator("xpath=ancestor::article").click();

  const sheet = page.locator("[data-detail-sheet]");
  await expect(sheet.locator("#board-activity-stream")).toBeVisible();
  await sheet.getByRole("button", { name: "Verbose" }).click();
  await expect(sheet.getByRole("button", { name: "Hide usage ticks" })).toBeVisible();

  const sessionTab = sheet.getByRole("tab", { name: "Live session" });
  await sessionTab.click();
  await expect(sessionTab).toHaveAttribute("aria-selected", "true");
  await expect(sheet.getByText("No active worker session")).toBeVisible();

  const sessionHost = sheet.locator("[data-board-live-session]");
  await sessionHost.evaluate((host) => {
    const probe = document.createElement("span");
    probe.dataset.sessionPreserveProbe = "true";
    probe.textContent = "preserved session content";
    host.append(probe);
  });

  await page.evaluate(() =>
    document.documentElement.setAttribute("data-theme", "light"),
  );
  await assertNoDocumentOverflow(page);

  await morphCurrentSnapshot(page, "detail-sheet-activity");
  await expect(sessionTab).toHaveAttribute("aria-selected", "true");
  await expect(sheet.getByText("preserved session content")).toBeVisible();

  await sheet.getByRole("tab", { name: "Timeline" }).click();
  await expect(sheet.locator("#board-activity-stream")).toBeVisible();
  await expect(sheet.getByText("preserved session content")).toHaveCount(0);
});

test("board runtime identity stays accessible across snapshot morphs", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  await page.locator('[data-density-choice="compact"]').click();
  const fallbackBadge = page
    .locator('[data-board-runtime-badge]', { hasText: "agent working" })
    .first();
  const fallbackCard = fallbackBadge.locator("xpath=ancestor::article");
  const fallbackID = await fallbackBadge.getAttribute("id");
  const fallbackCardID = await fallbackCard.getAttribute("id");
  expect(fallbackID).not.toBeNull();
  expect(fallbackCardID).not.toBeNull();
  await expect(fallbackBadge).toContainText("agent working");
  expect(
    await fallbackBadge.evaluate((element) =>
      element.hasAttribute("data-help-trigger"),
    ),
  ).toBe(false);
  const fallbackHeight = await fallbackCard.evaluate((element) =>
    element.getBoundingClientRect().height,
  );
  await fallbackBadge.evaluate((element) => {
    window.__detentRuntimeFallbackBadge = element;
  });
  const resolvedSnapshot = await page.locator("#snapshot").evaluate(
    (snapshot, runtimeBadgeID) => {
      const container = document.createElement("div");
      container.innerHTML = snapshot.innerHTML;
      const fallback = container.querySelector(`#${runtimeBadgeID}`);
      const resolved = container.querySelector(
        '[data-board-runtime-badge][data-help-description*="Provider session: thread-demo-core-5260"]',
      );
      const replacement = resolved.cloneNode(true);
      replacement.id = runtimeBadgeID;
      replacement.dataset.helpTerm = `${runtimeBadgeID}-runtime-identity`;
      fallback.replaceWith(replacement);
      return container.innerHTML;
    },
    fallbackID,
  );
  await morphSnapshot(page, "fallback-upgrade", resolvedSnapshot);
  const upgradedBadge = page.locator(`#${fallbackID}`);
  expect(
    await upgradedBadge.evaluate(
      (element) => window.__detentRuntimeFallbackBadge === element,
    ),
  ).toBe(true);
  await expect(upgradedBadge).not.toContainText("agent working");
  await expect(upgradedBadge).toContainText("gpt-5.6-sol · xhigh");
  expect(
    await page
      .locator(`#${fallbackCardID}`)
      .evaluate((element) => element.getBoundingClientRect().height),
  ).toBe(fallbackHeight);

  const card = page.locator("article", {
    hasText: "Implement page-addressable screenshot scenarios",
  });
  const badge = card.locator(
    '[data-board-runtime-badge][data-help-description*="Provider session: thread-demo-core-5260"]',
  );
  const compactIdentity = badge.locator('[data-runtime-density="compact"]');
  const cozyIdentity = badge.locator('[data-runtime-density="cozy"]');
  await expect(badge).toHaveAttribute(
    "data-help-description",
    "Provider: openai · Provider session: thread-demo-core-5260 · Role: code · Detent session: 5260",
  );
  await expect(compactIdentity).toBeVisible();
  await expect(compactIdentity).toHaveText("gpt-5.6-sol · xhigh");
  await expect(cozyIdentity).toBeHidden();
  await page.locator('[data-density-choice="cozy"]').click();
  await expect(compactIdentity).toBeHidden();
  await expect(cozyIdentity).toBeVisible();
  await expect(cozyIdentity).toHaveText("Codex · gpt-5.6-sol · xhigh");
  await page.locator('[data-density-choice="compact"]').click();
  const initialHeight = await card.evaluate((element) =>
    element.getBoundingClientRect().height,
  );
  await badge.hover();
  const tooltip = page.locator("body > #help-tooltip");
  await expect(tooltip).toBeVisible();
  await expect(tooltip).toContainText("Provider: openai");
  await expect(tooltip).toContainText(
    "Provider session: thread-demo-core-5260",
  );
  await expect(tooltip).toContainText("Role: code");
  await expect(tooltip).toContainText("Detent session: 5260");
  await page.evaluate(() =>
    document.documentElement.setAttribute("data-theme", "light"),
  );
  await expect(badge).toHaveCSS("color", "rgb(4, 108, 78)");
  await expect(tooltip).toHaveCSS("background-color", "rgb(240, 242, 245)");
  await page.evaluate(() =>
    document.documentElement.removeAttribute("data-theme"),
  );
  await expect(page.locator("#snapshot #help-tooltip")).toHaveCount(0);
  const initialTooltipBox = await tooltip.boundingBox();
  const initialCardBox = await card.boundingBox();
  expect(initialTooltipBox).not.toBeNull();
  expect(initialCardBox).not.toBeNull();
  expect(initialTooltipBox.x).toBeGreaterThanOrEqual(initialCardBox.x - 1);
  expect(initialTooltipBox.x + initialTooltipBox.width).toBeLessThanOrEqual(
    initialCardBox.x + initialCardBox.width + 1,
  );
  expect(initialTooltipBox.y).toBeGreaterThanOrEqual(initialCardBox.y - 1);
  expect(initialTooltipBox.y + initialTooltipBox.height).toBeLessThanOrEqual(
    initialCardBox.y + initialCardBox.height + 1,
  );
  await badge.focus();
  await expect(badge).toBeFocused();

  for (let index = 0; index < 3; index += 1) {
    await morphCurrentSnapshot(page, `tooltip-${index}`);
    await expect(badge).toBeFocused();
    await expect(tooltip).toBeVisible();
  }
  const settledHeight = await card.evaluate((element) =>
    element.getBoundingClientRect().height,
  );
  expect(settledHeight).toBe(initialHeight);
  await attachScreenshotEvidence(
    page,
    "board-runtime-identity-desktop.png",
    testInfo,
  );

  await page.setViewportSize(narrowViewport);
  await badge.scrollIntoViewIfNeeded();
  await badge.focus();
  await expect(tooltip).toBeVisible();
  const tooltipBox = await tooltip.boundingBox();
  expect(tooltipBox).not.toBeNull();
  expect(tooltipBox.x).toBeGreaterThanOrEqual(0);
  expect(tooltipBox.x + tooltipBox.width).toBeLessThanOrEqual(
    narrowViewport.width,
  );

  let detailRequests = 0;
  page.on("request", (request) => {
    if (request.url().includes("/api/v1/board/card?")) {
      detailRequests += 1;
    }
  });
  await card.focus();
  await card.press("Enter");
  const sheet = page.locator("[data-detail-sheet]");
  await expect(sheet).toBeVisible();
  for (const text of [
    "Agent system",
    "Codex",
    "Backend profile",
    "codex-high",
    "Provider",
    "openai · runtime",
    "Model",
    "gpt-5.6-sol · runtime",
    "Effort",
    "xhigh · runtime",
  ]) {
    await expect(sheet).toContainText(text);
  }

  for (let index = 0; index < 3; index += 1) {
    const expectedRequests = detailRequests + 1;
    await morphCurrentSnapshot(page, `sheet-${index}`);
    await expect(sheet).toBeVisible();
    await expect.poll(() => detailRequests).toBeGreaterThanOrEqual(
      expectedRequests,
    );
  }
  await expect.poll(() => detailRequests).toBeGreaterThanOrEqual(4);
  const sheetBox = await sheet.boundingBox();
  expect(sheetBox).not.toBeNull();
  expect(sheetBox.x).toBeGreaterThanOrEqual(0);
  expect(sheetBox.x + sheetBox.width).toBeLessThanOrEqual(
    narrowViewport.width,
  );
  await attachScreenshotEvidence(
    page,
    "board-runtime-identity-narrow.png",
    testInfo,
  );
});

test("board card opens the detail sheet on a glide click", async ({
  page,
}) => {
  await page.setViewportSize(desktopViewport);
  await page.goto(`${kanbanRuntime.url}/`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  const card = page.locator("[data-kanban-card][data-kanban-action='move']", {
    hasText: "Kanban demo backlog intake",
  });
  await expect(card).toBeVisible();
  const box = await card.boundingBox();
  if (!box) {
    throw new Error("Draggable card has no bounding box");
  }

  // Real mouse clicks travel a few pixels between press and release —
  // often past the drag threshold. That gesture must still read as a
  // click and open the sheet, not become a cancelled drag that swallows
  // the click.
  const startX = box.x + box.width / 2;
  const startY = box.y + box.height / 2;
  await page.mouse.move(startX, startY);
  await page.mouse.down();
  await page.mouse.move(startX + 8, startY + 6, { steps: 2 });
  await page.mouse.up();

  const sheet = page.locator("[data-detail-sheet]");
  await expect(sheet).toBeVisible();
  await expect(
    page.locator("[data-kanban-drop-allowed]"),
  ).toHaveCount(0);

  await page.keyboard.press("Escape");
  await expect(sheet).toHaveCount(0);

  // A plain zero-travel click must keep working too.
  await card.click();
  await expect(sheet).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(sheet).toHaveCount(0);
});

test("fleet page shows agent hero, PR lanes, and metric cards", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/fleet",
    waitSelector: "#agent-activity",
    viewport: desktopViewport,
  });

  await expect(page.locator("#agent-activity")).toBeVisible();
  await expect(page.locator("#fleet-pr-pipeline")).toBeVisible();
  await expect(page.locator("[data-merge-queue-depth]")).toContainText("Depth 6");
  await expect(page.locator("[data-merge-queue-eta]")).toContainText("Drain ETA 12m 0s");
  await expect(page.locator("#pr-lane-merging")).toContainText("Native #2 of 6 · ~12m 0s");
  await expect(page.locator("#fleet-metrics")).toBeVisible();
  await assertNoDocumentOverflow(page);
  await capturePageAndAttach(page, "fleet.png", testInfo);

  await page.setViewportSize(narrowViewport);
  await expect(page.locator("[data-merge-queue-summary]")).toBeVisible();
  await expect(page.locator("#pr-lane-merging")).toContainText("Native #2 of 6 · ~12m 0s");
  await assertNoDocumentOverflow(page);
  await attachScreenshotEvidence(page, "fleet-merge-queue-narrow.png", testInfo);
});

test("health page covers key rate-limit states", async ({ page }, testInfo) => {
  const scenarios = [
    "github-api-healthy",
    "github-api-warning",
    "github-api-secondary-backoff",
    "github-api-primary-exhausted",
  ];

  for (const scenario of scenarios) {
    await openScenario(page, {
      runtime: screenshotsRuntime,
      scenario,
      route: "/health/ui",
      waitSelector: "#health-verdict",
      viewport: desktopViewport,
    });

    await expect(page.locator("#health-verdict")).toBeVisible();
    await expect(page.locator("#health-details")).toBeVisible();
    await assertNoDocumentOverflow(page);
    await capturePageAndAttach(page, `${scenario}.png`, testInfo);
  }
});

test("health keeps full waiting and ramp recovery detail", async ({ page }) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "health-dispatch-recoveries",
    route: "/health/ui",
    waitSelector: "#dispatch-recovery-status",
    viewport: desktopViewport,
  });

  const recoveries = page.locator("#dispatch-recovery-status");
  await expect(
    recoveries.getByText("Dispatch waiting on GitHub REST capacity", {
      exact: true,
    }),
  ).toHaveCount(2);
  await expect(
    recoveries.getByText("Dispatch recovery ramp active", { exact: true }),
  ).toHaveCount(1);
  await expect(recoveries).toContainText("Project dogfood");
  await expect(recoveries).toContainText("Project docs-site");
  await expect(recoveries).toContainText("Project billing-api");
  await expect(page.locator("#health-verdict")).not.toContainText(
    "All systems nominal",
  );
  await expect(page.locator("#backend-overload-retries")).toContainText(
    "3 overload retries last hour",
  );
});

test("health keeps full scheduled pacing detail hidden from the board", async ({
  page,
}) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "health-scheduled-pacing",
    route: "/health/ui",
    waitSelector: "#dispatch-recovery-status",
    viewport: desktopViewport,
  });

  const recoveries = page.locator("#dispatch-recovery-status");
  await expect(
    recoveries.getByText("Dispatch waiting on GitHub REST capacity", {
      exact: true,
    }),
  ).toHaveCount(2);
  await expect(recoveries).toContainText(
    "remaining 288 at or below dispatch floor",
  );
  await expect(recoveries).toContainText("rest_budget_reserved");
  await expect(page.locator("#backend-capacity-outage")).toContainText(
    "GitHub REST dispatch paused",
  );
  await expect(
    page.locator('[data-sidebar-nav-item="health"] .bg-warn'),
  ).toBeVisible();
});

test("project overview renders tabs, hero, and recent runs", async ({
  page,
}, testInfo) => {
  await page.setViewportSize(desktopViewport);
  await page.goto(`${kanbanRuntime.url}/projects/demo-project`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator("#agent-activity").waitFor({ state: "visible" });

  await expect(page.locator("nav[aria-label='Project views']")).toBeVisible();
  await expect(page.locator("#project-recent-runs")).toBeVisible();
  await expect(page.locator("#project-recent-runs")).toContainText("Efficiency receipt");
  await assertNoDocumentOverflow(page);
  await capturePageAndAttach(page, "project-overview.png", testInfo);
});

test("project runs render completed efficiency receipts", async ({ page }) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/projects/dogfood/runs",
    waitSelector: "#project-runs",
    viewport: desktopViewport,
  });

  await expect(page.locator("#project-runs")).toContainText("Efficiency receipt");
  await expect(page.locator("#project-runs")).toContainText("cached");
  await expect(page.locator("#project-runs")).toContainText("$1.75");
});

test("project kanban board scopes cards to the project", async ({
  page,
}, testInfo) => {
  await page.setViewportSize(desktopViewport);
  await page.goto(`${kanbanRuntime.url}/projects/demo-project/kanban`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  await expect(
    page.locator('[data-board-key="project.demo-project"]'),
  ).toBeVisible();
  const foreign = await page
    .locator("#board-lanes article[id^='card-']")
    .evaluateAll(
      (cards) =>
        cards.filter((card) => !card.textContent.includes("demo-project"))
          .length,
    );
  expect(foreign).toBe(0);
  await assertNoDocumentOverflow(page);
  await capturePageAndAttach(page, "project-kanban.png", testInfo);
});

test("all-project board supports drag status moves", async ({ page }) => {
  const runtime = await startDetentRuntime("kanban-fleet-drag", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
  try {
    await page.setViewportSize(desktopViewport);
    await page.goto(`${runtime.url}/`, {
      waitUntil: "domcontentloaded",
    });
    await page.locator("#board-lanes").waitFor({ state: "visible" });

    const card = page.locator("[data-kanban-card]", {
      hasText: "Kanban demo backlog intake",
    });
    await expect(card).toHaveAttribute("data-kanban-action", "move");
    await expect(card).not.toHaveAttribute("data-kanban-move-disabled", "true");

    const sourceLane = page.locator('[data-kanban-drop-state="Backlog"]');
    const targetLane = page.locator('[data-kanban-drop-state="Todo"]');

    const moveRequest = page.waitForRequest((request) => {
      if (
        request.method() !== "POST" ||
        !request.url().endsWith("/api/v1/kanban/move")
      ) {
        return false;
      }
      const form = new URLSearchParams(request.postData() || "");
      return form.get("kanban_drag") === "true";
    });

    const box = await card.boundingBox();
    if (!box) {
      throw new Error("Fleet drag source has no bounding box");
    }
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(
      box.x + box.width / 2 + 16,
      box.y + box.height / 2 + 16,
      { steps: 5 },
    );
    await expect(targetLane).toBeVisible();
    const targetBox = await targetLane.boundingBox();
    if (!targetBox) {
      throw new Error("Fleet drag target lane has no bounding box");
    }
    await page.mouse.move(
      targetBox.x + targetBox.width / 2,
      targetBox.y + Math.min(80, targetBox.height / 2),
      { steps: 20 },
    );
    await page.mouse.up();

    const request = await moveRequest;
    const form = new URLSearchParams(request.postData() || "");
    expect(form.get("kanban_board")).toBe("fleet");
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
  } finally {
    await runtime.stop();
  }
});

test("pointer drag shows affordances past the threshold and cancels cleanly", async ({
  page,
}) => {
  await page.setViewportSize(desktopViewport);
  await page.goto(`${kanbanRuntime.url}/projects/demo-project/kanban`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  const card = page.locator(
    '[data-kanban-card][data-kanban-current-state="Backlog"]',
    {
      hasText: "Kanban demo backlog intake",
    },
  );
  await expect(card).toHaveAttribute("data-kanban-action", "move");

  const feedback = page.locator("#board-feedback");
  const hiddenLanes = page.locator(
    '[data-kanban-drop-state][data-lane-hidden="true"]',
  );
  const ghost = page.locator("body > [data-kanban-card][aria-hidden='true']");
  const hiddenBefore = await hiddenLanes.count();
  expect(hiddenBefore).toBeGreaterThan(0);

  const box = await card.boundingBox();
  if (!box) {
    throw new Error("Drag source has no bounding box");
  }
  const centerX = box.x + box.width / 2;
  const centerY = box.y + box.height / 2;

  // Below the 6px threshold nothing happens: no ghost, no lane changes.
  await page.mouse.move(centerX, centerY);
  await page.mouse.down();
  await page.mouse.move(centerX + 3, centerY + 3);
  await expect(ghost).toHaveCount(0);
  await expect(feedback).toBeHidden();
  expect(await hiddenLanes.count()).toBe(hiddenBefore);

  // Past the threshold the drag activates: ghost follows the pointer, all
  // lanes unhide with allowed/blocked states, and the feedback line reports
  // the move in flight.
  await page.mouse.move(centerX + 24, centerY + 24, { steps: 4 });
  await expect(ghost).toHaveCount(1);
  await expect(ghost).toContainText("From Backlog");
  await expect(feedback).toHaveText(/Moving Backlog/);
  await expect(hiddenLanes).toHaveCount(0);
  await expect(
    page.locator('[data-kanban-drop-state][data-kanban-drop-allowed="true"]'),
  ).not.toHaveCount(0);
  // The origin lane is marked as the source, not styled as a blocked target.
  const sourceLane = page.locator('[data-kanban-drop-state="Backlog"]');
  await expect(sourceLane).toHaveAttribute("data-kanban-drop-source", "true");
  await expect(sourceLane).not.toHaveAttribute(
    "data-kanban-drop-allowed",
    "false",
  );

  // Escape cancels: ghost removed, hidden lanes restored, move reported as
  // cancelled, and no request was posted.
  let moveRequests = 0;
  page.on("request", (request) => {
    if (
      request.method() === "POST" &&
      request.url().endsWith("/api/v1/kanban/move")
    ) {
      moveRequests += 1;
    }
  });
  await page.keyboard.press("Escape");
  await expect(ghost).toHaveCount(0);
  await expect(feedback).toHaveText(/Move cancelled/);
  await expect(sourceLane).not.toHaveAttribute(
    "data-kanban-drop-source",
    "true",
  );
  expect(await hiddenLanes.count()).toBe(hiddenBefore);
  await page.mouse.up();
  expect(moveRequests).toBe(0);

  // The card is still where it started and did not open the detail sheet.
  await expect(
    page.locator('[data-kanban-drop-state="Backlog"] [data-kanban-card]', {
      hasText: "Kanban demo backlog intake",
    }),
  ).toBeVisible();
});

test("project kanban board supports drag status moves", async ({ page }) => {
  await page.setViewportSize(desktopViewport);
  await page.goto(`${kanbanRuntime.url}/projects/demo-project/kanban`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  const card = page.locator(
    '[data-kanban-card][data-kanban-current-state="Backlog"]',
    {
      hasText: "Kanban demo backlog intake",
    },
  );
  const sourceLane = page.locator('[data-kanban-drop-state="Backlog"]');
  const targetLane = page.locator('[data-kanban-drop-state="Todo"]');
  await expect(card).toHaveAttribute("data-kanban-action", "move");

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

  const sourceBox = await card.boundingBox();
  if (!sourceBox) {
    throw new Error("Drag source has no bounding box");
  }
  await page.mouse.move(
    sourceBox.x + sourceBox.width / 2,
    sourceBox.y + sourceBox.height / 2,
  );
  await page.mouse.down();
  await page.mouse.move(
    sourceBox.x + sourceBox.width / 2 + 16,
    sourceBox.y + sourceBox.height / 2 + 16,
    { steps: 5 },
  );
  await expect(targetLane).toBeVisible();
  const targetBox = await targetLane.boundingBox();
  if (!targetBox) {
    throw new Error("Drag target lane has no bounding box");
  }
  await page.mouse.move(
    targetBox.x + targetBox.width / 2,
    targetBox.y + Math.min(80, targetBox.height / 2),
    { steps: 20 },
  );
  await page.mouse.up();

  const selectedText = await page.evaluate(
    () => window.getSelection()?.toString() || "",
  );
  expect(selectedText.trim()).toBe("");

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
  await expect(page.locator("#board-feedback")).toBeHidden();
});

test("project kanban move is immediate and survives a stale snapshot", async ({
  page,
}) => {
  const runtime = await startDetentRuntime("kanban-optimistic-move", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
  let releaseMove;
  const moveReleased = new Promise((resolve) => {
    releaseMove = resolve;
  });
  try {
    await page.route("**/api/v1/kanban/move", async (route) => {
      await moveReleased;
      await route.continue();
    });
    await page.setViewportSize(desktopViewport);
    await page.goto(`${runtime.url}/projects/demo-project/kanban`, {
      waitUntil: "domcontentloaded",
    });
    await page.locator("#board-lanes").waitFor({ state: "visible" });

    const staleSnapshot = await page
      .locator("#snapshot")
      .evaluate((snapshot) => snapshot.innerHTML);
    const card = page.locator(
      '[data-kanban-card][data-kanban-current-state="Backlog"]',
      { hasText: "Kanban demo backlog intake" },
    );
    const sourceLane = page.locator('[data-kanban-drop-state="Backlog"]');
    const targetLane = page.locator('[data-kanban-drop-state="Todo"]');

    const response = page.waitForResponse(
      (candidate) =>
        candidate.request().method() === "POST" &&
        candidate.url().endsWith("/api/v1/kanban/move"),
    );
    await dragKanbanCardToLane(page, card, targetLane);

    const pendingCard = targetLane.locator("[data-kanban-pending-move='Todo']", {
      hasText: "Kanban demo backlog intake",
    });
    await expect(pendingCard).toBeVisible();
    await expect(targetLane.locator("[data-kanban-empty-line]")).toBeHidden();
    await expect(
      sourceLane.locator("[data-kanban-card]", {
        hasText: "Kanban demo backlog intake",
      }),
    ).toHaveCount(0);

    await sseMorphSnapshot(page, staleSnapshot);
    await expect(pendingCard).toBeVisible();
    await expect(
      sourceLane.locator("[data-kanban-card]", {
        hasText: "Kanban demo backlog intake",
      }),
    ).toHaveCount(0);

    releaseMove();
    await response;
    await expect(
      targetLane.locator("[data-kanban-card]", {
        hasText: "Kanban demo backlog intake",
      }),
    ).toBeVisible();
    await expect(pendingCard).toHaveCount(0);
  } finally {
    releaseMove?.();
    await runtime.stop();
  }
});

test("project kanban move rolls back after a rejected response", async ({
  page,
}) => {
  const runtime = await startDetentRuntime("kanban-optimistic-rollback", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
  let rejectMove;
  const moveRejected = new Promise((resolve) => {
    rejectMove = resolve;
  });
  try {
    await page.route("**/api/v1/kanban/move", async (route) => {
      await moveRejected;
      await route.fulfill({
        status: 422,
        contentType: "text/html; charset=utf-8",
        body: "<p>Move rejected by test policy.</p>",
      });
    });
    await page.setViewportSize(desktopViewport);
    await page.goto(`${runtime.url}/projects/demo-project/kanban`, {
      waitUntil: "domcontentloaded",
    });
    await page.locator("#board-lanes").waitFor({ state: "visible" });

    const card = page.locator(
      '[data-kanban-card][data-kanban-current-state="Backlog"]',
      { hasText: "Kanban demo backlog intake" },
    );
    const sourceLane = page.locator('[data-kanban-drop-state="Backlog"]');
    const targetLane = page.locator('[data-kanban-drop-state="Todo"]');
    const response = page.waitForResponse(
      (candidate) =>
        candidate.request().method() === "POST" &&
        candidate.url().endsWith("/api/v1/kanban/move"),
    );

    await dragKanbanCardToLane(page, card, targetLane);
    await expect(
      targetLane.locator("[data-kanban-pending-move='Todo']", {
        hasText: "Kanban demo backlog intake",
      }),
    ).toBeVisible();

    rejectMove();
    expect((await response).status()).toBe(422);
    await expect(
      sourceLane.locator("[data-kanban-card]", {
        hasText: "Kanban demo backlog intake",
      }),
    ).toBeVisible();
    await expect(
      targetLane.locator("[data-kanban-card]", {
        hasText: "Kanban demo backlog intake",
      }),
    ).toHaveCount(0);
    await expect(page.locator("[data-kanban-pending-move]")).toHaveCount(0);
    await expect(targetLane).toHaveAttribute("data-lane-hidden", "true");
    await expect(page.locator("#board-feedback")).toHaveText(
      "Move rejected by test policy.",
    );
  } finally {
    rejectMove?.();
    await runtime.stop();
  }
});

test("project kanban move rolls back after a transport failure", async ({
  page,
}) => {
  const runtime = await startDetentRuntime("kanban-optimistic-send-error", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
  let failMove;
  const moveFailed = new Promise((resolve) => {
    failMove = resolve;
  });
  try {
    await page.route("**/api/v1/kanban/move", async (route) => {
      await moveFailed;
      await route.abort("connectionfailed");
    });
    await page.setViewportSize(desktopViewport);
    await page.goto(`${runtime.url}/projects/demo-project/kanban`, {
      waitUntil: "domcontentloaded",
    });
    await page.locator("#board-lanes").waitFor({ state: "visible" });

    const card = page.locator(
      '[data-kanban-card][data-kanban-current-state="Backlog"]',
      { hasText: "Kanban demo backlog intake" },
    );
    const sourceLane = page.locator('[data-kanban-drop-state="Backlog"]');
    const targetLane = page.locator('[data-kanban-drop-state="Todo"]');

    await dragKanbanCardToLane(page, card, targetLane);
    await expect(
      targetLane.locator("[data-kanban-pending-move='Todo']", {
        hasText: "Kanban demo backlog intake",
      }),
    ).toBeVisible();

    failMove();
    await expect(
      sourceLane.locator("[data-kanban-card]", {
        hasText: "Kanban demo backlog intake",
      }),
    ).toBeVisible();
    await expect(page.locator("[data-kanban-pending-move]")).toHaveCount(0);
    await expect(page.locator("#board-feedback")).toHaveText("Move failed.");
  } finally {
    failMove?.();
    await runtime.stop();
  }
});

test("project kanban drag survives snapshot refresh during drag", async ({
  page,
}) => {
  const runtime = await startDetentRuntime("kanban-drag-refresh", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
  try {
    await page.setViewportSize(desktopViewport);
    await page.goto(`${runtime.url}/projects/demo-project/kanban`, {
      waitUntil: "domcontentloaded",
    });
    await page.locator("#board-lanes").waitFor({ state: "visible" });

    const card = page.locator(
      '[data-kanban-card][data-kanban-current-state="Backlog"]',
      {
        hasText: "Kanban demo backlog intake",
      },
    );
    const sourceLane = page.locator('[data-kanban-drop-state="Backlog"]');
    const targetLane = page.locator('[data-kanban-drop-state="Todo"]');
    await expect(card).toHaveAttribute("data-kanban-action", "move");
    await expect(targetLane).toBeHidden();

    const incomingSnapshot = await page.evaluate(() => {
      const snapshot = document.querySelector("#snapshot");
      if (!snapshot) {
        throw new Error("Snapshot target not found");
      }
      return snapshot.innerHTML;
    });

    const moveRequest = page.waitForRequest(
      (request) => {
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
      },
      { timeout: 5_000 },
    );

    const sourceBox = await card.boundingBox();
    if (!sourceBox) {
      throw new Error("Drag source has no bounding box");
    }
    await page.mouse.move(
      sourceBox.x + sourceBox.width / 2,
      sourceBox.y + sourceBox.height / 2,
    );
    await page.mouse.down();
    await page.mouse.move(
      sourceBox.x + sourceBox.width / 2 + 16,
      sourceBox.y + sourceBox.height / 2 + 16,
      { steps: 5 },
    );
    await expect(targetLane).toBeVisible();

    await page.evaluate(
      (incomingSnapshot) =>
        new Promise((resolve) => {
          document.addEventListener("htmx:afterSettle", resolve, { once: true });
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
      incomingSnapshot,
    );
    await expect(targetLane).toHaveAttribute("data-lane-hidden", "false");

    const targetBox = await targetLane.boundingBox();
    if (!targetBox) {
      throw new Error("Drag target lane has no bounding box");
    }
    await page.mouse.move(
      targetBox.x + targetBox.width / 2,
      targetBox.y + Math.min(80, targetBox.height / 2),
      { steps: 20 },
    );
    await page.mouse.up();

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
  } finally {
    await runtime.stop();
  }
});

test("board applies snapshot updates without reload", async ({ page }) => {
  await page.setViewportSize(desktopViewport);
  await page.goto(`${kanbanRuntime.url}/`, { waitUntil: "domcontentloaded" });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  const marker = await page.evaluate(() => {
    window.__detentReloadMarker = true;
    return true;
  });
  expect(marker).toBe(true);

  const refresh = await fetch(`${kanbanRuntime.url}/api/v1/refresh`, {
    method: "POST",
  });
  expect(refresh.ok).toBeTruthy();

  await expect(page.locator("#board-lanes")).toBeVisible();
  const preserved = await page.evaluate(
    () => window.__detentReloadMarker === true,
  );
  expect(preserved).toBe(true);
});

test("dashboard prompts reload only when the serving build changes", async ({
  page,
}) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/fleet",
    waitSelector: "#agent-activity",
    viewport: desktopViewport,
  });

  const servedVersion = await page
    .locator("html")
    .getAttribute("data-detent-served-version");
  expect(servedVersion).toBeTruthy();

  const notice = page.locator("[data-detent-build-update]");
  const footer = page.locator("#detent-build-version");
  await dispatchBuildVersion(page, "v98.0.0", "#live-clock");
  await expect(notice).toBeHidden();
  await expect(footer).toHaveText(servedVersion);

  await dispatchBuildVersion(page, servedVersion);
  await expect(notice).toBeHidden();
  await expect(footer).toHaveText(servedVersion);

  await dispatchBuildVersion(page, "v99.0.0");
  await expect(footer).toHaveText("v99.0.0");
  await expect(notice).toBeVisible();
  await expect(notice).toContainText("Detent updated to v99.0.0 —");
  await expect(notice.locator("[data-detent-build-reload]")).toHaveText(
    "Reload",
  );
});

async function dispatchBuildVersion(
  page,
  version,
  targetSelector = "#detent-build-version",
) {
  await page.evaluate(({ liveVersion, selector }) => {
    const target = document.querySelector(selector);
    const footer = document.getElementById("detent-build-version");
    if (!target || !footer) {
      throw new Error("Build version SSE elements not found");
    }
    const incoming = footer.cloneNode(true);
    incoming.textContent = liveVersion;
    incoming.setAttribute("title", liveVersion);
    incoming.setAttribute("data-detent-build-version", liveVersion);
    target.dispatchEvent(
      new CustomEvent("htmx:sseBeforeMessage", {
        bubbles: true,
        detail: { elt: footer, data: incoming.outerHTML },
      }),
    );
  }, { liveVersion: version, selector: targetSelector });
}

test("reports page renders KPI figures and charts", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "reports-normal-window",
    route: "/reports",
    waitSelector: "#reports-kpis",
    viewport: desktopViewport,
  });

  await expect(page.locator("#reports-kpis")).toBeVisible();
  await expect(page.locator("#reports-spend")).toBeVisible();
  await expect(page.locator("#reports-top-issues")).toBeVisible();
  await expect(page.locator("#reports-efficiency")).toBeVisible();
  await expect(page.locator("#reports-efficiency")).toContainText("Tokens / merged issue");
  await expect(page.locator("#reports-efficiency")).toContainText("Baseline");
  await assertNoDocumentOverflow(page);
  await capturePageAndAttach(page, "reports.png", testInfo);
});

test("analytics page renders the scheduler log", async ({ page }, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "diagnostics-healthy",
    route: "/analytics",
    waitSelector: "#analytics-log",
    viewport: desktopViewport,
  });

  await expect(page.locator("#analytics-summary")).toBeVisible();
  await expect(page.locator("#analytics-log")).toBeVisible();
  await assertNoDocumentOverflow(page);
  await capturePageAndAttach(page, "analytics.png", testInfo);
});

test("settings page renders definition lists and preferences", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "settings-project-context",
    route: "/settings",
    waitSelector: "#settings-global",
    viewport: desktopViewport,
  });

  await expect(page.locator("#settings-preferences")).toBeVisible();
  await expect(page.locator("#settings-global")).toBeVisible();
  await assertNoDocumentOverflow(page);
  await capturePageAndAttach(page, "settings.png", testInfo);
});

test("density toggle changes rhythm across the shell", async ({ page }) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  const compactSpacing = await page.evaluate(() =>
    getComputedStyle(document.documentElement)
      .getPropertyValue("--spacing")
      .trim(),
  );
  await page.locator('[data-density-choice="cozy"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-density", "cozy");
  const cozySpacing = await page.evaluate(() =>
    getComputedStyle(document.documentElement)
      .getPropertyValue("--spacing")
      .trim(),
  );
  expect(compactSpacing).toBe("4px");
  expect(cozySpacing).toBe("5px");
  await page.locator('[data-density-choice="compact"]').click();
});

test("light theme applies through the token cascade", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  await page.evaluate(() =>
    document.documentElement.setAttribute("data-theme", "light"),
  );
  const background = await page.evaluate(
    () => getComputedStyle(document.body).backgroundColor,
  );
  expect(background).toBe("rgb(247, 248, 250)");
  await capturePageAndAttach(page, "board-light.png", testInfo);
});

test("onboarding project selection remains readable on narrow screens", async ({
  page,
}, testInfo) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "onboarding-project-selection",
    viewport: narrowViewport,
  });

  await assertNoDocumentOverflow(page);
  await capturePageAndAttach(
    page,
    "onboarding-project-selection.png",
    testInfo,
  );
});

async function openScenario(page, options) {
  const scenario = screenshotManifest.scenarios.find(
    (item) => item.id === options.scenario,
  );
  if (!scenario) {
    throw new Error(`Unknown screenshots scenario: ${options.scenario}`);
  }
  const route = options.route || scenario.route;
  const waitSelector = options.waitSelector || scenario.wait_selector;
  await page.setViewportSize(options.viewport);
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": options.scenario,
  });
  await page.goto(`${options.runtime.url}${route}`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator(waitSelector).waitFor({ state: "visible" });
  await page.evaluate(() => document.fonts?.ready);
}

async function sidebarSequence(content) {
  return content
    .locator("[data-sidebar-nav-item], [data-sidebar-section]")
    .evaluateAll((elements) =>
      elements.map((element) => {
        const nav = element.getAttribute("data-sidebar-nav-item");
        if (nav) {
          return `nav:${nav}`;
        }
        return `section:${element.getAttribute("data-sidebar-section")}`;
      }),
    );
}

async function waitForSidebarMorph(page) {
  const swaps = await page.evaluate(() => {
    window.__detentSidebarNode = document.getElementById("app-sidebar-content");
    return window.__detentSSEMetrics?.snapshot()?.["sidebar-v2"]?.swaps || 0;
  });
  await expect
    .poll(
      () =>
        page.evaluate(
          () => window.__detentSSEMetrics?.snapshot()?.["sidebar-v2"]?.swaps || 0,
        ),
      { timeout: 15_000 },
    )
    .toBeGreaterThan(swaps);
  return page.evaluate(() => {
    const target = document.getElementById("app-sidebar-content");
    return {
      preserved: target === window.__detentSidebarNode,
      swap: target?.getAttribute("hx-swap"),
    };
  });
}

async function startLaneHiddenRecorder(page, laneID) {
  await page.evaluate((laneID) => {
    if (window.__detentLaneHiddenObserver) {
      window.__detentLaneHiddenObserver.disconnect();
    }
    window.__detentLaneHiddenValues = [];
    const snapshot = document.querySelector("#snapshot");
    const record = (lane) => {
      if (!lane) {
        return;
      }
      window.__detentLaneHiddenValues.push(
        lane.getAttribute("data-lane-hidden"),
      );
    };
    record(document.querySelector(`[data-board-lane="${laneID}"]`));
    const observer = new MutationObserver((mutations) => {
      for (const mutation of mutations) {
        if (mutation.type === "attributes") {
          const lane =
            mutation.target instanceof Element
              ? mutation.target.closest(`[data-board-lane="${laneID}"]`)
              : null;
          record(lane);
        }
        if (mutation.type === "childList") {
          for (const node of mutation.addedNodes) {
            if (!(node instanceof Element)) {
              continue;
            }
            const lane = node.matches(`[data-board-lane="${laneID}"]`)
              ? node
              : node.querySelector(`[data-board-lane="${laneID}"]`);
            record(lane);
          }
        }
      }
    });
    observer.observe(snapshot, {
      subtree: true,
      childList: true,
      attributes: true,
      attributeFilter: ["data-lane-hidden"],
    });
    window.__detentLaneHiddenObserver = observer;
  }, laneID);
}

async function laneHiddenValues(page) {
  return page.evaluate(() => window.__detentLaneHiddenValues || []);
}

async function morphCurrentSnapshot(page, name) {
  const incoming = await page.locator("#snapshot").evaluate(
    (snapshot) => snapshot.innerHTML,
  );
  await morphSnapshot(page, name, incoming);
}

async function morphSnapshot(page, name, incoming) {
  const routePath = `/__detent-runtime-identity-${name}`;
  await page.route(`**${routePath}`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "text/html; charset=utf-8",
      body: incoming,
    });
  });
  await page.evaluate(
    (path) =>
      new Promise((resolve) => {
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
        window.htmx.ajax("GET", path, {
          target: "#snapshot",
          swap: "morph:innerHTML",
        });
      }),
    routePath,
  );
  await page.unroute(`**${routePath}`);
}

async function dragKanbanCardToLane(page, card, targetLane) {
  const sourceBox = await card.boundingBox();
  if (!sourceBox) {
    throw new Error("Drag source has no bounding box");
  }
  await page.mouse.move(
    sourceBox.x + sourceBox.width / 2,
    sourceBox.y + sourceBox.height / 2,
  );
  await page.mouse.down();
  await page.mouse.move(
    sourceBox.x + sourceBox.width / 2 + 16,
    sourceBox.y + sourceBox.height / 2 + 16,
    { steps: 5 },
  );
  await expect(targetLane).toBeVisible();
  const targetBox = await targetLane.boundingBox();
  if (!targetBox) {
    throw new Error("Drag target lane has no bounding box");
  }
  await page.mouse.move(
    targetBox.x + targetBox.width / 2,
    targetBox.y + Math.min(80, targetBox.height / 2),
    { steps: 20 },
  );
  await page.mouse.up();
}

async function sseMorphSnapshot(page, incoming) {
  await page.evaluate(
    (snapshotHTML) =>
      new Promise((resolve) => {
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
        const target = document.querySelector("#snapshot");
        const event = new CustomEvent("htmx:sseBeforeMessage", {
          bubbles: true,
          cancelable: true,
          detail: { elt: target, data: snapshotHTML },
        });
        target.dispatchEvent(event);
        if (!event.defaultPrevented) {
          window.htmx.swap(
            target,
            snapshotHTML,
            { swapStyle: target.getAttribute("hx-swap") || "innerHTML" },
            { contextElement: target },
          );
        }
      }),
    incoming,
  );
}

async function seedLongActivityHistory(activityPanel, eventCount) {
  await activityPanel.evaluate((panel, count) => {
    const stream = panel.querySelector("#board-activity-stream");
    const fixture = stream?.cloneNode(true);
    if (!stream || !fixture) {
      throw new Error("Activity history fixture requires the activity stream");
    }
    fixture.removeAttribute("hx-ext");
    fixture.removeAttribute("sse-connect");
    window.htmx.remove(stream);
    panel.append(fixture);
    const scroll = fixture.querySelector("[data-activity-list-scroll]");
    const list = scroll?.querySelector("[data-activity-list]");
    const template = list?.querySelector("li");
    if (!list || !template) {
      throw new Error("Activity history fixture requires an existing event");
    }
    list.replaceChildren();
    for (let index = 0; index < count; index += 1) {
      const event = template.cloneNode(true);
      event.id = `activity-long-history-${index}`;
      const title = event.querySelector("p");
      if (title) title.textContent = `Worker session cycle ${index + 1}`;
      list.append(event);
    }
  }, eventCount);
}

async function seedMixedSessionLog(session) {
  await session.evaluate((host) => {
    let log = host.querySelector("[data-live-session-log]");
    if (!log) {
      log = document.createElement("div");
      log.className =
        "min-h-64 min-w-0 overflow-x-auto overflow-y-auto p-3 font-mono text-xs leading-relaxed text-text";
      log.dataset.liveSessionLog = "";
      host.replaceChildren(log);
    }
    log.innerHTML = `
      <div class="mb-3 min-w-0 border-l-2 border-line pl-3" data-session-event="short">
        <pre class="mt-1 min-w-max whitespace-pre text-text">package orchestrator</pre>
      </div>
      <div class="mb-3 min-w-0 border-l-2 border-line pl-3" data-session-event="long">
        <pre class="mt-1 min-w-max whitespace-pre text-text">/Users/example/workspaces/detent/internal/orchestrator/session/stream/this-line-is-intentionally-long-enough-to-overflow-the-detail-sheet.go</pre>
      </div>`;
    log.scrollLeft = 80;
    log.dispatchEvent(
      new CustomEvent("htmx:afterSwap", {
        bubbles: true,
        detail: { target: log },
      }),
    );
  });
}

async function assertSessionLogStartsAtColumnZero(session) {
  const layout = await session.evaluate((host) => {
    const log = host.querySelector("[data-live-session-log]");
    const lines = Array.from(log.querySelectorAll("pre"));
    return {
      alignments: lines.map((line) => getComputedStyle(line).textAlign),
      leftEdges: lines.map((line) => line.getBoundingClientRect().left),
      scrollLeft: log.scrollLeft,
    };
  });
  expect(layout.alignments).toEqual(["left", "left"]);
  expect(layout.leftEdges[0]).toBeCloseTo(layout.leftEdges[1], 0);
  expect(layout.scrollLeft).toBe(0);
}

async function attachScreenshotEvidence(page, name, testInfo) {
  const evidenceDir = path.join(
    process.cwd(),
    "tmp",
    "playwright-evidence",
    testInfo.project.name,
  );
  fs.mkdirSync(evidenceDir, { recursive: true });
  const evidencePath = path.join(evidenceDir, name);
  await page.screenshot({
    path: evidencePath,
    animations: "disabled",
    caret: "hide",
  });
  await testInfo.attach(name, { path: evidencePath, contentType: "image/png" });
}

async function capturePageAndAttach(page, name, testInfo) {
  // Compare against the committed baseline so the visual gate catches pixel
  // regressions, not just selector/overflow breakage. Baselines are
  // Linux-rendered (see playwright.config.js: comparison is enabled on Linux
  // or under DETENT_VISUAL_STRICT); on other platforms this is a no-op.
  await expect(page).toHaveScreenshot(name);
  const evidenceDir = path.join(
    process.cwd(),
    "tmp",
    "playwright-evidence",
    testInfo.project.name,
  );
  fs.mkdirSync(evidenceDir, { recursive: true });
  const evidencePath = path.join(evidenceDir, name);
  await page.screenshot({
    path: evidencePath,
    animations: "disabled",
    caret: "hide",
  });
  await testInfo.attach(name, { path: evidencePath, contentType: "image/png" });
}

async function attachFailureEvidence(page, testInfo) {
  const evidenceDir = path.join(
    process.cwd(),
    "tmp",
    "playwright-evidence",
    testInfo.project.name,
  );
  fs.mkdirSync(evidenceDir, { recursive: true });
  const baseName = artifactName(testInfo.title);
  const htmlPath = path.join(evidenceDir, `${baseName}.html`);
  const screenshotPath = path.join(evidenceDir, `${baseName}.png`);

  try {
    fs.writeFileSync(htmlPath, await page.content());
    await testInfo.attach(`${baseName}.html`, {
      path: htmlPath,
      contentType: "text/html",
    });
  } catch (error) {
    await testInfo.attach(`${baseName}-html-error.txt`, {
      body: String(error),
      contentType: "text/plain",
    });
  }

  try {
    await page.screenshot({
      path: screenshotPath,
      animations: "disabled",
      caret: "hide",
    });
    await testInfo.attach(`${baseName}.png`, {
      path: screenshotPath,
      contentType: "image/png",
    });
  } catch (error) {
    await testInfo.attach(`${baseName}-screenshot-error.txt`, {
      body: String(error),
      contentType: "text/plain",
    });
  }
}

function artifactName(title) {
  return (
    title
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-|-$/g, "") || "failure"
  );
}

async function assertNoDocumentOverflow(page) {
  const overflow = await page.evaluate(() => {
    const root = document.documentElement;
    return root.scrollWidth - root.clientWidth;
  });
  expect(overflow).toBeLessThanOrEqual(1);
}
