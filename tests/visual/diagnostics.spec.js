const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");
const fixture = require("../../web/conversation/src/contracts/fixtures/diagnostics.json");
const healthFixture = require("../../web/conversation/src/contracts/fixtures/health-findings.json");
const fleetFixture = require("../../web/conversation/src/contracts/fixtures/account-fleet.json");

test.describe.configure({ mode: "serial" });
let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("diagnostics", {
    env: { DETENT_HOSTED_BROWSER_UNSIGNED_MEMBER: "1" },
  });
});

test.afterAll(async () => {
  await hub?.stop();
});

async function open(page, report = fixture) {
  await page.route("**/api/v2/organizations/*/fleet", (route) =>
    route.fulfill({ json: fleetFixture }),
  );
  await page.route("**/api/v2/organizations/*/diagnostics?*", (route) =>
    route.fulfill({ json: report }),
  );
  await page.route("**/api/v2/organizations/*/health/findings?*", (route) =>
    report.findings === null
      ? route.fulfill({
          status: 404,
          json: { code: "not_found", message: "Not available" },
        })
      : route.fulfill({ json: healthFixture }),
  );
  await page.goto(hub.fixture.accounts.owner, {
    waitUntil: "domcontentloaded",
  });
  await page.goto(new URL("/diagnostics", hub.fixture.url).toString(), {
    waitUntil: "domcontentloaded",
  });
  await expect(
    page.getByRole("heading", { level: 1, name: "Diagnostics", exact: true }),
  ).toBeVisible();
  await expect(
    page
      .getByRole("region", { name: "Needs attention", exact: true })
      .getByRole("article"),
  ).toHaveCount(report.findings?.length ?? 0);
}

test("Browse opens Diagnostics, orders findings, and changes the read window", async ({
  page,
}) => {
  await open(page);
  const rows = page
    .getByRole("region", { name: "Needs attention", exact: true })
    .getByRole("article");
  await expect(rows.first()).toContainText(
    "Repeated attempts without progress",
  );
  await expect(rows.first().getByRole("link")).toHaveAttribute(
    "href",
    /\/work\/i\/wi_fixture\?tab=diagnostics$/,
  );
  await expect(rows.nth(1).getByRole("link")).toHaveAttribute(
    "href",
    /\/fleet$/,
  );
  await expect(rows.nth(2).getByRole("link")).toHaveAttribute(
    "href",
    /\/work\/p\/prj_fixture$/,
  );
  await expect(
    page.getByRole("img", { name: "Hourly slots in use and Todo queue depth" }),
  ).toBeVisible();
  const capacity = page.getByRole("region", { name: "Capacity", exact: true });
  await expect(capacity).toContainText("Limit 2 · Reported 2 · 1 in use");
  await expect(capacity.getByLabel("1 of 2 slots in use")).toBeVisible();
  await rows.first().getByRole("link").click();
  await expect(page).toHaveURL(/\/work\/i\/wi_fixture\?tab=diagnostics$/);
  await page.goBack();
  const request = page.waitForRequest((request) =>
    request.url().includes("/diagnostics?range=7d"),
  );
  await page
    .getByRole("group", { name: "Diagnostics period" })
    .getByRole("button", { name: "7 days" })
    .click();
  await request;
  await page.goto(new URL("/usage", hub.fixture.url).toString());
  await page.getByRole("button", { name: "Diagnostics", exact: true }).click();
  await expect(page).toHaveURL(/\/diagnostics$/);
});

test("an unavailable findings read differs from an empty detector result", async ({
  page,
}) => {
  await open(page, {
    ...fixture,
    findings: null,
    detector_tick: null,
    skipped: null,
  });
  const attention = page.getByRole("region", {
    name: "Needs attention",
    exact: true,
  });
  await expect(attention).toContainText("Findings are unavailable");
  await expect(
    page.getByRole("region", { name: "Scheduler decisions", exact: true }),
  ).toContainText("Not recorded");
  await page.unroute("**/api/v2/organizations/*/diagnostics?*");
  await page.route("**/api/v2/organizations/*/diagnostics?*", (route) =>
    route.fulfill({ json: { ...fixture, findings: [] } }),
  );
  await page.unroute("**/api/v2/organizations/*/health/findings?*");
  await page.route("**/api/v2/organizations/*/health/findings?*", (route) =>
    route.fulfill({ json: { ...healthFixture, items: [] } }),
  );
  await page.getByRole("button", { name: "Refresh diagnostics" }).click();
  await expect(attention).toContainText("No open findings.");
  await page.unroute("**/api/v2/organizations/*/diagnostics?*");
  await page.route("**/api/v2/organizations/*/diagnostics?*", (route) =>
    route.fulfill({
      status: 503,
      json: { code: "unavailable", message: "Unavailable" },
    }),
  );
  await page.unroute("**/api/v2/organizations/*/health/findings?*");
  await page.route("**/api/v2/organizations/*/health/findings?*", (route) =>
    route.fulfill({
      status: 503,
      json: { code: "unavailable", message: "Unavailable" },
    }),
  );
  await page.getByRole("button", { name: "Refresh diagnostics" }).click();
  await expect(
    page.getByText("Diagnostics are temporarily unavailable.", { exact: true }),
  ).toBeVisible();
  await expect(attention).toContainText("Findings are unavailable");
});

for (const theme of ["light", "dark"]) {
  for (const width of [1440, 390]) {
    test(`Diagnostics ${theme} at ${width}px`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 1100 });
      await page.emulateMedia({ colorScheme: theme });
      await open(page);
      await page.evaluate((theme) => {
        document.documentElement.classList.toggle("dark", theme === "dark");
        document.documentElement.dataset.theme = theme;
      }, theme);
      for (const section of [
        "Needs attention",
        "Capacity",
        "Scheduler decisions",
        "Merge queue",
        "Instrumentation coverage",
        "Configuration that shapes these numbers",
      ]) {
        await expect(
          page.getByRole("heading", { name: section, exact: true }),
        ).toHaveCount(1);
      }
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= window.innerWidth,
        ),
      ).toBe(true);
      const bounds = await page
        .getByRole("region", { name: "Needs attention", exact: true })
        .boundingBox();
      expect(bounds.x + bounds.width).toBeLessThanOrEqual(width);
      const screenshot = testInfo.outputPath(
        `diagnostics-${theme}-${width}.png`,
      );
      await page.screenshot({ path: screenshot, animations: "disabled" });
      await testInfo.attach(`diagnostics-${theme}-${width}`, {
        path: screenshot,
        contentType: "image/png",
      });
      const coverage = page.getByRole("region", {
        name: "Instrumentation coverage",
        exact: true,
      });
      await coverage.scrollIntoViewIfNeeded();
      await expect(coverage).toContainText("841 / 841");
      await expect(coverage).toContainText("Detector last tick:");
      await expect(
        page.getByRole("meter", { name: "Transitions coverage" }),
      ).toHaveAttribute("aria-valuenow", "841");
    });
  }
}
