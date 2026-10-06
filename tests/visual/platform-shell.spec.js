const { test, expect } = require("@playwright/test");
const { platformPreview } = require("./platform-preview");
const fixture = require("./platform-preview-data.json");

const origin = "https://platform.detent.test";
let html;
test.beforeAll(async () => { html = await platformPreview(); });

async function openPlatform(page, { role = "admin", organizations = fixture.account.organizations, path = "/platform" } = {}) {
  const account = { ...fixture.account, platform_role: role, organizations };
  await page.route(origin + "/**", (route) => route.fulfill({ contentType: "text/html", body: html }));
  await page.route("**/api/cloud/session", (route) => route.fulfill({ json: account }));
  await page.route("**/api/cloud/organizations", (route) => route.fulfill({ json: account }));
  await page.route("**/api/cloud/platform/**", (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/organizations")) return route.fulfill({ json: { ...fixture.organizations, can_grant: false } });
    if (path.endsWith("/allowlist")) return route.fulfill({ json: fixture.allowlist });
    return route.fulfill({ json: fixture.health });
  });
  await page.goto(origin + path);
}

test("desktop platform shell redirects to tenants and keeps chrome across pages", async ({ page }) => {
  await openPlatform(page);
  await expect(page).toHaveURL(origin + "/platform/tenants");
  await expect(page.getByRole("heading", { name: "Tenants", exact: true })).toBeVisible();
  await expect(page.getByText("Platform", { exact: true })).toBeVisible();
  const nav = page.getByRole("navigation", { name: "Platform navigation" });
  await expect(nav.getByRole("link")).toHaveText(["Tenants", "Staff", "Audit", "Health", "Allowlist"]);
  await expect(nav.getByRole("link", { name: "Tenants" })).toHaveAttribute("data-active", "true");
  await expect(page.getByRole("table", { name: "Tenants", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Open organization" }).click();
  await expect(page.getByRole("menuitem", { name: "Model Choice Labs" })).toHaveAttribute("href", fixture.account.organizations[0].url);
  await page.keyboard.press("Escape");
  await nav.getByRole("link", { name: "Health" }).click();
  await expect(page.getByRole("region", { name: "Service health" })).toBeVisible();
  await expect(page).toHaveTitle("Health · Platform · Detent");
  await expect(nav.getByRole("link", { name: "Health" })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("table", { name: "Tenants", exact: true })).toHaveCount(0);
  await nav.getByRole("link", { name: "Allowlist" }).click();
  await expect(page.getByRole("region", { name: "Signup allowlist" })).toContainText("example.test");
  await expect(page.getByText("Platform", { exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "Service health" })).toHaveCount(0);
  await page.reload();
  await expect(page.getByRole("region", { name: "Signup allowlist" })).toBeVisible();
});

for (const role of ["support", "billing", "viewer"]) {
  test(`${role} has readable sections and no organization menu without memberships`, async ({ page }) => {
    await openPlatform(page, { role, organizations: [] });
    const nav = page.getByRole("navigation", { name: "Platform navigation" });
    await expect(nav.getByRole("link")).toHaveText(["Tenants", "Audit", "Health", "Allowlist"]);
    await expect(nav.getByRole("link", { name: "Staff" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Open organization" })).toHaveCount(0);
    await expect(page.locator('[data-slot="sidebar-footer"]').getByText(role[0].toUpperCase() + role.slice(1), { exact: true })).toBeVisible();
  });
}

test("mobile platform navigation opens a sheet and closes on selection", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openPlatform(page, { path: "/platform/health" });
  await expect(page.getByRole("region", { name: "Service health" })).toBeVisible();
  await expect(page.getByText("Platform", { exact: true })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Platform navigation" })).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
  await expect(page).toHaveScreenshot("platform-health-mobile.png");
  await page.getByRole("button", { name: "Toggle Sidebar" }).click();
  const sheet = page.getByRole("dialog", { name: "Sidebar" });
  await expect(sheet).toBeVisible();
  const bounds = await sheet.boundingBox();
  expect(bounds.width).toBeGreaterThan(390 * 0.75);
  expect(bounds.width).toBeLessThanOrEqual(390);
  await expect(sheet.getByRole("link", { name: "Staff" })).toBeVisible();
  await expect(page).toHaveScreenshot("platform-navigation-mobile.png");
  await sheet.getByRole("link", { name: "Allowlist" }).click();
  await expect(sheet).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Allowlist", exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "Signup allowlist" })).toBeVisible();
});

test("the chooser exposes the platform entry only for platform members", async ({ page }) => {
  await openPlatform(page, { path: "/organizations?switch=1" });
  await expect(page.getByRole("link", { name: "Open Model Choice Labs" })).toBeVisible();
  const link = page.getByRole("link", { name: "Open Platform console" });
  await expect(link).toHaveAttribute("href", "/platform/tenants");
  await expect(page).toHaveScreenshot("platform-chooser-desktop.png");
  await link.click();
  await expect(page.getByRole("heading", { name: "Tenants", exact: true })).toBeVisible();
  await openPlatform(page, { role: "", path: "/organizations?switch=1" });
  await expect(page.getByRole("link", { name: "Open Model Choice Labs" })).toBeVisible();
  await expect(link).toHaveCount(0);
  await expect(page.getByText("Platform console", { exact: true })).toHaveCount(0);
});
