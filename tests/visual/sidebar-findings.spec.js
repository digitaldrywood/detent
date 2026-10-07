const { test, expect } = require("@playwright/test");
const path = require("node:path");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("sidebar-findings");
});
test.afterAll(async () => { await hub?.stop(); });

async function installFindings(page) {
  const at = (hour) => `2026-10-06T${hour}:00:00Z`;
  await page.clock.install({ time: new Date(at("12")) });
  let findings = [
    { id: "hf_item", class: "human", summary: "Human attention", next_action: "Check issue", subject: { kind: "work_item", id: hub.fixture.work_item }, opened_at: at("11"), resolved_at: null, severity: "attention" },
    { id: "hf_runner", class: "instance", summary: "Runner attention", next_action: "Check runner", subject: { kind: "runner", id: "runner_studio" }, opened_at: at("10"), resolved_at: null, severity: "attention" },
    { id: "hf_project", class: "flow", summary: "Flow attention", next_action: "Check flow", subject: { kind: "project", id: hub.fixture.project_id }, opened_at: at("09"), resolved_at: null, severity: "attention" },
    { id: "hf_watch", class: "flow", summary: "Flow attention", next_action: "Check flow", subject: { kind: "project", id: hub.fixture.project_id }, opened_at: at("08"), resolved_at: null, severity: "watch" },
  ];
  const errors = [];
  page.on("pageerror", (error) => errors.push(String(error)));
  await page.route(/\/conversations(?:\?.*)?$/, async (route) => {
    const response = await route.fetch();
    const data = await response.json();
    if (data.conversations) data.conversations = data.conversations.map((conversation) => conversation.work_item ? { ...conversation, execution: { ...conversation.execution, status: "waiting_input", updated_at: at("08") } } : conversation);
    await route.fulfill({ json: data });
  });
  await page.route("**/health/findings*", (route) => {
    const items = route.request().url().includes(`/projects/${hub.fixture.project_id}/`) ? findings : findings.filter((finding) => finding.subject.kind === "runner");
    return route.fulfill({ json: { items, last_tick_at: at("12") } });
  });
  await page.route("**/api/v2/organizations/*/fleet", (route) => route.fulfill({ json: require("../../web/conversation/src/contracts/fixtures/account-fleet.json") }));
  await page.route("**/api/v2/organizations/*/diagnostics?*", (route) => route.fulfill({ json: require("../../web/conversation/src/contracts/fixtures/diagnostics.json") }));
  await page.route("**/fleet?include=names", (route) => route.fulfill({ json: { runner_names: { runner_studio: { display_name: "Mac Studio", hostname: "studio" } } } }));
  await page.addInitScript(() => {
    const NativeSource = window.EventSource;
    window.findingSources = [];
    window.EventSource = function (url, options) {
      if (!/\/projects\/[^/]+\/events(?:\?|$)/.test(String(url))) return new NativeSource(url, options);
      const source = new EventTarget();
      source.close = () => {};
      source.readyState = 1;
      window.findingSources.push(source);
      return source;
    };
  });
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(hub.fixture.chat);
  if (page.viewportSize().width < 768) await page.getByRole("button", { name: "Toggle main sidebar", exact: true }).click();
  const sidebar = page.locator("aside.dc-side");
  await expect(sidebar.getByTestId("diagnostics-findings-count")).toHaveText("3");
  return {
    sidebar, errors,
    resolve: async (ids) => {
      findings = findings.filter((finding) => !ids.includes(finding.id));
      await page.evaluate((tick) => {
        for (const source of window.findingSources) source.dispatchEvent(new MessageEvent("health.findings", { data: tick }));
      }, ids.join(","));
    },
  };
}

for (const theme of ["light", "dark"]) {
  for (const width of [1440, 390]) {
    test(`attention findings ${theme} ${width}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1100 });
      await page.emulateMedia({ colorScheme: theme });
      const { sidebar, resolve, errors } = await installFindings(page);
      await page.evaluate((value) => {
        document.documentElement.classList.toggle("dark", value === "dark");
        document.documentElement.dataset.theme = value;
      }, theme);
      const group = sidebar.getByTestId("sidebar-needs-you");
      await expect(group.locator('[data-testid^="finding-"]')).toHaveCount(3);
      await expect(group.locator('[data-testid^="attention-issue-"]')).toHaveCount(1);
      expect(await group.locator('[data-testid^="attention-issue-"], [data-testid^="finding-"]').evaluateAll((rows) => rows[0].dataset.testid.startsWith("attention-issue-"))).toBe(true);
      const colors = await group.locator('[data-testid^="finding-"] svg').evaluateAll((icons) => icons.map((icon) => getComputedStyle(icon).color));
      expect(colors[0]).toBe(colors[2]);
      expect(colors[0]).not.toBe(colors[1]);
      expect(await group.locator('[data-testid^="finding-"]').evaluateAll((rows) => rows.map((row) => row.dataset.testid))).toEqual(["finding-hf_project", "finding-hf_runner", "finding-hf_item"]);
      await expect(sidebar.getByTestId("finding-hf_runner")).toContainText("Mac Studio");
      await expect(sidebar.getByTestId("finding-hf_item")).toContainText(/#\d+ /);
      await expect(group.locator("time")).toHaveText(["3h", "2h", "1h"]);
      await expect(sidebar.getByTestId("finding-hf_watch")).toHaveCount(0);
      expect(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)).toBe(false);
      await expect(sidebar).toHaveScreenshot(`sidebar-findings-${theme}-${width}.png`);
      await sidebar.screenshot({ path: path.join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `sidebar-findings-${theme}-${width}.png`) });
      await resolve(["hf_runner"]);
      await expect(sidebar.getByTestId("finding-hf_runner")).toHaveCount(0);
      await expect(sidebar.getByTestId("diagnostics-findings-count")).toHaveText("2");
      await resolve(["hf_project", "hf_item"]);
      await expect(sidebar.getByTestId("diagnostics-findings-count")).toHaveCount(0);
      await expect(group).toHaveCount(0);
      expect(errors).toEqual([]);
    });
  }
}

test("findings open issue Diagnostics, Fleet, and the Diagnostics page", async ({ page }) => {
  const { sidebar } = await installFindings(page);
  await sidebar.getByTestId("finding-hf_item").click();
  await expect(page).toHaveURL(new RegExp(`/work/i/${hub.fixture.work_item}\\?tab=diagnostics$`));
  await expect(page.getByRole("tab", { name: "Diagnostics", exact: true })).toHaveAttribute("aria-selected", "true");
  await sidebar.getByTestId("finding-hf_project").click();
  await expect(page).toHaveURL(/\/diagnostics$/);
  await expect(page.getByRole("heading", { level: 1, name: "Diagnostics", exact: true })).toBeVisible();
  await sidebar.getByTestId("finding-hf_runner").click();
  await expect(page).toHaveURL(/\/settings\/runners$/);
});
