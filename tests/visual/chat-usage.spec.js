const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("chat-usage", { env: { DETENT_HOSTED_BROWSER_CHAT_USAGE: "1" } });
});
test.afterAll(async () => { await hub?.stop(); });

test("billing shows durable AI usage for the current period", async ({ page }) => {
  const errors = [];
  page.on("pageerror", error => errors.push(String(error)));
  page.on("console", message => { if (message.type() === "error") errors.push(message.text()); });
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/billing", hub.fixture.url).toString());
  await expect(page.getByText("AI usage this billing period", { exact: true })).toBeVisible();
  await expect(page.getByText("1,100,000 tokens · $0.114000 USD", { exact: true })).toBeVisible();
  const response = await page.request.get(new URL("/api/v2/organizations/org_browser_preview/billing", hub.fixture.url).toString());
  expect(response.status()).toBe(200);
  const billing = await response.json();
  expect(billing.chat_usage.tokens).toBe(1_100_000);
  expect(billing.chat_usage.cost_usd).toBeCloseTo(.114, 8);
  expect(billing.chat_usage.turns).toBe(1);
  await page.screenshot({ path: "tmp/playwright-evidence/chat-usage/billing.png" });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByText("AI usage this billing period", { exact: true })).toBeVisible();
  await expect(page.getByText("1,100,000 tokens · $0.114000 USD", { exact: true })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: "tmp/playwright-evidence/chat-usage/billing-mobile.png" });
  expect(errors).toEqual([]);
});
