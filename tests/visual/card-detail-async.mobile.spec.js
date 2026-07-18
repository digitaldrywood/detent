const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });
test.skip(({ isMobile }) => !isMobile, "mobile project only");

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("card-detail-async-mobile", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("tap paints a full-width card shell while details are pending", async ({
  page,
}, testInfo) => {
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  let releaseCard;
  const cardGate = new Promise((resolve) => {
    releaseCard = resolve;
  });
  await page.route("**/api/v1/board/card?**", async (route) => {
    await cardGate;
    await route.continue();
  });

  const card = page.locator("#board-lanes article[id^='card-']").first();
  const title = (await card.locator("[data-board-card-title]").textContent()).trim();
  await card.tap();

  const sheet = page.locator("[data-detail-sheet]");
  await expect(sheet).toBeVisible();
  await expect(sheet).toHaveAttribute("data-detail-sheet-immediate", "");
  await expect(sheet.getByRole("heading", { level: 2 })).toHaveText(title);
  const box = await sheet.locator('[role="dialog"]').boundingBox();
  expect(box.x).toBe(0);
  expect(box.width).toBe(390);
  await testInfo.attach("card-detail-pending-mobile.png", {
    body: await page.screenshot({ animations: "disabled", caret: "hide" }),
    contentType: "image/png",
  });

  releaseCard();
  await expect(sheet).not.toHaveAttribute("data-detail-sheet-immediate", "");
});
