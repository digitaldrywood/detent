const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("active-hours", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("off-hours project indicator supports density, tooltip, and morph", async ({ page }, testInfo) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-active-hours",
  });
  await page.goto(`${runtime.url}/fleet`, { waitUntil: "domcontentloaded" });
  await page.locator("#snapshot").waitFor({ state: "visible" });

  const row = page.locator('[data-sidebar-project="docs-site"]');
  const indicator = row.locator("[data-sidebar-project-active-hours]");
  await expect(indicator).toBeVisible();
  await expect(indicator.locator('[data-active-hours-label="cozy"]')).toHaveText(
    "Off · 22:00",
  );
  await expect(indicator.locator('[data-active-hours-label="compact"]')).toBeHidden();
  await expect(row).toHaveAttribute("data-sidebar-project-status", "off hours");

  const evidencePath = testInfo.outputPath("fleet-active-hours.png");
  await page.screenshot({
    path: evidencePath,
    animations: "disabled",
    caret: "hide",
  });
  await testInfo.attach("fleet-active-hours.png", {
    path: evidencePath,
    contentType: "image/png",
  });

  await indicator.hover();
  const tooltip = page.locator("body > #help-tooltip");
  await expect(tooltip).toBeVisible();
  await expect(tooltip).toContainText("Active hours · docs-site");
  await expect(tooltip).toContainText("In-flight agents continue draining");

  await page.locator('[data-density-choice="compact"]').click();
  await expect(indicator.locator('[data-active-hours-label="cozy"]')).toBeHidden();
  await expect(indicator.locator('[data-active-hours-label="compact"]')).toHaveText("22:00");
  await expect(indicator.locator('[data-active-hours-label="compact"]')).toBeVisible();

  const originalIndicator = await indicator.elementHandle();
  const incoming = await page.locator("#app-sidebar-content").evaluate((element) => element.innerHTML);
  await page.evaluate(
    (sidebarHTML) =>
      new Promise((resolve) => {
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
        const target = document.querySelector("#app-sidebar-content");
        window.htmx.swap(
          target,
          sidebarHTML,
          { swapStyle: target.getAttribute("hx-swap") || "innerHTML" },
          { contextElement: target },
        );
      }),
    incoming,
  );
  await expect(row.locator("[data-sidebar-project-active-hours]")).toHaveCount(1);
  expect(await originalIndicator.evaluate((element) => element.isConnected)).toBe(true);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= document.documentElement.clientWidth,
    ),
  ).toBe(true);
});

test("Health lists off hours without raising the fleet verdict", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "health-active-hours",
  });
  await page.goto(`${runtime.url}/health/ui`, { waitUntil: "domcontentloaded" });

  await expect(page.locator("#health-verdict")).toContainText("All systems nominal");
  const row = page.locator("#health-active-hours-docs-site");
  await expect(row).toContainText("Active hours · docs-site");
  await expect(row).toContainText("Off hours");
  await expect(row).toContainText("Mon, Jun 15 at 22:00 CDT");
  await expect(page.locator('[role="alert"]', { hasText: "Off hours" })).toHaveCount(0);
});
