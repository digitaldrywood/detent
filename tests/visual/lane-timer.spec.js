const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("external-lane-timer", [
    "--demo",
    "screenshots",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("externally moved card keeps its tracker lane-entry timer", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-kanban-external-lane-timer",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  const card = page.locator("article", {
    hasText: "Externally moved Blocked lane timer",
  });
  const footer = card.locator("[data-board-card-age-footer]");
  await expect(card).toBeVisible();
  await expect(footer).toContainText("In lane");
  await expect(footer).toContainText("1h");
  await expect(footer).toHaveAttribute("title", /Blocked since .*1h 51m/);
});
