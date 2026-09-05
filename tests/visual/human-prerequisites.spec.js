const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

for (const closed of [false, true]) {
  test(`human prerequisite waiting remains visible when closed=${closed}`, async ({ page }) => {
    const home = fs.mkdtempSync(path.join(os.tmpdir(), "detent-human-prerequisite-"));
    const fixture = path.join(home, "issues.json");
    fs.writeFileSync(fixture, JSON.stringify({ issues: [
      {
        id: "human-account",
        identifier: "owner/repo#10",
        title: "Enable test account authentication",
        state: closed ? "Done" : "Backlog",
        closed,
        labels: ["human-owned"],
        url: "https://github.test/owner/repo/issues/10",
      },
      {
        id: "dependent-child",
        identifier: "owner/repo#20",
        title: "Verify authenticated integration",
        state: "Todo",
        priority: 1,
        url: "https://github.test/owner/repo/issues/20",
        blocked_by: [{
          identifier: "owner/repo#10",
          state: closed ? "Done" : "Backlog",
          human_owned: true,
          human_completion_ready: false,
        }],
      },
    ] }));
    const runtime = await startDetentRuntime(`human-prerequisite-${closed}`, ["--fixture", fixture], { home });
    try {
      await page.goto(runtime.url, { waitUntil: "domcontentloaded" });
      const card = page.locator("[data-kanban-card]", { hasText: "Verify authenticated integration" });
      await expect(card).toBeVisible();
      await expect(card).toContainText("Waiting · 1");
      await expect(card).not.toContainText("Blocked · 1");
      await page.locator('[data-density-choice="comfy"]').click();
      await expect(card).toContainText("human prerequisite owner/repo#10");
      await page.screenshot({ path: path.join("tmp", "playwright-evidence", `human-prerequisite-${closed}.png`) });
    } finally {
      await runtime.stop();
    }
  });
}
