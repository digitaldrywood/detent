const { test, expect } = require("@playwright/test");
const { platformPreview } = require("./platform-preview");
const fixture = require("./platform-preview-data.json");
const users = require("./platform-users-data.json");

const origin = "https://platform.detent.test";
let html;
test.beforeAll(async () => { html = await platformPreview(); });

for (const width of [1440, 390]) {
  for (const theme of ["light", "dark"]) {
    test(`users list and user page in ${theme} at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: width === 390 ? 844 : 1100 });
      await page.emulateMedia({ colorScheme: theme });
      const themedHtml = html.replace('<html lang="en">', `<html lang="en" class="${theme === "dark" ? "dark" : ""}">`);
      await page.route(origin + "/**", (route) => route.fulfill({ contentType: "text/html", body: themedHtml }));
      const session = { ...fixture.account, platform_role: "viewer" };
      await page.route("**/api/cloud/session", (route) => route.fulfill({ json: session }));
      await page.route("**/api/cloud/organizations", (route) => route.fulfill({ json: session }));
      await page.route("**/api/cloud/platform/users?*", async (route) => {
        const query = new URL(route.request().url()).searchParams.get("q");
        await route.fulfill({ json: { ...users, users: users.users.filter((user) => user.email.includes(query)) } });
      });
      await page.goto(origin + "/platform/users");
      expect(await page.evaluate(() => getComputedStyle(document.documentElement).colorScheme)).toBe(theme);
      await expect(page.getByRole("heading", { name: "Users", exact: true })).toBeVisible();
      const input = page.getByRole("searchbox", { name: "Search by email" });
      await input.fill("example.test");
      await expect(page).toHaveURL(origin + "/platform/users?q=example.test");
      await expect(page.getByText("Search by email", { exact: true })).toBeVisible();
      const table = page.getByRole("table", { name: "Users" });
      await expect(table.getByRole("link", { name: "sam@example.test" })).toBeVisible();
      await expect(table.getByRole("row", { name: /sam@example.test/ })).toContainText("2026-10-06 14:02Z");
      await expect(table.getByRole("row", { name: /alex/ })).toContainText("None");
      await expect(page.getByText("Not searched: Slow Tenant")).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
      await page.reload();
      await expect(input).toHaveValue("example.test");
      await page.evaluate(() => { window.platformDocument = document; });
      await table.getByRole("link", { name: "sam@example.test" }).click();
      await expect(page).toHaveURL(origin + "/platform/users/sam%40example.test?q=example.test");
      const account = page.getByRole("region", { name: "Account" });
      await expect(account).toContainText("user_01HX_sam");
      expect(await page.evaluate(() => getComputedStyle(document.documentElement).colorScheme)).toBe(theme);
      await expect(account).toContainText("2026-10-06 14:02Z");
      await expect(account.getByRole("link", { name: "Manage in Staff" })).toHaveCount(0);
      const memberships = page.getByRole("region", { name: "Organizations" });
      await expect(memberships.getByRole("link", { name: "Example Alpha" })).toBeVisible();
      await expect(memberships).toContainText("Joined 2026-09-28");
      await expect(memberships).toContainText("owner");
      await expect(page.getByRole("region", { name: "Pending invitations" })).toContainText("Expires 2026-10-13");
      expect(await page.evaluate(() => window.platformDocument === document)).toBe(true);
      expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
      await page.reload();
      await expect(account).toContainText("sam@example.test");
      await page.getByRole("heading", { level: 1 }).getByRole("link", { name: "Users" }).click();
      await expect(input).toHaveValue("example.test");
      await table.getByRole("link", { name: "alex+test@example.test" }).click();
      await expect(page.getByRole("region", { name: "Organizations" })).toContainText("No organization membership");
      await expect(page.getByRole("region", { name: "Pending invitations" })).toContainText("No pending invitations");
      await expect(page.getByRole("table")).toHaveCount(0);
      await page.goBack();
      await expect(input).toHaveValue("example.test");
      await input.fill("missing");
      await expect(page.getByRole("row", { name: "No users match" })).toBeVisible();
      await expect(page.getByText("Not searched: Slow Tenant")).toBeVisible();
      const organization = { ...fixture.detail.organization, id: "org_alpha", name: "Example Alpha", can_support: false };
      await page.route("**/api/cloud/platform/organizations", (route) => route.fulfill({ json: { ...fixture.organizations, organizations: [organization] } }));
      await page.route("**/api/cloud/platform/organizations/org_alpha", (route) => route.fulfill({ json: { ...fixture.detail, organization, can_grant: false, can_resume: false, entitlements: fixture.entitlements } }));
      await input.fill("example.test");
      await table.getByRole("link", { name: "sam@example.test" }).click();
      await page.evaluate(() => { window.platformDocument = document; });
      await memberships.getByRole("link", { name: "Example Alpha" }).click();
      await expect(page).toHaveURL(origin + "/platform/tenants/org_alpha");
      await expect(page.getByRole("heading", { name: "Example Alpha", exact: true })).toBeVisible();
      expect(await page.evaluate(() => window.platformDocument === document)).toBe(true);
    });
  }
}
