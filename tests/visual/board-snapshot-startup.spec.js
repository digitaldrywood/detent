const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test("boot renders a seeded last-known board before the live morph", async ({ page }) => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "detent-board-snapshot-startup-"));
  const fixturePath = path.join(home, "issues.yaml");
  const snapshotPath = path.join(home, "detent-board-snapshot.json");
  const generatedAt = new Date(Date.now() - 30_000);
  const priority = 1;
  fs.writeFileSync(
    fixturePath,
    `issues:
  - id: snapshot-live-card
    identifier: digitaldrywood/detent#9135
    title: Live board card after hydration
    state: Todo
    url: https://github.test/digitaldrywood/detent/issues/9135
    priority: ${priority}
    priority_name: P1
`,
  );
  fs.writeFileSync(
    snapshotPath,
    JSON.stringify({
      schema: 1,
      saved_at: new Date().toISOString(),
      snapshot: {
        generated_at: generatedAt.toISOString(),
        project: { id: "dogfood", display_name: "dogfood" },
        projects: [
          {
            project: { id: "dogfood", display_name: "dogfood" },
            counts: { queue: 1 },
            refresh: {
              poll_interval_seconds: 60,
              status: "ready",
              last_refresh_at: generatedAt.toISOString(),
            },
          },
        ],
        refresh: {
          poll_interval_seconds: 60,
          status: "ready",
          last_refresh_at: generatedAt.toISOString(),
        },
        counts: { queue: 1 },
        board_issues: [
          {
            issue_id: "snapshot-live-card",
            identifier: "digitaldrywood/detent#9135",
            project_id: "dogfood",
            title: "Cached board card before hydration",
            state: "Todo",
            url: "https://github.test/digitaldrywood/detent/issues/9135",
            priority,
            priority_name: "P1",
          },
        ],
        running: [],
        queue: [],
        blocked: [],
        completed: [],
      },
    }),
  );

  const runtime = await startDetentRuntime(
    "board-snapshot-startup",
    ["--fixture", fixturePath],
    { home },
  );
  try {
    await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
    const snapshot = page.locator("#snapshot");
    const originalSnapshot = await snapshot.elementHandle();
    const tooltip = page.locator("#help-tooltip");
    const originalTooltip = await tooltip.elementHandle();

    const staleBanner = page.locator("#board-last-known");
    await expect(staleBanner).toBeVisible();
    await expect(staleBanner).toContainText("Showing last state from");
    await expect(staleBanner).toContainText("refreshing…");
    await expect(snapshot).toContainText("Cached board card before hydration");
    await expect(page.locator("#board-lanes")).toHaveClass(/grayscale/);

    const priorityTrigger = page.locator("[data-board-priority]");
    await priorityTrigger.hover();
    await expect(tooltip).toHaveAttribute("data-open", "true");
    const originalPriorityTrigger = await priorityTrigger.elementHandle();

    await expect(staleBanner).toHaveCount(0);
    await expect(snapshot).toContainText("Live board card after hydration");
    await expect(page.locator("#board-lanes")).not.toHaveClass(/grayscale/);
    expect(await originalSnapshot?.evaluate((element) => element.isConnected)).toBe(true);
    expect(await originalTooltip?.evaluate((element) => element.isConnected)).toBe(true);
    expect(await originalPriorityTrigger?.evaluate((element) => element.isConnected)).toBe(true);
  } finally {
    await runtime.stop();
  }
});
