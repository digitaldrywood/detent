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
  const toggle = page.locator(`[data-board-lane-toggle="${laneID}"]`);
  const count = page.locator("[data-board-lane-count]");
  const boardKey = await page
    .locator("[data-board-lanes]")
    .getAttribute("data-board-key");

  await page.evaluate(
    ({ boardKey, laneID }) => {
      localStorage.setItem(
        `detent.ui.board.lanes.${boardKey}`,
        JSON.stringify({ [laneID]: true }),
      );
      document.dispatchEvent(new Event("htmx:afterSettle"));
    },
    { boardKey, laneID },
  );

  await expect(lane).toBeVisible();
  await expect(toggle).toBeChecked();
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
    const toggle = template.content.querySelector(
      `[data-board-lane-toggle="${laneID}"]`,
    );
    if (toggle) {
      toggle.checked = false;
      toggle.removeAttribute("checked");
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
  await expect(toggle).toBeChecked();
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
  await expect(toggle).toBeChecked();
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

test("all-project board cards stay read-only on drag attempt", async ({
  page,
}) => {
  await page.setViewportSize(desktopViewport);
  await page.goto(`${kanbanRuntime.url}/`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  const card = page.locator("[data-kanban-card]", {
    hasText: "Kanban demo backlog intake",
  });
  await expect(card).toHaveAttribute("data-kanban-move-disabled", "true");
  await expect(card).toHaveAttribute(
    "data-kanban-move-disabled-reason",
    /All-project board is read-only/,
  );
  await expect(card).not.toHaveAttribute("draggable", "true");
  await expect(page.locator("[data-kanban-drop-state]")).toHaveCount(0);

  let moveRequests = 0;
  page.on("request", (request) => {
    if (
      request.method() === "POST" &&
      request.url().endsWith("/api/v1/kanban/move")
    ) {
      moveRequests += 1;
    }
  });

  const box = await card.boundingBox();
  if (!box) {
    throw new Error("Read-only card has no bounding box");
  }
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(
    box.x + box.width / 2 + 24,
    box.y + box.height / 2 + 24,
    { steps: 8 },
  );
  await page.mouse.up();

  const selectedText = await page.evaluate(
    () => window.getSelection()?.toString() || "",
  );
  expect(selectedText.trim()).toBe("");
  expect(moveRequests).toBe(0);
});

test("dragstart defers DOM mutations so Chrome keeps the native drag", async ({
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
  await expect(card).toHaveAttribute("draggable", "true");

  // Chrome cancels a native drag when the dragstart handler mutates the DOM
  // synchronously (the reflow moves the source card mid-capture). Assert the
  // handler leaves the DOM untouched in the dispatching tick and applies the
  // drag affordances one macrotask later.
  const observed = await card.evaluate((element) => {
    const readState = () => ({
      feedbackHidden: document.getElementById("board-feedback").hidden,
      cardDragging: element.dataset.kanbanDragging === "true",
      hiddenLanes: document.querySelectorAll(
        '[data-kanban-drop-state][data-lane-hidden="true"]',
      ).length,
      highlightedLanes: document.querySelectorAll(
        "[data-kanban-drop-allowed]",
      ).length,
    });
    element.dispatchEvent(
      new DragEvent("dragstart", {
        bubbles: true,
        cancelable: true,
        dataTransfer: new DataTransfer(),
      }),
    );
    const duringDispatchTick = readState();
    return new Promise((resolve) => {
      setTimeout(() => {
        const afterMacrotask = readState();
        element.dispatchEvent(new DragEvent("dragend", { bubbles: true }));
        resolve({ duringDispatchTick, afterMacrotask });
      }, 0);
    });
  });

  expect(observed.duringDispatchTick.feedbackHidden).toBe(true);
  expect(observed.duringDispatchTick.cardDragging).toBe(false);
  expect(observed.duringDispatchTick.hiddenLanes).toBeGreaterThan(0);
  expect(observed.duringDispatchTick.highlightedLanes).toBe(0);

  expect(observed.afterMacrotask.feedbackHidden).toBe(false);
  expect(observed.afterMacrotask.cardDragging).toBe(true);
  expect(observed.afterMacrotask.hiddenLanes).toBe(0);
  expect(observed.afterMacrotask.highlightedLanes).toBeGreaterThan(0);

  // dragend restores the hidden lanes and reports the abandoned move.
  await expect(page.locator("#board-feedback")).toHaveText(/Move cancelled/);
  await expect(
    page.locator('[data-kanban-drop-state][data-lane-hidden="true"]'),
  ).not.toHaveCount(0);
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
  await expect(card).toHaveAttribute("draggable", "true");

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
    await expect(card).toHaveAttribute("draggable", "true");
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
