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

  const banner = page.locator("#board-stale-data");
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("dogfood candidate fetch last succeeded");
  await expect(banner).toContainText("3 consecutive failures");
  await expect(banner).toContainText("status 503");

  const live = page.locator("#live-indicator");
  await expect(live).toHaveAttribute("data-freshness-kind", "warn");
  await expect(live).toContainText("Live · stale data");

  const originalBanner = await banner.elementHandle();
  await morphCurrentSnapshot(page);
  expect(await originalBanner?.evaluate((element) => element.isConnected)).toBe(true);

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

  await expect(page.locator("#board-stale-data")).toHaveCount(0);
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
