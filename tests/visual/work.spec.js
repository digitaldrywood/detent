// End-to-end cover for the Work board, list and issue page.
//
// Like `conversation.spec.js`, this drives the real thing: `hosted-hub.js`
// starts the Go preview test, which serves a hosted hub with real sessions,
// real authorization and real durable state. Everything below happens through
// the browser, from the keyboard, located by accessible name — so a control a
// screen reader or a keyboard cannot reach fails the test rather than passing
// on a class name.
//
// **What the fixture can and cannot cover.** The Go preview seeds one project
// with two issues: `Review the invitation flow` in `Todo`, and the issue the
// seeded conversation was linked to. It seeds no attempt, no change request
// and no second project. So this spec asserts what exists — lanes, cards, the
// keyboard move, the view switch, the URL state, the issue page — and the
// treatments that need an attempt or a change (the worker strip, the PR chip,
// the review dock's four tabs) are covered against fixtures in
// `web/conversation/tests/components/work.test.tsx` and against the mock hub
// in `web/conversation/tests/work.test.ts`. Each is named at the assertion
// that would otherwise be silently absent.
const { test, expect } = require("@playwright/test");
const AxeBuilder = require("@axe-core/playwright").default;
const { startHostedHub } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

let hub;

test.beforeAll(async () => {
  hub = await startHostedHub("work");
});

test.afterAll(async () => {
  await hub?.stop();
  hub = undefined;
});

function watchConsole(page) {
  const errors = [];
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
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

    // The stats strip is derived from the cards, so it cannot disagree.
    await expect(page.getByTestId("work-stats")).toBeVisible();
    await expect(page.getByTestId("stat-coverage")).toContainText("issues");

    // The fixture seeds no running attempt, so no worker strip is invented;
    // it does seed one pull request on the linked issue (section 18.6), so
    // exactly one card carries the PR chip.
    await expect(page.locator('[data-testid="worker-strip"]')).toHaveCount(0);
    await expect(page.locator('[data-testid="pr-chip"]')).toHaveCount(1);

    expect(errors).toEqual([]);
    await scan(page, "board");
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
    const board = page.getByTestId("board-freshness");
    await expect(board).toContainText("Live");
    await expect(page.getByRole("button", { name: "Reload", exact: true })).toHaveCount(0);

    await board.hover();
    await expect(page.locator("[data-slot='tooltip-popup']")).toContainText(
      "Live: updates arrive over the project event stream.",
    );

    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    await expect(page.getByTestId("board-freshness")).toContainText("Live");
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

    await page.getByTestId("view-board").click();
    await expect(page.getByTestId("work-board")).toBeVisible();
    await expect(page).not.toHaveURL(/view=list/);
    await scan(page, "list");
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
    await page.keyboard.press("Enter");
    const lane = page.locator(`[data-testid="board-lane"][data-lane="${target}"]`);
    await expect(
      lane.getByRole("button", { name: "Review the invitation flow", exact: true }),
    ).toBeVisible({ timeout: 1_000 });
    await page.waitForTimeout(600);
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

  test("reaches Work and the Browse group from the sidebar", async ({ page }) => {
    await openWork(page, "/work");
    const sidebar = page.locator("aside.dc-side");
    await expect(sidebar.getByTestId("nav-work")).toHaveAttribute("aria-current", "page");

    // Browse keeps only what the footer icon row does not already serve
    // (decisions.md §17.4): Pull requests, Usage and Settings left the group
    // when the footer took them, so none of the four rows left is a duplicate
    // and none of them is served yet.
    for (const id of ["activity", "diagnostics", "reports", "library"]) {
      await expect(sidebar.getByTestId(`nav-${id}`)).toBeDisabled();
    }
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

  // The feed is merged client-side from four sources. This issue is the one the
  // Go fixture seeds attempts against, so it can prove the attempt rows as well
  // as the history rows; the linked issue below proves the `moved` row.
  test("merges history and attempts into one activity feed", async ({ page }) => {
    const errors = watchConsole(page);
    await openWork(page, `/work/p/${hub.fixture.project_id}`);
    await page.getByRole("button", { name: "Review the invitation flow", exact: true }).first().click();
    await expect(page).toHaveURL(/\/work\/i\//);

    const feed = page.getByTestId("issue-activity");
    await expect(feed).toBeVisible();
    // Created: the work item's own `issue.created` history row.
    await expect(feed.getByText(/created the issue/).first()).toBeVisible();
    // Claimed: one row per attempt, from the attempts endpoint rather than
    // from the log, because that is where the status and the runner live.
    await expect(
      feed.getByText(/claimed the issue and started attempt/).first(),
    ).toBeVisible();
    expect(errors).toEqual([]);
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

  test("shows the stored attempt diff, hunks and denied file alike", async ({ page }) => {
    const errors = watchConsole(page);
    await openWork(page, `/work/i/${hub.fixture.work_item}`);

    // The panel opens on the conversation for a linked issue (§19.3), so Diff
    // is reached through the surface menu.
    await page.getByRole("button", { name: /^Toggle right panel/ }).click();
    await expect(page.getByTestId("conversation-surface")).toBeVisible();
    await page.getByRole("button", { name: "Add panel surface" }).click();
    await page.getByRole("menuitem", { name: "Diff" }).click();

    // The real diff, not the fallback card and not the old apology.
    const files = page.getByTestId("diff-files");
    await expect(files).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId("diff-empty")).toHaveCount(0);
    await expect(page.getByTestId("diff-round-card")).toHaveCount(0);

    await expect(files).toContainText("3 changed files");
    await expect(files.getByText("main.go", { exact: true })).toBeVisible();
    await expect(files.getByText(".env.local", { exact: true })).toBeVisible();
    await expect(files.getByText("renewal.go", { exact: true })).toBeVisible();

    // Clicking a row scrolls to that file's section, and the section carries
    // the hunk the runner posted — the `+` line included.
    await files.getByText("main.go", { exact: true }).click();
    const mainSection = page.locator('[data-diff-path="main.go"]');
    await expect(mainSection).toBeVisible();
    await expect(mainSection).toContainText("renewLease()", { timeout: 30_000 });

    // §18.5's denylist: the file is listed with its counts and without its
    // contents, so a reader learns that .env.local changed and not what it
    // now says.
    const denied = page.locator('[data-diff-path=".env.local"]');
    await expect(denied).toBeVisible();
    await expect(denied.getByTestId("diff-file-denied")).toContainText("denylist");
    await expect(denied).not.toContainText("TOKEN=");

    expect(errors).toEqual([]);
    await scan(page, "issue-diff-surface");
  });

  test("posts a comment that appears as a card in the feed", async ({ page }) => {
    const errors = watchConsole(page);
    await openWork(page, `/work/i/${hub.fixture.work_item}`);

    const composer = page.getByRole("textbox", { name: "Comment" });
    await expect(composer).toHaveCount(1);
    // This card posts comments and nothing else (§19.5, Michael's review of
    // September 12): a prompt and a send. No mode toggle, because the runner
    // is steered from the conversation surface the Activity feed opens; no
    // pickers, because a comment configures no turn; no paperclip, because the
    // hub's comment endpoint takes no files; no strip under the card and no
    // shortcut line.
    await expect(composer).toHaveAttribute("aria-placeholder", "Leave a comment…");
    await expect(page.getByTestId("issue-composer-mode")).toHaveCount(0);
    await expect(page.getByTestId("issue-composer-scope")).toHaveCount(0);
    await expect(page.getByTestId("composer-attach")).toHaveCount(0);

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
    await page.keyboard.press("l");
    await expect(page.getByTestId("label-picker")).toBeVisible();
    await scan(page, "label picker");
    await page.keyboard.press("Escape");
    await expect(page.getByTestId("label-picker")).toHaveCount(0);

    // A bare letter belongs to whatever is being typed into: the comment
    // composer keeps its "s", and no picker opens behind it.
    const composer = page.getByRole("textbox", { name: "Comment" });
    await composer.focus();
    await page.keyboard.type("status");
    await expect(page.getByTestId("state-picker")).toHaveCount(0);
    await expect(composer).toContainText("status");

    expect(errors).toEqual([]);
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

    const rows = related.getByTestId("issue-related-row");
    await expect(rows.first()).toBeVisible();
    const before = await rows.count();

    // The × appears on hover and removes the relation it sits beside.
    await rows.first().hover();
    await related.getByTestId("remove-related").first().click();
    await expect(rows).toHaveCount(before - 1);
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
