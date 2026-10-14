const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");
const fixture = require("../../web/conversation/src/contracts/fixtures/reports.json");

let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("cloud-reports");
});
test.afterAll(async () => {
  await hub?.stop();
});

async function openReports(page) {
  const errors = [];
  page.on("pageerror", (error) => errors.push(String(error)));
  await page.route("**/api/v2/organizations/*/reports?*", (route) => {
    const range = new URL(route.request().url()).searchParams.get("range");
    const report = structuredClone(fixture);
    report.analytics.project_id = hub.fixture.project_id;
    if (range === "24h") report.completion.done = 100;
    return route.fulfill({ json: report });
  });
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(hub.fixture.chat);
  if (page.viewportSize().width < 768)
    await page
      .getByRole("button", { name: "Toggle main sidebar", exact: true })
      .click();
  await page.getByTestId("nav-reports").click();
  await expect(page).toHaveURL(/\/reports$/);
  await expect(
    page.getByRole("region", { name: "Headline metrics" }),
  ).toContainText("204");
  return errors;
}

for (const theme of ["light", "dark"]) {
  for (const width of [1440, 390]) {
    test(`Cloud Reports ${theme} ${width}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1100 });
      await page.emulateMedia({ colorScheme: theme });
      const errors = await openReports(page);
      await page.evaluate((theme) => {
        document.documentElement.classList.toggle("dark", theme === "dark");
        document.documentElement.dataset.theme = theme;
      }, theme);
      const report = page.getByTestId("reports-page");
      for (const name of [
        "Where system time goes",
        "Throughput",
        "Aging work in progress",
        "Stages and models",
        "Rework by cause",
        "Cost concentration",
        "Failure signatures",
        "Source coverage",
      ])
        await expect(
          report.getByRole("region", { name, exact: true }),
        ).toBeAttached();
      await expect(report).toContainText("Disabled by project gates");
      await expect(report).toContainText("285 / 300");
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth > innerWidth,
        ),
      ).toBe(false);
      await test.info().attach(`cloud-reports-${theme}-${width}.png`, {
        body: await page.screenshot({ animations: "disabled", caret: "hide" }),
        contentType: "image/png",
      });
      for (const [name, file] of [
        ["Aging work in progress", "aging"],
        ["Stages and models", "stages"],
        ["Failure signatures", "failures"],
        ["Source coverage", "coverage"],
      ]) {
        const section = report.getByRole("region", { name, exact: true });
        await section.scrollIntoViewIfNeeded();
        await expect(section).toBeVisible();
        await expect(section.getByRole("heading", { name, exact: true })).toBeVisible();
        await test.info().attach(`cloud-reports-${file}-${theme}-${width}.png`, {
          body: await section.screenshot({ animations: "disabled", caret: "hide" }),
          contentType: "image/png",
        });
      }
      expect(errors).toEqual([]);
    });
  }
}

test("Reports time and window controls, unavailable state, and issue Diagnostics links", async ({
  page,
}) => {
  await openReports(page);
  await page.getByRole("button", { name: "Lead time", exact: true }).click();
  const headline = page.getByRole("region", { name: "Headline metrics" });
  await expect(headline).toContainText("Lead time / issue p50");
  await expect(headline).toContainText("5.0 h");
  await page.getByRole("button", { name: "Past 24h", exact: true }).click();
  await expect(headline).toContainText("100");
  await expect(
    page.getByRole("region", { name: "Where lead time goes" }),
  ).toContainText("Held by people");
  await expect(
    page
      .getByRole("region", { name: "Failure signatures" })
      .getByRole("link")
      .first(),
  ).toHaveAttribute("href", "/work/i/wi_old?tab=diagnostics");
  await page.route("**/api/v2/organizations/*/reports?*", (route) => {
    const report = structuredClone(fixture);
    report.unavailable.push(
      "recorded_usage",
      "lane_residence_and_unclaimed_waits_unavailable",
      "landed_first_try",
    );
    return route.fulfill({ json: report });
  });
  await page.getByRole("button", { name: "Refresh reports" }).click();
  await expect(headline).toContainText("Not recorded");
  await expect(
    page.getByRole("region", { name: "Cost concentration" }),
  ).toHaveText(/Not recorded/);
  await expect(headline).not.toContainText("$7.61");
  await expect(headline).toContainText("Prior window unavailable");
  await expect(
    page.getByRole("region", { name: "Throughput", exact: true }),
  ).toContainText("Issues done / h · Not recorded");
  await page.route("**/api/v2/organizations/*/reports?*", (route) =>
    route.fulfill({ status: 503, json: { error: "unavailable" } }),
  );
  await page.getByRole("button", { name: "Refresh reports" }).click();
  await expect(page.getByTestId("reports-page").getByRole("status")).toHaveText(
    "Reports are temporarily unavailable.",
  );
});

test("Reports follows the sidebar project switcher and decodes the real read", async ({
  page,
}) => {
  await page.goto(hub.fixture.accounts.owner);
  const response = await page.request.get(`${hub.fixture.url}/app/bootstrap`);
  const bootstrap = await response.json();
  const projects = bootstrap.projects;
  expect(projects.length).toBeGreaterThan(1);
  await page.goto(`${hub.fixture.url}/reports`);
  await expect(
    page
      .getByTestId("reports-page")
      .getByRole("region", { name: "Headline metrics" }),
  ).toBeVisible();
  await expect(page.getByTestId("reports-page")).not.toContainText(
    "Reports are temporarily unavailable.",
  );
  const next = projects.find((p) => p.id !== hub.fixture.project_id);
  const request = page.waitForRequest(
    (r) =>
      r.url().includes("/reports?") &&
      new URL(r.url()).searchParams.get("project") === next.id,
  );
  await page.getByLabel("Filter threads by project", { exact: true }).click();
  await page.getByRole("option", { name: next.name, exact: true }).click();
  await request;
  await expect(page).toHaveURL(/\/reports$/);
  await expect(
    page.getByRole("navigation", { name: "Reports breadcrumb" }),
  ).toContainText(next.name);
  await expect(
    page
      .getByTestId("reports-page")
      .getByRole("region", { name: "Headline metrics" }),
  ).toBeVisible();
});
