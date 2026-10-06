const { test, expect } = require("@playwright/test");
const { startEntryPreview } = require("./entry-preview");

test.describe.configure({ mode: "serial" });
let entry;
test.beforeAll(async () => {
  test.setTimeout(210_000);
  entry = await startEntryPreview({ fallback: true });
});
test.afterAll(async () => { await entry?.stop(); });

for (const mode of ["denied", "stopped", "unavailable"]) {
  test(`${mode} uses the standalone sign-in surface on desktop and mobile`, async ({ page }) => {
    const origin = entry.fixture.origin;
    const start = await page.request.get(`${origin}/auth/oidc/start?organization=org_alpha`, { maxRedirects: 0 });
    expect(start.status()).toBe(303);
    const state = new URL(start.headers().location).searchParams.get("state");
    const query = new URLSearchParams({ state, code: "user_alice:porg_alpha" });
    await page.goto(`${origin}/auth/oidc/callback?${query}`, { waitUntil: "domcontentloaded" });
    if (mode === "stopped") {
      expect((await page.request.post(`${origin}/__preview/stop-tenant`)).status()).toBe(204);
      const json = await page.request.get(`${origin}/organizations/org_alpha`, { headers: { Accept: "application/json" } });
      expect(json.status()).toBe(502);
      expect(await json.text()).toBe('{"code":"tenant_unavailable","message":"The organization is temporarily unavailable"}');
    }
    if (mode === "unavailable") {
      expect((await page.request.post(`${origin}/__preview/outage`)).status()).toBe(204);
    }
    const target = mode === "denied" ? "/platform" : mode === "stopped" ? "/organizations/org_alpha?tab=work&view=board" : "/organizations/org_alpha/organization?tab=work&view=board";
    const response = await page.goto(`${origin}${target}`, { waitUntil: "networkidle" });
    expect(response.status()).toBe(mode === "denied" ? 403 : mode === "stopped" ? 502 : 503);
    const card = page.getByRole("region", { name: mode === "denied" ? "Access unavailable" : "Temporarily unavailable" });
    await expect(card).toBeVisible();
    await expect(page.locator("script")).toHaveCount(0);
    await expect(page.locator("details")).toHaveCount(0);
    await expect(page.locator("nav")).toContainText("alice@example.test");
    await expect(card.locator("form")).toHaveAttribute("action", "/logout");
    await expect(card.locator("form")).toHaveAttribute("method", "post");
    await expect(card.locator('input[name="csrf"]')).not.toHaveValue("");
    if (mode === "denied") {
      await expect(card.getByRole("link", { name: "Return to organization" })).toHaveAttribute("href", "/organizations");
      await expect(card.getByRole("link", { name: "Sign in again" })).toHaveAttribute("href", "/organizations");
    } else {
      await expect(card.getByRole("link", { name: "Try again" })).toHaveAttribute("href", target);
    }
    await page.evaluate(() => document.fonts.ready);
    const appearance = await page.locator("body").evaluate((body) => {
      const style = getComputedStyle(body);
      return { font: style.fontFamily, background: style.backgroundColor };
    });
    expect(appearance.font).toContain("Geist");
    expect(appearance.background).toBe("rgb(247, 248, 250)");
    for (const [size, width, height] of [["desktop", 1440, 1100], ["mobile", 390, 844]]) {
      await page.setViewportSize({ width, height });
      await expect(card).toBeInViewport();
      await expect(card.getByRole("button", { name: "Sign out" })).toBeInViewport();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      await expect(page).toHaveScreenshot(`entry-${mode === "denied" ? "denied" : "unavailable"}-${size}.png`);
    }
    await page.evaluate(() => document.documentElement.classList.add("dark"));
    expect(await page.locator("body").evaluate((body) => getComputedStyle(body).backgroundColor)).toBe("rgb(11, 13, 16)");
    await expect(card).toBeInViewport();
    await page.evaluate(() => {
      document.documentElement.classList.remove("dark");
      document.querySelector(".entry-account").textContent = "a-long-account-email-that-must-wrap-in-the-header@example.test";
    });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await card.getByRole("button", { name: "Sign out" }).focus();
    await expect(card.getByRole("button", { name: "Sign out" })).toBeFocused();
    const csrf = await card.locator('input[name="csrf"]').inputValue();
    await page.route(`${origin}/logout`, async (route) => {
      expect(route.request().method()).toBe("POST");
      expect(new URLSearchParams(route.request().postData()).get("csrf")).toBe(csrf);
      await route.fulfill({ status: 200, contentType: "text/html", body: "Signed out" });
    });
    await page.keyboard.press("Enter");
    await expect(page.locator("body")).toHaveText("Signed out");
  });
}
