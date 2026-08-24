const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("freshness", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("faked tracker failures stay off the banner and remain visible elsewhere", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "board-tracker-stale",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });

  await expect(page.locator("#board-alerts")).toHaveCount(0);
  await expect(page.locator('[data-sidebar-nav-item="health"] .bg-err')).toHaveCount(1);

  const live = page.locator("#live-indicator");
  await expect(live).toHaveAttribute("data-freshness-kind", "err");
  await expect(live).toContainText("Live · refresh failed");

  await morphCurrentSnapshot(page);

  await page.goto(`${runtime.url}/diagnostics`, {
    waitUntil: "domcontentloaded",
  });
  const diagnostic = page.locator('[id^="diagnostics-condition-refresh-"]');
  await expect(diagnostic).toHaveAttribute("data-diagnostics-condition-class", "fault");
  await expect(diagnostic).toContainText("candidate fetch");
  await expect(diagnostic).toContainText("3 consecutive failures");
  await expect(diagnostic).toContainText("status 503");

  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "health-tracker-stale",
  });
  await page.goto(`${runtime.url}/health/ui`, {
    waitUntil: "domcontentloaded",
  });
  const health = page.locator('[id^="health-refresh-failure-"]');
  await expect(health).toContainText("Failed");
  await expect(health).toContainText("candidate fetch");
  await expect(health).toContainText("3 consecutive failures");
  await expect(health).toContainText("status 503");
});

test("idle healthy board keeps freshness while Done is opt-in", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "board-recent-completions",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });

  await expect(page.locator("#board-alerts")).toHaveCount(0);
  await expect(page.locator("#board-freshness")).toContainText("Data current");
  await expect(page.locator("#fig-completed")).toContainText("2 completed · 48h");

  const done = page.locator('[data-board-lane="done"]');

  await expect(done).toHaveAttribute("data-board-lane-default", "false");
  await expect(done).toBeHidden();
  await expect(page.locator("[data-board-lane-count]")).toHaveText("0/9");

  const picker = page.locator("#board-lane-picker");
  await picker.locator("summary").click();
  await picker
    .locator('[data-board-lane-visibility="done"]')
    .selectOption("show");

  await expect(done).toBeVisible();
  await expect(page.locator("[data-board-lane-count]")).toHaveText("1/9");
  await expect(done).toContainText("Release v0.44.0");
  await expect(done).toContainText("Finish overnight release batch");
  await expect(done.locator('[data-kanban-move-disabled-label]')).toHaveCount(0);

  const persisted = await page.locator("#board-lanes").evaluate((root) => {
    const key = `detent.ui.board.lanes.v2.${root.dataset.boardKey}`;
    return JSON.parse(localStorage.getItem(key) || "{}").show?.includes("done");
  });
  expect(persisted).toBe(true);

  const originalDone = await done.elementHandle();
  await morphCurrentSnapshot(page);
  expect(await originalDone?.evaluate((element) => element.isConnected)).toBe(true);
  await expect(done).toBeVisible();
});

test("heavy alerts stay one line and never reflow lanes", async ({ page }) => {
  const viewports = [
    { width: 1280, height: 800 },
    { width: 1440, height: 900 },
  ];

  for (const viewport of viewports) {
    await page.setViewportSize(viewport);
    await page.setExtraHTTPHeaders({
      "X-Detent-Demo-Scenario": "board-alerts-heavy",
    });
    await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });

    const bar = page.locator("#board-alerts");
    const toggle = page.locator("#board-alerts-toggle");
    const lanes = page.locator("#board-lanes");
    const overlay = page.locator("body > #board-alerts-overlay");
    await expect(bar).toHaveAttribute("data-board-alert-count", "3");
    await expect(bar).toContainText("Project failure breaker (2 projects)");
    await expect(bar).toContainText("+2");
    const heavyHeight = await bar.evaluate((element) => element.getBoundingClientRect().height);
    expect(heavyHeight).toBe(22);
    expect(await bar.evaluate((element) => element.scrollHeight)).toBe(20);
    const laneViewport = await lanes.boundingBox();
    expect(laneViewport?.y).toBeLessThan(viewport.height);

    const before = await boardLaneGeometry(page);
    await toggle.click();
    await expect(overlay).toBeVisible();
    await expect(toggle).toHaveAttribute("aria-expanded", "true");
    await expect(overlay).toHaveCSS("position", "fixed");
    const overlaySize = await overlay.evaluate((element) => ({
      clientHeight: element.clientHeight,
      scrollHeight: element.scrollHeight,
    }));
    expect(overlaySize.clientHeight).toBeLessThanOrEqual(viewport.height * 0.4);
    expect(overlaySize.scrollHeight).toBeGreaterThan(overlaySize.clientHeight);
    expect(await boardLaneGeometry(page)).toEqual(before);

    const originalOverlay = await overlay.elementHandle();
    await morphCurrentSnapshot(page);
    expect(
      await originalOverlay?.evaluate((element) => element.isConnected),
    ).toBe(true);
    await expect(toggle).toHaveAttribute("aria-expanded", "true");
    await expect(overlay).toBeVisible();
    expect(await boardLaneGeometry(page)).toEqual(before);

    await overlay.locator("[data-board-alerts-close]").click();
    await expect(overlay).toBeHidden();
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(await boardLaneGeometry(page)).toEqual(before);
  }
});

test("collapsed alert height is independent of alert count", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "board-staleness-one",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  const oneAlertHeight = await page
    .locator("#board-alerts")
    .evaluate((element) => element.getBoundingClientRect().height);

  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "board-alerts-heavy",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  const fiveAlertHeight = await page
    .locator("#board-alerts")
    .evaluate((element) => element.getBoundingClientRect().height);
  expect(oneAlertHeight).toBe(22);
  expect(fiveAlertHeight).toBe(oneAlertHeight);
});

test("zero, one, and twenty faults keep one indicator slot and fixed lanes", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 800 });

  await openBoardScenario(page, "fleet-healthy-parallel-work");
  await expect(page.locator("#board-alerts")).toHaveCount(0);
  const zeroFaultLaneY = await page
    .locator("#board-lanes")
    .evaluate((element) => element.getBoundingClientRect().y);

  await openBoardScenario(page, "board-staleness-one");
  const oneIndicator = page.locator("#board-alerts");
  await expect(oneIndicator).toHaveCount(1);
  await expect(oneIndicator).toHaveAttribute("data-board-alert-count", "1");
  await expect(oneIndicator).toContainText("1 fault");
  await expect(oneIndicator).toHaveClass(/border-err/);
  await expect(page.locator("[id^='exception-staleness-']")).toHaveCount(0);
  expect(
    await page
      .locator("#board-lanes")
      .evaluate((element) => element.getBoundingClientRect().y),
  ).toBe(zeroFaultLaneY);

  await openBoardScenario(page, "board-staleness-twenty");
  const twentyIndicator = page.locator("#board-alerts");
  await expect(twentyIndicator).toHaveCount(1);
  await expect(twentyIndicator).toHaveAttribute(
    "data-board-alert-count",
    "20",
  );
  await expect(twentyIndicator).toContainText("20 faults");
  await expect(twentyIndicator).toContainText("+19");
  expect(
    await page
      .locator("#board-lanes")
      .evaluate((element) => element.getBoundingClientRect().y),
  ).toBe(zeroFaultLaneY);

  await page.locator("#board-alerts-toggle").click();
  const overlay = page.locator("body > #board-alerts-overlay");
  const faults = overlay.locator('[data-board-alert="staleness-warning"]');
  await expect(faults).toHaveCount(20);
  await expect(faults.first()).toContainText("digitaldrywood/detent#1573");
  await expect(faults.last()).toContainText("digitaldrywood/detent#1554");
  await expect(overlay.getByRole("link", { name: "Open" })).toHaveCount(20);
});

test("dismiss updates both counts and stays gone through the next SSE morph", async ({
  page,
}) => {
  await page.route("**/api/v1/projects/**", async (route) => {
    if (route.request().url().includes("/staleness-warnings/")) {
      await route.fulfill({ status: 200, body: "" });
      return;
    }
    await route.continue();
  });
  await openBoardScenario(page, "board-staleness-twenty");
  await page.locator("#board-alerts-toggle").click();

  const bar = page.locator("#board-alerts");
  const overlay = page.locator("body > #board-alerts-overlay");
  const warnings = overlay.locator('[data-board-alert="staleness-warning"]');
  const dismissedID = await warnings.first().getAttribute("data-staleness-warning-id");
  const nextLead = warnings.nth(1);
  const nextLeadSummary = await nextLead.getAttribute("data-board-alert-summary");
  const nextLeadTone = await nextLead.getAttribute("data-board-alert-tone");
  const nextLeadGlyph = await nextLead
    .locator("template[data-board-alert-glyph-template]")
    .evaluate((element) => element.innerHTML);
  await warnings.first().getByRole("button", { name: "Dismiss", exact: true }).click();

  await expect(bar).toHaveAttribute("data-board-alert-count", "19");
  await expect(bar.locator("[data-board-alert-count-label]")).toHaveText("19 faults");
  await expect(overlay.locator("[data-board-alert-count-label]")).toHaveText("19 faults");
  await expect(bar).toHaveAttribute("data-board-alert-tone", nextLeadTone);
  await expect(bar.locator("[data-board-alert-lead-summary]")).toHaveText(nextLeadSummary);
  await expect(page.locator("#board-alerts-toggle")).toHaveAttribute(
    "aria-label",
    `19 board faults. Highest severity: ${nextLeadSummary}. Expand details.`,
  );
  expect(
    await bar.locator("[data-board-alert-lead-glyph]").evaluate((element) => element.innerHTML),
  ).toBe(nextLeadGlyph);
  expect(
    await overlay
      .locator("[data-board-alert-lead-glyph]")
      .evaluate((element) => element.innerHTML),
  ).toBe(nextLeadGlyph);
  await expect(warnings).toHaveCount(19);
  await expect(
    overlay.locator(`[data-staleness-warning-id="${dismissedID}"]`),
  ).toHaveCount(0);
  await expect(
    overlay.getByRole("button", { name: "Dismiss all staleness warnings (19)" }),
  ).toBeVisible();

  await morphCurrentSnapshot(page);
  await expect(bar).toHaveAttribute("data-board-alert-count", "19");
  await expect(warnings).toHaveCount(19);
  await expect(
    overlay.locator(`[data-staleness-warning-id="${dismissedID}"]`),
  ).toHaveCount(0);
});

test("bulk dismiss submits the rendered fault IDs and clears both counts", async ({
  page,
}) => {
  let submittedBody = "";
  await page.route("**/api/v1/projects/**", async (route) => {
    if (route.request().url().includes("/staleness-warnings/")) {
      submittedBody = route.request().postData() || "";
      await route.fulfill({ status: 200, body: "" });
      return;
    }
    await route.continue();
  });
  page.on("dialog", (dialog) => dialog.accept());
  await openBoardScenario(page, "board-staleness-twenty");
  await page.locator("#board-alerts-toggle").click();

  const overlay = page.locator("body > #board-alerts-overlay");
  await overlay
    .getByRole("button", { name: "Dismiss all staleness warnings (20)" })
    .click();

  await expect(page.locator("#board-alerts")).toHaveCount(0);
  await expect(overlay).toBeHidden();
  expect(new URLSearchParams(submittedBody).getAll("warning_id")).toHaveLength(20);
  await morphCurrentSnapshot(page);
  await expect(page.locator("#board-alerts")).toHaveCount(0);
});

test("fault disclosure state survives morphs without auto-expanding", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await openBoardScenario(page, "board-staleness-one");

  const toggle = page.locator("#board-alerts-toggle");
  const overlay = page.locator("body > #board-alerts-overlay");
  const twentyWarnings = await scenarioSnapshotHTML(
    page,
    "board-staleness-twenty",
  );
  await morphSnapshot(page, twentyWarnings);
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await expect(overlay).toBeHidden();
  await expect(page.locator("#board-alerts")).toHaveAttribute(
    "data-board-alert-count",
    "20",
  );

  await toggle.click();
  await expect(overlay).toBeVisible();
  await page.evaluate(() => {
    const host = document.querySelector("[data-board-alerts-overlay-host]");
    window.__detentAlertVisibility = [];
    const record = () => {
      window.__detentAlertVisibility.push({
        hidden: host.hidden,
        classHidden: host.classList.contains("hidden"),
        ariaHidden: host.getAttribute("aria-hidden"),
      });
    };
    record();
    window.__detentAlertObserver = new MutationObserver(record);
    window.__detentAlertObserver.observe(host, {
      attributes: true,
      attributeFilter: ["hidden", "class", "aria-hidden"],
    });
  });
  await morphSnapshot(page, twentyWarnings);
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(overlay).toBeVisible();
  expect(
    await page.evaluate(() => {
      window.__detentAlertObserver?.disconnect();
      return window.__detentAlertVisibility.every(
        (state) =>
          !state.hidden &&
          !state.classHidden &&
          state.ariaHidden === "false",
      );
    }),
  ).toBe(true);
});

test("twenty-fault panel scrolls internally at narrow width", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openBoardScenario(page, "board-staleness-twenty");
  await page.locator("#board-alerts-toggle").click();

  const overlay = page.locator("body > #board-alerts-overlay");
  await expect(overlay).toBeVisible();
  const geometry = await overlay.evaluate((element) => {
    const rect = element.getBoundingClientRect();
    return {
      left: rect.left,
      right: rect.right,
      width: rect.width,
      clientHeight: element.clientHeight,
      scrollHeight: element.scrollHeight,
      documentHeight: document.documentElement.scrollHeight,
      viewportHeight: window.innerHeight,
    };
  });
  expect(geometry.left).toBeGreaterThanOrEqual(0);
  expect(geometry.right).toBeLessThanOrEqual(390);
  expect(geometry.width).toBeGreaterThanOrEqual(370);
  expect(geometry.scrollHeight).toBeGreaterThan(geometry.clientHeight);
  expect(geometry.clientHeight).toBeLessThanOrEqual(844 * 0.4);
  expect(geometry.documentHeight).toBe(geometry.viewportHeight);
});

test("project board installs alert disclosure behavior", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "board-alerts-heavy",
  });
  await page.goto(`${runtime.url}/projects/dogfood/kanban`, {
    waitUntil: "domcontentloaded",
  });

  const toggle = page.locator("#board-alerts-toggle");
  const overlay = page.locator("body > #board-alerts-overlay");
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(overlay).toBeVisible();
  await expect(overlay.locator("#board-alert-backend-capacity")).toContainText(
    "Dispatch is paused for anthropic",
  );
});

async function boardLaneGeometry(page) {
  return page.locator("#board-lanes").evaluate((root) => ({
    rect: root.getBoundingClientRect().toJSON(),
    scrollLeft: root.scrollLeft,
    scrollTop: root.scrollTop,
    lanes: Array.from(root.querySelectorAll("[data-preserve-scroll]")).map(
      (lane) => ({
        key: lane.getAttribute("data-preserve-scroll"),
        scrollLeft: lane.scrollLeft,
        scrollTop: lane.scrollTop,
      }),
    ),
  }));
}

async function morphCurrentSnapshot(page) {
  const incoming = await page
    .locator("#snapshot")
    .evaluate((snapshot) => snapshot.innerHTML);
  await morphSnapshot(page, incoming);
}

async function openBoardScenario(page, scenario) {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": scenario,
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  await page.locator("#board-lanes").waitFor({ state: "visible" });
}

async function scenarioSnapshotHTML(page, scenario) {
  const response = await fetch(`${runtime.url}/`, {
    headers: { "X-Detent-Demo-Scenario": scenario },
  });
  if (!response.ok) {
    throw new Error(`scenario response failed: ${response.status}`);
  }
  const html = await response.text();
  return page.evaluate((markup) => {
    const document = new DOMParser().parseFromString(
      markup,
      "text/html",
    );
    const snapshot = document.getElementById("snapshot");
    if (!snapshot) {
      throw new Error("scenario snapshot missing");
    }
    return snapshot.innerHTML;
  }, html);
}

async function morphSnapshot(page, incoming) {
  await page.evaluate(
    (snapshotHTML) =>
      new Promise((resolve) => {
        const snapshot = document.getElementById("snapshot");
        if (!snapshot || !window.htmx) {
          throw new Error("snapshot morph dependencies unavailable");
        }
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
        const event = new CustomEvent("htmx:sseBeforeMessage", {
          bubbles: true,
          cancelable: true,
          detail: { elt: snapshot, data: snapshotHTML },
        });
        snapshot.dispatchEvent(event);
        if (!event.defaultPrevented) {
          window.htmx.swap(
            snapshot,
            snapshotHTML,
            { swapStyle: snapshot.getAttribute("hx-swap") || "innerHTML" },
            { contextElement: snapshot },
          );
        }
      }),
    incoming,
  );
}
