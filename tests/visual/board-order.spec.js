const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

async function orderedRuntime(name, refreshed) {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), `detent-${name}-`));
  const now = Date.now();
  const at = (hours) => new Date(now - hours * 3_600_000).toISOString();
  const issues = [
    { issue_id: "todo-urgent", identifier: "DD-1", title: "Urgent older card", state: "Backlog", priority: 1, updated_at: at(5) },
    { issue_id: "todo-new", identifier: "DD-2", title: "Newest normal card", state: "Backlog", priority: 3, updated_at: at(1) },
    { issue_id: "todo-label", identifier: "DD-3", title: "Older labelled normal card", state: "Backlog", priority: 3, labels: ["hotfix"], unblocker_count: 8, updated_at: at(refreshed ? 0 : 3) },
    { issue_id: "done-urgent", identifier: "DD-4", title: "Older urgent completion", state: "Done", priority: 1, updated_at: at(refreshed ? 0 : 4) },
    { issue_id: "done-new", identifier: "DD-5", title: "Newest completion without priority", state: "Done", updated_at: at(1) },
  ].map((issue) => ({ ...issue, project_id: "dogfood", stage_updated_at: at(6) }));
  fs.writeFileSync(path.join(home, "detent-board-snapshot.json"), JSON.stringify({
    schema: 1,
    saved_at: new Date(now).toISOString(),
    snapshot: {
      generated_at: new Date(now).toISOString(),
      project: { id: "dogfood", display_name: "dogfood" },
      projects: [{
        project: { id: "dogfood", display_name: "dogfood" },
        counts: { queue: 3 },
        refresh: { poll_interval_seconds: 60, status: "ready", last_refresh_at: new Date(now).toISOString() },
      }],
      counts: { queue: 3 },
      refresh: { poll_interval_seconds: 60, status: "ready", last_refresh_at: new Date(now).toISOString() },
      board_issues: issues,
      running: [], queue: [], blocked: [], completed: [],
    },
  }));
  const fixturePath = path.join(home, "issues.yaml");
  fs.writeFileSync(fixturePath, JSON.stringify({ issues: issues.map(({ issue_id, project_id, ...issue }) => ({ ...issue, id: issue_id })) }));
  return startDetentRuntime(name, ["--fixture", fixturePath], { home });
}

test("fleet and project lanes keep priority and newest activity order through SSE morphs", async ({ page }) => {
  const initial = await orderedRuntime("board-order-initial", false);
  const refreshed = await orderedRuntime("board-order-refreshed", true);
  try {
    for (const runtime of [initial, refreshed]) {
      await expect.poll(async () => {
        const response = await page.request.get(`${runtime.url}/api/v1/state`);
        const snapshot = await response.json();
        return snapshot.projects?.some((project) => project.tracker?.complete);
      }).toBe(true);
    }
    await page.addInitScript(() => {
      window.boardOrderSources = [];
      window.EventSource = class extends EventTarget {
        constructor() {
          super();
          this.readyState = 1;
          window.boardOrderSources.push(this);
          queueMicrotask(() => this.dispatchEvent(new Event("open")));
        }
        close() { this.readyState = 2; }
      };
    });
    for (const route of ["/", "/projects/dogfood/kanban"]) {
      await page.goto(`${initial.url}${route}`, { waitUntil: "domcontentloaded" });
      const snapshot = page.locator("#snapshot");
      await expect(snapshot).toHaveAttribute("hx-swap", "morph:innerHTML");
      const todo = page.locator('[data-board-lane="backlog"] [data-work-representation="board"]');
      const done = page.locator('[data-board-lane="done"] [data-work-representation="board"]');
      await expect(todo.locator("[data-board-card-title]")).toHaveText([
        "Urgent older card", "Newest normal card", "Older labelled normal card",
      ]);
      await expect(done.locator("[data-board-card-title]")).toHaveText([
        "Newest completion without priority", "Older urgent completion",
      ]);
      const originalSnapshot = await snapshot.elementHandle();
      const originalCard = await todo.last().elementHandle();
      const response = await page.request.get(`${refreshed.url}${route}`);
      expect(response.ok()).toBe(true);
      await page.evaluate((html) => {
        const fresh = new DOMParser().parseFromString(html, "text/html");
        window.boardOrderSources[0].dispatchEvent(new MessageEvent("snapshot", {
          data: fresh.querySelector("#snapshot").innerHTML,
        }));
      }, await response.text());
      await expect(todo.locator("[data-board-card-title]")).toHaveText([
        "Urgent older card", "Older labelled normal card", "Newest normal card",
      ]);
      await expect(done.locator("[data-board-card-title]")).toHaveText([
        "Older urgent completion", "Newest completion without priority",
      ]);
      expect(await originalSnapshot.evaluate((node) => node === document.querySelector("#snapshot"))).toBe(true);
      expect(await originalCard.evaluate((node) => node.isConnected)).toBe(true);
    }
  } finally {
    await Promise.all([initial.stop(), refreshed.stop()]);
  }
});
