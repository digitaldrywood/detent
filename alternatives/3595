const { test, expect } = require("@playwright/test");
const path = require("node:path");
const AxeBuilder = require("@axe-core/playwright").default;
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });
let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("ai-credits", {
    env: { DETENT_HOSTED_BROWSER_AI_CREDITS: "1" },
  });
});
test.afterAll(async () => {
  await hub?.stop();
});

async function openBilling(page, account = "owner") {
  await page.goto(hub.fixture.accounts[account], {
    waitUntil: "domcontentloaded",
  });
  await page.goto(new URL("/settings/billing", hub.fixture.url).toString());
}

test("owner sees credits, contextual failure and history, and saves auto-fund", async ({
  page,
}) => {
  await openBilling(page);
  await expect(
    page.getByRole("heading", { name: "AI credits", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Credit balance", { exact: true }).locator("../.."),
  ).toContainText("$1.250000 USD");
  await expect(
    page.getByText("Auto-fund disabled", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText(/Credit purchase ·/)).toBeVisible();
  const violations = (
    await new AxeBuilder({ page }).include("#settings-ai-credits").analyze()
  ).violations;
  expect(
    violations.filter((v) => ["serious", "critical"].includes(v.impact)),
  ).toEqual([]);
  await page.locator("#settings-ai-credits").screenshot({
    path: path.join(
      process.env.TMPDIR || process.env.TMP || process.env.TEMP,
      "ai-credits-failure-desktop.png",
    ),
  });
  await page.getByLabel("Enable auto-fund").check();
  await page.getByLabel("Top up below (USD)").fill("1.00");
  await page
    .getByLabel("Credit pack", { exact: true })
    .selectOption("price_credit");
  const saved = page.waitForResponse(
    (r) =>
      r.url().endsWith("/billing/credits/auto-fund") &&
      r.request().method() === "PUT",
  );
  await page
    .getByRole("button", { name: "Save auto-fund", exact: true })
    .click();
  expect((await saved).status()).toBe(204);
  await expect(
    page.getByText("Auto-fund disabled", { exact: true }),
  ).toHaveCount(0);
  await expect(page.getByLabel("Enable auto-fund")).toBeChecked();
  await page.screenshot({
    path: path.join(
      process.env.TMPDIR || process.env.TMP || process.env.TEMP,
      "ai-credits-desktop.png",
    ),
    fullPage: true,
  });
});

test("credit checkout opens Stripe without crediting a purchase", async ({
  page,
}) => {
  await openBilling(page);
  await page.route("https://checkout.stripe.com/**", (route) =>
    route.fulfill({
      contentType: "text/html",
      body: "<h1>Stripe checkout fixture</h1>",
    }),
  );
  await page.getByRole("button", { name: "Buy credits", exact: true }).click();
  await expect(page).toHaveURL("https://checkout.stripe.com/c/pay/test_credit");
  await openBilling(page);
  await page.goto(
    new URL("/settings/billing?credits=returned", hub.fixture.url).toString(),
  );
  await expect(
    page.getByText("Credit checkout returned", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Credit balance", { exact: true }).locator("../.."),
  ).toContainText("$1.250000 USD");
});

test("viewer cannot access credit billing", async ({ page }) => {
  await openBilling(page, "viewer");
  await expect(
    page.getByRole("heading", { name: "AI credits", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Billing", exact: true }),
  ).toHaveCount(0);
  const denied = await page.request.get(
    new URL(
      "/api/v2/organizations/org_browser_preview/billing",
      hub.fixture.url,
    ).toString(),
  );
  expect(denied.status()).toBe(403);
});
