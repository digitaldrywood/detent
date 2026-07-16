const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("chat", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test.beforeEach(async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-healthy-parallel-work",
  });
  await page.goto(`${runtime.url}/fleet`, { waitUntil: "domcontentloaded" });
  await page.getByRole("button", { name: "Chat", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "Detent chat" })).toBeVisible();
});

test("answers a board question through the fake LLM", async ({ page }) => {
  const panel = page.getByRole("dialog", { name: "Detent chat" });
  await panel.getByLabel("Message Detent").fill("What's blocked and why?");
  await panel.getByRole("button", { name: "Send message" }).click();

  await expect(panel).toContainText("Two items are blocked");
  await expect(panel).toContainText("billing-api#5280");
  await expect(panel).toContainText("workspace hook failed");
});

test("confirms before executing a proposed board mutation", async ({ page }) => {
  const panel = page.getByRole("dialog", { name: "Detent chat" });
  await panel
    .getByLabel("Message Detent")
    .fill("Move digitaldrywood/detent-core#5250 to Todo");
  await panel.getByRole("button", { name: "Send message" }).click();

  const action = panel.locator("[data-chat-action]");
  await expect(action).toContainText("Confirmation required");
  await expect(action).toContainText("Backlog to Todo");
  await expect(action).toHaveAttribute("data-chat-action-status", "pending");

  await action.getByRole("button", { name: "Confirm action" }).click();
  await expect(panel.locator('[data-chat-action-status="succeeded"]')).toContainText(
    "Executed",
  );
  await expect(panel).toContainText("Moved digitaldrywood/detent-core#5250 to Todo via chat");
});
