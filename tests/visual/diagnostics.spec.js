const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");
const fixture = require("../../web/conversation/src/contracts/fixtures/diagnostics.json");
const healthFixture = require("../../web/conversation/src/contracts/fixtures/health-findings.json");
const fleetFixture = require("../../web/conversation/src/contracts/fixtures/account-fleet.json");

let hub;

test.beforeEach(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("diagnostics", {
    env: { DETENT_HOSTED_BROWSER_UNSIGNED_MEMBER: "1" },
  });
});

test.afterEach(async () => {
  await hub?.stop();
});

function fleet(longErrors = false) {
  const base = fleetFixture.runners[0];
  const raw = "policy_digest git command error_output ".repeat(50);
  return { ...fleetFixture, runners: [8, 4, 8].map((capacity, index) => ({
    ...base, id: `runner_${index}`, machine_id: `machine_${index}`, display_name: ["Example desktop", "Example laptop", "Example build host"][index], hostname: `example-host-${index}`,
    health: "online", capacity_health: "online", connection_health: "online", state: index === 2 ? "draining" : "active", claim_refusal_reason: "", host_capacity: capacity, host_used: 1, capacity_limit: index === 0 ? 2 : capacity, reported_capacity: index === 0 ? 2 : capacity, provider_capacity: [],
    update: index === 2 ? { status: "draining", desired: { version: "v1.0.0" } } : undefined,
    problems: longErrors ? [{ code: index === 1 ? "policy_mismatch" : "settings_invalid", project_id: index === 1 ? hub.fixture.project_id : "prj_optional", subject: "project configuration", check: "load settings", message: raw, error_output: raw, fix_hint: raw, first_seen: "2026-10-07T10:00:00Z" }] : [],
  })) };
}

async function open(page, report = fixture, longErrors = false) {
  await page.route("**/app/bootstrap", async (route) => {
    const response = await route.fetch();
    const bootstrap = await response.json();
    bootstrap.organization.name = "Example organization";
    bootstrap.projects = bootstrap.projects.map((project) => ({ ...project, name: "Example project" }));
    if (longErrors) bootstrap.projects.push({ ...bootstrap.projects[0], id: "prj_optional", name: "Optional project" });
    await route.fulfill({ response, json: bootstrap });
  });
  await page.route("**/api/v2/organizations/*/fleet", (route) =>
    route.fulfill({ json: fleet(longErrors) }),
  );
  await page.route("**/api/v2/organizations/*/diagnostics?*", (route) =>
    route.fulfill({ json: report }),
  );
  await page.route("**/api/v2/organizations/*/projects/*/health/findings*", (route) => {
    if (report.findings === null) return route.fulfill({
      status: 404,
      json: { code: "not_found", message: "Not available" },
    });
    const project = new URL(route.request().url()).pathname.split("/").at(-3);
    const items = healthFixture.items
      .filter((finding) => finding.subject.kind === "runner" || project === hub.fixture.project_id)
      .map((finding) => finding.subject.kind === "work_item"
        ? { ...finding, subject: { kind: "work_item", id: hub.fixture.work_item } }
        : finding.subject.kind === "runner"
          ? { ...finding, summary: "Runner connection needs review", next_action: "Runner operator: check the runner process and its Hub connection.", subject: { ...finding.subject, id: "runner_0", project_id: hub.fixture.project_id } }
          : { ...finding, subject: { ...finding.subject, id: hub.fixture.project_id } });
    return route.fulfill({ json: { ...healthFixture, items } });
  });
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
  ).toHaveCount((report.findings?.length ?? 0) + (longErrors ? 3 : 0));
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
  await expect(rows.first()).toContainText("Example project");
  await expect(rows.first()).not.toContainText("Project unavailable to you");
  await expect(rows.first().getByRole("link")).toHaveAttribute(
    "href",
    /\/work\/i\/wi_[^?]+\?tab=diagnostics$/,
  );
  await expect(rows.nth(1).getByRole("link")).toHaveAttribute(
    "href",
    /\/fleet$/,
  );
  await expect(rows.nth(2).getByRole("link")).toHaveAttribute(
    "href",
    `/work/p/${hub.fixture.project_id}`,
  );
  await expect(
    page.getByRole("img", { name: "Hourly slots in use and Todo queue depth" }),
  ).toBeVisible();
  const capacity = page.getByRole("region", { name: "Capacity right now", exact: true });
  await expect(capacity).toContainText("3 of 14 slots occupied");
  await expect(capacity).toContainText("7 slots can't take work");
  await expect(capacity.getByRole("link", { name: "Example desktop" })).toBeVisible();
  await rows.first().getByRole("link").click();
  await expect(page).toHaveURL(/\/local-organization\/browser-collaboration\/issues\/2\?tab=diagnostics$/);
  await expect(page.getByRole("tab", { name: "Diagnostics", exact: true })).toHaveAttribute("aria-selected", "true");
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
  await page.getByRole("button", { name: /^Diagnostics(?: \d+)?$/ }).click();
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
    page.getByRole("region", { name: "Work throughput", exact: true }),
  ).toContainText("Not recorded");
  await page.unroute("**/api/v2/organizations/*/diagnostics?*");
  await page.route("**/api/v2/organizations/*/diagnostics?*", (route) =>
    route.fulfill({ json: { ...fixture, findings: [] } }),
  );
  await page.unroute("**/api/v2/organizations/*/projects/*/health/findings*");
  await page.route("**/api/v2/organizations/*/projects/*/health/findings*", (route) =>
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
  await page.unroute("**/api/v2/organizations/*/projects/*/health/findings*");
  await page.route("**/api/v2/organizations/*/projects/*/health/findings*", (route) =>
    route.fulfill({
      status: 503,
      json: { code: "unavailable", message: "Unavailable" },
    }),
  );
  await page.getByRole("button", { name: "Refresh diagnostics" }).click();
  const refreshError = attention.getByText(
    "Unable to refresh findings. Any findings below are from the last successful read.",
    { exact: true },
  );
  await expect(refreshError).toBeVisible();
  await expect(attention).toContainText("No open findings.");
  await expect(attention).not.toContainText("Findings are unavailable");
  const diagnosticsError = page.getByText(
    "Diagnostics are temporarily unavailable.",
    { exact: true },
  );
  await expect(diagnosticsError).not.toBeVisible();

  await page.reload();
  await expect(diagnosticsError).toBeVisible();
  await expect(attention).toContainText("Findings are unavailable");
  await expect(attention).not.toContainText("No open findings.");

  await page.unroute("**/api/v2/organizations/*/diagnostics?*");
  await page.route("**/api/v2/organizations/*/diagnostics?*", (route) =>
    route.fulfill({ json: { ...fixture, findings: [] } }),
  );
  await page.unroute("**/api/v2/organizations/*/projects/*/health/findings*");
  await page.route("**/api/v2/organizations/*/projects/*/health/findings*", (route) =>
    route.fulfill({ json: { ...healthFixture, items: [] } }),
  );
  await page.getByRole("button", { name: "Refresh diagnostics" }).click();
  await expect(attention).toContainText("No open findings.");
  await expect(refreshError).not.toBeVisible();
  await expect(diagnosticsError).not.toBeVisible();
  await expect(attention).not.toContainText("Findings are unavailable");
});

for (const theme of ["light", "dark"]) {
  for (const width of [1440, 390]) {
    test(`Diagnostics ${theme} at ${width}px`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 900 });
      await page.emulateMedia({ colorScheme: theme });
      await open(page, fixture, true);
      await page.evaluate((theme) => {
        document.documentElement.classList.toggle("dark", theme === "dark");
        document.documentElement.dataset.theme = theme;
      }, theme);
      for (const section of [
        "Needs attention",
        "Capacity right now",
        "Capacity and throughput history",
        "Hourly capacity",
        "Work throughput",
        "Merge throughput",
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
      const live = page.getByRole("region", { name: "Capacity right now", exact: true });
      const attention = page.getByRole("region", { name: "Needs attention", exact: true });
      const liveBounds = await live.boundingBox();
      const attentionBounds = await attention.boundingBox();
      expect(liveBounds.y + liveBounds.height).toBeLessThanOrEqual(attentionBounds.y);
      for (const name of ["Example desktop", "Example laptop", "Example build host"]) {
        const summary = live.getByRole("link", { name });
        await expect(summary).toBeVisible();
        const summaryBounds = await summary.boundingBox();
        expect(summaryBounds.y + summaryBounds.height).toBeLessThan(900);
      }
      await expect(live).toContainText("Finishing work before updating");
      await expect(attention).toContainText("Optional project");
      await expect(attention).toContainText("Not taking work because project settings could not be loaded");
      await expect(attention).toContainText("Example project");
      await expect(attention.getByRole("link", { name: "Review policy" })).toHaveAttribute("href", `/settings/projects/${hub.fixture.project_id}/workflow`);
      await expect(page.getByText("Instrumentation coverage", { exact: true })).toHaveCount(0);
      await expect(page.getByText(/policy_digest|git command error_output/)).toHaveCount(0);
      await expect(page.getByRole("button", { name: "Refresh diagnostics" })).toBeEnabled();
    });
  }
}
