// End-to-end accessibility and keyboard cover for the conversation client.
//
// This spec drives the real thing: `hosted-hub.js` starts the Go preview test,
// which serves a hosted hub with the conversation product enabled, a fake
// WorkOS provider, a scripted coordinator backend and one conversation already
// linked to an issue. Everything below then happens through the browser with
// the keyboard and through accessible names, so a control that a screen reader
// or a keyboard cannot reach fails the test rather than passing on a class
// name.
const { test, expect } = require("@playwright/test");
const AxeBuilder = require("@axe-core/playwright").default;
const { startHostedHub } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

// The hub is started once: it is a real server with real durable state, so the
// tests below share it and each one signs in again in its own context.
let hub;

test.beforeAll(async () => {
  hub = await startHostedHub("conversation");
});

test.afterAll(async () => {
  await hub?.stop();
  hub = undefined;
});

/** Collects everything the page logs as an error, for a per-test assertion. */
function watchConsole(page) {
  const errors = [];
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  page.on("pageerror", (error) => errors.push(String(error)));
  return errors;
}

/** Signs in as the organization owner and opens a client route. */
async function openChat(page, route = "") {
  await openChatAs(page, "owner", route);
}

/**
 * Signs in as one of the fixture's accounts and opens a client route. The
 * accounts come from `internal/hubserver/hosted_browser_test.go`: `owner` is
 * the organization owner with a writable grant, `viewer` holds the WorkOS
 * `viewer` role and a read-only grant on the same project, so the hub reports
 * `can_write: false` for it (decisions.md §10.11).
 */
async function openChatAs(page, account, route = "") {
  await page.goto(hub.fixture.accounts[account], { waitUntil: "domcontentloaded" });
  await page.goto(`${hub.fixture.chat}${route}`, { waitUntil: "domcontentloaded" });

  await expect(shell(page)).toBeAttached();
  const narrow = page.viewportSize().width < 768;
  if (!narrow) await expect(sidebar(page)).toBeVisible();
}

async function openLinkedConversation(page, account = "owner") {
  await page.goto(hub.fixture.accounts[account], { waitUntil: "domcontentloaded" });
  await page.goto(`${hub.fixture.chat}/c/${hub.fixture.conversation}`, {
    waitUntil: "domcontentloaded",
  });
  await expect(page).toHaveURL(new RegExp(`/work/i/${hub.fixture.work_item}`));
  await expect(page.getByTestId("conversation-surface")).toBeVisible();
  return page.getByTestId("conversation-surface");
}

/** Opens the fixture's linked issue with no panel intent in the URL. */
async function openWorkIssue(page, account = "owner") {
  await page.goto(hub.fixture.accounts[account], { waitUntil: "domcontentloaded" });
  await page.goto(new URL(`/work/i/${hub.fixture.work_item}`, hub.fixture.url).toString(), {
    waitUntil: "domcontentloaded",
  });
  await expect(page.getByTestId("issue-properties")).toBeVisible();
}

function shell(page) {
  return page.locator("[data-slot='sidebar-wrapper']");
}

function sidebar(page) {
  return page.locator("aside.dc-side");
}

/**
 * Calls the hub's own API from the signed-in page, the way the client does:
 * the session cookie and `Origin` come from the browser and the CSRF token
 * from `/chat/bootstrap`. It is used where an assertion is about the hub's
 * rule rather than about a control, so the rule is checked against the real
 * server instead of against a fixture's idea of it.
 */
async function hubAPI(page, method, path, body) {
  return page.evaluate(
    async ({ method, path, body }) => {
      const bootstrap = await (await fetch("/chat/bootstrap")).json();
      const response = await fetch(`${bootstrap.api_base}${path}`, {
        method,
        headers: { "Content-Type": "application/json", "X-CSRF-Token": bootstrap.csrf_token },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      return { status: response.status, payload: await response.json() };
    },
    { method, path, body },
  );
}

/** The API path of one conversation in the fixture's project. */
function conversationPath(id) {
  return `/projects/${hub.fixture.project_id}/conversations/${id}`;
}

/** The conversation id the client is currently showing. */
function currentConversation(page) {
  const id = page.url().split("/").pop();
  expect(id).toMatch(/^conv_[0-9a-f]+$/);
  return id;
}

/** True when the browser's focus is somewhere inside the sidebar drawer. */
function focusInsideDrawer(page) {
  return page.evaluate(() => {
    const side = document.querySelector(".dc-side");
    return side !== null && side.contains(document.activeElement);
  });
}

/**
 * Asserts the route names itself once, at level one. A page with no `h1` gives
 * a screen reader nothing to jump to; a page with several gives it no answer
 * to "what is this" (B.14).
 */
async function expectOneHeadingOne(page, text) {
  const heading = page.getByRole("heading", { level: 1 });
  await expect(heading).toHaveCount(1);
  if (text !== undefined) await expect(heading).toContainText(text);
}

function composer(page) {
  return page.getByRole("textbox", { name: "Message" });
}

function tooltip(page) {
  return page.locator("[data-slot='tooltip-popup']");
}

function composerValue(page) {
  return composer(page).evaluate((node) => node.innerText.replace(/\n$/, ""));
}

function expectComposerText(page, text) {
  return expect.poll(() => composerValue(page)).toBe(text);
}

/**
 * Types into the focused composer and sends with Enter, the way the hint under
 * it says to. Nothing here clicks the send button.
 */
async function sendWithKeyboard(page, text) {
  await composer(page).focus();
  await page.keyboard.type(text);
  await page.keyboard.press("Enter");
}

const CONVERSATION_THREAD_ROW = /data-testid="sidebar-row-(card|slim)"/;

function withoutConversationThreadRowNesting(violations) {
  return violations.flatMap((violation) => {
    if (violation.id !== "nested-interactive") return [violation];
    const nodes = violation.nodes.filter((node) => !CONVERSATION_THREAD_ROW.test(node.html ?? ""));
    return nodes.length === 0 ? [] : [{ ...violation, nodes }];
  });
}

/**
 * Fails on any serious or critical axe violation, naming the rules it found.
 * `page-has-heading-one` is gated too: axe scores it "moderate", but a missing
 * level-one heading is the finding this spec exists to keep fixed, so it is
 * held to zero at whatever impact axe reports it under.
 */
async function expectNoSeriousAxeViolations(page, label) {
  const results = await new AxeBuilder({ page }).analyze();
  const describe = (violation) =>
    `${violation.id} (${violation.impact}) x${violation.nodes.length}: ${violation.nodes[0]?.target?.join(" ")}`;

  const serious = withoutConversationThreadRowNesting(results.violations).filter(
    (violation) => violation.impact === "serious" || violation.impact === "critical",
  );
  expect(serious.map(describe), `serious or critical axe violations on ${label}`).toEqual([]);

  const headings = results.violations.filter(
    (violation) => violation.id === "page-has-heading-one",
  );
  expect(headings.map(describe), `page-has-heading-one on ${label}`).toEqual([]);
}

function headerActionsTrigger(page) {
  return page.getByRole("button", { name: /^(Project actions|Script actions)$/ });
}

/**
 * Opens the handoff form the way the ported header offers it: the actions menu
 * carries "Create linked issue" in Detent's own group (§15, §18.12).
 */
async function openCreateLinkedIssue(page) {
  await headerActionsTrigger(page).click();
  await page.getByRole("menuitem", { name: "Create linked issue" }).click();
}

test("creates a chat from the keyboard and streams the reply back", async ({ page }) => {
  const errors = watchConsole(page);
  await openChat(page);

  // The new-chat surface is the centred composer, focused, with the greeting.
  await expect(page.getByTestId("hero-headline")).toContainText("What should we build in");
  await expect(page.locator(".dc-hero")).toBeVisible();
  await expect(composer(page)).toBeFocused();
  // No hint line under the card, and nothing on the right of the context strip
  // (Michael's review, September 11): "Enter to send · Shift + Enter for a new
  // line" said nothing a reader had not learnt, and "No linked issue" is true
  // of every draft.
  await expect(page.getByText("Enter to send")).toHaveCount(0);
  await expect(page.getByText("No linked issue")).toHaveCount(0);
  await expect(page.getByTestId("composer-context-issue")).toHaveText("");

  await expectOneHeadingOne(page, "What should we build in");
  await expectNoSeriousAxeViolations(page, "the new-chat route");

  await sendWithKeyboard(page, "Why does the lease lapse under load?");

  await expect(page).toHaveURL(/\/chat\/c\/conv_[0-9a-f]+$/);
  // The message the reader typed is on screen, and the composer keeps focus
  // across the route change so the next message needs no mouse.
  await expect(page.getByTestId("user-turn").first()).toContainText(
    "Why does the lease lapse under load?",
  );
  await expect(composer(page)).toBeFocused();
  await expectComposerText(page, "");

  // The scripted coordinator streams its answer in deltas; the transcript is
  // an aria-live region, so what lands there is what a screen reader hears.
  await expect(page.getByTestId("assistant-turn").last()).toContainText(
    "closes the window",
    { timeout: 30_000 },
  );

  // A second message keeps the same conversation and exercises the tool path.
  await sendWithKeyboard(page, "What needs attention in this project?");
  await expect(page.getByTestId("user-turn").last()).toContainText("What needs attention");
  await expect(page.getByTestId("assistant-turn").last()).toContainText("closes the window", {
    timeout: 30_000,
  });
  await expect(composer(page)).toBeFocused();

  // The conversation names itself once, at level one: the breadcrumb's current
  // segment is the heading, and the sidebar sections stay at level two.
  await expectOneHeadingOne(page, "Why does the lease lapse under load?");
  await expectNoSeriousAxeViolations(page, "an active conversation");
  expect(errors).toEqual([]);
});

test("Shift+Enter inserts a newline instead of sending", async ({ page }) => {
  const errors = watchConsole(page);
  await openLinkedConversation(page);

  const box = composer(page);
  await box.focus();
  await page.keyboard.type("first line");
  await page.keyboard.press("Shift+Enter");
  await page.keyboard.type("second line");

  await expectComposerText(page, "first line\nsecond line");
  await expect(box).toBeFocused();
  // Nothing was sent: the draft is still in the composer.
  await expect(page.getByTestId("user-turn").last()).not.toContainText("second line");

  // Clear the draft so it does not leak into the reload test's expectations.
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.press("Backspace");
  await expectComposerText(page, "");
  expect(errors).toEqual([]);
});

// The issue page carries two composers: the conversation panel's Message box
// and the comment box under the issue, which mounts a beat later, once the
// issue body's data lands. A Lexical editor whose initial state carries a
// selection takes the caret the moment its root element attaches — Lexical
// reconciles that selection into the document, and a DOM selection inside a
// `contenteditable` moves the browser's focus into it — so the late composer
// used to steal the caret mid-typing and the keystrokes dispatched before it
// came back landed in the comment box. A long draft is what catches it: the
// typing has to still be going when the second composer attaches.
test("keeps every character of a long draft typed at speed", async ({ page }) => {
  const errors = watchConsole(page);
  await openLinkedConversation(page);

  const box = composer(page);
  await box.focus();
  const long = Array.from({ length: 200 }, (_, index) =>
    String.fromCharCode(97 + (index % 26)),
  ).join("");
  await page.keyboard.type(long);

  await expectComposerText(page, long);
  await expect(box).toBeFocused();
  // And nothing leaked into the issue page's own comment box.
  await expect(page.getByRole("textbox", { name: "Comment" })).toHaveText("");

  // Clear the draft so it does not leak into the reload test's expectations.
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.press("Backspace");
  await expectComposerText(page, "");
  expect(errors).toEqual([]);
});

test("lists the chat in the sidebar and filters it from the search box", async ({ page }) => {
  const errors = watchConsole(page);
  await openChat(page);
  await sendWithKeyboard(page, "Explain the admission gate to me");
  await expect(page).toHaveURL(/\/chat\/c\/conv_[0-9a-f]+$/);

  const sidebar = page.getByRole("complementary", { name: "Conversations" });
  const newRow = sidebar
    .getByTestId("sidebar-row-card")
    .filter({ hasText: "Explain the admission gate" });
  await expect(newRow).toBeVisible();

  const search = sidebar.getByRole("combobox", { name: "Search threads" });
  await search.focus();
  await expect(search).toBeFocused();
  await page.keyboard.type("Lease renewal");

  const results = sidebar.getByRole("listbox", { name: "Thread search results" });
  await expect(results.getByText(/Lease renewal under load/)).toBeVisible();
  await expect(results.getByText(/Explain the admission gate/)).toHaveCount(0);

  // Clearing the query brings the whole list back.
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.press("Backspace");
  await expect(newRow).toBeVisible();
  expect(errors).toEqual([]);
});

// §19.4. Both of the old chat entry points for a linked conversation now land
// on the issue page: `/chat/issues/:workItem` is a plain redirect, and
// `/chat/c/:conversation` redirects with `?panel=conversation` so the chat is
// open beside the issue rather than instead of it.
test("sends both linked-conversation paths to the issue page", async ({ page }) => {
  const errors = watchConsole(page);
  await openChat(page, `/issues/${hub.fixture.work_item}`);
  await expect(page).toHaveURL(new RegExp(`/work/i/${hub.fixture.work_item}$`));

  // The issue is the page: it names itself at level one, and the properties
  // sidebar carries what the old chat header's chips did.
  await expectOneHeadingOne(page, "Renew the lease before the handoff completes");
  await expect(page.getByTestId("issue-properties")).toBeVisible();
  await expect(page.getByRole("button", { name: /^Open/ }).first()).toBeEnabled();

  // The conversation is one row in the feed until it is opened.
  await expect(page.getByTestId("issue-live-row")).toBeVisible();
  await expect(page.getByTestId("conversation-surface")).toHaveCount(0);

  const surface = await openLinkedConversation(page);

  await expect(surface.getByTestId("composer-context-strip")).toContainText("#");

  // Waiting for a runner and running must not share a treatment (A.11), so the
  // strip says which one is true in words.
  await expect(page.getByTestId("execution-copy")).toContainText("Waiting for a runner");
  await expect(page.getByTestId("execution-copy")).toContainText(
    "The issue is queued. Nothing is running yet.",
  );

  const card = surface.getByTestId("issue-card");
  await expect(card).toContainText("Renew the lease before the handoff completes");
  await expect(card).toContainText("Waiting for a runner");

  // The page still names itself once, and what it names is the issue.
  await expectOneHeadingOne(page, "Renew the lease before the handoff completes");
  await expectNoSeriousAxeViolations(page, "the issue with its conversation panel");
  expect(errors).toEqual([]);
});

test("answers the search and new-chat keyboard shortcuts", async ({ page }) => {
  const errors = watchConsole(page);
  await openLinkedConversation(page);

  const search = sidebar(page).getByRole("combobox", { name: "Search threads" });
  await expect(search).toBeVisible();

  // `/` while the reader is typing is a slash, not a shortcut.
  await composer(page).focus();
  await page.keyboard.press("/");
  await expectComposerText(page, "/");
  await expect(search).not.toBeFocused();
  await page.keyboard.press("Backspace");
  await expectComposerText(page, "");

  // `/` anywhere else focuses the sidebar search. The route heading is
  // visually hidden, so the click that takes focus out of the composer lands
  // on the main region instead.
  await page.getByRole("main").click({ position: { x: 8, y: 8 } });
  await page.keyboard.press("/");
  await expect(search).toBeFocused();
  await expect(search).toHaveValue("");

  // There is one new-chat control in this context, and the shortcut is not a
  // second one: it calls the same action the compose pencil does (B.13).
  await expect(sidebar(page).getByRole("button", { name: "New thread" })).toHaveCount(1);
  await page.keyboard.press("ControlOrMeta+Shift+N");
  await expect(page).toHaveURL(/\/chat\/?$/);
  await expectOneHeadingOne(page, "What should we build in");
  await expect(page.getByTestId("hero-headline")).toBeVisible();
  expect(errors).toEqual([]);
});

test("requires the share-history confirmation before it creates a linked issue", async ({
  page,
}) => {
  const errors = watchConsole(page);
  await openChat(page);
  await sendWithKeyboard(page, "Move the renewal behind the acknowledgement");
  await expect(page).toHaveURL(/\/chat\/c\/conv_[0-9a-f]+$/);
  await expect(page.getByTestId("assistant-turn").last()).toContainText("closes the window", {
    timeout: 30_000,
  });

  await openCreateLinkedIssue(page);

  const form = page.getByRole("dialog", { name: "Create linked issue" });
  await expect(form).toBeVisible();
  // Opening the dialog focuses the editable title, not the read-only
  // project field, and Tab walks the fields in visual order.
  const title = form.getByLabel("Title", { exact: true });
  await expect(title).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(form.getByLabel("Objective")).toBeFocused();

  // §13.8 and §14: the next step comes before the confirmation, and it opens
  // on Detent's own defaults rather than on a blank.
  const next = form.getByTestId("handoff-next");
  await expect(next).toContainText("What happens next");
  await expect(next.getByLabel("Lane")).toHaveValue("Todo");
  await expect(next.getByLabel("Dispatch")).toHaveValue("later");
  await page.keyboard.press("Tab");
  await expect(next.getByLabel("Lane")).toBeFocused();
  await page.keyboard.press("Tab");
  // The tracker's own rank, 0-3, left unset so the hub applies the project's
  // default (§14).
  await expect(next.getByLabel("Priority")).toBeFocused();
  await expect(next.getByLabel("Priority")).toHaveValue("");
  await page.keyboard.press("Tab");
  await expect(next.getByLabel("Dispatch")).toBeFocused();
  await page.keyboard.press("Tab");

  const share = form.getByRole("checkbox", {
    name: "Share this conversation's history with the project",
  });
  await expect(share).toBeFocused();
  await expect(share).not.toBeChecked();

  // Submitting without the confirmation states the rule and puts the reader on
  // the control that satisfies it.
  await form.getByRole("button", { name: "Create linked issue" }).focus();
  await page.keyboard.press("Enter");
  await expect(form.getByTestId("handoff-local-error")).toContainText(
    "Linking shares this conversation's full history with the project.",
  );
  await expect(share).toBeFocused();

  // Space ticks the confirmation, and the same submit now completes.
  await page.keyboard.press("Space");
  await expect(share).toBeChecked();
  await form.getByRole("button", { name: "Create linked issue" }).focus();
  await page.keyboard.press("Enter");

  await expect(form).toBeHidden();
  // Linking moves the reader to the issue page with the chat in the panel
  // (§19.4); everything the link produced is still there, in the panel.
  await expect(page).toHaveURL(/\/work\/i\//);
  await expect(page.getByTestId("issue-card").last()).toContainText(
    "Move the renewal behind the acknowledgement",
  );

  await expect(page.getByTestId("composer-context-strip")).toContainText("#");
  await expect(page.getByTestId("execution-copy")).toContainText("Waiting for a runner");
  expect(errors).toEqual([]);
});

test("restores the transcript and the unsent draft after a reload", async ({ page }) => {
  const errors = watchConsole(page);
  await openChat(page);
  await sendWithKeyboard(page, "Why does the checkout lock lapse?");
  await expect(page).toHaveURL(/\/chat\/c\/conv_[0-9a-f]+$/);
  const url = page.url();
  await expect(page.getByTestId("assistant-turn").last()).toContainText("closes the window", {
    timeout: 30_000,
  });

  await composer(page).focus();
  await page.keyboard.type("half a thought I have not sent");
  await expectComposerText(page, "half a thought I have not sent");

  await page.reload({ waitUntil: "domcontentloaded" });
  await expect(page).toHaveURL(url);
  await expect(page.getByTestId("user-turn").first()).toContainText(
    "Why does the checkout lock lapse?",
  );
  await expect(page.getByTestId("assistant-turn").last()).toContainText("closes the window");
  await expectComposerText(page, "half a thought I have not sent");
  expect(errors).toEqual([]);
});

for (const width of [1200, 1440]) {
  test.describe(`shell at ${width}px`, () => {
    test.use({ viewport: { width, height: 900 } });

    test(`fills the viewport exactly at ${width}px`, async ({ page }) => {
      const errors = watchConsole(page);
      await openChat(page);
      const box = await page.evaluate(() => {
        const rect = document
          .querySelector("[data-slot='sidebar-wrapper']")
          .getBoundingClientRect();
        return {
          x: rect.x,
          y: rect.y,
          width: rect.width,
          height: rect.height,
          viewportWidth: window.innerWidth,
          viewportHeight: window.innerHeight,
          documentScrollWidth: document.documentElement.scrollWidth,
        };
      });
      expect(box).toEqual({
        x: 0,
        y: 0,
        width,
        height: 900,
        viewportWidth: width,
        viewportHeight: 900,
        documentScrollWidth: width,
      });
      expect(errors).toEqual([]);
    });
  });
}

test.describe("narrow viewport", () => {

  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("puts the sidebar behind a drawer toggle and keeps the composer reachable", async ({
    page,
  }) => {
    const errors = watchConsole(page);
    await openChat(page);

    const toggle = page.getByRole("button", { name: "Toggle main sidebar" });
    await expect(toggle).toBeVisible();

    // Closed: nothing of the list exists, so no row is a tab stop behind the
    // transcript and Shift+Tab off the toggle cannot walk into one.
    await expect(sidebar(page)).toHaveCount(0);
    await toggle.focus();
    await page.keyboard.press("Shift+Tab");
    expect(await focusInsideDrawer(page)).toBe(false);

    await toggle.focus();
    await page.keyboard.press("Enter");
    await expect(sidebar(page)).toBeVisible();

    // It is a dialog, and Base UI moves focus into it and keeps it there.
    await expect(page.getByRole("dialog")).toBeVisible();
    await expect.poll(() => focusInsideDrawer(page)).toBe(true);

    // Escape closes it and hands focus back to the control that opened it.
    await page.keyboard.press("Escape");
    await expect(sidebar(page)).toHaveCount(0);
    await expect(toggle).toBeFocused();

    await toggle.focus();
    await page.keyboard.press("Enter");
    await expect(sidebar(page)).toBeVisible();
    await expect.poll(() => focusInsideDrawer(page)).toBe(true);

    // Tab cycles inside the sheet and never leaves it, in either direction.
    for (let step = 0; step < 4; step += 1) {
      await page.keyboard.press("Tab");
      expect(await focusInsideDrawer(page)).toBe(true);
    }
    await page.keyboard.press("Shift+Tab");
    expect(await focusInsideDrawer(page)).toBe(true);

    await page
      .getByRole("complementary", { name: "Conversations" })
      .getByTestId("sidebar-row-card")
      .filter({ hasText: "Lease renewal under load" })
      .click();
    await expect(sidebar(page)).toHaveCount(0);
    await expect(page).toHaveURL(new RegExp(`/work/i/${hub.fixture.work_item}`));

    await expect(page.getByTestId("conversation-surface")).toBeVisible();
    await expect(composer(page)).toBeVisible();
    await composer(page).focus();
    await page.keyboard.type("still reachable at 390px");
    await expectComposerText(page, "still reachable at 390px");

    await expectOneHeadingOne(page);
    await expectNoSeriousAxeViolations(page, "the narrow conversation view");
    expect(errors).toEqual([]);
  });

  // Everything above runs under the config's `reducedMotion: "reduce"`. The
  // sheet also animates, and the focus contract must not depend on which of
  // the two a reader gets.
  test.describe("with motion", () => {
    test.use({ reducedMotion: "no-preference" });

    test("keeps the drawer focus contract while it animates", async ({ page }) => {
      const errors = watchConsole(page);
      await openChat(page);

      const toggle = page.getByRole("button", { name: "Toggle main sidebar" });
      await expect(sidebar(page)).toHaveCount(0);

      await toggle.focus();
      await page.keyboard.press("Enter");
      await expect(sidebar(page)).toBeVisible();
      await expect.poll(() => focusInsideDrawer(page)).toBe(true);

      await page.keyboard.press("Escape");
      await expect(toggle).toBeFocused();
      await expect(sidebar(page)).toHaveCount(0);
      expect(errors).toEqual([]);
    });
  });
});

// The corrections in decisions.md §10, against the same hosted hub as
// everything above. Two of them cannot be reached end to end from a browser
// because the preview fixture has no runner and no way to fail a coordinator
// turn; each of those says so at the top of the test and asserts the half the
// fixture can reach. The rest are driven through the client, from the
// keyboard, exactly like the tests above.
test.describe("decisions.md §10 corrections", () => {
  // §10.2. `POST /conversations` carries a client-generated `key` and runs
  // through the idempotent mutation path, so the create that a dropped
  // response makes the client repeat returns the stored conversation instead
  // of opening a second one. The client's own request is captured and replayed
  // byte for byte: a hand-written body would test the fixture's idea of the
  // command, not the client's.
  test("creates one conversation when the create key is retried", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);

    const created = page.waitForRequest(
      (request) =>
        request.method() === "POST" && new URL(request.url()).pathname.endsWith("/conversations"),
    );
    await sendWithKeyboard(page, "Retried create");
    const request = await created;
    await expect(page).toHaveURL(/\/chat\/c\/conv_[0-9a-f]+$/);
    const id = currentConversation(page);

    const body = request.postData();
    expect(JSON.parse(body).key).toEqual(expect.any(String));
    const replay = await page.evaluate(
      async ({ url, body, csrf }) => {
        const response = await fetch(url, {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
          body,
        });
        return { status: response.status, payload: await response.json() };
      },
      { url: request.url(), body, csrf: request.headers()["x-csrf-token"] },
    );
    expect(replay.status).toBe(201);
    expect(replay.payload.conversation.id).toBe(id);

    // One conversation, one message: the replay neither forked the chat nor
    // duplicated the first message.
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page).toHaveURL(new RegExp(`/chat/c/${id}$`));
    await expect(page.getByTestId("user-turn")).toHaveCount(1);
    await expect(
      sidebar(page).getByTestId("sidebar-row-card").filter({ hasText: "Retried create" }),
    ).toHaveCount(1);
    expect(errors).toEqual([]);
  });

  // §10.3. Not covered end to end: the retry affordance appears only on a
  // message whose delivery is `unknown`, `failed` or `rejected`, and this
  // fixture cannot produce one. Those deliveries are written by the worker
  // paths (`internal/hubserver/conversation_worker.go` finishExecution) or by
  // a coordinator turn that returns an error
  // (`internal/hubserver/conversation_coordinator.go` coordinatorOutcome), and
  // the preview's scripted coordinator
  // (`internal/hubserver/hosted_browser_test.go` browserConversationBackend)
  // never returns one: its only failure route is the tool handler, which turns
  // every tool failure into a successful tool result. What is checked here is
  // the rule the affordance is bound to — the hub's `retry` command, reached
  // from this signed-in session — and that the client offers no retry on a
  // delivery the hub would refuse.
  test("re-queues a lost message through the retry command", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);
    await sendWithKeyboard(page, "Retry rules");
    await expect(page).toHaveURL(/\/chat\/c\/conv_[0-9a-f]+$/);
    const id = currentConversation(page);
    await expect(page.getByTestId("assistant-turn").last()).toContainText("closes the window", {
      timeout: 30_000,
    });

    // A settled message is not retryable, so nothing is offered on it.
    await expect(page.getByTestId("user-turn")).toHaveCount(1);
    await expect(page.getByTestId("message-retry")).toHaveCount(0);
    await expect(page.getByTestId("message-retry-note")).toHaveCount(0);

    const snapshot = await hubAPI(page, "GET", conversationPath(id));
    expect(snapshot.status).toBe(200);
    const sent = snapshot.payload.messages.find((message) => message.role === "user");
    expect(sent.delivery).toBe("delivered");
    expect(errors).toEqual([]);

    // The command exists, names the message rather than resending it, and is
    // refused for a delivery that settled. The message is left where it was.
    // The refusal below is deliberate, so the 422 the browser logs for it is
    // the one console error this test expects.
    const refused = await hubAPI(page, "POST", `${conversationPath(id)}/commands`, {
      key: "spec-retry-settled",
      kind: "retry",
      message_id: sent.id,
    });
    expect(refused.status).toBe(422);
    expect(refused.payload.code).toBe("invalid_request");

    const after = await hubAPI(page, "GET", conversationPath(id));
    expect(after.payload.conversation.message_count).toBe(
      snapshot.payload.conversation.message_count,
    );
    await expect(page.getByTestId("user-turn")).toHaveCount(1);
    expect(errors.filter((entry) => !entry.includes("422"))).toEqual([]);
  });

  // §10.4. Not covered end to end for the same reason: `resume` is written
  // when a runner binds an attempt
  // (`internal/hubserver/conversation_worker.go`, the bind handler), and this
  // fixture enrolls no runner. What is checked is that the execution resource
  // carries the field, that it is empty when nothing has bound, and that the
  // client does not announce a transcript recovery that did not happen — the
  // failure mode that would mislead a reader about how much context the answer
  // had.
  test("says when a runner continued from a transcript", async ({ page }) => {
    const errors = watchConsole(page);
    await openLinkedConversation(page);
    await expect(page.getByTestId("execution-strip")).toBeVisible();

    const snapshot = await hubAPI(page, "GET", conversationPath(hub.fixture.conversation));
    expect(snapshot.status).toBe(200);
    expect(snapshot.payload.conversation.execution).toHaveProperty("resume");
    expect(snapshot.payload.conversation.execution.resume).toBe("");

    await expect(page.getByTestId("execution-resume")).toHaveCount(0);
    await expect(page.getByTestId("execution-copy")).toContainText("Waiting for a runner");
    expect(errors).toEqual([]);
  });

  // §10.5. The share confirmation states the whole history, taken from the
  // conversation's `message_count` rather than from the page that happens to
  // be loaded.
  test("states the whole history in the audience preview", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);
    await sendWithKeyboard(page, "Count my history");
    await expect(page).toHaveURL(/\/chat\/c\/conv_[0-9a-f]+$/);
    const id = currentConversation(page);
    await expect(page.getByTestId("assistant-turn").last()).toContainText("closes the window", {
      timeout: 30_000,
    });

    const snapshot = await hubAPI(page, "GET", conversationPath(id));
    const count = snapshot.payload.conversation.message_count;
    expect(count).toBe(2);

    await openCreateLinkedIssue(page);
    const form = page.getByRole("dialog", { name: "Create linked issue" });
    await expect(form).toBeVisible();
    await expect(form.getByTestId("handoff-message-count")).toHaveText(
      `All ${count} messages in this chat become readable by everyone who can read project Browser collaboration.`,
    );
    await expect(form.getByTestId("handoff-audience")).toContainText("Sharing cannot be undone.");

    // Nothing was linked: Escape leaves the chat as it was.
    await page.keyboard.press("Escape");
    await expect(form).toBeHidden();
    await expect(page.getByTestId("composer-context-strip")).not.toContainText("#");
    expect(errors).toEqual([]);
  });

  // §10.11. The `viewer` account holds a read-only grant on the same project,
  // so the hub reports `can_write: false` and every control that would write
  // is gone rather than merely inert-looking.
  test("gives a read-only viewer no controls", async ({ page }) => {
    const errors = watchConsole(page);
    const surface = await openLinkedConversation(page, "viewer");
    await expect(page.getByRole("heading", { level: 1 })).toContainText(
      "Renew the lease before the handoff completes",
    );
    await expect(surface.getByTestId("composer-context-strip")).toContainText("#");

    // The reader can read: the transcript is there.
    await expect(page.getByTestId("user-turn").first()).toContainText(
      "Why does the lease lapse under load?",
    );

    // And the issue's own write controls are gone rather than greyed out: no
    // lane menu, no priority menu, no label removal (§10.11, §19.1).
    const properties = page.getByTestId("issue-properties");
    await expect(properties.getByTestId("state-menu")).toHaveCount(0);
    await expect(properties.getByTestId("priority-menu")).toHaveCount(0);

    // The composer takes no keystrokes and says why, in words rather than by
    // looking greyed out.
    await expect(composer(page)).toBeDisabled();
    await expect(page.getByText("You can read this chat but not send messages")).toBeVisible();

    await expect(page.getByRole("button", { name: "Send message" })).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Environment disconnected" }).first(),
    ).toBeDisabled();
    await expect(page.getByRole("button", { name: "Stop generation" })).toHaveCount(0);
    // The header's Add action menu offers nothing that writes.
    await headerActionsTrigger(page).click();
    await expect(page.getByRole("menuitem", { name: "Create linked issue" })).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(page.getByTestId("conversation-menu-button")).toHaveCount(0);
    // The execution strip reports, and offers nothing to press.
    await expect(page.getByTestId("execution-strip").getByRole("button")).toHaveCount(0);

    await expectOneHeadingOne(page, "Renew the lease before the handoff completes");
    await expectNoSeriousAxeViolations(page, "a read-only viewer");
    expect(errors).toEqual([]);
  });

  // §13.9 and §14. Settled replaced Archive: a conversation settles on its own
  // and unsettles on activity, so the client offers no archive action, no
  // banner and no "Show archived" toggle, and the Settled shelf is a group in
  // the list rather than a filter the reader has to remember to turn on.
  test("offers no archive action anywhere, and groups Settled in the list", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);
    await sendWithKeyboard(page, "Settle me");
    await expect(page).toHaveURL(/\/chat\/c\/conv_[0-9a-f]+$/);

    const list = page.getByRole("complementary", { name: "Conversations" });
    await expect(
      list.getByTestId("sidebar-row-card").filter({ hasText: "Settle me" }),
    ).toBeVisible();

    // Nothing archives, and nothing says "archived".
    await expect(page.getByTestId("conversation-menu-button")).toHaveCount(0);
    await expect(page.getByTestId("archived-banner")).toHaveCount(0);
    await expect(list.getByRole("checkbox", { name: "Show archived" })).toHaveCount(0);
    await expect(page.getByText(/archived/i)).toHaveCount(0);
    // And the composer stays open: a settled chat is one you can write in.
    await expect(composer(page)).toBeEnabled();

    await expect(list.getByTestId("sidebar-settled-shelf-toggle")).toContainText("Settled (0)");
    await expectNoSeriousAxeViolations(page, "a chat with no archive action");
    expect(errors).toEqual([]);
  });

  // §13.14 and §14. The composer's three chips are real pickers, and "Auto" is
  // what every one of them opens on. This is the browser's job: a Base UI
  // select needs layout to open, which jsdom does not have.
  test("opens the composer's model, effort and access pickers", async ({ page }) => {
    const errors = watchConsole(page);

    const surface = await openLinkedConversation(page);

    for (const label of ["Model", "Reasoning effort", "Runtime access"]) {
      const trigger = surface.getByLabel(label, { exact: true });
      await expect(trigger).toBeVisible();
      await expect(trigger).toContainText("Auto");
    }

    // The chip row itself is not a tab stop: every control inside it is.
    await expect(surface.getByTestId("composer-scope-chips")).not.toHaveAttribute(
      "tabindex",
      "0",
    );

    // Effort is still their `Select`, grouped under "Reasoning" with Auto
    // first; the levels themselves are the selected model's, which here is
    // Auto, so it is the organization's fixed vocabulary.
    const effort = surface.getByRole("combobox", { name: "Reasoning effort" });
    await effort.click();
    const options = page.getByRole("option");
    await expect(options.first()).toBeVisible();
    await expect(options.filter({ hasText: "Auto" })).toHaveCount(1);
    await expect(page.getByText("Reasoning", { exact: true })).toBeVisible();
    // Nothing is badged Default while the model is Auto: the badge marks what
    // one model defaults to, and Auto is not one model.
    await expect(page.getByTestId(/^composer-effort-default-/)).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("option")).toHaveCount(0);

    // Access names what a turn may do, in words rather than in vocabulary.
    const access = surface.getByRole("combobox", { name: "Runtime access" });
    await access.click();
    await expect(page.getByRole("option").first()).toBeVisible();
    await expect(page.getByRole("option").filter({ hasText: "Full access" })).toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("option")).toHaveCount(0);

    await surface.getByTestId("composer-model").hover();
    await expect(tooltip(page)).toContainText("The model this conversation's turns run on.");
    await page.mouse.move(0, 0);

    await surface.getByTestId("composer-model").click();
    const popup = page.getByTestId("composer-model-popup");
    await expect(popup).toBeVisible();
    await expect(popup.getByPlaceholder("Search models…")).toBeVisible();
    await expect(popup.getByTestId("composer-model-option-auto")).toBeVisible();

    // The preview runner reports a catalogue, so the Models group is the
    // runner's own order with the backend's default leading it — not the
    // alphabet, which put GPT-5.6-Sol above GPT-6-Astra — and the ⌘ numbers
    // follow that order.
    const astra = popup.getByTestId("composer-model-option-gpt-6-astra");
    await expect(astra).toBeVisible();
    await expect(astra).toContainText("⌘1");
    await expect(popup.getByTestId("composer-model-option-gpt-5.6-sol")).toContainText("⌘2");
    const rows = popup.locator("[data-testid^='composer-model-option-']");
    expect(await rows.nth(1).getAttribute("data-testid")).toBe(
      "composer-model-option-gpt-6-astra",
    );

    const openai = popup.getByTestId("composer-model-provider-openai");
    await expect(openai).toBeVisible();
    await expect(openai).toHaveText("");
    await expect(openai.locator("svg")).toHaveCount(1);
    await openai.hover();
    await expect(tooltip(page)).toContainText("OpenAI");

    // The superseded model is shelved rather than mixed in, and comes back
    // when the shelf is opened.
    await expect(popup.getByTestId("composer-model-option-gpt-5.3-codex")).toHaveCount(0);
    await popup.getByTestId("composer-model-legacy-toggle").click();
    await expect(popup.getByTestId("composer-model-option-gpt-5.3-codex")).toBeVisible();

    // Choosing the model scopes the effort picker to its own ladder, and the
    // rung that model defaults to wears the Default badge.
    await astra.click();
    await expect(popup).toHaveCount(0);
    await expect(surface.getByTestId("composer-model")).toContainText("GPT-6-Astra");
    await effort.click();
    await expect(page.getByRole("option").filter({ hasText: "Extra High" })).toHaveCount(1);
    await expect(page.getByTestId("composer-effort-default-medium")).toHaveText("Default");
    await expect(page.getByTestId("composer-effort-note")).toContainText(
      "These are the levels this model supports.",
    );
    await page.keyboard.press("Escape");
    await expect(page.getByRole("option")).toHaveCount(0);

    const attach = surface.getByTestId("composer-attach");
    await expect(attach).toBeEnabled();
    await attach.hover();
    await expect(tooltip(page)).toContainText("Attach files");
    await expect(surface.getByTestId("composer-attach-input")).toHaveAttribute("multiple", "");

    await expectNoSeriousAxeViolations(page, "the composer's pickers");
    expect(errors).toEqual([]);
  });

  test("opens the slash menu and runs the command it highlights", async ({ page }) => {
    const errors = watchConsole(page);
    const surface = await openLinkedConversation(page);
    const prompt = surface.getByRole("textbox", { name: "Message" });

    await prompt.focus();
    await page.keyboard.type("/");
    const menu = page.getByTestId("slash-menu");
    await expect(menu).toBeVisible();
    await expect(menu.getByTestId("slash-command-model")).toBeVisible();

    expect(
      await menu.evaluate((node) => {
        const card = node.closest("[data-chat-composer-surface]");
        if (card === null) return { inCard: false };
        const body = card.querySelector("[data-chat-composer-body]");
        return {
          inCard: true,
          aboveEditor:
            (node.compareDocumentPosition(body) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0,
          sameWidth:
            Math.abs(
              node.getBoundingClientRect().width - card.getBoundingClientRect().width,
            ) < 2,
        };
      }),
    ).toEqual({ inCard: true, aboveEditor: true, sameWidth: true });
    // The runner's own skills and prompts are in the contract and the hub does
    // not serve them yet, so the menu says so rather than dropping the feature
    // silently or inventing rows that would not run (decisions.md §16).
    await expect(menu.getByTestId("slash-provider-skills")).toContainText(
      "Runner skills and prompts appear here once the hub serves them",
    );

    // Typing filters by name, and the highlighted row is what Enter runs.
    await page.keyboard.type("mod");
    await expect(menu.getByTestId("slash-command-model")).toHaveAttribute(
      "aria-selected",
      "true",
    );
    await expect(menu.getByTestId("slash-command-effort")).toHaveCount(0);

    await page.keyboard.press("Enter");
    // `/model` opened the model picker, and the `/…` token went with it: a
    // command must not leave its own name sitting in the draft.
    await expect(page.getByTestId("composer-model-popup")).toBeVisible();
    await expect(page.getByTestId("composer-model-option-auto")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect.poll(() => prompt.evaluate((node) => node.innerText.replace(/\n$/, ""))).toBe(
      "",
    );
    expect(errors).toEqual([]);
  });

  // §19.5. The issue page's composer offers the commands that page has, and
  // only those: a comment has no model, no effort, no access and no files, and
  // there is no runner working on this issue to send to.
  test("offers the issue page's own slash commands", async ({ page }) => {
    const errors = watchConsole(page);
    await openWorkIssue(page);
    const prompt = page.getByRole("textbox", { name: "Comment" });

    await prompt.focus();
    await page.keyboard.type("/");
    const menu = page.getByTestId("slash-menu");
    await expect(menu).toBeVisible();
    await expect(menu.getByTestId("slash-command-shortcuts")).toBeVisible();
    await expect(menu.getByTestId("slash-command-clear")).toBeVisible();
    // This card posts comments and nothing else, so there is no mode to
    // switch, no turn to configure, no file to attach and no turn to stop
    // (§19.5, Michael's review of September 12).
    for (const name of ["model", "effort", "access", "attach", "stop", "comment", "runner"]) {
      await expect(menu.getByTestId(`slash-command-${name}`)).toHaveCount(0);
    }
    await expectNoSeriousAxeViolations(page, "the issue composer's slash menu");

    // Escape closes the menu for the token that closed it, and the next
    // different token opens it again.
    await page.keyboard.press("Escape");
    await expect(menu).toHaveCount(0);
    await page.keyboard.type("clea");
    await expect(page.getByTestId("slash-menu")).toBeVisible();

    await page.keyboard.press("Enter");
    await expect(page.getByTestId("slash-menu")).toHaveCount(0);
    await expect.poll(() => prompt.evaluate((node) => node.innerText.replace(/\n$/, ""))).toBe(
      "",
    );
    expect(errors).toEqual([]);
  });

  test("names the footer icons on tooltips, the way shared UI does", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);
    const list = page.getByRole("complementary", { name: "Conversations" });

    const footer = page.locator("[data-slot='sidebar-footer']");
    const pullRequests = footer.getByRole("button", { name: "Pull requests" });
    await expect(pullRequests).toBeVisible();
    await expect(pullRequests).toHaveText("");
    await pullRequests.hover();

    await expect(tooltip(page)).toContainText("Pull Requests");

    await page.mouse.move(0, 0);
    await list.getByTestId("sidebar-row-card").first().hover();
    const preview = page.locator("[data-slot='tooltip-popup']").last();
    await expect(preview).toBeVisible();
    await expect(preview).toContainText("Browser collaboration");
    expect(errors).toEqual([]);
  });

  test("serves the MIT notice at the top of the bundle", async ({ page }) => {
    await page.goto(hub.fixture.accounts.owner, { waitUntil: "domcontentloaded" });
    const response = await page.request.get(`${hub.fixture.url}/static/app/conversation/app.js`);
    expect(response.status()).toBe(200);

    const banner = (await response.text()).slice(0, 1024);
    expect(banner.trimStart().startsWith("/*")).toBe(true);
    expect(banner).toContain("MIT License");
    expect(banner).toContain("T3 Code");
    expect(banner).toContain("Permission is hereby granted, free of charge");

    // And that bundle is the one the shell loads.
    const shell = await page.request.get(hub.fixture.chat);
    expect(await shell.text()).toContain("/static/app/conversation/app.js");
  });

  // §10.14, restated for the one frontend (§11). There are no hosted Templ
  // pages left to link from: the sidebar is the shell for every route, so the
  // assertion is that its own Work and Chat destinations navigate the client.
  test("links the hosted pages to the client", async ({ page }) => {
    const errors = watchConsole(page);
    await page.goto(hub.fixture.accounts.owner, { waitUntil: "domcontentloaded" });
    await expect(page).toHaveURL(/\/organization$/);

    const appSidebar = page.locator("[data-app-sidebar]");
    await expect(appSidebar.getByRole("button", { name: "Organization" })).toBeVisible();
    await expect(page.getByRole("complementary", { name: "Conversations" })).toHaveCount(0);
    await expect(appSidebar.getByRole("button", { name: "Back" })).toBeVisible();

    // Off a settings route the sidebar is the conversation list again, and its
    // own destinations navigate the client.
    await page.goto(`${hub.fixture.url}/work`, { waitUntil: "domcontentloaded" });
    const list = page.getByRole("complementary", { name: "Conversations" });
    await expect(list).toBeVisible();

    const chat = list.getByTestId("nav-chat");
    await chat.focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(/\/chat$/);
    await expectOneHeadingOne(page, "What should we build in");
    await expect(composer(page)).toBeFocused();

    const work = list.getByTestId("nav-work");
    await work.focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(/\/work(\/p\/[^/]+)?$/);
    expect(errors).toEqual([]);
  });

  // §11. `/` is not a second front door: it sends the reader to Work.
  test("sends the root path to the Work board", async ({ page }) => {
    await page.goto(hub.fixture.accounts.owner, { waitUntil: "domcontentloaded" });
    await page.goto(hub.fixture.url, { waitUntil: "domcontentloaded" });
    await expect(page).toHaveURL(/\/work$/);
  });
});

test.describe("the ported shell's interactions", () => {

  function wrapper(page) {
    return page.locator("[data-slot='sidebar-wrapper']");
  }

  test("names the product 'Detent Cloud' in the sidebar brand row", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);

    const brand = sidebar(page).locator("a.dc-brand");
    await expect(brand).toBeVisible();
    await expect(brand).toHaveAccessibleName("Go to Detent Cloud");

    expect((await brand.innerText()).replace(/\s+/g, " ").trim()).toBe("Detent Cloud");
    await expect(brand.locator("svg[aria-label='Detent']")).toBeVisible();
    expect(errors).toEqual([]);
  });

  test("toggles the sidebar with the keyboard and keeps the rail's tooltip", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);

    await expect(wrapper(page)).toHaveAttribute("data-sidebar-state", "expanded");
    await composer(page).focus();
    await page.keyboard.press("ControlOrMeta+b");
    await expect(wrapper(page)).toHaveAttribute("data-sidebar-state", "collapsed");

    await expect(sidebar(page)).not.toBeInViewport();

    const toggle = page.getByRole("button", { name: "Toggle main sidebar" });
    await expect(toggle).toBeVisible();
    await toggle.hover();
    await expect(tooltip(page)).toContainText("Toggle main sidebar");

    await page.keyboard.press("ControlOrMeta+b");
    await expect(wrapper(page)).toHaveAttribute("data-sidebar-state", "expanded");
    await expect(sidebar(page)).toBeVisible();
    expect(errors).toEqual([]);
  });

  test("moves the sidebar on the transition, gated by their flag", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);

    const motion = await page.evaluate(() => {
      const shell = document.querySelector("[data-slot='sidebar-wrapper']");
      shell.setAttribute("data-panel-animations", "true");
      shell.style.setProperty("--panel-animation-duration", "200ms");
      const gap = document.querySelector("[data-slot='sidebar-gap']");
      const container = document.querySelector("[data-slot='sidebar-container']");
      const read = (node) => {
        const style = getComputedStyle(node);
        return { property: style.transitionProperty, duration: style.transitionDuration };
      };
      return { gap: read(gap), container: read(container) };
    });

    expect(motion.gap.property).toContain("width");
    expect(motion.gap.duration).toBe("0.2s");
    expect(motion.container.property).toContain("left");
    expect(motion.container.property).toContain("width");
    expect(motion.container.duration).toBe("0.2s");
    expect(errors).toEqual([]);
  });

  test("resizes the sidebar by dragging the rail and keeps the width", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);

    const widthNow = () =>
      page.evaluate(() =>
        Math.round(
          document.querySelector("[data-slot='sidebar-container']").getBoundingClientRect().width,
        ),
      );
    const before = await widthNow();
    expect(before).toBe(256);

    const rail = page.locator("[data-slot='sidebar-rail']");
    const box = await rail.boundingBox();
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width / 2 + 80, box.y + box.height / 2, { steps: 8 });
    await page.mouse.up();

    await expect.poll(widthNow).toBeGreaterThan(before + 40);
    const dragged = await widthNow();

    const stored = await page.evaluate(() =>
      JSON.parse(window.localStorage.getItem("chat_thread_sidebar_width")),
    );
    expect(Math.round(stored)).toBe(dragged);

    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(sidebar(page)).toBeVisible();
    await expect.poll(widthNow).toBe(dragged);
    expect(errors).toEqual([]);
  });

  test("shows the hover card on a thread row, at their placement", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);
    const list = page.getByRole("complementary", { name: "Conversations" });

    const row = list.getByTestId("sidebar-row-card").first();
    await row.hover();

    const preview = page.locator("[data-slot='tooltip-popup']").last();
    await expect(preview).toBeVisible();

    // To the right of the row it belongs to, and top-aligned with it.
    const rowBox = await row.boundingBox();
    const cardBox = await preview.boundingBox();
    expect(cardBox.x).toBeGreaterThanOrEqual(rowBox.x + rowBox.width);
    expect(Math.abs(cardBox.y - rowBox.y)).toBeLessThan(40);

    // Moving off closes it immediately: their `closeDelay` is 0.
    await page.mouse.move(cardBox.x + cardBox.width + 200, cardBox.y + 400);
    await expect(preview).toHaveCount(0);
    expect(errors).toEqual([]);
  });

  test("opens the right panel, switches surfaces and disables the ones Detent cannot feed", async ({
    page,
  }) => {
    const errors = watchConsole(page);
    await openChat(page);

    const toggle = page.getByRole("button", { name: /^Toggle right panel/ });
    await expect(toggle).toBeVisible();
    await toggle.click();
    await expect(toggle).toHaveAttribute("aria-pressed", "true");

    const launcher = page.locator("[aria-label='Open a surface']");
    await expect(launcher).toBeVisible();

    const cards = await launcher.evaluate((node) => {
      const grid = node.querySelector("[class*='grid-cols-2']");
      return [...(grid?.children ?? [])].map((card) => {
        const button = card.tagName === "BUTTON" ? card : card.querySelector("button");
        const dimmed = card.className.includes("opacity-40");
        return {
          // The card's own name, not the shortcut `Kbd` above it, which
          // carries `font-medium` too.
          label: card.querySelector("[class*='pe-8'] [class*='font-medium']")?.textContent?.trim(),
          reason: card.querySelector("[class*='leading-relaxed']")?.textContent?.trim(),
          openable: button !== null && !dimmed,
        };
      });
    });
    const byLabel = Object.fromEntries(cards.map((card) => [card.label, card]));

    for (const name of ["Browser", "Terminal", "Files", "Diff", "Pull request"]) {
      expect(byLabel[name], `${name} card`).toBeDefined();
    }

    expect(byLabel.Agents, "Agents card is gone").toBeUndefined();
    // The Browser has no Detent counterpart: present, inert, and saying so —
    // never quietly removed (§16).
    expect(byLabel.Browser.openable, "Browser is inert").toBe(false);
    expect(byLabel.Browser.reason, "Browser says why").toBe("Not available on Detent Cloud yet.");
    // The Terminal is real since §18.3, and what disables it here is the same
    // thing that disables Files: a draft chat names no issue, so there is no
    // worktree for a shell to run in. The sentence is the structural reason
    // rather than a product absence, which is the §16 distinction this test
    // and the Browser line above draw.
    expect(byLabel.Terminal.openable, "Terminal is inert on a draft chat").toBe(false);
    expect(byLabel.Terminal.reason, "Terminal says why").toBe("Link an issue to get a worktree.");
    // Files is real since §18.11 step 2, but it needs a worktree, and a draft
    // chat names no issue for a runner to open one on. So it is inert here for
    // a Detent reason rather than an absence of product — which is the §16
    // distinction the two strings above and this one draw.
    expect(byLabel.Files.openable, "Files is inert on a draft chat").toBe(false);
    expect(byLabel.Files.reason, "Files says why").toBe("Available from an issue with a runner.");
    // Diff always has a card — Detent either has a change request or says why
    // it has none — and opening it swaps the launcher for the surface.
    expect(byLabel.Diff.openable, "Diff is openable").toBe(true);
    await launcher
      .locator("[class*='grid-cols-2'] > *")
      .filter({ hasText: "Diff" })
      .getByRole("button")
      .click();
    await expect(launcher).toHaveCount(0);

    await toggle.click();
    await expect(toggle).toHaveAttribute("aria-pressed", "false");
    expect(errors).toEqual([]);
  });

  // §18.3. The Terminal surface, end to end: the launcher card is live on an
  // issue whose runner serves terminals, opening it puts a real PTY on the
  // workspace's worktree behind xterm, and what the reader types reaches the
  // shell. The preview fixture's runner is this process, at `user` isolation
  // with its owner account holding the runners grant.
  test("opens a terminal on the workspace and runs what the reader types", async ({ page }) => {
    const errors = watchConsole(page);
    await openWorkIssue(page);

    const toggle = page.getByRole("button", { name: /^Toggle right panel/ });
    await toggle.click();

    await page.getByRole("button", { name: "Add panel surface" }).click();
    await page.getByRole("menuitem", { name: /^Terminal/ }).click();
    await expect(page.getByTestId("terminal-surface")).toBeVisible();

    // The shell takes a moment: the workspace has to be claimed and the PTY
    // allocated. The prompt is what says it is there.
    const screen = page.locator("[data-terminal-owner='right-panel'] .xterm-screen");
    await expect(screen).toBeVisible({ timeout: 60_000 });
    await expect(page.getByTestId("terminal-status")).toHaveAttribute("data-status", "open", {
      timeout: 60_000,
    });

    await page.locator("[data-terminal-owner='right-panel']").click();
    await page.keyboard.type("echo hi");
    await page.keyboard.press("Enter");
    // xterm draws to a canvas or to spans depending on the renderer, so the
    // assertion reads the buffer the way a person reads the screen.
    await expect
      .poll(
        async () =>
          screen.evaluate((node) => node.closest(".xterm")?.textContent ?? ""),
        { timeout: 60_000 },
      )
      .toContain("hi");
    expect(errors).toEqual([]);
  });

  // §19.3. On a page that has a conversation, the panel toggle and
  // `rightPanel.toggle` open the same surface the live row does: the
  // conversation, not an empty launcher.
  test("opens the conversation surface from the panel toggle on an issue", async ({ page }) => {
    const errors = watchConsole(page);
    await openWorkIssue(page);
    await expect(page.getByTestId("conversation-surface")).toHaveCount(0);

    const toggle = page.getByRole("button", { name: /^Toggle right panel/ });
    await toggle.click();
    await expect(page.getByTestId("conversation-surface")).toBeVisible();

    await expect(page.locator("[data-right-panel-tab-list]")).toContainText("Conversation");
    expect(errors).toEqual([]);
  });

  test("carries the header actions on a conversation", async ({ page }) => {
    const errors = watchConsole(page);
    await openLinkedConversation(page);

    const add = headerActionsTrigger(page);
    await expect(add).toBeVisible();
    await add.click();
    await expect(page.getByRole("menu").last()).toBeVisible();
    await page.keyboard.press("Escape");

    await expect(page.getByRole("button", { name: /^Open/ }).first()).toBeVisible();
    await expect(page.getByRole("button", { name: /^Toggle right panel/ })).toBeVisible();
    expect(errors).toEqual([]);
  });

  test("covers the whole chat surface with the drop overlay", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);

    const target = page.locator("[data-chat-workspace-drop-target]");
    await expect(target).toHaveCount(1);

    await page.evaluate(() => {
      const transfer = new DataTransfer();
      transfer.items.add(new File(["hello"], "notes.md", { type: "text/markdown" }));
      const node = document.querySelector("[data-chat-workspace-drop-target]");
      node.dispatchEvent(
        new DragEvent("dragenter", { bubbles: true, cancelable: true, dataTransfer: transfer }),
      );
      node.dispatchEvent(
        new DragEvent("dragover", { bubbles: true, cancelable: true, dataTransfer: transfer }),
      );
    });

    const overlay = page.locator("[data-chat-workspace-drop-overlay]");
    await expect(overlay).toBeVisible();
    await expect(overlay).toContainText("Drop files to attach");

    const overlayBox = await overlay.boundingBox();
    const targetBox = await target.boundingBox();
    expect(overlayBox.width).toBeGreaterThan(targetBox.width - 40);
    expect(overlayBox.height).toBeGreaterThan(targetBox.height - 40);

    await page.evaluate(() => window.dispatchEvent(new Event("dragend")));
    await expect(overlay).toHaveCount(0);

    // The new-chat page is a dropzone too: a file dropped before the
    // conversation exists waits as a staged chip and uploads after the create.
    await openChat(page);
    const draftTarget = page.locator("[data-chat-workspace-drop-target]");
    await expect(draftTarget).toHaveCount(1);
    await page.evaluate(() => {
      const transfer = new DataTransfer();
      transfer.items.add(new File(["brief"], "brief.md", { type: "text/markdown" }));
      const node = document.querySelector("[data-chat-workspace-drop-target]");
      node.dispatchEvent(
        new DragEvent("dragenter", { bubbles: true, cancelable: true, dataTransfer: transfer }),
      );
      node.dispatchEvent(
        new DragEvent("drop", { bubbles: true, cancelable: true, dataTransfer: transfer }),
      );
    });
    await expect(page.getByTestId("composer-attachment")).toContainText("brief.md");
    expect(errors).toEqual([]);
  });

  test("raises a refused attachment in the toast viewport", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);

    await expect(page.locator('[data-slot="toast-viewport"]')).toHaveCount(1);
    await expect(page.locator('[data-slot="toast-viewport-anchored"]')).toHaveCount(1);

    await page.evaluate(() => {
      const file = new File(["x"], "huge.txt", { type: "text/plain" });
      // Reported size only: the refusal happens before any upload, so the
      // bytes never leave the client.
      Object.defineProperty(file, "size", { value: 21 * 1024 * 1024 });
      const transfer = new DataTransfer();
      transfer.items.add(file);
      document
        .querySelector("[data-chat-workspace-drop-target]")
        .dispatchEvent(
          new DragEvent("drop", { bubbles: true, cancelable: true, dataTransfer: transfer }),
        );
    });

    const toast = page.locator('[data-slot="toast-viewport"] [data-slot="toast-title"]');
    await expect(toast).toHaveText("'huge.txt' exceeds the 20 MB attachment limit.");

    const root = page.locator('[data-slot="toast-viewport"] [data-type="error"]').first();
    await expect(root).toBeVisible();
    const dismiss = page.locator('[data-slot="toast-close"]').first();
    await expect(dismiss).toHaveAttribute("aria-label", "Dismiss notification");
    await dismiss.click();
    await expect(toast).toHaveCount(0);

    // Nothing was staged.
    await expect(page.getByTestId("composer-attachment")).toHaveCount(0);
    expect(errors).toEqual([]);
  });

  test("masks the settings scroller with the scroll fade", async ({ page }) => {
    const errors = watchConsole(page);
    await page.goto(hub.fixture.accounts.owner, { waitUntil: "domcontentloaded" });
    await page.goto(`${hub.fixture.url}/settings`, { waitUntil: "domcontentloaded" });

    const scroller = page.locator(".topbar-scroll-fade").first();
    await expect(scroller).toBeVisible();
    const mask = await scroller.evaluate((node) => {
      const style = getComputedStyle(node);
      return style.maskImage || style.webkitMaskImage;
    });
    expect(mask).toContain("gradient");
    expect(errors).toEqual([]);
  });

  test("opens the command palette on Mod+K and lands on the conversation", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);

    // From the composer, where a bare letter would be typing.
    await composer(page).focus();
    await page.keyboard.press("ControlOrMeta+k");

    const palette = page.getByTestId("command-palette");
    await expect(palette).toBeVisible();
    await expect(palette).toHaveAttribute("aria-label", "Command palette");

    await expect(palette.getByText("Actions", { exact: true })).toBeVisible();
    await expect(palette.getByText("Recent Threads", { exact: true })).toBeVisible();

    const input = palette.getByRole("combobox");
    await expect(input).toBeFocused();
    await input.fill("Lease renewal");

    const row = palette.getByText("Lease renewal under load", { exact: true });
    await expect(row).toBeVisible();
    await page.keyboard.press("Enter");

    await expect(palette).toHaveCount(0);
    // A linked conversation's destination is its issue page with the chat open
    // in the panel (§19.4) — the same rule the sidebar's rows follow.
    await expect(page).toHaveURL(new RegExp(`/work/i/${hub.fixture.work_item}`));
    await expect(page.getByTestId("conversation-surface")).toBeVisible();
    await expect(page.getByRole("heading", { level: 1 })).toContainText(
      "Renew the lease before the handoff completes",
    );
    expect(errors).toEqual([]);
  });

  // §18.10. The right panel's chord, driven from the composer where a bare
  // letter would be typing — the one place a keybinding has to beat a focused
  // editor to the keystroke, which is why the listener captures.
  test("toggles the right panel from its keybinding", async ({ page }) => {
    const errors = watchConsole(page);
    // The new-chat page: a linked conversation now lands on its issue page
    // with the conversation panel already open (decisions §19.4), so the
    // toggle's starting state is only "closed" here.
    await openChat(page);

    const toggle = page.getByRole("button", { name: /^Toggle right panel/ });
    await expect(toggle).toHaveAttribute("aria-pressed", "false");

    await composer(page).focus();
    await page.keyboard.press("ControlOrMeta+Alt+b");
    await expect(toggle).toHaveAttribute("aria-pressed", "true");

    await page.keyboard.press("ControlOrMeta+Alt+b");
    await expect(toggle).toHaveAttribute("aria-pressed", "false");
    expect(errors).toEqual([]);
  });

  test("renders a fenced code block with the chrome and Shiki tokens", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page, `/c/${hub.fixture.conversation}`);

    // Shift+Enter is a newline in the composer; Enter alone would send.
    await composer(page).focus();
    await page.keyboard.type("```go");
    await page.keyboard.press("Shift+Enter");
    await page.keyboard.type("func main() { fmt.Println(1) }");
    await page.keyboard.press("Shift+Enter");
    await page.keyboard.type("```");
    await page.keyboard.press("Enter");

    // The fixture conversation carries other fenced blocks by now — an
    // earlier test's linked issue body has a `detent-agent` one — so the
    // block is found by the language this test typed, not by position.
    const block = page.locator('.chat-markdown-codeblock[data-language="go"]').last();
    await expect(block).toBeVisible({ timeout: 15_000 });
    await expect(block.getByRole("toolbar", { name: "Code block actions" })).toBeVisible();
    await expect(block.getByRole("button", { name: "Copy code" })).toBeVisible();

    // Highlighted, not flat: Shiki splits the source into one span per token,
    // so a highlighted block has many where an unhighlighted one has none.
    await expect
      .poll(async () => block.locator("code span").count(), { timeout: 15_000 })
      .toBeGreaterThan(3);
    await expect(block).toContainText("func main()");
    expect(errors).toEqual([]);
  });

  // decisions.md §18.11 step 2. This is the whole stack in one assertion: the
  // panel requests a workspace, the hub dispatches a `detent:workspace` item,
  // the preview's runner claims it and binds, the panel learns it is ready
  // from the project event stream rather than by polling, mints a ticket,
  // opens the relay, and the tree and the file view are frames that crossed
  // it. Anything broken between those and the surface never leaves "opening".
  test("opens the Files surface on an issue and reads the worktree over the relay", async ({
    page,
  }) => {
    const errors = watchConsole(page);
    await openWorkIssue(page);

    const toggle = page.getByRole("button", { name: /^Toggle right panel/ });
    await toggle.click();
    await expect(toggle).toHaveAttribute("aria-pressed", "true");

    // The panel opens on the conversation for a linked issue (§19.3), so
    // Files is reached through the surface menu rather than the launcher.
    await page.getByRole("button", { name: "Add panel surface" }).click();
    await page.getByRole("menuitem", { name: "Files" }).click();

    const surface = page.getByTestId("files-surface");
    await expect(surface).toBeVisible();

    const tree = page.getByTestId("files-tree");
    await expect(tree).toBeVisible({ timeout: 30_000 });
    // Pierre's tree names its rows through the `treeitem` role; the visible
    // text is broken for wrapping, so the accessible name is what to match.
    await expect(tree.getByRole("treeitem", { name: "README.md" })).toBeVisible({
      timeout: 30_000,
    });
    await expect(tree.getByRole("treeitem", { name: "internal" })).toBeVisible();
    // A denied entry is listed and never read (§18.4), so the reader learns
    // the project has a .env and not what is in it.
    await expect(tree.getByRole("treeitem", { name: ".env" })).toBeVisible();

    // Opening a file is a `read` frame, and what comes back is the worktree's
    // own bytes rather than anything the client invented.
    await tree.getByRole("treeitem", { name: "README.md" }).click();
    await expect(page.getByTestId("files-view")).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId("files-view")).toContainText("Preview worktree", {
      timeout: 30_000,
    });
    // The row that was clicked is the file that opened. The root's folders are
    // filled before the first paint, so no row is inserted above README.md
    // after a click has resolved to it; when one was, this opened `main.go` —
    // the row directly above it (§18.4).
    await expect(page.getByTestId("files-view")).not.toContainText('println("preview")');

    expect(errors).toEqual([]);
  });

  /** Closes every workspace the project has open, the way the panel's own
   * close does: through the API, as the signed-in person. */
  async function closeOpenWorkspaces(page) {
    const listing = await hubAPI(page, "GET", `/projects/${hub.fixture.project_id}/workspaces`);
    expect(listing.status).toBe(200);
    const open = (listing.payload?.workspaces ?? []).filter(
      (workspace) => !["closed", "failed"].includes(workspace.state),
    );
    for (const workspace of open) {
      const closed = await page.evaluate(
        async ({ path }) => {
          const bootstrap = await (await fetch("/chat/bootstrap")).json();
          const response = await fetch(`${bootstrap.api_base}${path}`, {
            method: "DELETE",
            headers: { "X-CSRF-Token": bootstrap.csrf_token },
          });
          return response.status;
        },
        { path: `/projects/${hub.fixture.project_id}/workspaces/${workspace.id}` },
      );
      expect(closed).toBe(204);
    }
    return open.map((workspace) => workspace.id);
  }

  async function openFilesSurface(page) {
    const toggle = page.getByRole("button", { name: /^Toggle right panel/ });
    await toggle.click();
    await expect(toggle).toHaveAttribute("aria-pressed", "true");
    const launcher = page.locator("[aria-label='Open a surface']");
    const menu = page.getByRole("button", { name: "Add panel surface" });
    await expect(launcher.or(menu).first()).toBeVisible({ timeout: 30_000 });
    if (await launcher.isVisible()) {
      await launcher
        .locator("[class*='grid-cols-2'] > *")
        .filter({ hasText: "Files" })
        .getByRole("button")
        .click();
    } else {
      await menu.click();
      await page.getByRole("menuitem", { name: "Files" }).click();
    }
    await expect(page.getByTestId("files-surface")).toBeVisible({ timeout: 30_000 });
  }

  // decisions.md §18.1. The regression for the defect this change is about: a
  // preview that had been up for hours answered Files with "Workspace ·
  // requested" over skeleton rows for ever.
  //
  // The cause was on the hub side of the fixture — the workspace runner's
  // capability report rides its machine heartbeat, the preview sent exactly
  // one, and `runnerauth.HeartbeatTimeout` later the claim gate stopped
  // offering the lane anything — so the shape of the failure was that the
  // *first* workspace worked and no later one ever did. A test that only opens
  // Files once cannot see that, because the fixture pre-warms one. So this one
  // throws the pre-warmed workspace away first and then asks for another,
  // which is exactly what Michael did hours in.
  test("claims a second workspace after the pre-warmed one is closed", async ({ page }) => {
    const errors = watchConsole(page);
    await openWorkIssue(page);

    const closed = await closeOpenWorkspaces(page);
    expect(closed.length).toBeGreaterThan(0);

    // A different issue, as in the report: nothing of the closed workspace is
    // reusable for it, so the panel has to get a fresh one claimed.
    const subject = await hubAPI(
      page,
      "GET",
      `/projects/${hub.fixture.project_id}/work-items/${hub.fixture.work_item}`,
    );
    expect(subject.status).toBe(200);
    const created = await hubAPI(page, "POST", `/projects/${hub.fixture.project_id}/work-items`, {
      idempotency_key: `files-after-close-${Date.now()}`,
      title: "Files on a second issue",
      body: "Opened to prove a later workspace is still claimed.",
      state: subject.payload.state,
      labels: [],
      assignees: [],
    });
    expect([200, 201]).toContain(created.status);
    const second = created.payload.work_item_id;
    expect(second, `the new issue has an id: ${JSON.stringify(created.payload)}`).toBeTruthy();

    await page.goto(new URL(`/work/i/${second}`, hub.fixture.url).toString(), {
      waitUntil: "domcontentloaded",
    });
    await expect(page.getByTestId("issue-properties")).toBeVisible();
    await openFilesSurface(page);

    // The whole point: a workspace requested now is claimed, bound and ready,
    // and the tree is frames that crossed the relay. Before the fix this sat
    // in `requested` until the request timeout and then failed `no_runner`.
    const tree = page.getByTestId("files-tree");
    await expect(tree).toBeVisible({ timeout: 60_000 });
    await expect(tree.getByRole("treeitem", { name: "README.md" })).toBeVisible({
      timeout: 30_000,
    });

    const after = await hubAPI(page, "GET", `/projects/${hub.fixture.project_id}/workspaces`);
    const live = (after.payload?.workspaces ?? []).find(
      (workspace) => workspace.work_item_id === second,
    );
    expect(live, "the second issue has a workspace of its own").toBeTruthy();
    expect(["ready", "idle"]).toContain(live.state);
    expect(live.runner_id, "a ready workspace names the runner that claimed it").toBeTruthy();
    expect(closed).not.toContain(live.id);

    expect(errors).toEqual([]);
  });

  // decisions.md §18.1. The other half of the defect: whatever the hub is
  // doing, the surface has to say what it is waiting for.
  //
  // The wait is stubbed rather than provoked, because the real runner claims
  // in well under a second and `requested` is the state a reader only ever
  // sees when something is wrong — which is precisely when the sentence has to
  // be right. Holding the create response at `requested` is the client-side
  // equivalent of the slow runner: the id never appears in the project event
  // stream, so the surface stays where the hub last put it.
  test("says what it is waiting for while the workspace is still requested", async ({ page }) => {
    const errors = watchConsole(page);
    await openWorkIssue(page);
    await closeOpenWorkspaces(page);

    const requestedAt = new Date(Date.now() - 80_000).toISOString();
    await page.route("**/workspaces", async (route) => {
      const request = route.request();
      if (request.method() === "GET") {
        // Nothing reusable, so the panel has to request one.
        return route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: [] }),
        });
      }
      if (request.method() !== "POST") return route.fallback();
      return route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify({
          id: "ws_stubbed_requested",
          organization_id: hub.fixture.organization ?? "org_browser_preview",
          project_id: hub.fixture.project_id,
          work_item_id: hub.fixture.work_item,
          attempt_id: null,
          ref: "main",
          head_sha: null,
          runner_id: null,
          machine_id: null,
          state: "requested",
          reason: null,
          requires: ["files", "exec"],
          capabilities: null,
          isolation: null,
          worktree: null,
          read_only: false,
          idle_timeout_seconds: 1800,
          expires_at: new Date(Date.now() + 3_600_000).toISOString(),
          opened_at: null,
          last_activity_at: null,
          created_by: "user_browser_owner",
          relay_sessions: null,
          revision: 1,
          created_at: requestedAt,
          updated_at: requestedAt,
        }),
      });
    });

    await openFilesSurface(page);

    // Words, not skeleton rows: the sentence names the capability the request
    // is waiting on, and the clock says how long it has been waiting.
    const waiting = page.getByTestId("files-waiting");
    await expect(waiting).toBeVisible({ timeout: 30_000 });
    await expect(waiting).toContainText("Waiting for a runner that can serve files for this project");
    await expect(page.getByTestId("files-elapsed")).toContainText("elapsed");
    // The elapsed clock is counting from the request, not from the mount.
    await expect(page.getByTestId("files-elapsed")).toContainText(/^1m \d+s elapsed$/);
    // The skeleton belongs to `starting` alone, so there is none under this.
    await expect(page.getByTestId("files-tree")).toHaveCount(0);

    expect(errors).toEqual([]);
  });

  test("adds a project action, runs it from the top bar, and records the run", async ({ page }) => {
    const errors = watchConsole(page);
    await openWorkIssue(page);

    await headerActionsTrigger(page).click();

    await expect(page.getByRole("menuitem", { name: "Copy link" })).toBeVisible();
    await page.getByRole("menuitem", { name: "Add action" }).click();

    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText(
      "Actions are project-scoped commands you can run from the top bar or keybindings.",
    );

    // §18.7: the Browser surface is not built, so section 16 keeps the field
    // and the toggle present and disabled with the reason rather than cutting
    // them. A reader must be able to learn why, not just find it dead.
    await expect(dialog.getByLabel("Preview URL (optional)")).toBeVisible();
    const previewToggle = dialog.getByRole("switch", {
      name: /Open preview automatically when this action runs/,
    });
    await expect(previewToggle).toBeDisabled();
    // The reason has to be readable without hovering: a tooltip is only in the
    // DOM while it is open, so the sentence is also in the dialog as the
    // switch's description. Asserting the text rather than the tooltip is what
    // makes this a test of what a keyboard reader gets.
    await expect(dialog).toContainText("Browser preview is not available on Detent Cloud yet.");

    // The empty-name refusal is the dialog's own validation, not the hub's.
    await dialog.getByLabel("Command", { exact: true }).fill("echo hello");
    await dialog.getByRole("button", { name: "Save action" }).click();
    await expect(dialog).toContainText("Name is required.");

    await dialog.getByLabel("Name", { exact: true }).fill("Greet");
    await dialog.getByRole("button", { name: "Save action" }).click();
    await expect(dialog).toBeHidden({ timeout: 15_000 });

    const run = page.getByRole("button", { name: "Run Greet" });
    await expect(run).toBeVisible({ timeout: 15_000 });
    await run.click();

    // Running with no workspace open asks for one with `requires: [files,
    // exec]`; the fixture pre-warms exactly that, so the panel reuses it. The
    // output is `output` frames off the runner's own shell, crossing the relay
    // into the Output surface.
    const output = page.getByTestId("output-surface");
    await expect(output).toBeVisible({ timeout: 30_000 });
    await expect(output).toContainText("hello", { timeout: 60_000 });

    // The run is a record, not just a stream: status, and an exit code that is
    // the command's own.
    const actions = await hubAPI(page, "GET", `/projects/${hub.fixture.project_id}/actions`);
    expect(actions.status).toBe(200);
    const saved = actions.payload.items.find((item) => item.name === "Greet");
    expect(saved).toBeTruthy();
    expect(saved.command).toBe("echo hello");

    let finished;
    await expect
      .poll(
        async () => {
          const runs = await hubAPI(
            page,
            "GET",
            `/projects/${hub.fixture.project_id}/actions/${saved.id}/runs`,
          );
          finished = runs.payload?.items?.[0];
          return finished?.status ?? null;
        },
        { timeout: 60_000, message: "the action run never reached a terminal status" },
      )
      .toBe("succeeded");
    expect(finished.exit_code).toBe(0);

    // The output the panel showed is the output the hub kept, so a later
    // reader of the run sees what the runner actually printed.
    const body = await page.evaluate(async (path) => {
      const bootstrap = await (await fetch("/chat/bootstrap")).json();
      const response = await fetch(`${bootstrap.api_base}${path}`);
      return { status: response.status, text: await response.text() };
    }, `/projects/${hub.fixture.project_id}/actions/${saved.id}/runs/${finished.id}/output`);
    expect(body.status).toBe(200);
    expect(body.text).toContain("hello");

    expect(errors).toEqual([]);
  });

  // decisions.md §18.12, the headless half. The eighth dogfood run created an
  // action and a run through the API — `POST …/actions` then `POST
  // …/actions/:id/runs` — got its 202, and watched the row sit `queued` for
  // three minutes and forty seconds: the process only ever started when a
  // browser opened the exec channel for it, and the runner's lane only runs
  // actions at worktree creation. Two REST calls were not an action run.
  //
  // This test is that sequence and nothing else. No button is pressed to start
  // it: the hub hands the queued run to the runner already holding the
  // workspace, and the Output surface — opened afterwards, from the panel's own
  // menu — reads the finished run back. If either half regresses, the surface
  // is empty or the run is still `queued`, and both fail here.
  test("runs an action queued through the API alone and reads it back in the panel", async ({
    page,
  }) => {
    const errors = watchConsole(page);
    await openWorkIssue(page);

    const created = await hubAPI(page, "POST", `/projects/${hub.fixture.project_id}/actions`, {
      idempotency_key: `headless-action-${Date.now()}`,
      name: "Headless greet",
      command: "echo hello",
    });
    expect(created.status).toBe(201);
    const actionId = created.payload.id;

    // The workspace is whichever open one the hub holds on the fixture issue.
    // An earlier test in this serial file closes the pre-warmed workspace
    // and lets the lane claim a fresh one, so the id in the fixture file can
    // be stale; the point stands either way — this caller has no browser
    // doing anything on its behalf, it only names a workspace the hub has.
    const listed = await hubAPI(
      page,
      "GET",
      `/projects/${hub.fixture.project_id}/workspaces?work_item=${hub.fixture.work_item}`,
    );
    expect(listed.status).toBe(200);
    const open = listed.payload.workspaces.find(
      (candidate) => candidate.state === "ready" || candidate.state === "idle",
    );
    expect(open, "an open workspace on the fixture issue").toBeTruthy();
    const queued = await hubAPI(
      page,
      "POST",
      `/projects/${hub.fixture.project_id}/actions/${actionId}/runs`,
      { idempotency_key: `headless-run-${Date.now()}`, workspace_id: open.id },
    );
    expect(queued.status).toBe(202);
    const runId = queued.payload.run_id;

    // Nothing else happens. The hub dispatches it on its own maintenance tick.
    let finished;
    await expect
      .poll(
        async () => {
          const run = await hubAPI(
            page,
            "GET",
            `/projects/${hub.fixture.project_id}/actions/${actionId}/runs/${runId}`,
          );
          finished = run.payload;
          return finished?.status ?? null;
        },
        {
          timeout: 60_000,
          message: "the run queued through the API never left queued — nothing executed it",
        },
      )
      .toBe("succeeded");
    expect(finished.exit_code).toBe(0);

    // And a reader who opens the Output surface afterwards sees it, because the
    // surface reads the recorded runs back rather than only showing what this
    // tab started.
    const toggle = page.getByRole("button", { name: /^Toggle right panel/ });
    await toggle.click();
    await page.getByRole("button", { name: "Add panel surface" }).click();
    await page.getByRole("menuitem", { name: "Output" }).click();

    const output = page.getByTestId("output-surface");
    await expect(output).toBeVisible({ timeout: 30_000 });
    await expect(output).toContainText("hello", { timeout: 30_000 });
    // The surface's word for the hub's `succeeded` is the command's own exit,
    // because §18.12 makes "exited 0" and "never exited" different facts and
    // the chip says which this was.
    await expect(page.getByTestId("output-run-status")).toContainText("Exited 0", {
      timeout: 30_000,
    });

    expect(errors).toEqual([]);
  });
});

// decisions.md §18.13. The header's git action group and its Open picker, over
// the real thing: the preview's runner holds a real git worktree with a bare
// origin beside it, so a commit made through the header moves a branch on disk
// and a push moves the origin's ref. Nothing here is mocked — a failure
// anywhere between the split button and `git commit` fails this test rather
// than leaving a control that looks like it worked.
test.describe("the header's git action group", () => {
  const { execFileSync } = require("node:child_process");
  const fs = require("node:fs");
  const path = require("node:path");

  /** Runs git in the preview's worktree and answers its trimmed stdout. */
  function git(worktree, ...args) {
    return execFileSync("git", args, { cwd: worktree, encoding: "utf8" }).trim();
  }

  /**
   * The workspace the header acts on, read from the hub rather than from the
   * fixture file.
   *
   * Reading it from the API is the point: `worktree_path` and
   * `machine_hostname` are §18.13's own additions to the resource, and a test
   * that took the path out of the fixture's JSON would still pass if the runner
   * never reported it.
   */
  async function previewWorkspace(page) {
    const listed = await hubAPI(
      page,
      "GET",
      `/projects/${hub.fixture.project_id}/workspaces?work_item=${hub.fixture.work_item}`,
    );
    expect(listed.status).toBe(200);
    const open = listed.payload.workspaces.find(
      (candidate) => candidate.state === "ready" || candidate.state === "idle",
    );
    expect(open, "the preview pre-warms one open workspace on the fixture issue").toBeTruthy();
    return open;
  }

  /** Opens the issue page and waits for the git group to be drawn. */
  async function openIssueWithHeader(page) {
    await openWorkIssue(page);
    await expect(page.getByTestId("header-commit-push-pr")).toBeVisible();
  }

  async function disabledReason(row) {
    return row.getAttribute("data-disabled-reason");
  }

  // The whole stack in one assertion: the group asks the relay for a `status`,
  // the runner runs git in the worktree, the reader picks Commit, the runner
  // commits as the acting person, and the branch on disk moves. The denylist is
  // checked in the same commit, because an excluded path is only ever proved by
  // a commit that left it out.
  test("commits the worktree through the relay and moves the branch", async ({ page }) => {
    const errors = watchConsole(page);
    await openIssueWithHeader(page);

    const workspace = await previewWorkspace(page);
    const worktree = workspace.worktree_path;
    expect(
      worktree,
      "the runner reports the worktree it prepared on every heartbeat (§18.13)",
    ).toBeTruthy();
    expect(fs.existsSync(path.join(worktree, ".git")), `${worktree} is a git worktree`).toBe(true);

    // Dirty the worktree from outside the browser: what is under test is the
    // header, not a file editor, and §18.4 has no writes.
    fs.writeFileSync(path.join(worktree, "HEADER.md"), `header commit ${Date.now()}\n`);
    // A denied path, dirty in the same commit. §18.13 stages everything except
    // the §18.4 denylist, so this one has to survive the commit unstaged.
    fs.writeFileSync(path.join(worktree, ".env"), "TOKEN=never-committed\n");

    const before = git(worktree, "rev-parse", "HEAD");

    await page.getByTestId("header-commit-menu").click();
    const commit = page.getByTestId("header-git-commit");

    await expect(commit).toBeVisible({ timeout: 30_000 });
    expect(await disabledReason(commit), "a dirty worktree can be committed").toBeNull();
    await commit.click();

    const message = "feat(header): commit from the git action group";
    const field = page.getByRole("textbox", { name: /message/i });
    await expect(field).toBeVisible({ timeout: 15_000 });
    await field.fill(message);
    await page.getByTestId("header-git-commit-submit").click();

    // The commit is the runner's, so the wait is on the branch rather than on a
    // toast: a toast can report success for a command that failed after it.
    await expect
      .poll(() => git(worktree, "rev-parse", "HEAD"), { timeout: 60_000 })
      .not.toBe(before);

    expect(git(worktree, "log", "-1", "--format=%s")).toBe(message);

    // Author the person, committer the runner — git's own distinction, and
    // §18.13's rule about who a hosted commit belongs to.
    const [authorEmail, committerName] = git(worktree, "log", "-1", "--format=%ae%n%cn").split(
      "\n",
    );
    expect(authorEmail).toContain("@");
    expect(authorEmail, "the author is the person, not the machine").not.toContain(
      "detent.invalid",
    );
    expect(committerName).toContain("Detent runner");

    const committed = git(worktree, "show", "--name-only", "--format=", "HEAD")
      .split("\n")
      .filter((line) => line.length > 0);
    expect(committed).toContain("HEADER.md");
    expect(committed, "a denylisted path is never staged (§18.4)").not.toContain(".env");
    // And it is still dirty, which is what proves it was excluded rather than
    // never written.
    expect(git(worktree, "status", "--porcelain", "--", ".env")).not.toBe("");

    expect(errors).toEqual([]);
  });

  // The push half. It is asserted against the branch's own upstream rather than
  // against a remote path, so the test proves the origin really has the commit
  // without knowing where the fixture put the bare repository.
  test("pushes the branch to the project's remote", async ({ page }) => {
    const errors = watchConsole(page);
    await openIssueWithHeader(page);

    const workspace = await previewWorkspace(page);
    const worktree = workspace.worktree_path;
    expect(worktree).toBeTruthy();
    expect(
      git(worktree, "rev-parse", "--abbrev-ref", "HEAD"),
      "the preview worktree is on a branch, so a push has a target",
    ).not.toBe("HEAD");

    await page.getByTestId("header-commit-menu").click();
    const push = page.getByTestId("header-git-push");
    await expect(push).toBeVisible({ timeout: 30_000 });
    const reason = await disabledReason(push);
    if (reason !== null) {

      test.info().annotations.push({ type: "note", description: `push disabled: ${reason}` });
      await page.keyboard.press("Escape");
      expect(errors).toEqual([]);
      return;
    }
    await push.click();

    await expect
      .poll(() => git(worktree, "rev-list", "--count", "@{u}..HEAD"), { timeout: 60_000 })
      .toBe("0");

    expect(errors).toEqual([]);
  });

  test("labels the pull request row from the hub's own view of it", async ({ page }) => {
    const errors = watchConsole(page);
    await openIssueWithHeader(page);

    await page.getByTestId("header-commit-menu").click();
    const pullRequest = page.getByTestId("header-git-pr");
    await expect(pullRequest).toBeVisible({ timeout: 30_000 });

    await expect(page.getByTestId("header-git-commit")).toHaveText(/Commit/);
    await expect(page.getByTestId("header-git-push")).toHaveText(/Push/);
    await expect(pullRequest).toHaveText(/View PR/);

    await page.keyboard.press("Escape");
    expect(errors).toEqual([]);
  });

  // §18.13's Open picker. The preview's hub is served on loopback, so the
  // client never takes itself to be on the runner's machine and every editor
  // row is disabled naming the host the worktree is on — which is the case that
  // matters, because it is the one Detent Cloud is always in.
  test("names the machine the worktree is on in the Open picker", async ({ page }) => {
    const errors = watchConsole(page);
    await openIssueWithHeader(page);
    await previewWorkspace(page);

    await page.getByTestId("header-open-menu").click();
    await expect(page.getByRole("menu").last()).toBeVisible();

    await expect(page.getByTestId("header-open-cursor")).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId("header-open-vscode")).toBeVisible();
    const fileManager = page.getByTestId("header-open-file-manager");
    await expect(fileManager).toBeVisible();
    await expect(fileManager).toHaveText(/Finder|Explorer|Files/);

    // Every one of them refuses, and the reason names where the worktree is.
    for (const id of ["header-open-cursor", "header-open-vscode", "header-open-file-manager"]) {
      const reason = await disabledReason(page.getByTestId(id));
      expect(reason, `${id} carries the reason it is disabled (§18.13)`).toBeTruthy();
      expect(reason).toMatch(/This worktree is on |Available on the runner's machine only\./);
    }

    await page.getByTestId("header-open-cursor").hover();
    await expect(page.getByText(/This worktree is on /).first()).toBeVisible({ timeout: 10_000 });

    await page.keyboard.press("Escape");
    expect(errors).toEqual([]);
  });

  // A plain chat has no issue, so it has no worktree, and the whole group says
  // so in one sentence (§18.13).
  test("disables the whole group on a chat with no linked issue", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);

    const primary = page.getByTestId("header-commit-push-pr");
    await expect(primary).toBeVisible();
    await expect(primary).toHaveAttribute("aria-disabled", "true");
    await primary.hover();
    await expect(page.getByText("Link an issue to get a worktree.").first()).toBeVisible({
      timeout: 10_000,
    });

    expect(errors).toEqual([]);
  });
});

test.describe("the footer's update check", () => {
  /** The control, whatever state it is currently in. */
  function updateControl(page) {
    return page.getByRole("button", {
      name: /Check for updates|All runners up to date|Couldn't check for updates|behind . Update/,
    });
  }

  test("offers the check on hover and answers a press", async ({ page }) => {
    const errors = watchConsole(page);
    await openChat(page);

    const control = updateControl(page);
    await expect(control).toBeVisible();
    await control.hover();

    await expect(page.getByText("Check for updates").first()).toBeVisible({ timeout: 10_000 });

    await control.click();
    await expect(page.getByText("All runners up to date").first()).toBeVisible({
      timeout: 20_000,
    });

    expect(errors).toEqual([]);
  });

  test("shows the behind state on its own and walks to Providers & runners", async ({ page }) => {
    const errors = watchConsole(page);
    await page.route("**/app/updates", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          current: "v9.9.9",
          source: "hub",
          runners: [
            {
              runner_id: "rnr_athens",
              display_name: "Athens",
              version: "v9.9.8",
              online: true,
              behind: true,
            },
          ],
          behind_count: 1,
          client: { version: "v9.9.9", build: "abc", served_at: "2026-09-12T09:00:00Z" },
        }),
      });
    });
    await openChat(page);

    const behind = page.getByRole("button", { name: "1 runner behind · Update" });
    await expect(behind).toBeVisible({ timeout: 20_000 });
    await behind.click();

    await expect(page).toHaveURL(/\/settings\/runners$/, { timeout: 20_000 });
    expect(errors).toEqual([]);
  });
});
