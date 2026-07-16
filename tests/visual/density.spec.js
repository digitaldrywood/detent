const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

const desktopViewport = { width: 1440, height: 1100 };

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("density", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("card density changes rendered information and persists", async ({
  page,
}) => {
  await page.setViewportSize(desktopViewport);
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-healthy-parallel-work",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  const runningCard = page.locator("article", {
    hasText: "Implement page-addressable screenshot scenarios",
  });
  const reviewCard = page.locator("article", {
    hasText: "Review deterministic chart colors",
  });

  await expect(page.locator("html")).toHaveAttribute("data-density", "cozy");
  await expect(runningCard.locator('[data-board-card-content="compact"]')).toBeHidden();
  await expect(runningCard.locator('[data-board-card-content="cozy"]')).toBeVisible();
  await expect(runningCard.locator('[data-board-card-content="comfy"]')).toBeHidden();

  await page.locator('[data-density-choice="compact"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-density", "compact");
  await expect(runningCard.locator('[data-board-card-content="compact"]')).toBeVisible();
  await expect(runningCard.locator('[data-board-card-content="cozy"]')).toBeHidden();
  await expect(runningCard.locator('[data-board-card-content="comfy"]')).toBeHidden();

  await page.locator('[data-density-choice="comfy"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-density", "comfy");
  await expect(runningCard.locator('[data-board-card-content="compact"]')).toBeHidden();
  await expect(runningCard.locator('[data-board-card-content="cozy"]')).toBeVisible();
  await expect(runningCard.locator('[data-board-card-content="comfy"]')).toBeVisible();
  await expect(runningCard.locator("[data-board-card-labels]")).toContainText("enhancement");
  await expect(runningCard.locator("[data-board-card-effort]")).toContainText("xhigh");
  await expect(runningCard.locator("[data-board-card-activity]")).toContainText(
    "Rendered manifest and route smoke checks.",
  );
  await expect(reviewCard.locator("[data-board-card-pr-status]")).toContainText(
    "PR #5290",
  );

  await page.reload({ waitUntil: "domcontentloaded" });
  await page.locator("#board-lanes").waitFor({ state: "visible" });
  await expect(page.locator("html")).toHaveAttribute("data-density", "comfy");
});
