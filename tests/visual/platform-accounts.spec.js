const { test, expect } = require("@playwright/test");
const { platformPreview } = require("./platform-preview");
const fixture = require("./platform-preview-data.json");
const accounts = require("./platform-accounts-data.json");

const origin = "https://platform.detent.test";
let html;
test.beforeAll(async () => { html = await platformPreview(); });

for (const width of [1440, 390]) {
  test(`accounts explicit search and results at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: width === 390 ? 844 : 1100 });
    await page.route(origin + "/**", (route) => route.fulfill({ contentType: "text/html", body: html }));
    const session = { ...fixture.account, platform_role: "viewer" };
    await page.route("**/api/cloud/session", (route) => route.fulfill({ json: session }));
    await page.route("**/api/cloud/organizations", (route) => route.fulfill({ json: session }));
    const queries = [];
    await page.route("**/api/cloud/platform/accounts?*", async (route) => {
      const query = new URL(route.request().url()).searchParams.get("q");
      queries.push(query);
      await route.fulfill({ json: query === "missing" ? { accounts: [], unsearched: accounts.unsearched } : accounts });
    });
    await page.goto(origin + "/platform/accounts");
    await expect(page.getByRole("heading", { name: "Accounts", exact: true })).toBeVisible();
    const input = page.getByRole("searchbox", { name: "Search by email" });
    const search = page.getByRole("button", { name: "Search", exact: true });
    await input.fill("ab");
    await expect(search).toBeDisabled();
    await input.fill("example.test");
    expect(queries).toEqual([]);
    await input.press("Enter");
    const member = page.getByRole("region", { name: "michael@example.test" });
    await expect(member).toBeVisible();
    expect(queries).toEqual(["example.test"]);
    await expect(member.getByRole("row")).toHaveCount(4);
    await expect(member).toContainText("user_01HX_michael");
    await expect(member).toContainText("2026-10-06 14:02Z");
    await expect(member.getByRole("link", { name: "Open tenant →" })).toHaveCount(3);
    await expect(member.getByRole("link").first()).toHaveAttribute("href", "/platform/tenants?tenant=org_parable");
    await expect(page.getByText("No organization membership")).toBeVisible();
    await expect(page.getByText("Not searched: Slow Tenant")).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
    if (width === 390) {
      const table = member.locator('[data-slot="table-container"]');
      await table.evaluate((element) => { element.scrollLeft = element.scrollWidth; });
      await expect(member.getByRole("link").first()).toBeInViewport();
    }
    await input.fill("missing");
    await search.click();
    await expect(page.getByText("No accounts match")).toBeVisible();
    await expect(member).toHaveCount(0);
    await expect(page.getByText("Not searched: Slow Tenant")).toBeVisible();
    expect(queries).toEqual(["example.test", "missing"]);
    const organization = { ...fixture.detail.organization, id: "org_parable", name: "Parable", can_support: false };
    await page.route("**/api/cloud/platform/organizations", (route) => route.fulfill({ json: { ...fixture.organizations, organizations: [organization] } }));
    await page.route("**/api/cloud/platform/organizations/org_parable", (route) => route.fulfill({ json: { ...fixture.detail, organization, can_grant: false, can_resume: false, entitlements: fixture.entitlements } }));
    await input.fill("example.test");
    await search.click();
    await member.getByRole("link", { name: "Open tenant →" }).first().click();
    await expect(page).toHaveURL(origin + "/platform/tenants?tenant=org_parable");
    await expect(page.getByRole("dialog", { name: "Parable", exact: true })).toBeVisible();
  });
}
