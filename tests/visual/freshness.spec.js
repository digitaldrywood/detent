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

test("faked tracker failures show stale data without hiding live SSE", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "board-tracker-stale",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });

  const bar = page.locator("#board-alerts");
  const toggle = page.locator("#board-alerts-toggle");
  const overlay = page.locator("body > #board-alerts-overlay");
  await expect(bar).toBeVisible();
  await expect(bar).toHaveAttribute("data-board-alert-count", "1");
  await expect(bar).toHaveAttribute("data-board-data-stale", "true");
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(overlay).toBeVisible();
  await expect(overlay.locator("#board-alert-tracker-stale")).toContainText(
    "dogfood",
  );
  await expect(overlay).toContainText("candidate last succeeded");
  await expect(overlay).toContainText("3 consecutive failures");
  await expect(overlay).toContainText("status 503");

  const live = page.locator("#live-indicator");
  await expect(live).toHaveAttribute("data-freshness-kind", "warn");
  await expect(live).toContainText("Live · stale data");

  const originalBar = await bar.elementHandle();
  const originalOverlay = await overlay.elementHandle();
  await morphCurrentSnapshot(page);
  expect(await originalBar?.evaluate((element) => element.isConnected)).toBe(true);
  expect(
    await originalOverlay?.evaluate((element) => element.isConnected),
  ).toBe(true);
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(overlay).toBeVisible();

  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "health-tracker-stale",
  });
  await page.goto(`${runtime.url}/health/ui`, {
    waitUntil: "domcontentloaded",
  });
  const health = page.locator("#health-tracker-dogfood");
  await expect(health).toContainText("Stale");
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
    await expect(bar).toHaveAttribute("data-board-alert-count", "6");
    await expect(bar).toContainText("Board showing last-known state");
    await expect(bar).toContainText("+5");
    const heavyHeight = await bar.evaluate((element) => element.getBoundingClientRect().height);
    expect(heavyHeight).toBe(40);
    expect(await bar.evaluate((element) => element.scrollHeight)).toBe(40);
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
    "X-Detent-Demo-Scenario": "board-tracker-stale",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  const oneAlertHeight = await page
    .locator("#board-alerts")
    .evaluate((element) => element.getBoundingClientRect().height);

  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "board-alerts-heavy",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  const sixAlertHeight = await page
    .locator("#board-alerts")
    .evaluate((element) => element.getBoundingClientRect().height);
  expect(oneAlertHeight).toBe(40);
  expect(sixAlertHeight).toBe(oneAlertHeight);
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
  await expect(overlay.locator("#board-alert-update-pending")).toContainText(
    "A Detent update is ready to apply.",
  );
  await expect(overlay.getByRole("button", { name: "Apply now" })).toBeVisible();
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
  await page.evaluate(
    () =>
      new Promise((resolve) => {
        const snapshot = document.getElementById("snapshot");
        if (!snapshot || !window.htmx) {
          throw new Error("snapshot morph dependencies unavailable");
        }
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
        const event = new CustomEvent("htmx:sseBeforeMessage", {
          bubbles: true,
          cancelable: true,
          detail: { elt: snapshot, data: snapshot.innerHTML },
        });
        snapshot.dispatchEvent(event);
        if (!event.defaultPrevented) {
          window.htmx.swap(
            snapshot,
            snapshot.innerHTML,
            { swapStyle: snapshot.getAttribute("hx-swap") || "innerHTML" },
            { contextElement: snapshot },
          );
        }
      }),
  );
}
