const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");
const { startFakeSMTP } = require("./fake-smtp");

const allowedEmail = "operator@example.com";

test("magic-link login gates dashboard, API, SSE, and mobile views", async ({ browser }) => {
  const smtp = await startFakeSMTP();
  const runtime = await startDetentRuntime("magic-link-auth", [
    "--db",
    "detent.db",
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
    "--auth-email",
    allowedEmail,
    "--auth-smtp",
    smtp.address,
    "--auth-from",
    "detent@example.com",
  ]);
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    const boardPath = "/projects/demo-project/kanban";
    const gatedAPI = await context.request.get(`${runtime.url}/api/v1/state`, { maxRedirects: 0 });
    expect(gatedAPI.status()).toBe(303);
    expect(gatedAPI.headers().location).toBe("/login?next=%2Fapi%2Fv1%2Fstate");

    await page.goto(`${runtime.url}${boardPath}`, { waitUntil: "domcontentloaded" });
    await expect(page).toHaveURL(new RegExp(`/login\\?next=${encodeURIComponent(boardPath)}`));
    await expect(page).toHaveScreenshot("magic-link-login.png");
    await page.getByLabel("Email address").fill("other@example.com");
    await page.getByRole("button", { name: "Email me a sign-in link" }).click();
    await expect(page.getByRole("heading", { name: "Check your inbox" })).toBeVisible();
    expect(smtp.messageCount()).toBe(0);

    await page.getByRole("link", { name: "Try another address" }).click();
    await page.getByLabel("Email address").fill(allowedEmail);
    const messagePromise = smtp.waitForMessage();
    await page.getByRole("button", { name: "Email me a sign-in link" }).click();
    const message = await messagePromise;
    await expect(page.getByRole("heading", { name: "Check your inbox" })).toBeVisible();
    const linkMatch = message.match(/https?:\/\/[^\s]+\/auth\/magic-link\?[^\s]+/);
    expect(linkMatch).not.toBeNull();

    const eventStream = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return url.pathname === "/events" && response.status() === 200;
    });
    await page.goto(linkMatch[0], { waitUntil: "domcontentloaded" });
    await expect(page).toHaveURL(`${runtime.url}${boardPath}`);
    await expect(page.getByRole("button", { name: /Kanban demo backlog intake/ })).toBeVisible();
    await eventStream;

    const authenticatedAPI = await context.request.get(`${runtime.url}/api/v1/state`);
    expect(authenticatedAPI.status()).toBe(200);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByRole("button", { name: /Kanban demo backlog intake/ })).toBeVisible();

    const reuseContext = await browser.newContext();
    const reusePage = await reuseContext.newPage();
    const reuseResponse = await reusePage.goto(linkMatch[0], { waitUntil: "domcontentloaded" });
    expect(reuseResponse.status()).toBe(401);
    await expect(reusePage.getByRole("heading", { name: "Link unavailable" })).toBeVisible();
    await reuseContext.close();
  } finally {
    await context.close();
    await runtime.stop();
    await smtp.stop();
  }
});
