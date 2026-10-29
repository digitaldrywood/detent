const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
async function startHub(env) {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("chat-usage", { env });
}
test.afterEach(async () => { await hub?.stop(); });

test("billing shows durable AI usage for the current period", async ({ page }) => {
  await startHub({ DETENT_HOSTED_BROWSER_CHAT_USAGE: "1", DETENT_HOSTED_BROWSER_AI_CREDITS: "1" });
  const errors = [];
  page.on("pageerror", error => errors.push(String(error)));
  page.on("console", message => { if (message.type() === "error") errors.push(message.text()); });
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/billing", hub.fixture.url).toString());
  await expect(page.getByRole("heading", { name: "AI credits", exact: true })).toBeVisible();
  await expect(page.getByText("1.1M", { exact: true })).toBeVisible();
  await expect(page.getByText("Tokens · 1 turns", { exact: true })).toBeVisible();
  const response = await page.request.get(new URL("/api/v2/organizations/org_browser_preview/billing", hub.fixture.url).toString());
  expect(response.status()).toBe(200);
  const billing = await response.json();
  await expect(page.getByText("$0/mo", { exact: true })).toBeVisible();
  await expect(page.getByText("Complimentary", { exact: true })).toBeVisible();
  expect(billing.chat_usage.tokens).toBe(1_100_000);
  expect(billing.chat_usage.cost_usd).toBeCloseTo(.114, 8);
  expect(billing.chat_usage.turns).toBe(1);
  await page.screenshot({ path: "tmp/playwright-evidence/chat-usage/billing.png" });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByRole("heading", { name: "AI credits", exact: true })).toBeVisible();
  await expect(page.getByText("1.1M", { exact: true })).toBeVisible();
  await expect(page.getByText("Tokens · 1 turns", { exact: true })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: "tmp/playwright-evidence/chat-usage/billing-mobile.png" });
  expect(errors).toEqual([]);
});

test("only the owner can start the approved test checkout", async ({ page }) => {
  await startHub({ DETENT_HOSTED_BROWSER_CAPACITY: "1" });
  await page.goto(hub.fixture.accounts.viewer);
  await page.goto(new URL("/settings/billing", hub.fixture.url).toString());
  await expect(page.getByRole("heading", { name: "General", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Upgrade", exact: true })).toHaveCount(0);
  const bootstrap = await (await page.request.get(new URL("/app/bootstrap", hub.fixture.url).toString())).json();
  const denied = await page.request.post(new URL("/api/v2/organizations/org_browser_preview/billing/checkout", hub.fixture.url).toString(), { headers: { "X-CSRF-Token": bootstrap.csrf_token }, data: { price: "price_starter_test", idempotency_key: "viewer-capacity" } });
  expect(denied.status()).toBe(403);
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/billing", hub.fixture.url).toString());
  const billing = await (await page.request.get(new URL("/api/v2/organizations/org_browser_preview/billing", hub.fixture.url).toString())).json();
  expect(billing.entitlement.name).toBe("Starter");
  expect(billing.entitlement.monthly_usd_cents).toBe(4900);
  await expect(page.getByText("Starter", { exact: true })).toBeVisible();
  await page.getByRole("link", { name: "Change plan →", exact: true }).click();
  await expect(page.getByRole("button", { name: "Upgrade", exact: true })).toBeVisible();
  await expect(page.getByRole("meter", { name: "Concurrent work", exact: true })).toHaveAttribute("aria-valuenow", "0");
  await expect(page.getByRole("meter", { name: "Concurrent work", exact: true })).toHaveAttribute("aria-valuemax", "8");
  await expect(page.getByRole("meter", { name: "Open issues", exact: true })).toHaveAttribute("aria-valuenow", "2");
  await expect(page.getByRole("meter", { name: "Open issues", exact: true })).toHaveAttribute("aria-valuemax", "10000");
  const owner = await (await page.request.get(new URL("/app/bootstrap", hub.fixture.url).toString())).json();
  const checkout = await page.request.post(new URL("/api/v2/organizations/org_browser_preview/billing/checkout", hub.fixture.url).toString(), { headers: { "X-CSRF-Token": owner.csrf_token }, data: { price: "price_starter_test", idempotency_key: "owner-capacity" } });
  expect(checkout.status()).toBe(200);
  expect((await checkout.json()).url).toContain("/pay/test_fixture");
});
