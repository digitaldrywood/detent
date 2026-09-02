const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

const storageKey = "detent-ai-debug-privacy-warning-dismissed";
const prompt = "Detent AI Debug prompt\nprivate fleet detail\n";
let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("ai-debug-privacy", [
    "--demo",
    "screenshots",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

async function openHealth(page, context, options = {}) {
  await context.grantPermissions(["clipboard-read", "clipboard-write"], {
    origin: runtime.url,
  });
  if (options.throwingStorage) {
    await page.addInitScript(() => {
      Storage.prototype.getItem = function () {
        throw new Error("localStorage unavailable");
      };
      Storage.prototype.setItem = function () {
        throw new Error("localStorage unavailable");
      };
    });
  }
  const requests = { count: 0 };
  await page.route("**/api/v1/ai-debug?scope=fleet", async (route) => {
    requests.count += 1;
    await route.fulfill({
      status: 200,
      contentType: "text/plain",
      body: prompt,
    });
  });
  await page.goto(runtime.url + "/health/ui", {
    waitUntil: "domcontentloaded",
  });
  return requests;
}

test("gates the first request and cancel or Escape restores focus", async ({
  context,
  page,
}) => {
  const requests = await openHealth(page, context);
  const button = page.getByRole("button", { name: "Copy AI Debug prompt" });
  const dialog = page.getByRole("alertdialog", {
    name: "Copy private fleet details?",
  });
  const cancel = dialog.getByRole("button", { name: "Cancel" });

  await button.click();
  await expect(dialog).toBeVisible();
  await expect(cancel).toBeFocused();
  expect(requests.count).toBe(0);
  const dialogBox = await dialog.boundingBox();
  const viewport = page.viewportSize();
  expect(dialogBox).not.toBeNull();
  expect(viewport).not.toBeNull();
  expect(
    Math.abs(dialogBox.x + dialogBox.width / 2 - viewport.width / 2),
  ).toBeLessThan(2);
  expect(
    Math.abs(dialogBox.y + dialogBox.height / 2 - viewport.height / 2),
  ).toBeLessThan(2);
  expect(await dialog.evaluate((element) => !element.closest("#snapshot"))).toBe(
    true,
  );

  await dialog.getByRole("checkbox").check();
  await cancel.click();
  await expect(dialog).toBeHidden();
  await expect(button).toBeFocused();
  expect(requests.count).toBe(0);
  expect(await page.evaluate((key) => localStorage.getItem(key), storageKey)).toBe(
    null,
  );

  await button.click();
  await expect(dialog).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  await expect(button).toBeFocused();
  expect(requests.count).toBe(0);
});

test("confirms and copies the existing prompt path", async ({ context, page }) => {
  const requests = await openHealth(page, context);
  const button = page.getByRole("button", { name: "Copy AI Debug prompt" });
  const dialog = page.getByRole("alertdialog", {
    name: "Copy private fleet details?",
  });

  await button.click();
  await expect(dialog).toBeVisible();
  expect(requests.count).toBe(0);
  await dialog.getByRole("button", { name: "Copy prompt" }).click();

  await expect(dialog).toBeHidden();
  await expect(button).toBeFocused();
  await expect.poll(() => requests.count).toBe(1);
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe(prompt);
});

test("persists the opt-out across reload", async ({ context, page }) => {
  const requests = await openHealth(page, context);
  let button = page.getByRole("button", { name: "Copy AI Debug prompt" });
  const dialog = page.getByRole("alertdialog", {
    name: "Copy private fleet details?",
  });

  await button.click();
  await dialog.getByRole("checkbox").check();
  await dialog.getByRole("button", { name: "Copy prompt" }).click();
  await expect.poll(() => requests.count).toBe(1);
  expect(await page.evaluate((key) => localStorage.getItem(key), storageKey)).toBe(
    "true",
  );

  await page.reload({ waitUntil: "domcontentloaded" });
  button = page.getByRole("button", { name: "Copy AI Debug prompt" });
  await button.click();
  await expect.poll(() => requests.count).toBe(2);
  await expect(dialog).toBeHidden();
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe(prompt);
});

test("shows the dialog when localStorage throws", async ({ context, page }) => {
  const requests = await openHealth(page, context, { throwingStorage: true });

  await page.getByRole("button", { name: "Copy AI Debug prompt" }).click();
  await expect(
    page.getByRole("alertdialog", { name: "Copy private fleet details?" }),
  ).toBeVisible();
  expect(requests.count).toBe(0);
});
