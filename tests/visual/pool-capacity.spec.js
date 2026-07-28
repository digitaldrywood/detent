const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("pool-capacity", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("fleet renders every pool and distinguishes saturation", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-agent-pools",
  });
  await page.goto(`${runtime.url}/fleet`, { waitUntil: "domcontentloaded" });

  const pools = page.locator("[data-agent-pool]");
  await expect(pools).toHaveCount(3);
  await expect(page.locator('[data-agent-pool="default"] [data-pool-usage]')).toHaveText("0 / 2");

  const borrowing = page.locator('[data-agent-pool="video"]');
  await expect(borrowing.locator("[data-pool-usage]")).toHaveText(
    "12 / 15 · floor 10 · 2 borrowed",
  );
  await expect(borrowing).toHaveAttribute("data-pool-saturated", "false");

  const saturated = page.locator('[data-agent-pool="code"]');
  await expect(saturated.locator("[data-pool-usage]")).toHaveText("5 / 5");
  await expect(saturated).toHaveAttribute("data-pool-saturated", "true");
  await expect(saturated.locator("[data-pool-capacity-status]")).toHaveText("At capacity");
  await expect(saturated).toHaveClass(/border-err/);
});

test("project renders only its assigned pool utilization", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "project-agent-pool",
  });
  await page.goto(`${runtime.url}/projects/dogfood`, {
    waitUntil: "domcontentloaded",
  });

  await expect(page.locator("[data-agent-pool]")).toHaveCount(1);
  await expect(page.locator('[data-agent-pool="code"] [data-pool-usage]')).toHaveText("5 / 5");
  await expect(page.locator('[data-agent-pool="video"]')).toHaveCount(0);
});

test("default-only fleet keeps capacity in the existing count", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-agent-pool-default",
  });
  await page.goto(`${runtime.url}/fleet`, { waitUntil: "domcontentloaded" });

  await expect(page.locator("#agent-activity header")).toContainText(
    "3 running · 3 / 6 capacity",
  );
  await expect(page.locator("#agent-pool-capacity")).toHaveCount(0);
});

test("pool readout responds to density and survives repeated morphs", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-agent-pools",
  });
  await page.goto(`${runtime.url}/fleet`, { waitUntil: "domcontentloaded" });

  const label = page.locator("[data-pool-capacity-label]");
  const status = page.locator('[data-agent-pool="code"] [data-pool-capacity-status]');
  await expect(label).toBeVisible();
  await expect(status).toBeVisible();

  await page.locator('[data-density-choice="compact"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-density", "compact");
  await expect(label).toBeHidden();
  await expect(status).toBeHidden();

  await page.locator('[data-density-choice="cozy"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-density", "cozy");
  await expect(label).toBeVisible();
  await expect(status).toBeVisible();

  const snapshot = page.locator("#snapshot");
  const originalStrip = await page.locator("#agent-pool-capacity").elementHandle();
  const incoming = await snapshot.evaluate((element) => element.innerHTML);
  await page.route("**/__detent-test-pool-refresh", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "text/html; charset=utf-8",
      body: incoming,
    });
  });

  for (let index = 0; index < 3; index += 1) {
    await page.evaluate(
      () =>
        new Promise((resolve) => {
          document.addEventListener("htmx:afterSettle", resolve, { once: true });
          window.htmx.ajax("GET", "/__detent-test-pool-refresh", {
            target: "#snapshot",
            swap: "morph:innerHTML",
          });
        }),
    );
    await expect(page.locator("#agent-pool-capacity")).toHaveCount(1);
    await expect(page.locator("[data-agent-pool]")).toHaveCount(3);
  }

  expect(await originalStrip?.evaluate((element) => element.isConnected)).toBe(true);
});
