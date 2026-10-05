const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("conversation-actions", {
    env: { DETENT_HOSTED_BROWSER_CHAT_ACTIONS: "1" },
  });
});

test.afterAll(async () => {
  await hub?.stop();
});

async function projectRead(page, suffix) {
  return page.evaluate(async ({ project, suffix }) => {
    const bootstrap = await (await fetch("/chat/bootstrap")).json();
    const response = await fetch(`${bootstrap.api_base}/projects/${project}/${suffix}`);
    if (!response.ok) throw new Error(`Project read failed: ${response.status}`);
    return response.json();
  }, { project: hub.fixture.project_id, suffix });
}

async function askLuna(page, text) {
  await page.getByRole("textbox", { name: "Message" }).focus();
  await page.keyboard.type(text);
  await page.keyboard.press("Enter");
}

test("unavailable Hub PR mode is refused and the owner can retry a blocked issue", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.chat}/p/${hub.fixture.project_id}`);
  expect((await projectRead(page, "integration")).repository_enabled).toBe(false);
  expect((await projectRead(page, `work-items/${hub.fixture.work_item}`)).state).toBe("Blocked");

  await askLuna(page, "Can you enable the GitHub pull request mode?");
  await expect(page.getByText(/Hub GitHub integration is unavailable/).first()).toBeVisible();
  await expect(page.getByTestId("operator-approval-card")).toHaveCount(0);
  expect((await projectRead(page, "integration")).repository_enabled).toBe(false);

  await askLuna(page, "Please retry the blocked issue.");
  await expect(page.getByTestId("operator-approval-card")).toHaveCount(1);
  const retry = page.getByTestId("operator-approval-card").last();
  await expect(retry.getByText(/Blocked.*Todo/)).toBeVisible();
  expect((await projectRead(page, `work-items/${hub.fixture.work_item}`)).state).toBe("Blocked");
  await retry.getByRole("button", { name: "Approve" }).click();
  await expect.poll(async () => (await projectRead(page, `work-items/${hub.fixture.work_item}`)).state).toBe("Todo");
  await expect(page.getByText(/move item: succeeded/)).toBeVisible();
});

test("an issue split is reviewed once, cancelled without filing, and confirmed atomically", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.chat}/p/${hub.fixture.project_id}`);
  const workItems = async () => (await projectRead(page, "work-items")).items;
  const initial = await workItems();

  await askLuna(page, "Propose a cyclic split.");
  await expect(page.getByText(/Dependencies cannot form a cycle/).first()).toBeVisible();
  await expect(page.getByTestId("operator-approval-card")).toHaveCount(0);
  expect((await workItems()).length).toBe(initial.length);

  await askLuna(page, "Split this issue into three children.");
  const cancelled = page.getByTestId("operator-approval-card").last();
  await expect(cancelled.getByTestId("issue-split-proposal")).toBeVisible();
  await expect(cancelled.getByRole("heading", { name: "1. Split storage", exact: true })).toBeVisible();
  await expect(cancelled.getByRole("heading", { name: "2. Split API", exact: true })).toBeVisible();
  await expect(cancelled.getByRole("heading", { name: "3. Split UI", exact: true })).toBeVisible();
  await expect(cancelled.getByRole("group", { name: "Dependency graph" })).toContainText("2. Split API → blocked by → 1. Split storage");
  expect((await workItems()).length).toBe(initial.length);
  await cancelled.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByText(/rejected/).first()).toBeVisible();
  expect((await workItems()).length).toBe(initial.length);

  await askLuna(page, "Split this issue now, with the same three children.");
  const confirmed = page.getByTestId("operator-approval-card").last();
  await expect(confirmed.getByTestId("issue-split-proposal").last()).toBeVisible();
  await confirmed.getByRole("button", { name: "Approve", exact: true }).click();
  await expect.poll(async () => (await workItems()).length).toBe(initial.length + 3);
  const children = (await workItems()).filter((item) => item.title.startsWith("Split "));
  const storage = children.find((item) => item.title === "Split storage");
  const api = await projectRead(page, `work-items/${children.find((item) => item.title === "Split API").work_item_id}`);
  expect(api.state).toBe("Todo");
  expect(api.dependencies).toEqual([storage.work_item_id]);
  const parent = await projectRead(page, `work-items/${hub.fixture.work_item}`);
  expect(new Set(parent.dependencies)).toEqual(new Set(children.map((item) => item.work_item_id)));
  const comments = await projectRead(page, `work-items/${hub.fixture.work_item}/comments`);
  const comment = comments.items.find((item) => item.body.includes("Approved issue split:"));
  for (const child of children) expect(comment.body).toContain(child.work_item_id);
});


test("five issues are archived with one confirmation and cancellation preserves the set", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.chat}/p/${hub.fixture.project_id}`);
  const active = async () => (await projectRead(page, "work-items")).items;
  const initial = await active();
  const targets = initial.filter((item) => /^Seed [0-4]$/.test(item.title));
  expect(targets).toHaveLength(5);

  await askLuna(page, "Archive the running issue too.");
  await expect(page.getByText(/Finish or stop the running issue before archiving/).first()).toBeVisible();
  await expect(page.getByTestId("operator-approval-card")).toHaveCount(0);
  expect(await active()).toHaveLength(initial.length);

  await askLuna(page, "Archive the test issues.");
  const cancelled = page.getByTestId("operator-approval-card").last();
  const preview = cancelled.getByTestId("issue-archive-proposal");
  await expect(preview).toBeVisible();
  await expect(preview.getByRole("listitem")).toHaveCount(5);
  for (const item of targets) {
    await expect(preview).toContainText(`#${item.number}: ${item.title}`);
  }
  await expect(preview).toContainText("They can be restored.");
  expect(await active()).toHaveLength(initial.length);
  await cancelled.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByText(/Archive 5 issues: rejected/).first()).toBeVisible();
  expect(await active()).toHaveLength(initial.length);
  expect((await projectRead(page, "work-items?archived=true")).items).toHaveLength(0);

  await askLuna(page, "Archive the test issues now.");
  const confirmed = page.getByTestId("operator-approval-card").last();
  await expect(confirmed.getByTestId("issue-archive-proposal").last()).toBeVisible();
  await confirmed.getByRole("button", { name: "Approve", exact: true }).click();
  await expect.poll(async () => (await active()).length).toBe(initial.length - 5);
  await expect(page.getByText(/Archive 5 issues: succeeded/).first()).toBeVisible();
  const archived = (await projectRead(page, "work-items?archived=true")).items;
  expect(new Set(archived.map((item) => item.work_item_id))).toEqual(new Set(targets.map((item) => item.work_item_id)));
  for (const item of archived) expect(item.archived).toBe(true);

  await page.goto(new URL(`/work?project=${hub.fixture.project_id}`, hub.fixture.url).toString());
  await expect(page.getByTestId("work-board")).toBeVisible();
  for (const item of targets) await expect(page.getByRole("button", { name: item.title, exact: true })).toHaveCount(0);
  await page.getByTestId("work-archived").click();
  for (const item of targets) await expect(page.getByRole("button", { name: item.title, exact: true })).toBeVisible();
});
