const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("kanban-action-success", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("moving a detail-sheet card completes without uncaught errors", async ({
  page,
}) => {
  const uncaughtErrors = [];
  page.on("pageerror", (error) => uncaughtErrors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error") uncaughtErrors.push(message.text());
  });

  await page.goto(`${runtime.url}/projects/demo-project/kanban`, {
    waitUntil: "domcontentloaded",
  });
  await page.getByRole("button", { name: /Kanban demo backlog intake/ }).click();

  const sheet = page.locator("[data-detail-sheet]");
  await expect(sheet).toBeVisible();
  await sheet.getByRole("button", { name: /^Move$/ }).click();

  const moveDialog = page
    .getByRole("dialog")
    .filter({ hasText: "Move card" })
    .first();
  await moveDialog.getByLabel("Target state").selectOption("Todo");
  await moveDialog.getByRole("button", { name: /^Move$/ }).click();

  await expect(
    page
      .locator('[data-board-lane="todo"]')
      .getByRole("button", { name: /Kanban demo backlog intake/ }),
  ).toBeVisible();
  await expect(page.getByText("Moved card to Todo.")).toBeVisible();
  await expect(page.locator("[data-detail-sheet]")).toHaveCount(0);
  await expect(page.locator("#kanban-feedback")).toHaveCount(1);

  await page
    .locator('[data-board-lane="todo"]')
    .getByRole("button", { name: /Kanban demo backlog intake/ })
    .click();
  const commentSheet = page.locator("[data-detail-sheet]");
  await commentSheet.getByRole("button", { name: /Comment on issue/ }).click();
  const commentDialog = page
    .getByRole("dialog")
    .filter({ hasText: "Comment on issue" })
    .first();
  await commentDialog.getByLabel("Comment").fill("Playwright success comment");
  await commentDialog.getByRole("button", { name: /^Comment$/ }).click();
  await expect(page.locator("[data-detail-sheet]")).toHaveCount(0);
  await expect(page.locator("#kanban-feedback")).toHaveCount(1);

  await page
    .locator('[data-board-lane="todo"]')
    .getByRole("button", { name: /Kanban demo backlog intake/ })
    .click();
  const removeSheet = page.locator("[data-detail-sheet]");
  page.once("dialog", async (dialog) => dialog.accept());
  await removeSheet.getByRole("button", { name: /^Remove$/ }).click();
  await expect(
    page.getByRole("button", { name: /Kanban demo backlog intake/ }),
  ).toHaveCount(0);
  await expect(page.locator("[data-detail-sheet]")).toHaveCount(0);
  await expect(page.locator("#kanban-feedback")).toHaveCount(1);
  expect(uncaughtErrors).toEqual([]);
});
