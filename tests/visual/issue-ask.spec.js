const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
test.beforeEach(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("issue-ask", {
    env: { DETENT_HOSTED_BROWSER_ISSUE_ASK: "1" },
  });
});
test.afterEach(async () => {
  await hub?.stop();
});

async function openIssue(page) {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/work/i/${hub.fixture.work_item}`);
  await expect(page.getByTestId("issue-body")).toBeVisible();
}
async function read(page, suffix) {
  return page.evaluate(
    async ({ project, suffix }) => {
      const bootstrap = await (await fetch("/chat/bootstrap")).json();
      const response = await fetch(
        `${bootstrap.api_base}/projects/${project}/${suffix}`,
      );
      if (!response.ok) throw new Error(`Read failed: ${response.status}`);
      return response.json();
    },
    { project: hub.fixture.project_id, suffix },
  );
}

const panel = (page) => page.getByTestId("issue-ask-panel");

test("header and keyboard open the private Ask tab", async ({ page }) => {
  await openIssue(page);
  await page.getByTestId("issue-ask-button").click();
  await expect(panel(page)).toBeVisible();
  await expect(panel(page).getByRole("textbox", { name: "Ask message", exact: true })).toBeFocused();
  await expect(panel(page).getByTestId("issue-ask-context")).toContainText(
    "body, comments, history, runs and pull request on every turn",
  );
  await expect(panel(page)).toContainText(
    "Only you see this chat.",
  );
  await page.getByRole("heading", { name: "Renew the lease before the handoff completes", exact: true, level: 1 }).click();
  await page.getByTestId("issue-ask-button").click();
  await expect(panel(page).getByRole("textbox", { name: "Ask message", exact: true })).toBeFocused();
  await page.getByTestId("issue-resources")
    .getByRole("button", { name: /^Conversation/ }).click();
  await expect(page.getByTestId("conversation-surface")).toBeVisible();
  // The shortcut applies outside the composer's editable focus.
  await page.getByRole("heading", { name: "Renew the lease before the handoff completes", exact: true, level: 1 }).click();
  await page.keyboard.press("a");
  await expect(panel(page)).toBeVisible();
  await expect(panel(page).getByRole("textbox", { name: "Ask message", exact: true })).toBeFocused();
});

for (const viewport of [{ width: 1440, height: 1100 }, { width: 390, height: 844 }]) {
  test(`Ask composer grows and sends multiline questions at ${viewport.width}px`, async ({ page }, testInfo) => {
    await page.setViewportSize(viewport);
    const title = "Renew the lease before the handoff completes while preserving the earlier questions and attached issue context across the entire conversation";
    await page.route(`**/work-items/${hub.fixture.work_item}`, async (route) => {
      const response = await route.fetch();
      await route.fulfill({ response, json: { ...await response.json(), title } });
    });
    await openIssue(page);
    await expect(page.getByTestId("issue-ask-inline")).toHaveCount(0);
    await page.getByTestId("issue-ask-button").click();
    const ask = panel(page);
    const editor = ask.getByRole("textbox", { name: "Ask message", exact: true });
    await expect(editor).toBeFocused();
    await expect(ask.getByRole("combobox", { name: "Issue chats" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Add panel surface" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Split into smaller issues", exact: true })).toHaveCount(1);
    const chip = ask.getByTestId("composer-context-attachment");
    await expect(chip).toHaveText(`#2 ${title}`);
    await expect(chip).toHaveAttribute("title", title);
    const chipLayout = await chip.locator("span").evaluate((node) => ({
      whiteSpace: getComputedStyle(node).whiteSpace,
      overflow: getComputedStyle(node).overflow,
      ellipsis: getComputedStyle(node).textOverflow,
      height: node.clientHeight,
      line: parseFloat(getComputedStyle(node).lineHeight),
      width: node.closest("[data-testid=composer-context-attachment]").getBoundingClientRect().width,
      composerWidth: node.closest("form").getBoundingClientRect().width,
      visibleWidth: node.clientWidth,
      fullWidth: node.scrollWidth,
    }));
    expect(chipLayout.whiteSpace).toBe("nowrap");
    expect(chipLayout.overflow).toBe("hidden");
    expect(chipLayout.ellipsis).toBe("ellipsis");
    expect(chipLayout.height).toBeCloseTo(chipLayout.line, 0);
    expect(chipLayout.width).toBeLessThanOrEqual(chipLayout.composerWidth);
    expect(chipLayout.fullWidth).toBeGreaterThan(chipLayout.visibleWidth);
    await expect(editor).toHaveAttribute("aria-placeholder", "Ask about #2…");
    const size = () => editor.evaluate((node) => ({ height: node.clientHeight, scroll: node.scrollHeight, line: parseFloat(getComputedStyle(node).lineHeight), overflow: getComputedStyle(node).overflowY }));
    const empty = await size();
    expect(empty.height).toBeCloseTo(empty.line * 3, 0);
    const lines = Array.from({ length: 7 }, (_, i) => `Question line ${i + 1}`).join("\n");
    await editor.fill(lines);
    const growing = await size();
    expect(growing.height).toBeGreaterThan(empty.height);
    expect(growing.scroll).toBe(growing.height);
    await editor.fill(Array.from({ length: 20 }, (_, i) => `Question line ${i + 1}`).join("\n"));
    const capped = await size();
    expect(capped.height).toBeCloseTo(capped.line * 12, 0);
    expect(capped.scroll).toBeGreaterThan(capped.height);
    expect(capped.overflow).toBe("auto");
    await editor.fill("Why is this Blocked?");
    await page.keyboard.press("Shift+Enter");
    await page.keyboard.type("Please summarize the history.");
    await expect(editor).toHaveText("Why is this Blocked?\nPlease summarize the history.", { useInnerText: true });
    await editor.evaluate((node) => {
      node.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true }));
      node.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", code: "Enter", keyCode: 13, isComposing: true, bubbles: true }));
      node.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true }));
    });
    expect((await read(page, `conversations?subject_work_item_id=${hub.fixture.work_item}`)).conversations).toHaveLength(0);
    await page.screenshot({ path: testInfo.outputPath(`issue-ask-empty-${viewport.width}.png`) });
    if (viewport.width === 390) {
      const sheet = page.getByRole("dialog", { name: /^Right panel/ });
      const bounds = await sheet.boundingBox();
      expect(bounds.y).toBe(0);
      expect(bounds.height).toBe(viewport.height);
    }
    await page.keyboard.press("Enter");
    await expect(ask.getByText(/lane history records/)).toBeVisible();
    await expect(ask.getByRole("textbox", { name: "Ask message", exact: true })).toHaveCount(1);
    await expect(ask.getByRole("button", { name: "#2 attached", exact: true })).toHaveCount(0);
    await expect(ask.getByRole("button", { name: "Split into smaller issues", exact: true })).toHaveCount(0);
    const chats = await read(page, `conversations?subject_work_item_id=${hub.fixture.work_item}`);
    const detail = await read(page, `conversations/${chats.conversations[0].id}`);
    expect(detail.messages.find((message) => message.role === "user").text).toBe("Why is this Blocked?\nPlease summarize the history.");
    await page.screenshot({ path: testInfo.outputPath(`issue-ask-thread-${viewport.width}.png`) });
    await ask.getByRole("button", { name: "New question", exact: true }).click();
    for (const name of ["Why is this Blocked?", "Summarize the history", "What is left before it can start?", "Split into smaller issues"]) {
      await expect(ask.getByRole("button", { name, exact: true })).toBeVisible();
    }
    const picker = ask.getByRole("combobox", { name: "Issue chats" });
    await expect(picker).toHaveValue("");
    await expect(picker.locator("option:checked")).toHaveText("Earlier questions");
    await expect(picker.getByRole("option", { name: "New question", exact: true })).toHaveCount(0);
    await expect(chip).toHaveText(`#2 ${title}`);
    await page.screenshot({ path: testInfo.outputPath(`issue-ask-new-question-${viewport.width}.png`) });
    await ask.getByRole("button", { name: "Summarize the history", exact: true }).click();
    await expect(ask.getByText(/lane history records/)).toBeVisible();
    await expect(ask.getByRole("button", { name: "Summarize the history", exact: true })).toHaveCount(0);
    await expect(chip).toHaveText(`#2 ${title}`);
    await expect(chip.getByRole("button")).toHaveCount(0);
    await picker.selectOption(chats.conversations[0].id);
    await expect(ask.getByText("Why is this Blocked?\nPlease summarize the history.", { exact: true })).toBeVisible();
  });
}

test("panel question creates a subject thread and citations highlight Activity", async ({
  page,
}) => {
  await openIssue(page);
  await page.getByTestId("issue-ask-button").click();
  const before = await read(
    page,
    `work-items/${hub.fixture.work_item}/comments`,
  );
  await panel(page)
    .getByRole("textbox", { name: "Ask message", exact: true })
    .fill("Why is this Blocked?");
  await page
    .getByTestId("issue-ask-panel")
    .getByRole("button", { name: "Ask", exact: true })
    .click();
  await expect(panel(page).getByText(/lane history records/)).toBeVisible();
  await page.getByRole("heading", { name: "Renew the lease before the handoff completes", exact: true, level: 1 }).click();
  await page.keyboard.press("a");
  await expect(panel(page).getByRole("textbox", { name: "Ask message", exact: true })).toBeFocused();
  const citation = panel(page).getByRole("link", { name: /Lane history/ });
  await citation.click();
  await expect(page.locator('[data-citation-highlight="true"]')).toContainText(
    "Blocked",
  );
  const chats = await read(
    page,
    `conversations?subject_work_item_id=${hub.fixture.work_item}`,
  );
  const thread = chats.conversations.find((thread) =>
    thread.title.endsWith("Why is this Blocked?"),
  );
  expect(thread.subject_work_item_id).toBe(hub.fixture.work_item);
  expect(thread.work_item_id).toBeNull();
  expect(thread.visibility).toBe("private");
  expect(
    (await read(page, `work-items/${hub.fixture.work_item}/comments`)).items,
  ).toEqual(before.items);
  await panel(page)
    .getByRole("button", { name: "Open in Chat", exact: true })
    .click();
  await expect(page).toHaveURL(new RegExp(`/chat/c/${thread.id}$`));
  await expect(
    page.getByRole("textbox", { name: "Message", exact: true }),
  ).toBeVisible();
});

test("suggestions create independent threads and Ask reopens the latest", async ({
  page,
}) => {
  await openIssue(page);
  await page.getByTestId("issue-ask-button").click();
  await page
    .getByTestId("issue-ask-panel")
    .getByRole("button", { name: "Summarize the history", exact: true })
    .click();
  await expect(panel(page).getByText(/lane history records/)).toBeVisible();
  await panel(page)
    .getByRole("button", { name: "New question", exact: true })
    .click();
  await expect(panel(page).getByRole("button", { name: "Summarize the history", exact: true })).toBeVisible();
  await panel(page).getByRole("textbox", { name: "Ask message", exact: true }).fill("What is left before it can start?");
  await page.keyboard.press("Enter");
  await expect(panel(page).getByText(/lane history records/)).toBeVisible();
  const chats = await read(
    page,
    `conversations?subject_work_item_id=${hub.fixture.work_item}`,
  );
  expect(
    chats.conversations.filter((thread) => thread.work_item_id === null).length,
  ).toBeGreaterThanOrEqual(2);
  const latest = chats.conversations.sort((a, b) =>
    b.created_at.localeCompare(a.created_at),
  )[0];
  await page.reload();
  await page.getByTestId("issue-ask-button").click();
  await expect(
    panel(page).getByRole("combobox", { name: "Issue chats" }),
  ).toHaveValue(latest.id);
  await expect(
    panel(page)
      .getByRole("combobox", { name: "Issue chats" })
      .locator("option")
      .nth(1),
  ).toHaveAttribute("value", latest.id);
});

test("posting an answer remains approval gated", async ({ page }) => {
  await openIssue(page);
  await page.getByTestId("issue-ask-button").click();
  const before = await read(
    page,
    `work-items/${hub.fixture.work_item}/comments`,
  );
  await page
    .getByTestId("issue-ask-panel")
    .getByRole("button", { name: "Why is this Blocked?", exact: true })
    .click();
  await expect(panel(page).getByText(/lane history records/)).toBeVisible();
  await panel(page)
    .getByRole("textbox", { name: "Ask message", exact: true })
    .fill("Post answer as comment");
  await page.keyboard.press("Enter");
  const approval = panel(page)
    .getByTestId("operator-action-card")
    .last();
  await expect(
    approval.getByRole("button", { name: "Approve" }),
  ).toBeVisible();
  expect(
    (await read(page, `work-items/${hub.fixture.work_item}/comments`)).items,
  ).toEqual(before.items);
  await approval.getByRole("button", { name: "Approve" }).click();
  await expect
    .poll(
      async () =>
        (await read(page, `work-items/${hub.fixture.work_item}/comments`)).items
          .length,
    )
    .toBe(before.items.length + 1);
});

test("the split suggestion proposes one batch in Ask and confirmation files its dependencies", async ({ page }, testInfo) => {
  await openIssue(page);
  await page.getByTestId("issue-ask-button").click();
  const workItems = async () => (await read(page, "work-items")).items;
  const before = await workItems();
  const splitPrompt = "Use the split-issue skill to break this issue into smaller issues that can each land on their own. Wire up the dependencies so independent pieces can run in parallel, and show me the whole split as one proposal so I can confirm it once.";
  await page.getByTestId("issue-ask-panel")
    .getByRole("button", { name: "Split into smaller issues", exact: true }).click();
  await expect(panel(page).getByTestId("operator-action-card")).toHaveCount(1);
  const approval = panel(page).getByTestId("operator-action-card");
  const proposal = approval.getByTestId("issue-split-proposal");
  await expect(proposal).toBeVisible();
  for (const title of ["1. Split storage", "2. Split API", "3. Split UI"]) {
    await expect(proposal.getByRole("heading", { name: title, exact: true })).toBeVisible();
  }
  await expect(proposal.getByRole("group", { name: "Dependency graph" }))
    .toContainText("2. Split API — Blocked by: 1. Split storage");
  await expect(approval.locator("iframe")).toHaveCount(0);
  await expect(panel(page)).not.toContainText("(empty response)");
  const threadId = await panel(page).getByRole("combobox", { name: "Issue chats" }).inputValue();
  const detail = await read(page, `conversations/${threadId}`);
  expect(detail.conversation.subject_work_item_id).toBe(hub.fixture.work_item);
  expect(detail.messages.find((message) => message.role === "user").text).toBe(splitPrompt);
  expect(await workItems()).toHaveLength(before.length);
  const action = detail.messages.find((message) => message.data.operator_action).data.operator_action.action;
  const bootstrap = await page.evaluate(async () => (await fetch("/chat/bootstrap")).json());
  const denied = await page.request.post(`${hub.fixture.url}${bootstrap.api_base}/projects/${hub.fixture.project_id}/conversations/${threadId}/actions`, {
    headers: { "X-CSRF-Token": "forged" }, data: action,
  });
  expect(denied.status()).toBe(403);
  expect(await workItems()).toHaveLength(before.length);
  await panel(page).getByTestId("operator-action-card").scrollIntoViewIfNeeded();
  await expect(proposal).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("issue-ask-split-proposal.png") });
  await approval.getByRole("button", { name: "Approve", exact: true }).click();
  await expect.poll(async () => (await workItems()).length).toBe(before.length + 3);
  const children = (await workItems()).filter((item) => item.title.startsWith("Split "));
  const storage = children.find((item) => item.title === "Split storage");
  const api = await read(page, `work-items/${children.find((item) => item.title === "Split API").work_item_id}`);
  expect(api.dependencies).toEqual([storage.work_item_id]);
  const ui = await read(page, `work-items/${children.find((item) => item.title === "Split UI").work_item_id}`);
  expect(ui.dependencies).toEqual([]);
  const parent = await read(page, `work-items/${hub.fixture.work_item}`);
  expect(new Set(parent.dependencies)).toEqual(new Set(children.map((item) => item.work_item_id)));
  await expect(panel(page).getByText(/into 3 issues: succeeded/)).toBeVisible();
  for (const path of [hub.fixture.chat, `${hub.fixture.url}/work/i/${hub.fixture.work_item}`]) {
    const response = await page.request.get(path);
    expect(response.headers()["content-security-policy"]).toContain("frame-ancestors 'none'");
  }
});
