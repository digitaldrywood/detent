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
  const retry = page.getByTestId("operator-approval-card").last().frameLocator("iframe");
  await expect(retry.getByText(/Blocked.*Todo/)).toBeVisible();
  expect((await projectRead(page, `work-items/${hub.fixture.work_item}`)).state).toBe("Blocked");
  await retry.getByRole("button", { name: "Confirm action" }).click();
  await expect.poll(async () => (await projectRead(page, `work-items/${hub.fixture.work_item}`)).state).toBe("Todo");
  await expect(page.getByText(/move item: succeeded/)).toBeVisible();
});
