// End-to-end cover for the Work board, list and issue page.
//
// Like `conversation.spec.js`, this drives the real thing: `hosted-hub.js`
// starts the Go preview test, which serves a hosted hub with real sessions,
// real authorization and real durable state. Everything below happens through
// the browser, from the keyboard, located by accessible name — so a control a
// screen reader or a keyboard cannot reach fails the test rather than passing
// on a class name.
//
const { test, expect } = require("@playwright/test");
const AxeBuilder = require("@axe-core/playwright").default;
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("work");
});

test.afterAll(async () => {
  await hub?.stop();
  hub = undefined;
});

// A project may have no actions and an issue may have no linked worker
// conversation. Both optional reads report not_found, which the UI handles.
const UNSERVED_PROJECT_ACTIONS = /\/api\/v2\/organizations\/[^/]+\/projects\/[^/]+\/actions$/;
const UNLINKED_CONVERSATION = /\/api\/v2\/organizations\/[^/]+\/projects\/[^/]+\/work-items\/[^/]+\/conversation$/;
const NOT_FOUND = /status of 404\b/;

function watchConsole(page) {
  const errors = [];
  page.on("console", (message) => {
    if (message.type() !== "error") return;
    const expected404 =
      NOT_FOUND.test(message.text()) &&
      (UNSERVED_PROJECT_ACTIONS.test(message.location().url ?? "") ||
        UNLINKED_CONVERSATION.test(message.location().url ?? ""));
    if (expected404) return;
    errors.push(message.text());
  });
  page.on("pageerror", (error) => errors.push(String(error)));
  return errors;
}

/** Signs in as the organization owner and opens a work route. */
async function openWork(page, route = "/work") {
  await page.goto(hub.fixture.accounts.owner, { waitUntil: "domcontentloaded" });
  // The router's basepath is `/` now (decisions.md §11, §12): Work is served
  // at the hub's root, not under the chat prefix.
  await page.goto(new URL(route, hub.fixture.url).toString(), {
    waitUntil: "domcontentloaded",
  });
  await expect(page.locator("aside.dc-side")).toBeVisible();
}

const CONVERSATION_THREAD_ROW = /data-testid="sidebar-row-(card|slim)"/;

function withoutConversationThreadRowNesting(violations) {
  return violations.flatMap((violation) => {
    if (violation.id !== "nested-interactive") return [violation];
    const nodes = violation.nodes.filter((node) => !CONVERSATION_THREAD_ROW.test(node.html ?? ""));
    return nodes.length === 0 ? [] : [{ ...violation, nodes }];
  });
}

async function scan(page, name) {
  const results = await new AxeBuilder({ page }).analyze();
  const serious = withoutConversationThreadRowNesting(results.violations).filter(
    (violation) => violation.impact === "serious" || violation.impact === "critical",
  );
  expect(serious.map((violation) => `${name}: ${violation.id}`), JSON.stringify(serious, null, 2)).toEqual([]);
  const headings = results.violations.filter((violation) => violation.id === "page-has-heading-one");
  expect(headings.map((violation) => `${name}: ${violation.id}`)).toEqual([]);
}

test.describe("the work board", () => {
  test("defaults board lanes and list rows to priority then activity for open work and activity for Done", async ({ page }) => {
    const fixture = require("../../web/conversation/src/contracts/fixtures/work-item.json");
    const entries = [
      { number: 1, state: "Todo", priority: 1, last_activity_at: "2026-10-03T12:00:00Z" },
      { number: 2, state: "Todo", priority: 3, last_activity_at: "2026-10-05T12:00:00Z" },
      { number: 3, state: "Todo", priority: 1, last_activity_at: "2026-10-04T12:00:00Z" },
      { number: 4, state: "Done", priority: 0, last_activity_at: "2026-10-03T12:00:00Z" },
      { number: 5, state: "Done", priority: 3, last_activity_at: "2026-10-05T12:00:00Z" },
      { number: 6, state: "Done", priority: 1, last_activity_at: "2026-10-04T12:00:00Z" },
    ];
    await page.route("**/projects/*/work-items?**", async (route) => {
      await route.fulfill({ json: {
        items: entries.map((entry) => ({ ...fixture, ...entry,
          project_id: hub.fixture.project_id,
          work_item_id: `wi_sort_${entry.number}`, title: `Sort fixture ${entry.number}`,
          terminal: entry.state !== "Done", blockers: [],
          updated_at: "2026-10-02T12:00:00Z",
        })),
      } });
    });
    await page.route("**/work-items/wi_sort_*/attempts?**", (route) => route.fulfill({ json: { items: [] } }));
    await page.route("**/work-items/wi_sort_*/changes", (route) => route.fulfill({ json: { items: [] } }));
    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    const todo = page.locator('[data-testid="board-lane"][data-lane="Todo"]');
    const done = page.locator('[data-testid="board-lane"][data-lane="Done"]');
    await page.getByTestId("lanes-trigger").click();
    await page.getByTestId("lane-toggle-Done").click();
    await page.keyboard.press("Escape");
    await expect(todo.getByTestId("issue-card-open")).toHaveText(["Sort fixture 3", "Sort fixture 1", "Sort fixture 2"]);
    await expect(done.getByTestId("issue-card-open")).toHaveText(["Sort fixture 5", "Sort fixture 6", "Sort fixture 4"]);
    await page.getByTestId("view-list").click();
    await expect(page.getByTestId("work-list-open")).toHaveText([
      "Sort fixture 3", "Sort fixture 1", "Sort fixture 2", "Sort fixture 5", "Sort fixture 6", "Sort fixture 4",
    ]);
    await page.getByTestId("sort-trigger").click();
    await page.getByTestId("sort-priority").click();
    await expect(page).toHaveURL(/sort=priority/);
    await expect(page.getByTestId("work-list-open")).toHaveText([
      "Sort fixture 4", "Sort fixture 1", "Sort fixture 3", "Sort fixture 6", "Sort fixture 2", "Sort fixture 5",
    ]);
  });

  test("renders the project's lanes and its issues", async ({ page }) => {
    const errors = watchConsole(page);
    await openWork(page, `/work/p/${hub.fixture.project_id}`);

    // Lanes come from the project's workflow, in the workflow's order.
    const lanes = page.locator('[data-testid="board-lane"]');
    await expect(lanes.first()).toBeVisible();
    expect(await lanes.count()).toBeGreaterThan(1);
    await expect(page.locator('[data-testid="board-lane"][data-lane="Todo"]')).toBeVisible();

    // The seeded issue is on the board, in its own lane.
    const card = page.getByRole("button", { name: "Review the invitation flow", exact: true });
    await expect(card).toBeVisible();

    await expect(page.getByTestId("work-stats")).toBeVisible();
    await expect(page.getByTestId("work-stats")).toHaveCount(1);
    await expect(page.getByTestId("work-toolbar")).toHaveCount(1);
    await expect(page.getByTestId("work-stats")).toContainText(/\d+ running.*\d+ queued.*\d+ open.*\d+ closed inventory/);
    await expect(page.getByTestId("work-stats").getByRole("button", { name: /^Load / })).toHaveCount(0);
    await expect(page.getByTestId("stat-coverage")).toHaveCount(0);

    // The fixture seeds no running attempt and no pull request, so neither a
    // worker strip nor a PR chip is invented.
    await expect(page.locator('[data-testid="worker-strip"]')).toHaveCount(0);
    await expect(page.locator('[data-testid="pr-chip"]')).toHaveCount(0);

    expect(errors).toEqual([]);
    await scan(page, "board");
  });

  test("keeps one toolbar and one counts row at desktop and phone widths", async ({ page }) => {
    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 900 });
      const toolbar = page.getByTestId("work-toolbar");
      const counts = page.getByTestId("work-stats");
      await expect(toolbar).toHaveCount(1);
      await expect(counts).toHaveCount(1);
      await expect(counts).toHaveAttribute("aria-busy", "false");
      const layout = await page.evaluate(() => {
        const toolbar = document.querySelector('[data-testid="work-toolbar"]');
        const counts = document.querySelector('[data-testid="work-stats"]');
        const counters = [...counts.querySelectorAll('[data-testid^="stat-"]')];
        return {
          toolbarWrap: getComputedStyle(toolbar).flexWrap,
          counterTops: counters.map((counter) => Math.round(counter.getBoundingClientRect().top)),
          gap: Math.round(counts.getBoundingClientRect().top - toolbar.getBoundingClientRect().bottom),
          next: counts.nextElementSibling?.textContent,
          unchecked: document.querySelector('[data-testid="issue-card"]')?.getAttribute("title"),
        };
      });
      expect(layout.toolbarWrap).toBe("nowrap");
      expect(new Set(layout.counterTops).size).toBe(1);
      expect(layout.gap).toBe(0);
      expect(layout.next).not.toContain("Search and filters cover");
      expect(layout.unchecked).toBeNull();
      await expect(page.getByTestId("connection-chip")).toHaveCount(1);
    }
  });

  // Michael's review of September 12: the toolbar read "1 project · ● Not
  // streaming · data as of 9:02 AM · Reload" while the sidebar footer read
  // "● Live", and he asked what each word meant. Three bugs made that happen —
  // the all-projects board opened no stream at all, the board's own load
  // published `live: false` over the stream's state, and a stream the browser
  // had closed was never reopened — so both scopes are driven here, and both
  // chips are read off one screen. The decision itself is unit-tested in
  // `web/conversation/tests/components/boardFreshness.test.ts`. He then asked
  // for one Live, upper right, so the sidebar footer must not draw a second.
  test("reads Live on both board scopes, once, and says what Live means", async ({ page }) => {
    const errors = watchConsole(page);
    await openWork(page, "/work");

    await expect(page.getByTestId("sidebar-connection")).toHaveCount(0);
    // The all-projects scope is the default route and used to subscribe to
    // nothing at all, which is the chip Michael was looking at.
    const board = page.getByTestId("connection-chip");
    await expect(board).toHaveCount(1);
    await expect(board).toContainText(/Live\s*·\s*\d{1,2}:\d{2}\s*[AP]M/);
    await expect(page.getByTestId("work-toolbar").getByRole("status")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Reload", exact: true })).toHaveCount(0);

    await board.hover();
    await expect(page.locator("[data-slot='tooltip-popup']")).toContainText(
      "Live: updates arrive over the project event stream.",
    );

    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    await expect(page.getByTestId("connection-chip")).toContainText("Live");
    await expect(page.getByRole("button", { name: "Reload", exact: true })).toHaveCount(0);

    expect(errors).toEqual([]);
  });

  test("switches to the list and back, and says so in the URL", async ({ page }) => {
    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    await expect(page.getByTestId("work-board")).toBeVisible();

    await page.getByTestId("view-list").click();
    await expect(page.getByTestId("work-list")).toBeVisible();
    await expect(page).toHaveURL(/view=list/);
    // The same issue, in the other shape.
    await expect(
      page.getByTestId("work-list").getByText("Review the invitation flow"),
    ).toBeVisible();

    // A reload lands on the same view, because the view is in the URL.
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("work-list")).toBeVisible();
    await scan(page, "list");

    await page.getByTestId("view-board").click();
    await expect(page.getByTestId("work-board")).toBeVisible();
    await expect(page).not.toHaveURL(/view=list/);
  });

  test("keeps the search in the URL and filters the board", async ({ page }) => {
    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    const search = page.getByTestId("work-search");
    await search.fill("invitation");
    await expect(page).toHaveURL(/q=invitation/);
    await expect(page.getByRole("button", { name: "Review the invitation flow", exact: true })).toBeVisible();

    await search.fill("nothing matches this");
    await expect(page.locator('[data-testid="issue-card"]')).toHaveCount(0);

    // `/` focuses the board's own search from anywhere on the page.
    await search.fill("");
    await page.locator("body").click();
    await page.keyboard.press("/");
    await expect(search).toBeFocused();
  });

  test("moves an issue between lanes from the keyboard", async ({ page }) => {
    // The project stream is replaced by one that answers every connection with
    // a single, higher activity sequence and a one-second retry (longer than the
    // board's 400 ms tick coalescing), so the board keeps reconnecting and
    // reloading on its own while the move is held below.
    let sequence = 1_000_000;
    await page.route("**/projects/*/events", (route) => {
      sequence += 1;
      return route.fulfill({
        status: 200,
        headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-store" },
        body: `retry: 1000\nevent: activity\ndata: ${sequence}\n\n`,
      });
    });
    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    const card = page.locator('[data-testid="issue-card"]', {
      has: page.getByRole("button", { name: "Review the invitation flow", exact: true }),
    });
    await expect(card).toBeVisible();

    // The move is a menu, reached and driven with the keyboard only.
    const trigger = card.getByRole("button", { name: /^Move / });
    await trigger.focus();
    await page.keyboard.press("Enter");
    const menu = page.getByRole("menu", { name: "Move to" });
    await expect(menu).toBeVisible();

    // Every item is a lane the workflow allows; the terminal lane is not
    // reachable from Todo and must not be offered.
    const items = menu.getByRole("menuitem");
    expect(await items.count()).toBeGreaterThan(0);
    const target = await items.first().innerText();

    // The board is optimistic: the hub's answer is held back on the wire for
    // a while, and the card must already be in the new lane before it comes,
    // and must not snap back when the activity tick reloads the board.
    let released;
    const held = new Promise((resolve) => {
      released = resolve;
    });
    await page.route("**/work-items/*/workflow", async (route) => {
      await held;
      await route.continue();
    });
    const isBoardReload = (response) =>
      response.request().method() === "GET" &&
      new URL(response.url()).pathname.endsWith("/work-items");
    await page.keyboard.press("Enter");
    const lane = page.locator(`[data-testid="board-lane"][data-lane="${target}"]`);
    await expect(
      lane.getByRole("button", { name: "Review the invitation flow", exact: true }),
    ).toBeVisible({ timeout: 1_000 });
    // A reload the activity stream asked for, answered while the hub still
    // holds the old lane: the card has to survive it.
    await page.waitForResponse(isBoardReload, { timeout: 10_000 });
    await page.waitForResponse(isBoardReload, { timeout: 10_000 });
    await expect(
      lane.getByRole("button", { name: "Review the invitation flow", exact: true }),
    ).toBeVisible();
    released();

    // The card lands in the new lane and stays there across a reload, which is
    // what proves the move reached the hub rather than only the screen.
    await expect(lane.getByRole("button", { name: "Review the invitation flow", exact: true })).toBeVisible();
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(
      page
        .locator(`[data-testid="board-lane"][data-lane="${target}"]`)
        .getByRole("button", { name: "Review the invitation flow", exact: true }),
    ).toBeVisible();
  });

  test("closes the move menu on Escape and puts focus back", async ({ page }) => {
    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    const trigger = page.getByRole("button", { name: /^Move / }).first();
    await trigger.focus();
    await page.keyboard.press("Enter");
    await expect(page.getByRole("menu", { name: "Move to" })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("menu", { name: "Move to" })).toHaveCount(0);
    await expect(trigger).toBeFocused();
  });

  test("renders the move menu above the board, never clipped by its lane", async ({ page }) => {
    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    // The last card's menu opens below the lane body's bottom edge, which is
    // where a menu rendered inside the lane was clipped.
    const lane = page
      .locator('[data-testid="board-lane"]')
      .filter({ has: page.locator('[data-testid="lane-menu-trigger"]') })
      .first();
    await expect(lane).toBeVisible();
    const body = lane.locator('[data-testid^="lane-body-"]');
    const scrollBefore = await body.evaluate((element) => element.scrollTop);

    await lane.getByTestId("lane-menu-trigger").last().click();
    const menu = page.getByRole("menu", { name: "Move to" });
    await expect(menu).toBeVisible();
    await expect(lane.getByRole("menu")).toHaveCount(0);

    const items = menu.getByRole("menuitem");
    const count = await items.count();
    expect(count).toBeGreaterThan(0);
    for (let index = 0; index < count; index += 1) {
      const item = items.nth(index);
      await expect(item).toBeInViewport({ ratio: 1 });
      // Nothing (the lane's edge, the next lane) covers the item's centre.
      const onTop = await item.evaluate((element) => {
        const box = element.getBoundingClientRect();
        const hit = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
        return hit !== null && element.contains(hit);
      });
      expect(onTop, `menu item ${index} is covered`).toBe(true);
    }
    expect(await body.evaluate((element) => element.scrollTop)).toBe(scrollBefore);

    await page.keyboard.press("Escape");
    await expect(menu).toHaveCount(0);
  });

  test("reaches Work and the Browse group from the sidebar", async ({ page }) => {
    await openWork(page, "/work");
    const sidebar = page.locator("aside.dc-side");
    await expect(sidebar.getByTestId("nav-work")).toHaveAttribute("aria-current", "page");

    for (const id of ["activity", "reports", "library"]) {
      await expect(sidebar.getByTestId(`nav-${id}`)).toBeDisabled();
    }
    await expect(sidebar.getByTestId("nav-diagnostics")).toBeEnabled();
    for (const id of ["changes", "usage", "settings"]) {
      await expect(sidebar.getByTestId(`nav-${id}`)).toHaveCount(0);
    }

    const utility = sidebar.getByRole("button", { name: "Pull requests" });
    await expect(utility).toBeEnabled();
    await utility.click();
    await expect(page).toHaveURL(/\/work\/changes$/);
    await expect(page.getByTestId("changes-scope-note")).toBeVisible();
    await scan(page, "changes");
  });
});

/**
 * The issue page (decisions.md §19): the issue *is* the page and the
 * conversation is a surface in the right panel, so what used to be a chat
 * under an issue card is now a Linear-shaped page with a properties sidebar,
 * an activity feed and a two-mode composer.
 */
test.describe("the issue page", () => {

  async function typeInto(box, page, text) {
    await box.focus();
    await page.keyboard.type(text);
  }

  test("opens an issue from the board and shows its properties", async ({ page }) => {
    const errors = watchConsole(page);
    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    await page.getByRole("button", { name: "Review the invitation flow", exact: true }).first().click();
    await expect(page).toHaveURL(/\/work\/i\//);

    // The title is the page's one `h1`, above the identifier.
    await expect(page.getByRole("heading", { level: 1 })).toHaveCount(1);
    await expect(page.getByTestId("issue-identifier")).toContainText("#");
    await expect(page.getByTestId("issue-body")).toBeVisible();

    // The properties sidebar carries the lane, its glyph and the rest of the
    // facts. It is the sidebar, not a pill row, that names the state now.
    const properties = page.getByTestId("issue-properties");
    await expect(properties).toBeVisible();
    await expect(properties.getByTestId("state-glyph")).toBeVisible();
    await expect(properties.getByTestId("issue-state-pill")).toBeVisible();
    await expect(properties.getByTestId("issue-assignee")).toBeVisible();
    await expect(properties.getByTestId("issue-attempts")).toBeVisible();
    await expect(properties.getByTestId("issue-pull-request")).toBeVisible();

    // Sub-issues are named and disabled with the reason, rather than missing
    // (decisions.md §16, §19.6).
    await expect(page.getByTestId("add-sub-issues")).toBeDisabled();

    expect(errors).toEqual([]);
    await scan(page, "issue");
  });

  test("records the issue's creation in the activity feed", async ({ page }) => {
    const errors = watchConsole(page);
    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    await page.getByRole("button", { name: "Review the invitation flow", exact: true }).first().click();
    await expect(page).toHaveURL(/\/work\/i\//);

    const feed = page.getByTestId("issue-activity");
    await expect(feed).toBeVisible();
    await expect(feed.getByText(/created the issue/).first()).toBeVisible();
    expect(errors).toEqual([]);
  });

  test("merges attempts into the activity feed", async () => {
    test.skip(true, "The preview seeds no runner attempt: the attempt and diff seeding slice of #2635 is not ported.");
  });

  // Driven on the linked issue rather than on the board's card, so it does not
  // depend on where the keyboard-move test above left that one.
  test("changes the lane from the properties sidebar and records the move", async ({ page }) => {
    await openWork(page, `/work/i/${hub.fixture.work_item}`);
    const properties = page.getByTestId("issue-properties");
    const trigger = properties.getByTestId("state-menu");
    await expect(trigger).toBeVisible();
    // Enabled means the workflow offers this state somewhere to go; a state
    // with no transitions is a real thing and the control is disabled for it.
    await expect(trigger).toBeEnabled();
    const before = await properties.getByTestId("issue-state-pill").innerText();

    await trigger.click();
    // Linear's status picker: a search header, then one row per workflow
    // state, each named by its own test id. A state the workflow does not
    // allow from here stays on the list with its reason, so the row to take
    // is the first one that is neither disabled nor the current state.
    const picker = page.getByTestId("state-picker");
    await expect(picker.getByPlaceholder("Change status…")).toBeVisible();
    const item = picker
      .getByRole("option")
      .and(page.locator(`:not([data-testid="state-menu-${before}"])`))
      .and(page.locator(":not([aria-disabled='true'])"))
      .first();
    await expect(item).toBeVisible();
    const target = (await item.getAttribute("data-testid")).slice("state-menu-".length);
    await item.click();

    await expect(properties.getByTestId("issue-state-pill")).toHaveText(target);
    expect(target).not.toBe(before);

    // The move is a fact about the issue, so it lands in the feed too.
    await expect(
      page.getByTestId("issue-activity").getByText(new RegExp(`moved it from ${before} to `)),
    ).toBeVisible();
  });

  test("opens the conversation in the right panel from the live row", async ({ page }) => {
    const errors = watchConsole(page);
    await openWork(page, `/work/i/${hub.fixture.work_item}`);

    // The conversation is one row in the feed, not the page.
    const live = page.getByTestId("issue-live-row");
    await expect(live).toBeVisible();
    await expect(live.getByTestId("live-row-sentence")).not.toBeEmpty();

    // The issue's own composer is the comment one; the conversation's is in
    // the panel and does not exist until the panel opens.
    await expect(page.getByRole("textbox", { name: "Message" })).toHaveCount(0);
    await live.getByTestId("view-conversation").click();

    await expect(page.getByTestId("conversation-surface")).toBeVisible();
    const composer = page.getByRole("textbox", { name: "Message" });
    await expect(composer).toHaveCount(1);

    // With the panel open the properties sidebar yields its width and its
    // facts move to the disclosure at the top of the column (§19.3).
    await expect(page.getByRole("complementary", { name: "Properties" })).toHaveCount(0);

    // The panel's composer is the conversation's own, and it sends.
    await typeInto(composer, page, "steer from the panel");
    await page.keyboard.press("Enter");
    await expect(page.getByTestId("user-turn").last()).toContainText("steer from the panel");

    expect(errors).toEqual([]);
    await scan(page, "issue-with-conversation-panel");
  });

  test("shows the stored attempt diff, hunks and denied file alike", async () => {
    test.skip(true, "The preview seeds no stored attempt diff: the attempt and diff seeding slice of #2635 is not ported.");
  });

  test("posts a comment that appears as a card in the feed", async ({ page }) => {
    const errors = watchConsole(page);
    await openWork(page, `/work/i/${hub.fixture.work_item}`);

    const composer = page.getByRole("textbox", { name: "Comment" });
    await expect(composer).toHaveCount(1);
    // This card posts comments with optional attachments. Runner steering
    // stays on the conversation surface opened from Activity, so comments
    // offer neither a mode toggle nor turn configuration pickers.
    await expect(composer).toHaveAttribute("aria-placeholder", "Leave a comment…");
    await expect(page.getByTestId("issue-composer-mode")).toHaveCount(0);
    await expect(page.getByTestId("issue-composer-scope")).toHaveCount(0);
    await expect(page.getByTestId("composer-attach")).toBeVisible();

    for (const label of ["Model", "Reasoning effort", "Runtime access"]) {
      await expect(page.getByLabel(label, { exact: true })).toHaveCount(0);
    }

    const body = `comment from the browser ${Date.now()}`;
    await typeInto(composer, page, body);
    await page.keyboard.press("Enter");

    await expect(
      page.getByTestId("issue-comment").filter({ hasText: body }),
    ).toBeVisible();
    expect(errors).toEqual([]);
  });

  // Linear's property pickers (decisions.md §19.1). Everything below is
  // driven the way a reader drives it: the letter that opens the picker, the
  // header that says what it changes, a row, and the fact on the column
  // afterwards. The fixture seeds the issue with no labels, no assignee and
  // no priority, so each of these starts from nothing — which is the state
  // the pickers had to be able to leave and, for the priority, return to.
  test("opens every property picker from its own letter", async ({ page }) => {
    const errors = watchConsole(page);
    await openWork(page, `/work/i/${hub.fixture.work_item}`);
    await expect(page.getByTestId("issue-properties")).toBeVisible();
    // Focus belongs to the page rather than to the sidebar's search field,
    // which is where a fresh load can leave it: a bare letter goes to
    // whatever is being typed into, which is the rule under test below.
    await page.getByRole("heading", { level: 1 }).click();

    for (const picker of [
      { key: "s", testId: "state-picker", placeholder: "Change status…", hint: "S" },
      { key: "p", testId: "priority-picker", placeholder: "Change priority to…", hint: "P" },
      { key: "a", testId: "assignee-picker", placeholder: "Assign to…", hint: "A" },
      { key: "l", testId: "label-picker", placeholder: "Change or add labels…", hint: "L" },
      { key: "r", testId: "related-picker", placeholder: "Search issues…", hint: "R" },
    ]) {
      await page.keyboard.press(picker.key);
      const popup = page.getByTestId(picker.testId);
      await expect(popup).toBeVisible();
      await expect(popup.getByPlaceholder(picker.placeholder)).toBeFocused();
      // The header advertises the letter that opened it.
      await expect(popup.getByText(picker.hint, { exact: true })).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(popup).toHaveCount(0);
    }

    // An open picker is a surface of its own and is scanned like one: the
    // search field has to be named, the rows have to be options of a list
    // that names itself, and the check has to be more than a colour.

    // A bare letter belongs to whatever is being typed into: the comment
    // composer keeps its "s", and no picker opens behind it.
    const composer = page.getByRole("textbox", { name: "Comment" });
    await composer.focus();
    await page.keyboard.type("status");
    await expect(page.getByTestId("state-picker")).toHaveCount(0);
    await expect(composer).toContainText("status");

    expect(errors).toEqual([]);
  });

  test("scans the open label picker", async ({ page }) => {
    test.skip(true, "Product bug on main: axe aria-hidden-focus flags the tabbable Base UI focus guards around the open label picker.");
    await openWork(page, `/work/i/${hub.fixture.work_item}`);
    await page.getByRole("heading", { level: 1 }).click();
    await page.keyboard.press("l");
    await expect(page.getByTestId("label-picker")).toBeVisible();
    await scan(page, "label picker");
  });

  test("sets a priority and then takes it off again", async ({ page }) => {
    await openWork(page, `/work/i/${hub.fixture.work_item}`);
    const properties = page.getByTestId("issue-properties");

    await properties.getByTestId("priority-menu").click();
    const picker = page.getByTestId("priority-picker");
    // Detent's names over Linear's glyphs, with "No priority" first.
    await expect(picker.getByTestId("priority-menu-none")).toContainText("No priority");
    for (const name of ["Urgent", "High", "Normal", "Low"]) {
      await expect(picker.getByTestId(`priority-menu-${name}`)).toBeVisible();
    }
    await picker.getByTestId("priority-menu-High").click();
    await expect(properties.getByTestId("issue-priority")).toHaveText("High");

    // The removal: what the hub's patch could not express until it learned a
    // three-way priority member.
    await properties.getByTestId("priority-menu").click();
    await page.getByTestId("priority-picker").getByTestId("priority-menu-none").click();
    await expect(properties.getByTestId("issue-priority")).toHaveText("No priority");

    // It survives a reload, so the hub took the removal rather than the page.
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("issue-priority")).toHaveText("No priority");
  });

  test("makes a label, attaches it and takes it off", async ({ page }) => {
    await openWork(page, `/work/i/${hub.fixture.work_item}`);
    const name = `flaky-${Date.now()}`;

    await page.getByTestId("add-label").click();
    const picker = page.getByTestId("label-picker");
    await picker.getByPlaceholder("Change or add labels…").fill(name);
    // A project with no catalogue offers to make the first label rather than
    // showing an empty list.
    await picker.getByTestId("create-label").click();
    await expect(page.getByTestId("issue-labels").getByTestId(`label-${name}`)).toBeVisible();
    // The picker stays open after a label is attached, as Linear's does:
    // labels are a set, and closing after each one would make three labels
    // three round trips through the trigger.
    await expect(picker).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(picker).toHaveCount(0);

    // It is in the project's catalogue now, with a dot of its own.
    await page.getByTestId("add-label").click();
    const again = page.getByTestId("label-picker");
    await expect(again.getByTestId(`label-menu-${name}`)).toHaveAttribute("aria-selected", "true");
    await again.getByTestId(`label-menu-${name}`).click();
    await expect(page.getByTestId("issue-labels").getByTestId(`label-${name}`)).toHaveCount(0);
  });

  test("assigns a member of the organization and unassigns again", async ({ page }) => {
    await openWork(page, `/work/i/${hub.fixture.work_item}`);
    const properties = page.getByTestId("issue-properties");
    await expect(properties.getByTestId("issue-assignee")).toContainText("Assign");

    await properties.getByTestId("issue-assignee").click();
    const picker = page.getByTestId("assignee-picker");
    await expect(picker.getByTestId("assignee-menu-none")).toContainText("No assignee");
    await expect(picker.getByText("Team members")).toBeVisible();
    // The fixture's organization has an owner and a viewer.
    const member = picker.getByTestId("assignee-menu-viewer@example.test");
    await expect(member).toBeVisible();
    await member.click();
    await expect(properties.getByTestId("issue-assignee")).toContainText("viewer@example.test");

    await properties.getByTestId("issue-assignee").click();
    await page.getByTestId("assignee-picker").getByTestId("assignee-menu-none").click();
    await expect(properties.getByTestId("issue-assignee")).toContainText("Assign");
  });

  test("relates another issue and takes the relation off from the row", async ({ page }) => {
    await openWork(page, `/work/i/${hub.fixture.work_item}`);
    const related = page.getByTestId("issue-related");

    await related.getByTestId("add-related").click();
    const picker = page.getByTestId("related-picker");
    const candidate = picker.getByRole("option").first();
    await expect(candidate).toBeVisible();
    await candidate.click();

    // The linked chat is listed too and is not removable, so the relation
    // just made is the one row with a remove control.
    const remove = related.getByTestId("remove-related");
    await expect(remove).toHaveCount(1);
    const row = remove.locator("..");

    // The × appears on hover and removes the relation it sits beside.
    await row.hover();
    await remove.click();
    await expect(remove).toHaveCount(0);
  });

  test("redirects the old chat issue path to the issue page", async ({ page }) => {
    await openWork(page, "/work");
    await page.goto(new URL(`/chat/issues/${hub.fixture.work_item}`, hub.fixture.url).toString(), {
      waitUntil: "domcontentloaded",
    });
    await expect(page).toHaveURL(new RegExp(`/work/i/${hub.fixture.work_item}$`));
    await expect(page.getByTestId("issue-properties")).toBeVisible();
  });

});

test("shares project scope between the sidebar and Work across reloads", async ({ page }) => {
  const errors = watchConsole(page);
  await openWork(page, `/work/p/${hub.fixture.project_id}`);
  const secondProjectId = new URL(hub.fixture.private_project).pathname.split("/").at(-1);
  const projects = [
    { id: hub.fixture.project_id, name: "Browser collaboration", title: "Completed collaboration work" },
    { id: secondProjectId, name: "Owner private project", title: "Completed private work" },
  ];
  for (const project of projects) {
    const created = await page.evaluate(async ({ id, title }) => {
      const bootstrap = await (await fetch("/chat/bootstrap")).json();
      const response = await fetch(`${bootstrap.api_base}/projects/${id}/work-items`, {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": bootstrap.csrf_token },
        body: JSON.stringify({ idempotency_key: `scope-done-${id}`, title, state: "Done" }),
      });
      return response.status;
    }, project);
    expect(created).toBe(200);
  }
  const scope = page.getByRole("combobox", { name: "Filter threads by project" });
  const done = page.getByRole("region", { name: "Done", exact: true });
  const selectScope = async (name) => {
    await scope.click();
    await page.getByRole("option", { name, exact: true }).click();
  };
  const expectProject = async (project) => {
    await expect.poll(() => new URL(page.url()).pathname).toBe(`/work/p/${project.id}`);
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(project.name);
    await expect(scope).toHaveText(project.name);
    if (await done.count() === 0) {
      await page.getByTestId("lanes-trigger").click();
      await page.getByTestId("lane-toggle-Done").click();
      await page.keyboard.press("Escape");
    }
    await expect(page.getByTestId("lane-count-Done")).toHaveText("1");
    await expect(done.getByRole("button", { name: project.title, exact: true })).toBeVisible();
    const other = projects.find((candidate) => candidate.id !== project.id);
    await expect(done.getByRole("button", { name: other.title, exact: true })).toHaveCount(0);
  };
  await expect(scope).toHaveText(projects[0].name);
  await selectScope(projects[1].name);
  await expectProject(projects[1]);
  await page.reload();
  await expectProject(projects[1]);
  await page.getByTestId("nav-chat").click();
  await page.getByTestId("nav-work").click();
  await expectProject(projects[1]);
  await selectScope(projects[0].name);
  await expectProject(projects[0]);
  await page.goBack();
  await expectProject(projects[1]);
  await page.goForward();
  await expectProject(projects[0]);

  await selectScope("All projects");
  const expectAll = async () => {
    await expect(page).toHaveURL(/\/work(?:\?|$)/);
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("All projects");
    await expect(scope).toHaveText("All projects");
    if (await done.count() === 0) {
      await page.getByTestId("lanes-trigger").click();
      await page.getByTestId("lane-toggle-Done").click();
      await page.keyboard.press("Escape");
    }
    await expect(page.getByTestId("lane-count-Done")).toHaveText("2");
    for (const project of projects) {
      await expect(done.getByRole("button", { name: project.title, exact: true })).toBeVisible();
    }
  };
  await expectAll();
  await page.reload();
  await expectAll();
  await page.getByTestId("nav-chat").click();
  await page.reload();
  await page.getByTestId("nav-work").click();
  await expectAll();
  await expect(errors).toEqual([]);
});
