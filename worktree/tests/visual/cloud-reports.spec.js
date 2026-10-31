const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");
const fixture = require("../../web/conversation/src/contracts/fixtures/reports.json");
const optionsFixture = require("../../web/conversation/src/contracts/fixtures/reportOptions.json");
const organizationFixture = require("../../web/conversation/src/contracts/fixtures/organizationReports.json");

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
  await page.goto(`${hub.fixture.chat}/p/${hub.fixture.project_id}`);
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
        const height = await report.evaluate((node) => node.scrollHeight + node.getBoundingClientRect().top);
        await page.setViewportSize({ width, height: Math.max(1100, Math.ceil(height)) });
        await test.info().attach(`cloud-reports-${tab.id}-${theme}-${width}.png`, {
          body: await page.screenshot({ animations: "disabled", caret: "hide" }),
          contentType: "image/png",
        });
        await page.setViewportSize({ width, height: 1100 });
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

async function routeReportRequests(page, edit = () => {}) {
  const requests = [];
  await page.route("**/api/v2/organizations/*/reports/options?*", (route) => route.fulfill({ json: optionsFixture }));
  await page.route("**/api/v2/organizations/*/reports?*", (route) => {
    const params = new URL(route.request().url()).searchParams;
    requests.push(params);
    if (params.get("scope") === "organization") {
      const report = structuredClone(organizationFixture);
      edit(report, params);
      return route.fulfill({ json: report });
    }
    const report = structuredClone(fixture);
    report.analytics.project_id = params.get("project") === "all" ? "all" : hub.fixture.project_id;
    if (params.has("from")) {
      report.analytics.window = { from: `${params.get("from")}T00:00:00Z`, to: `${params.get("to")}T00:00:00Z` };
      report.previous.done = 301;
    }
    edit(report, params);
    return route.fulfill({ json: report });
  });
  return requests;
}

async function attachFilterRow(page, name) {
  await page.evaluate(() => document.activeElement instanceof HTMLElement && document.activeElement.blur());
  const row = page.getByTestId("reports-page").getByRole("group", { name: "Filters" }).locator("..");
  await test.info().attach(name, { body: await row.screenshot({ animations: "disabled", caret: "hide" }), contentType: "image/png" });
}

for (const theme of ["light", "dark"]) {
  test(`Reports filters by chip, custom range and prior period, and keeps them in the URL ${theme}`, async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 1100 });
    await page.emulateMedia({ colorScheme: theme });
    const errors = [];
    page.on("pageerror", (error) => errors.push(String(error)));
    const requests = await routeReportRequests(page, (report, params) => {
      if (params.get("scope") === "organization") report.current.completed = 4321;
    });
    await page.goto(hub.fixture.accounts.owner);
    await page.goto(`${hub.fixture.url}/reports`);
    await page.evaluate((theme) => {
      document.documentElement.classList.toggle("dark", theme === "dark");
      document.documentElement.dataset.theme = theme;
    }, theme);
    const headline = page.getByRole("region", { name: "Headline metrics" });
    await expect(headline).toContainText("276");
    const filters = page.getByTestId("reports-page").getByRole("group", { name: "Filters" });
    for (const name of ["Label", "Priority", "Type", "Filed by", "Runner"]) await expect(filters.getByRole("combobox", { name: `${name}, filter` })).toBeEnabled();

    await filters.getByRole("combobox", { name: "Label, filter" }).click();
    await page.getByRole("option", { name: "cloud", exact: true }).click();
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(/\/reports\?labels=cloud$/);
    await expect.poll(() => requests.at(-1)?.getAll("labels")).toEqual(["cloud"]);
    await expect(filters.getByRole("combobox", { name: "Label: cloud, filter" })).toHaveAttribute("aria-pressed", "true");
    await expect(filters.getByRole("button", { name: "Clear Label filter" })).toBeVisible();
    await attachFilterRow(page, `cloud-reports-filter-set-chip-${theme}.png`);

    await filters.getByRole("combobox", { name: "Priority, filter" }).click();
    await page.getByRole("option", { name: "High", exact: true }).click();
    await page.getByRole("option", { name: "Urgent", exact: true }).click();
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(/\/reports\?labels=cloud&priorities=High&priorities=Urgent$/);
    await expect.poll(() => requests.at(-1)?.getAll("priorities")).toEqual(["1", "0"]);
    await expect(filters.getByRole("combobox", { name: "Priority: 2 selected, filter" })).toBeVisible();

    const range = page.getByRole("group", { name: "Report range" });
    await range.getByRole("button", { name: "Custom range", exact: true }).click();
    const picker = page.getByRole("dialog", { name: "Custom range" });
    await expect(picker).toBeVisible();
    const days = picker.locator("[data-day]:not([data-disabled]) button");
    await days.nth(2).click();
    await days.nth(6).click();
    await expect(picker.getByText(/· 5 days, UTC$/)).toBeVisible();
    await test.info().attach(`cloud-reports-custom-range-${theme}.png`, { body: await page.screenshot({ animations: "disabled", caret: "hide" }), contentType: "image/png" });
    await picker.getByRole("button", { name: "Apply", exact: true }).click();
    await expect(page).toHaveURL(/\/reports\?from=\d{4}-\d{2}-\d{2}&to=\d{4}-\d{2}-\d{2}&labels=cloud&priorities=High&priorities=Urgent$/);
    const custom = requests.at(-1);
    expect(custom.get("range")).toBeNull();
    expect(custom.get("from")).toMatch(/^\d{4}-\d{2}-\d{2}$/);
    await expect(range.getByRole("button", { name: /^Custom range, / })).toHaveAttribute("aria-pressed", "true");
    await expect(page.getByRole("button", { name: "vs prior period", exact: true })).toHaveAttribute("aria-pressed", "true");
    await expect(headline).toContainText("25 vs prior period");
    await test.info().attach(`cloud-reports-compare-delta-${theme}.png`, { body: await headline.screenshot({ animations: "disabled", caret: "hide" }), contentType: "image/png" });

    const url = page.url();
    await page.goto("about:blank");
    await page.goto(url);
    await expect(filters.getByRole("combobox", { name: "Label: cloud, filter" })).toBeVisible();
    await expect.poll(() => [requests.at(-1)?.get("from"), requests.at(-1)?.getAll("labels")]).toEqual([custom.get("from"), ["cloud"]]);

    await filters.getByRole("button", { name: "Clear Label filter" }).click();
    await expect(page).toHaveURL(/\/reports\?from=[^&]+&to=[^&]+&priorities=High&priorities=Urgent$/);

    await page.getByRole("combobox", { name: "Project", exact: true }).click();
    await page.getByRole("option", { name: "All projects", exact: true }).click();
    await expect(page).toHaveURL(/[?&]project=all(&|$)/);
    await expect.poll(() => requests.at(-1)?.get("project")).toBe("all");
    await expect(headline).toContainText("4,321");
    await expect(page.getByRole("navigation", { name: "Reports breadcrumb" })).toContainText("All projects");
    expect(errors).toEqual([]);
  });
}

test("Reports opens the filter chips from the Filters button on a phone", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 900 });
  const requests = await routeReportRequests(page);
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/reports/cost?labels=cloud&stages=code`);
  await expect(page.getByRole("region", { name: "Cost headline" })).toBeVisible();
  await expect.poll(() => [requests.at(-1)?.getAll("labels"), requests.at(-1)?.getAll("stages")]).toEqual([["cloud"], ["code"]]);
  await page.getByRole("button", { name: "Filters, 2 set", exact: true }).click();
  const filters = page.getByRole("dialog").getByRole("group", { name: "Filters" });
  for (const name of ["Label: cloud", "Priority", "Type", "Filed by", "Runner", "Model", "Stage: Code"]) await expect(filters.getByRole("combobox", { name: `${name}, filter` })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
  await test.info().attach("cloud-reports-filters-390.png", { body: await page.screenshot({ animations: "disabled", caret: "hide" }), contentType: "image/png" });
  await page.getByRole("button", { name: "Clear filters", exact: true }).click();
  await expect(page).toHaveURL(/\/reports\/cost$/);
});

test("Reports shows No prior data instead of a delta when the period before did not exist", async ({ page }) => {
  await routeReportRequests(page, (report) => {
    report.prior_recorded = false;
  });
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/reports`);
  const headline = page.getByRole("region", { name: "Headline metrics" });
  await expect(headline).toContainText("276");
  await expect(headline.getByText("No prior data")).toHaveCount(5);
  await expect(headline).not.toContainText("vs prior 7d");
});

test("Reports shows a dash for unrecorded data, the unavailable state and issue Diagnostics links", async ({ page }) => {
  await openReports(page);
  const headline = page.getByRole("region", { name: "Headline metrics" });
  await expect(page.getByRole("region", { name: "Stuck right now" }).getByRole("link", { name: /prove native pilot readiness/ })).toHaveAttribute(
    "href",
    "/work/i/wi_pilot?tab=diagnostics",
  );
  await expect(page.getByRole("link", { name: "All aging work" })).toHaveAttribute("href", "/reports/flow");
  await page.unrouteAll();
  await routeReports(page, (report) => {
    report.unavailable.push("recorded_usage", "lane_residence_and_unclaimed_waits_unavailable", "landed_first_try");
    delete report.cancelled;
    delete report.previous_first_try_percent;
  });
  await page.getByRole("button", { name: "Refresh reports" }).click();
  await expect(headline).toContainText("Not recorded");
  await expect(headline).not.toContainText("$22.47");
  await expect(headline).toContainText("No prior data");
  await expect(headline).not.toContainText("▼ 0 pts");
  await expect(page.getByRole("region", { name: "Most expensive shipped issues" })).toContainText("Not recorded");
  await page.unrouteAll();
  await page.route("**/api/v2/organizations/*/reports?*", (route) => route.fulfill({ status: 503, json: { error: "unavailable" } }));
  await page.getByRole("button", { name: "Refresh reports" }).click();
  await expect(page.getByTestId("reports-page").getByRole("status")).toHaveText("Reports are temporarily unavailable.");
  await expect(headline).toHaveCount(0);
  await page.reload();
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
  await expect(page).toHaveURL(new RegExp(`/reports/quality\\?project=${next.id}&range=90d$`));
  await expect(page.getByRole("navigation", { name: "Reports breadcrumb" })).toContainText(next.name);
});
