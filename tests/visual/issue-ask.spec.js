const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("issue-ask", {
    env: { DETENT_HOSTED_BROWSER_ISSUE_ASK: "1" },
  });
});
test.afterAll(async () => {
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
  await expect(panel(page).getByTestId("issue-ask-context")).toContainText(
    "Lane history",
  );
  await expect(panel(page)).toContainText(
    "Private · Never posted to the issue",
  );
  await page.getByTestId("issue-resources")
    .getByRole("button", { name: /^Conversation/ }).click();
  await expect(page.getByTestId("conversation-surface")).toBeVisible();
  // The shortcut applies outside the composer's editable focus.
  await page.getByRole("heading", { name: "Renew the lease before the handoff completes", exact: true, level: 1 }).click();
  await page.keyboard.press("a");
  await expect(panel(page)).toBeVisible();
});

test("inline question creates a subject thread and citations highlight Activity", async ({
  page,
}) => {
  await openIssue(page);
  const before = await read(
    page,
    `work-items/${hub.fixture.work_item}/comments`,
  );
  await page
    .getByRole("textbox", { name: "Ask about this issue", exact: true })
    .fill("Why is this Blocked?");
  await page
    .getByTestId("issue-ask-inline")
    .getByRole("button", { name: "Ask", exact: true })
    .click();
  await expect(panel(page).getByText(/lane history records/)).toBeVisible();
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
  await page
    .getByTestId("issue-ask-inline")
    .getByRole("button", { name: "Summarize the history", exact: true })
    .click();
  await expect(panel(page).getByText(/lane history records/)).toBeVisible();
  await panel(page)
    .getByRole("button", { name: "New chat", exact: true })
    .click();
  await panel(page)
    .getByRole("button", {
      name: "What is left before it can start?",
      exact: true,
    })
    .click();
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
  const before = await read(
    page,
    `work-items/${hub.fixture.work_item}/comments`,
  );
  await page
    .getByTestId("issue-ask-inline")
    .getByRole("button", { name: "Why is this Blocked?", exact: true })
    .click();
  await expect(panel(page).getByText(/lane history records/)).toBeVisible();
  await panel(page)
    .getByRole("textbox", { name: "Message", exact: true })
    .fill("Post answer as comment");
  await page.keyboard.press("Enter");
  const approval = panel(page)
    .getByTestId("operator-approval-card")
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

test("a split is approved inside Ask with its children and dependencies", async ({ page }) => {
  await openIssue(page);
  const initial = (await read(page, "work-items")).items;
  await page.getByRole("textbox", { name: "Ask about this issue", exact: true }).fill("Split this issue");
  await page.getByTestId("issue-ask-inline").getByRole("button", { name: "Ask", exact: true }).click();
  const approval = panel(page).getByTestId("operator-approval-card").last();
  await expect(approval.getByRole("heading", { name: "1. Ask split storage", exact: true })).toBeVisible();
  await expect(approval.getByRole("heading", { name: "2. Ask split API", exact: true })).toBeVisible();
  await expect(approval.getByRole("group", { name: "Dependency graph" })).toContainText("2. Ask split API → blocked by → 1. Ask split storage");
  await expect(approval.locator("iframe")).toHaveCount(0);
  await expect(panel(page)).not.toContainText("(empty response)");
  expect((await read(page, "work-items")).items).toHaveLength(initial.length);

  const approvalURL = await page.evaluate(() => performance.getEntriesByType("resource").map((entry) => entry.name).find((name) => name.includes("/chat/approval?")));
  expect(approvalURL).toBeTruthy();
  const preview = await page.request.get(approvalURL, { headers: { Accept: "application/json" } });
  expect(preview.headers()["content-security-policy"]).toContain("frame-ancestors 'none'");
  const data = await preview.json();
  const denied = await page.request.post(approvalURL, { headers: { Accept: "application/json" }, form: {
    csrf: data.csrf, connection_id: data.connection_id, action_id: data.actions[0].id, form_token: "forged", decision: "confirm",
  } });
  expect(denied.status()).toBe(403);
  expect((await read(page, "work-items")).items).toHaveLength(initial.length);

  await approval.getByRole("button", { name: "Approve", exact: true }).click();
  await expect.poll(async () => (await read(page, "work-items")).items.length).toBe(initial.length + 2);
  await expect(approval.getByText("Executed", { exact: true })).toBeVisible();
  await expect(panel(page).getByText(/Split #.*into 2 issues: succeeded/)).toBeVisible();
  const children = (await read(page, "work-items")).items.filter((item) => item.title.startsWith("Ask split "));
  const storage = children.find((item) => item.title === "Ask split storage");
  const api = await read(page, `work-items/${children.find((item) => item.title === "Ask split API").work_item_id}`);
  expect(api.dependencies).toEqual([storage.work_item_id]);
  const parent = await read(page, `work-items/${hub.fixture.work_item}`);
  expect(parent.dependencies).toContain(api.work_item_id);
  for (const path of [hub.fixture.chat, `${hub.fixture.url}/work/i/${hub.fixture.work_item}`]) {
    const response = await page.request.get(path);
    expect(response.headers()["content-security-policy"]).toContain("frame-ancestors 'none'");
  }
});
