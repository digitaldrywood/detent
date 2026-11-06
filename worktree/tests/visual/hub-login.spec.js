const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");
const { startEntryPreview } = require("./entry-preview");

test.describe.configure({ mode: "serial" });
let hub, entry;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS * 2 + 30_000);
  hub = await startHostedHub("hub-login");
  entry = await startEntryPreview();
});
test.afterAll(async () => {
  await entry?.stop();
  await hub?.stop();
});

for (const surface of ["organization", "shared-entry"]) {
  test(`${surface} sign-in uses the site navigation and two OIDC actions`, async ({ page }) => {
    const url = surface === "organization" ? `${hub.fixture.url}/login` : `${entry.fixture.origin}/`;
    await page.goto(url, { waitUntil: "domcontentloaded" });
    const card = page.getByRole("region", { name: "Sign in to Detent" });
    await expect(card).toBeVisible();
    await expect(card.getByRole("link")).toHaveText(["Sign in", "Create account"]);
    await expect(page.getByRole("textbox")).toHaveCount(0);
    await expect(page.locator("body")).not.toContainText("WorkOS");
    await expect(page.getByRole("link", { name: "Join with invitation" })).toHaveCount(0);
    const nav = page.getByRole("navigation", { name: "Primary" });
    for (const [name, path] of [["detent.build home", "/"], ["How it works", "/how-it-works"], ["Why Detent", "/why-detent"], ["Dashboard", "/dashboard"], ["Install", "/install"], ["Docs", "/docs"], ["Videos", "/videos"], ["Open source", "/open-source"]]) {
      await expect(nav.getByRole("link", { name, exact: true })).toHaveAttribute("href", `https://detent.build${path}`);
    }
    await test.info().attach(`${surface}-login.png`, {
      body: await page.screenshot({ animations: "disabled", caret: "hide" }),
      contentType: "image/png",
    });
    for (const action of ["Sign in", "Create account"]) {
      await page.context().clearCookies();
      await page.goto(url);
      const authorization = page.waitForRequest((request) => new URL(request.url()).pathname === "/__preview/authorize");
      await page.getByRole("link", { name: action, exact: true }).click();
      const query = new URL((await authorization).url()).searchParams;
      expect(query.get("screen_hint")).toBe(action === "Create account" ? "sign-up" : null);
      expect(query.get("state")).toBeTruthy();
      if (surface === "organization") {
        await expect(page).toHaveURL(/\/organization$/);
        await page.goto(url);
        await expect(card).toBeVisible();
        await expect(page.getByRole("complementary")).toHaveCount(0);
      }
    }
    await page.context().clearCookies();
    await page.goto(`${url}?error=no_membership`);
    await expect(page.getByRole("alert")).toContainText("not a member of this organization");
    await test.info().attach(`${surface}-login-error.png`, {
      body: await page.screenshot({ animations: "disabled", caret: "hide" }),
      contentType: "image/png",
    });
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(card).toBeInViewport();
    await expect(card.getByRole("link", { name: "Create account" })).toBeInViewport();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}
