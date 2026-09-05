const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

async function traceStreams(page) {
  await page.addInitScript(() => {
    window.sseTrace = [];
    window.sseSources = [];
    const NativeEventSource = window.EventSource;
    window.EventSource = class extends NativeEventSource {
      constructor(...args) {
        super(...args);
        window.sseSources.push(this);
      }
    };
    document.addEventListener("htmx:sseOpen", (event) => {
      const owner = event.target;
      const record = (event) => window.sseTrace.push({
        type: event.type,
        target: owner.id || owner.tagName,
        url: event.detail.source.url,
        reason: event.detail.type,
        state: document.documentElement.dataset.detentConnection,
      });
      record(event);
      owner.addEventListener("htmx:sseClose", record);
      owner.addEventListener("htmx:sseError", record);
    });
  });
}

async function chooseDensity(page, density) {
  const toggle = page.getByRole("button", { name: "More topbar controls" });
  if (await toggle.isVisible()) await toggle.click();
  await page.locator(`[data-density-choice="${density}"]`).click();
  await expect(page.locator("html")).toHaveAttribute("data-density", density);
  if (await toggle.isVisible()) await toggle.click();
}

test("sheet activity and live session leave the dashboard connected", async ({ page }, testInfo) => {
  const runtime = await startDetentRuntime("sheet-sse", ["--demo", "screenshots"]);
  try {
    await traceStreams(page);
    await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": "board-card-identity-maximal" });
    await page.goto(runtime.url, { waitUntil: "domcontentloaded" });
    await expect(page.locator("html")).toHaveAttribute("data-detent-connection", "connected");
    const card = page.locator("article", {
      hasText: "Keep project, issue, and pull request identity visible with maximal metadata",
    });
    await card.click();
    await expect.poll(() => page.evaluate(() => window.sseTrace.some(
      (event) => event.type === "htmx:sseOpen" && event.url.includes("/activity/events"),
    ))).toBe(true);
    await page.getByRole("tab", { name: "Live session", exact: true }).click();
    await expect.poll(() => page.evaluate(() => window.sseTrace.some(
      (event) => event.type === "htmx:sseOpen" && event.url.includes("/session/events"),
    ))).toBe(true);
    await expect(page.locator("html")).toHaveAttribute("data-detent-connection", "connected");
    await page.getByRole("button", { name: "Close details", exact: true }).click();
    await expect(page.locator("[data-live-session-attach]")).toHaveCount(0);
    await page.evaluate(() => {
      for (const source of window.sseSources.slice(1)) {
        const type = source.url.includes("/activity/") ? "board-activity" : "session-activity";
        source.dispatchEvent(new MessageEvent(type, { data: "" }));
      }
    });
    await expect.poll(() => page.evaluate(() => window.sseSources
      .filter((source) => new URL(source.url).pathname !== "/events")
      .every((source) => source.readyState === EventSource.CLOSED),
    )).toBe(true);
    expect(await page.evaluate(() => window.sseSources[0].readyState)).toBe(1);
    await expect(page.locator("html")).toHaveAttribute("data-detent-connection", "connected");
    await expect(page.locator("html")).toHaveAttribute("data-detent-sse-status", "open");
    await expect(page.locator("#detent-connection-notice")).toBeHidden();
    for (const density of ["compact", "cozy"]) await chooseDensity(page, density);
    await testInfo.attach("sheet-sse-trace.json", {
      body: JSON.stringify(await page.evaluate(() => window.sseTrace), null, 2),
      contentType: "application/json",
    });
  } finally {
    await runtime.stop();
  }
});

test("only dashboard lifecycle events gate moves and recovery", async ({ page }) => {
  const args = ["--demo", "kanban", "--demo-project", "demo-project"];
  let runtime = await startDetentRuntime("sheet-sse-recovery", args);
  try {
    await page.goto(runtime.url, { waitUntil: "domcontentloaded" });
    const html = page.locator("html");
    const moves = page.locator('[data-kanban-action="move"]');
    await expect(html).toHaveAttribute("data-detent-connection", "connected");
    await expect(moves.first()).toBeVisible();
    const count = await moves.count();
    expect(count).toBeGreaterThan(0);
    await moves.first().click();
    await expect(page.locator("#board-activity-stream[sse-connect]")).toBeAttached();
    await page.getByRole("button", { name: "Close details", exact: true }).click();
    await expect(page.locator("#board-activity-stream")).toHaveCount(0);
    await expect(html).toHaveAttribute("data-detent-connection", "connected");
    await expect(moves).toHaveCount(count);
    for (const density of ["compact", "cozy"]) await chooseDensity(page, density);
    for (const type of ["sseOpen", "sseError", "sseClose", "sseBeforeMessage"]) {
      await page.evaluate((type) => {
        const board = document.querySelector("[data-detent-dashboard-stream]");
        const sheet = document.createElement("div");
        sheet.setAttribute("sse-connect", "/api/v1/board/session/events");
        board.append(sheet);
        sheet.dispatchEvent(new CustomEvent(`htmx:${type}`, { bubbles: true }));
        sheet.remove();
        document.dispatchEvent(new CustomEvent(`htmx:${type}`, { bubbles: true }));
      }, type);
      await expect(html).toHaveAttribute("data-detent-connection", "connected");
      await expect(html).toHaveAttribute("data-detent-sse-status", "open");
      await expect(moves).toHaveCount(count);
      await expect(page.locator("#detent-connection-notice")).toBeHidden();
    }
    await page.evaluate(() => document.querySelector("[data-detent-dashboard-stream]")
      .dispatchEvent(new CustomEvent("htmx:sseClose", { bubbles: true })));
    await expect(html).toHaveAttribute("data-detent-connection", "disconnected");
    await expect(moves).toHaveCount(0);
    const { port } = new URL(runtime.url);
    const home = runtime.home;
    await runtime.stop();
    await expect(html).toHaveAttribute("data-detent-sse-status", "error");
    await expect(page.locator("#detent-connection-notice")).toBeVisible();
    await expect(page.locator('[data-kanban-connection-disabled="true"]')).toHaveCount(count);
    runtime = await startDetentRuntime("sheet-sse-recovered", args, { home, port: Number(port) });
    await expect(html).toHaveAttribute("data-detent-connection", "connected");
    await expect(moves).toHaveCount(count);
    await expect(page.locator("#detent-connection-notice")).toBeHidden();
  } finally {
    await runtime.stop();
  }
});
