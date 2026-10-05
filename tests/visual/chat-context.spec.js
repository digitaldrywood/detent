const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
test.beforeEach(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("chat-context", {
    env: { DETENT_HOSTED_BROWSER_ISSUE_ASK: "1" },
  });
});
test.afterEach(async () => { await hub?.stop(); });

async function openIssue(page) {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/work/i/${hub.fixture.work_item}`);
  await expect(page.getByTestId("issue-body")).toBeVisible();
}

async function conversation(page) {
  const id = new URL(page.url()).pathname.split("/").at(-1);
  return page.evaluate(async ({ project, id }) => {
    const bootstrap = await (await fetch("/chat/bootstrap")).json();
    const response = await fetch(`${bootstrap.api_base}/projects/${project}/conversations/${id}`);
    if (!response.ok) throw new Error(`Read failed: ${response.status}`);
    return response.json();
  }, { project: hub.fixture.project_id, id });
}

async function sidebarChat(page) {
  await page.getByRole("button", { name: "Chat", exact: true }).click({ timeout: 10_000 });
  await expect(page.getByRole("textbox", { name: "Message", exact: true })).toBeVisible();
}

async function send(page, label = "Message") {
  await page.getByRole("textbox", { name: label, exact: true }).fill("Why is this Blocked?");
  await page.keyboard.press("Enter");
}

for (const width of [1440, 390]) {
  test(`sidebar Chat attaches the viewed issue and uses its context at ${width}px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: width === 390 ? 844 : 1100 });
    await openIssue(page);
    if (width === 390) await page.getByRole("button", { name: "Toggle main sidebar", exact: true }).click();
    await sidebarChat(page);
    await expect(page.getByTestId("composer-context-attachment")).toHaveText("#2");
    await expect(page.getByRole("button", { name: "Remove #2 context" })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath(`chat-context-${width}.png`) });
    await send(page);
    await expect(page.getByText(/lane history records/)).toBeVisible();
    const detail = await conversation(page);
    expect(detail.conversation.project_id).toBe(hub.fixture.project_id);
    expect(detail.conversation.subject_work_item_id).toBe(hub.fixture.work_item);
    expect(detail.conversation.work_item_id).toBeNull();
    expect(detail.messages.find((message) => message.role === "user").text).toBe("Why is this Blocked?");
    await expect(page.getByRole("button", { name: "Remove #2 context" })).toHaveCount(0);
    await send(page);
    await expect(page.getByText(/lane history records/)).toHaveCount(2);
    await page.screenshot({ path: testInfo.outputPath(`chat-context-answer-${width}.png`) });
  });
}

for (const surface of ["Chat", "Ask"]) {
  test(`removing the issue chip in ${surface} creates a project-wide chat`, async ({ page }) => {
    await openIssue(page);
    if (surface === "Chat") await sidebarChat(page);
    else await page.getByTestId("issue-ask-button").click();
    await page.getByRole("button", { name: "Remove #2 context" }).click();
    await expect(page.getByTestId("composer-context-attachment")).toHaveCount(0);
    await send(page, surface === "Ask" ? "Ask message" : "Message");
    await expect(page.getByText("This is a project-wide chat.", { exact: true })).toBeVisible();
    const detail = await conversation(page);
    expect(detail.conversation.subject_work_item_id ?? null).toBeNull();
    expect(detail.conversation.project_id).toBe(hub.fixture.project_id);
  });
}

for (const remove of [false, true]) {
  test(`a project page attaches the project${remove ? " and lets it be removed" : ""}`, async ({ page }) => {
    await page.goto(hub.fixture.accounts.owner);
    await page.goto(`${hub.fixture.url}/work/p/${hub.fixture.project_id}`);
    await expect(page.getByTestId("work-board")).toBeVisible();
    await sidebarChat(page);
    const chip = page.getByTestId("composer-context-attachment");
    await expect(chip).toBeVisible();
    const projectName = await chip.locator("span").innerText();
    expect(projectName).not.toMatch(/^#/);
    if (remove) {
      await chip.getByRole("button", { name: `Remove ${projectName} context` }).click();
      await expect(chip).toHaveCount(0);
    }
    await send(page);
    await expect(page.getByText("This is a project-wide chat.", { exact: true })).toBeVisible();
    const detail = await conversation(page);
    expect(detail.conversation.subject_work_item_id ?? null).toBeNull();
    expect(detail.conversation.project_id).toBe(hub.fixture.project_id);
  });
}

test("choosing another project replaces the issue attachment for subsequent drafts", async ({ page }) => {
  await openIssue(page);
  await sidebarChat(page);
  await expect(page.getByTestId("composer-context-attachment")).toHaveText("#2");
  await page.getByRole("button", { name: "Change project", exact: true }).click();
  const choice = page.locator('[role="menuitemradio"][aria-checked="false"]').first();
  const name = (await choice.innerText()).trim();
  await choice.click();
  await expect(page.getByTestId("composer-context-attachment")).toHaveText(name);
  await sidebarChat(page);
  await expect(page.getByTestId("composer-context-attachment")).toHaveText(name);
  await expect(page.getByRole("button", { name: "Remove #2 context" })).toHaveCount(0);
});
