const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");
const { startEntryPreview } = require("./entry-preview");

// Real invitation routes and callbacks; the identity provider is synthetic.
test.describe.configure({ mode: "serial" });
let hub, entry;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS * 2 + 30_000);
  hub = await startHostedHub("invitations");
  entry = await startEntryPreview();
});
test.afterAll(async () => {
  await entry?.stop();
  await hub?.stop();
});

async function invite(request, state) {
  const response = await request.post(`${hub.fixture.url}/__preview/invite/${state}`);
  expect(response.ok()).toBe(true);
  return (await response.json()).url;
}

for (const state of ["new", "existing", "alias"]) {
  test(`${state} account follows its email link into the organization`, async ({ page, request }) => {
    await page.goto(hub.fixture.accounts.invitee);
    let url = await invite(request, state);
    if (state === "alias") url = url.replace("invitation_token=", "token=");
    const provider = page.waitForRequest((request) => new URL(request.url()).pathname === "/__preview/authorize");
    await page.goto(url);
    const query = new URL((await provider).url()).searchParams;
    expect(query.get("invitation_token")).toBeTruthy();
    expect(query.get("screen_hint")).toBe(state === "new" ? "sign-up" : null);
    await expect(page).toHaveURL(`${hub.fixture.url}/organization`);
    await expect(page.getByRole("heading", { name: "Browser organization", exact: true })).toBeVisible();
    await expect(page.locator('input[name="token"]')).toHaveCount(0);
    // The local invitation is consumed after the scoped organization session.
    await page.goto(url);
    await expect(page.getByRole("alert")).toHaveText("This invitation has already been used.");
  });
}

for (const [state, message] of [
  ["expired", "This invitation has expired."],
  ["used", "This invitation has already been used."],
  ["wrong-account", "This invitation was sent to a different account at example.test."],
]) {
  test(`${state} invitation explains how to get a new email link`, async ({ page, request }) => {
    if (state === "wrong-account") await page.goto(hub.fixture.accounts.viewer);
    const url = await invite(request, state);
    await page.goto(url);
    await expect(page.getByRole("heading", { name: "Invitation unavailable" })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveText(message);
    await expect(page.getByText("Ask the person who invited you for a new invitation.")).toBeVisible();
    await expect(page.getByRole("link", { name: "Go to detent.build" })).toHaveAttribute("href", "https://detent.build");
    await expect(page.locator("body")).not.toContainText("invitee@example.test");
    await expect(page.locator("input")).toHaveCount(0);
    await test.info().attach(`invitation-${state}.png`, {
      body: await page.screenshot({ animations: "disabled", caret: "hide" }),
      contentType: "image/png",
    });
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(page.getByRole("link", { name: "Go to detent.build" })).toBeInViewport();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}

test("account with no organization requests an invitation from an owner", async ({ page }) => {
  await page.goto(entry.fixture.origin);
  await page.getByRole("link", { name: "Sign in", exact: true }).click();
  await page.getByRole("link", { name: "dana@example.test (account)", exact: true }).click();
  await expect(page.getByText("Ask an owner to send you an invitation.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Join with invitation" })).toHaveCount(0);
  await expect(page.locator('input[name="token"]')).toHaveCount(0);
  const response = await page.request.get(`${entry.fixture.origin}/invitations/join`);
  expect(response.status()).toBe(404);
});
