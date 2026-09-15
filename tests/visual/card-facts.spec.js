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
    await expect(card.locator('[data-card-fact="push"]')).toHaveText("push 2h");
    await expect(card.locator('[data-card-fact="ci"]')).toHaveText(`CI ${ci}`);
    await expect(card.locator('[data-card-fact="mergeability"]')).toHaveText(merge);
    await expect(card.locator('[data-card-fact="reason"]')).toHaveText("merge_conflict");
    await expect(card.locator('[data-card-fact="reason"]')).toHaveAttribute("title", "merge_conflict · 2026-06-15T09:00:00Z");
    await expect(card.locator('[data-card-fact="session"]')).toContainText("75 today");
    await expect(card.locator('[data-card-fact="session"]')).toContainText(state === "in_progress" ? "30m · 12345 tok" : "no session");
  }
  const absent = page.locator("article").filter({ hasText: "Card facts absent" });
  await expect(absent.locator('[data-card-fact="push"]')).toHaveText("no PR");
  await expect(absent.locator('[data-card-fact="session"]')).toContainText("no session · 0 today");
  const response = await page.request.get(`${runtime.url}/api/v1/state`, { headers: { "X-Detent-Demo-Scenario": "board-card-facts" } });
  const { board } = await response.json();
  const running = board.cards.find(card => card.issue_id === "facts-in_progress");
  expect(running).toMatchObject({ has_pr: true, head_sha: "head-in_progress", head_committed_at: "2026-06-15T10:00:00Z", ci: "running", mergeability: "blocked", session_started_at: "2026-06-15T11:30:00Z", session_tokens: 12345, attempts_today: 75, lane_reason: "merge_conflict", lane_reason_at: "2026-06-15T09:00:00Z" });
  expect(board.cards.find(card => card.issue_id === "facts-none")).toMatchObject({ has_pr: false, session_started_at: null, attempts_today: 0 });
  await expect(page.locator("#snapshot")).toHaveAttribute("hx-swap", "morph:innerHTML");
  await page.evaluate(() => {
    const snapshot = document.querySelector("#snapshot");
    window.__factsNode = snapshot.querySelector('[data-card-fact="reason"]');
    const next = snapshot.cloneNode(true);
    next.querySelector('[data-card-fact="reason"]').textContent = "workpad_status_invalid";
    next.querySelector('[data-card-fact="reason"]').title = "workpad_status_invalid · 2026-06-15T12:00:00Z";
    window.Idiomorph.morph(snapshot, next.innerHTML, { morphStyle: "innerHTML" });
  });
  expect(await page.evaluate(() => window.__factsNode === document.querySelector('[data-card-fact="reason"]'))).toBe(true);
  await expect(page.locator('[data-card-fact="reason"]').first()).toHaveText("workpad_status_invalid");
  await expect(page.locator('[data-card-fact="reason"]').first()).toHaveAttribute("title", "workpad_status_invalid · 2026-06-15T12:00:00Z");
  await page.getByRole("button", { name: "Compact", exact: true }).click();
  const geometry = await page.locator('[data-board-card-facts]').evaluateAll(rows => rows.map(row => ({
    height: row.getBoundingClientRect().height,
    fields: [...row.querySelectorAll('[data-card-fact]:not([data-card-fact="reason"])')].map(field => ({ width: field.clientWidth, content: field.scrollWidth })),
  })));
  for (const row of geometry) {
    expect(row.height).toBeLessThanOrEqual(16);
    for (const field of row.fields) expect(field.content).toBeLessThanOrEqual(field.width + 1);
  }

});
