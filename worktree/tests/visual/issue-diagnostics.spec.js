const { test, expect } = require("@playwright/test");
const path = require("node:path");
const AxeBuilder = require("@axe-core/playwright").default;
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");
const { installDiagnostics } = require("./issue-diagnostics-fixture");

let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("issue-diagnostics");
});
test.afterAll(async () => { await hub?.stop(); });

for (const state of ["Blocked", "Done"]) {
  for (const theme of ["light", "dark"]) {
    for (const width of [1440, 390]) {
      test(`${state} diagnostics ${theme} ${width}`, async ({ page }) => {
        await page.setViewportSize({ width, height: 1100 });
        await page.emulateMedia({ colorScheme: theme });
        await installDiagnostics(page, hub.fixture, state);
        await page.evaluate((resolvedTheme) => {
          document.documentElement.classList.toggle("dark", resolvedTheme === "dark");
          document.documentElement.dataset.theme = resolvedTheme;
        }, theme);
        const tab = page.getByRole("tab", { name: "Diagnostics", exact: true });
        await tab.click();
        await expect(tab).toHaveAttribute("aria-selected", "true");
        const panel = page.getByTestId("issue-diagnostics");
        await expect(panel.getByRole("heading", { name: "Evidence", exact: true })).toBeVisible();
        await expect(panel.getByText("Loading diagnostics…")).toHaveCount(0);
        await expect(panel.getByText("historical scheduler decision", { exact: true }).locator("..")).toContainText("unavailable");
        await expect(panel.getByText("gpt-6.1-sol · high", { exact: true })).toBeVisible();
        if (state === "Blocked") {
          await expect(panel.getByText("failed ×3", { exact: true })).toBeVisible();
          await expect(panel.getByText("Reduce the instruction payload before resuming this issue.", { exact: true }).first()).toBeVisible();
          await expect(panel.getByText("6,000", { exact: true })).toBeVisible();
        } else {
          await expect(panel.getByText("This issue is Done.", { exact: true })).toBeVisible();
          await expect(panel.getByText("No further execution is scheduled.", { exact: true })).toBeVisible();
        }
        const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
        expect(overflow).toBe(false);
        const violations = (await new AxeBuilder({ page }).include('[data-testid="issue-diagnostics-panel"]').include('[aria-label="Issue detail tabs"]').analyze()).violations;
        expect(violations.filter((violation) => ["serious", "critical"].includes(violation.impact))).toEqual([]);
        await panel.screenshot({ path: path.join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `diagnostics-${state.toLowerCase()}-${theme}-${width}.png`) });
        await panel.getByRole("heading", { name: "Evidence", exact: true }).evaluate((heading) => heading.parentElement.scrollIntoView({ block: "end" }));
        await page.screenshot({ path: path.join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `diagnostics-evidence-${state.toLowerCase()}-${theme}-${width}.png`) });
        await page.getByRole("tab", { name: "Timeline", exact: true }).click();
        await expect(panel).toHaveCount(0);
      });
    }
  }
}

test("unavailable reads retain the tab and the unavailable states", async ({ page }) => {
  await installDiagnostics(page, hub.fixture, "Blocked", true);
  await page.getByRole("tab", { name: "Diagnostics", exact: true }).click();
  await expect(page.getByText("Current state explanation unavailable.", { exact: true })).toBeVisible();
  await expect(page.getByText("Attempts unavailable.", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry diagnostics", exact: true })).toBeVisible();
});

test("Slack issue link opens the Diagnostics tab", async ({ page }) => {
  await installDiagnostics(page, hub.fixture, "Blocked", false, "?tab=diagnostics");
  await expect(page.getByRole("tab", { name: "Diagnostics", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByTestId("issue-diagnostics").getByRole("heading", { name: "Evidence", exact: true })).toBeVisible();
});
