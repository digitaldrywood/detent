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

const TABS = [
  { id: "overview", label: "Overview", path: "/reports", headline: "Headline metrics", sections: ["Shipped per day", "Where cycle time goes", "Most expensive shipped issues", "Stuck right now"] },
  { id: "flow", label: "Flow", path: "/reports/flow", headline: "Flow headline", sections: ["Cycle time per day", "Hours spent in each lane, per day", "Aging work in progress"] },
  { id: "quality", label: "Quality", path: "/reports/quality", headline: "Quality headline", sections: ["Why work went back, per day", "Rework causes"] },
  { id: "cost", label: "Cost", path: "/reports/cost", headline: "Cost headline", sections: ["Spend per day by stage", "By stage and model", "Most expensive issues"] },
  { id: "failures", label: "Failures", path: "/reports/failures", headline: "Failures headline", sections: ["Failed attempts per day", "Recurring failures"] },
];

async function routeReports(page, edit = () => {}) {
  const ranges = [];
  await page.route("**/api/v2/organizations/*/reports?*", (route) => {
    const range = new URL(route.request().url()).searchParams.get("range");
    ranges.push(range);
    const report = structuredClone(fixture);
    report.analytics.project_id = hub.fixture.project_id;
    if (range === "30d") report.completion.done = 1234;
    edit(report);
    return route.fulfill({ json: report });
  });
  return ranges;
}

async function openReports(page) {
  const errors = [];
  page.on("pageerror", (error) => errors.push(String(error)));
  const ranges = await routeReports(page);
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(hub.fixture.chat);
  if (page.viewportSize().width < 768)
    await page.getByRole("button", { name: "Toggle main sidebar", exact: true }).click();
  await page.getByTestId("nav-reports").click();
  await expect(page).toHaveURL(/\/reports$/);
  await expect(page.getByRole("region", { name: "Headline metrics" })).toContainText("276");
  return { errors, ranges };
}

for (const theme of ["light", "dark"]) {
  for (const width of [1440, 390]) {
    test(`Cloud Reports tabs ${theme} ${width}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1100 });
      await page.emulateMedia({ colorScheme: theme });
      const { errors } = await openReports(page);
      await page.evaluate((theme) => {
        document.documentElement.classList.toggle("dark", theme === "dark");
        document.documentElement.dataset.theme = theme;
      }, theme);
      const report = page.getByTestId("reports-page");
      for (const tab of TABS) {
        await page.getByRole("tab", { name: tab.label, exact: true }).click();
        await expect(page).toHaveURL(new RegExp(`${tab.path.replace("/", "\\/")}$`));
        await expect(page.getByRole("tab", { name: tab.label, exact: true })).toHaveAttribute("aria-selected", "true");
        await expect(report.getByRole("region", { name: tab.headline, exact: true })).toBeVisible();
        for (const name of tab.sections) await expect(report.getByRole("region", { name, exact: true })).toBeAttached();
        const sections = await report.locator('[data-slot="chart-panel"]').evaluateAll((panels) =>
          panels.map((panel) => {
            const chart = panel.querySelector('[data-slot="chart"]');
            const table = panel.querySelector("table");
            return !chart || !table || (chart.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
          }),
        );
        expect(sections.every(Boolean)).toBe(true);
        await expect(report).not.toContainText("Source coverage");
        await expect(report).not.toContainText("Partial");
        expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
        await test.info().attach(`cloud-reports-${tab.id}-${theme}-${width}.png`, {
          body: await page.screenshot({ animations: "disabled", caret: "hide", fullPage: true }),
          contentType: "image/png",
        });
      }
      expect(errors).toEqual([]);
    });
  }
}

test("Reports keeps the tab and range in the URL and restores them from it", async ({ page }) => {
  const ranges = await routeReports(page);
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/reports/cost?range=30d`);
  await expect(page.getByRole("tab", { name: "Cost", exact: true })).toHaveAttribute("aria-selected", "true");
  const range = page.getByRole("group", { name: "Report range" });
  await expect(range.getByRole("button", { name: "30d", exact: true })).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByRole("region", { name: "Cost headline" })).toBeVisible();
  expect(ranges.at(-1)).toBe("30d");
  await page.getByRole("tab", { name: "Flow", exact: true }).click();
  await expect(page).toHaveURL(/\/reports\/flow\?range=30d$/);
  await range.getByRole("button", { name: "24h", exact: true }).click();
  await expect(page).toHaveURL(/\/reports\/flow\?range=24h$/);
  await expect.poll(() => ranges.at(-1)).toBe("24h");
  await page.getByRole("button", { name: "From created", exact: true }).click();
  await expect(page).toHaveURL(/\/reports\/flow\?range=24h&clock=created$/);
  await page.goBack();
  await expect(page).toHaveURL(/\/reports\/flow\?range=24h$/);
  await page.reload();
  await expect(page.getByRole("tab", { name: "Flow", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(range.getByRole("button", { name: "24h", exact: true })).toHaveAttribute("aria-pressed", "true");
  await range.getByRole("button", { name: "7d", exact: true }).click();
  await expect(page).toHaveURL(/\/reports\/flow$/);
  await page.goto(`${hub.fixture.url}/reports/unknown?range=30d`);
  await expect(page).toHaveURL(/\/reports\?range=30d$/);
  await expect(page.getByRole("region", { name: "Headline metrics" })).toContainText("1,234");
});

test("Reports shows a dash for unrecorded data, the unavailable state and issue Diagnostics links", async ({ page }) => {
  await openReports(page);
  const headline = page.getByRole("region", { name: "Headline metrics" });
  await expect(page.getByRole("region", { name: "Stuck right now" }).getByRole("link").first()).toHaveAttribute(
    "href",
    "/work/i/wi_pilot?tab=diagnostics",
  );
  await page.unrouteAll();
  await routeReports(page, (report) => {
    report.unavailable.push("recorded_usage", "lane_residence_and_unclaimed_waits_unavailable", "landed_first_try");
    delete report.cancelled;
    delete report.previous_first_try_percent;
  });
  await page.getByRole("button", { name: "Refresh reports" }).click();
  await expect(headline).toContainText("Not recorded");
  await expect(headline).not.toContainText("$22.47");
  await expect(page.getByRole("region", { name: "Most expensive shipped issues" })).toContainText("Not recorded");
  await page.unrouteAll();
  await page.route("**/api/v2/organizations/*/reports?*", (route) => route.fulfill({ status: 503, json: { error: "unavailable" } }));
  await page.getByRole("button", { name: "Refresh reports" }).click();
  await expect(page.getByTestId("reports-page").getByRole("status")).toHaveText("Reports are temporarily unavailable.");
});

test("Reports follows the project select and decodes the real read on every tab", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  const response = await page.request.get(`${hub.fixture.url}/app/bootstrap`);
  const projects = (await response.json()).projects;
  expect(projects.length).toBeGreaterThan(1);
  for (const tab of TABS) {
    await page.goto(`${hub.fixture.url}${tab.path}`);
    await expect(page.getByTestId("reports-page").getByRole("region", { name: tab.headline, exact: true })).toBeVisible();
    await expect(page.getByTestId("reports-page")).not.toContainText("Reports are temporarily unavailable.");
  }
  await page.goto(`${hub.fixture.url}/reports/quality?range=90d`);
  await expect(page.getByRole("region", { name: "Quality headline" })).toBeVisible();
  await expect(page.getByTestId("reports-page")).not.toContainText("Reports are temporarily unavailable.");
  const next = projects.find((p) => p.id !== hub.fixture.project_id);
  const request = page.waitForRequest(
    (r) => r.url().includes("/reports?") && new URL(r.url()).searchParams.get("project") === next.id,
  );
  await page.getByRole("combobox", { name: "Project", exact: true }).click();
  await page.getByRole("option", { name: next.name, exact: true }).click();
  await request;
  await expect(page).toHaveURL(/\/reports\/quality\?range=90d$/);
  await expect(page.getByRole("navigation", { name: "Reports breadcrumb" })).toContainText(next.name);
});
