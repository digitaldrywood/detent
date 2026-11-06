const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");
let runtime;
test.beforeAll(async () => { runtime = await startDetentRuntime("card-facts", ["--demo", "screenshots"]); });
test.afterAll(async () => { await runtime?.stop(); });

test("active cards and state API expose the same head and session facts", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": "board-card-facts" });
  await page.goto(runtime.url, { waitUntil: "domcontentloaded" });
  await expect(page.locator("#board-lanes")).toBeVisible();
  for (const [state, ci, merge] of [["queued", "queued", "clean"], ["in_progress", "running", "blocked"], ["success", "green", "unstable"], ["failure", "red", "dirty"], ["skipped", "skipped", "clean"]]) {
    const card = page.locator("article").filter({ has: page.locator("[data-board-card-title]", { hasText: `Card facts ${state}` }) });
    await expect(card.locator('[data-card-fact]')).toHaveCount(0);
    await expect(card).toHaveAttribute("title", new RegExp(`CI ${ci}`));
    await expect(card).toHaveAttribute("title", /merge_conflict · 2026-06-15T09:00:00Z/);
    await expect(card).toHaveAttribute("title", /attempts today/);
  }

  const response = await page.request.get(`${runtime.url}/api/v1/state`, { headers: { "X-Detent-Demo-Scenario": "board-card-facts" } });
  const { board } = await response.json();
  const running = board.cards.find(card => card.issue_id === "facts-in_progress");
  expect(running).toMatchObject({ has_pr: true, head_sha: "head-in_progress", head_committed_at: "2026-06-15T10:00:00Z", ci: "running", mergeability: "blocked", session_started_at: "2026-06-15T11:30:00Z", session_tokens: 12345, attempts_today: 75, lane_reason: "merge_conflict", lane_reason_at: "2026-06-15T09:00:00Z" });
  expect(board.cards.find(card => card.issue_id === "facts-none")).toMatchObject({ has_pr: false, session_started_at: null, attempts_today: 0 });
  await expect(page.locator("#snapshot")).toHaveAttribute("hx-swap", "morph:innerHTML");
  for (const density of ["compact", "cozy", "comfy"]) {
    await page.locator(`[data-density-choice="${density}"]`).click();
    await expect(page.locator("[data-board-card-facts]")).toHaveCount(0);
  }
});
