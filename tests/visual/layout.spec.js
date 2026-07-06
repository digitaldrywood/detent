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

test("board lane picker hides and restores lanes", async ({ page }) => {
  await openScenario(page, {
    runtime: screenshotsRuntime,
    scenario: "fleet-healthy-parallel-work",
    route: "/",
    waitSelector: "#board-lanes",
    viewport: desktopViewport,
  });

  const firstLane = page.locator("[data-board-lane]").first();
  const laneID = await firstLane.getAttribute("data-board-lane");
  const toggle = page.locator(`[data-board-lane-toggle="${laneID}"]`);

  await page.locator("#board-lane-picker summary").click();
  await toggle.uncheck();
  await expect(firstLane).toBeHidden();
  await toggle.check();
  await expect(firstLane).toBeVisible();
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
  await expect(sheet.locator("text=State")).toBeVisible();
  await capturePageAndAttach(page, "board-detail-sheet.png", testInfo);

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
  await expect(page.locator("#fleet-metrics")).toBeVisible();
  await assertNoDocumentOverflow(page);
  await capturePageAndAttach(page, "fleet.png", testInfo);
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
  await assertNoDocumentOverflow(page);
  await capturePageAndAttach(page, "project-overview.png", testInfo);
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
  const targetLane = page.locator('[data-kanban-drop-state="Todo"]');
  await expect(card).toHaveAttribute("draggable", "true");
  await page.locator("#board-lane-picker summary").click();
  await page.locator('[data-board-lane-toggle="todo"]').check();
  await expect(targetLane).toBeVisible();

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

  await page.evaluate(() => {
    const source = Array.from(
      document.querySelectorAll(
        '[data-kanban-card][data-kanban-current-state="Backlog"]',
      ),
    ).find((element) =>
      element.textContent.includes("Kanban demo backlog intake"),
    );
    const target = document.querySelector('[data-kanban-drop-state="Todo"]');
    if (!(source instanceof HTMLElement) || !(target instanceof HTMLElement)) {
      throw new Error("Drag source or target lane not found");
    }
    const dataTransfer = new DataTransfer();
    source.dispatchEvent(
      new DragEvent("dragstart", {
        bubbles: true,
        cancelable: true,
        dataTransfer,
      }),
    );
    target.dispatchEvent(
      new DragEvent("dragover", {
        bubbles: true,
        cancelable: true,
        dataTransfer,
      }),
    );
    target.dispatchEvent(
      new DragEvent("drop", {
        bubbles: true,
        cancelable: true,
        dataTransfer,
      }),
    );
    source.dispatchEvent(
      new DragEvent("dragend", {
        bubbles: true,
        cancelable: true,
        dataTransfer,
      }),
    );
  });
  const request = await moveRequest;
  const form = new URLSearchParams(request.postData() || "");
  expect(form.get("kanban_drag")).toBe("true");
  expect(form.get("target_state")).toBe("Todo");

  await expect(
    targetLane.locator("[data-kanban-card]", {
      hasText: "Kanban demo backlog intake",
    }),
  ).toBeVisible();
  await expect(page.locator("#board-feedback")).toBeHidden();
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
