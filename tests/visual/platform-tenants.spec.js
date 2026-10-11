const { test, expect } = require("@playwright/test");
const { platformPreview } = require("./platform-preview");
const fixture = require("./platform-preview-data.json");

const origin = "https://platform.detent.test";
let html;
test.beforeAll(async () => { html = await platformPreview(); });

const ready = { ...fixture.detail.organization, billing: { available: true, status: "active", customer_id: "cus_preview", plan: "growth", price_label: "Growth monthly" }, plan: "growth", grants: 1, member_count: 1, runner_count: 1 };
const failed = { ...ready, id: "org_failed", name: "Example Co", state: "failed", error_code: "tenant_start_failed", error_detail: "disk floor", step: "allocate_tenant", creator_email: "", plan: null, grants: null, member_count: null, runner_count: null, billing: { available: false }, created_at: "2026-10-06T12:00:00Z" };
const free = { ...ready, id: "org_free", name: "Parable", plan: "free", grants: 0, runner_count: 0, created_at: "2026-10-05T14:00:00Z" };

async function openTenants(page, { role = "admin", path = "/platform/tenants" } = {}) {
  const account = { ...fixture.account, platform_role: role };
  await page.route(origin + "/**", (route) => route.fulfill({ contentType: "text/html", body: html }));
  await page.route("**/api/cloud/session", (route) => route.fulfill({ json: account }));
  await page.route("**/api/cloud/organizations", (route) => route.fulfill({ json: account }));
  await page.route("**/api/cloud/platform/**", (route) => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname.endsWith("/organizations")) return route.fulfill({ json: { ...fixture.organizations, organizations: [ready, free, failed], unavailable: ["plan", "grants", "member_count", "runner_count"] } });
    if (pathname.endsWith("/health")) return route.fulfill({ json: fixture.health });
    const organization = pathname.endsWith("org_failed") ? failed : pathname.endsWith("org_free") ? free : ready;
    return route.fulfill({ json: { ...fixture.detail, organization: { ...organization, can_support: organization.state === "ready" && ["support", "admin"].includes(role) }, can_grant: ["billing", "admin"].includes(role), can_resume: organization.state === "failed" && role === "admin",
      members: organization.state === "failed" ? null : fixture.detail.members, runners: organization.state === "failed" ? null : organization.id === "org_free" ? [] : fixture.detail.runners, projects: organization.state === "failed" ? null : fixture.detail.projects,
      entitlements: organization.state === "failed" ? null : { ...fixture.entitlements, grants: organization.grants ? [{ id: "grant", plan: { id: "growth", version: 1 }, scope: null, starts_at: "2026-10-01T00:00:00Z", expires_at: "2026-10-31T00:00:00Z", reason: "Partner", granted_by: "admin@example.test", granted_at: null }] : [] },
    } });
  });
  await page.goto(origin + path);
  if (path.includes("?tenant=") || path.includes("/tenants/org_")) await expect(page.getByRole("region", { name: "Model Choice Labs", exact: true })).toBeVisible();
  else await expect(page.getByRole("table", { name: "Tenants", exact: true })).toBeVisible();
}

for (const viewport of [{ width: 1440, height: 1100 }, { width: 390, height: 844 }]) {
  for (const theme of ["light", "dark"]) {
    test(`tenant table and filter navigation at ${viewport.width}px in ${theme}`, async ({ page }) => {
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: theme });
      await openTenants(page);
      await page.evaluate((theme) => document.documentElement.classList.toggle("dark", theme === "dark"), theme);
      const table = page.getByRole("table", { name: "Tenants", exact: true });
      await expect(table.getByRole("link", { name: "Model Choice Labs" })).toBeVisible();
      await expect(page.getByRole("heading", { name: "3 tenants", exact: true })).toBeVisible();
      await expect(table.getByRole("row").nth(1)).toContainText("Example Co");
      await expect(table.getByText("Capacity allocation")).toBeVisible();
      await expect(table.getByText("Until 2026-10-31")).toBeVisible();
      await expect(table.getByText("Complimentary", { exact: true })).toBeVisible();
      await expect(table.getByText("Registered externally")).toBeVisible();
      await expect(table).not.toContainText("tenant_start_failed");
      await expect(table).not.toContainText("allocate_tenant");
      await expect(table.getByText("Unavailable")).toHaveCount(6);
      expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
      if (viewport.width === 390) {
        const scrolls = await table.evaluate((element) => element.parentElement.scrollWidth > element.parentElement.clientWidth);
        expect(scrolls).toBe(true);
        await table.evaluate((element) => { element.parentElement.scrollLeft = element.parentElement.scrollWidth; });
        await expect(table.getByText("0 runners")).toBeVisible();
        await table.evaluate((element) => { element.parentElement.scrollLeft = 0; });
      }
      await page.screenshot({ path: process.env.TMPDIR + `/tenants-${theme}-${viewport.width}.png`, fullPage: true });
      await page.getByRole("button", { name: /^Needs attention/ }).click();
      await expect(table.getByRole("link", { name: "Example Co" })).toBeVisible();
      await expect(table.getByRole("link", { name: "Parable" })).toHaveCount(0);
      await page.getByRole("button", { name: /^Complimentary/ }).click();
      await page.getByRole("combobox", { name: "Status", exact: true }).click();
      await page.getByRole("option", { name: "Ready", exact: true }).click();
      await page.getByRole("combobox", { name: "Plan", exact: true }).click();
      await page.getByRole("option", { name: "Growth", exact: true }).click();
      await table.getByRole("button", { name: "Tenant", exact: true }).click();
      await expect(page).toHaveURL(/sort=name/);
      const listURL = page.url();
      await page.reload();
      await expect(page.getByRole("combobox", { name: "Plan", exact: true })).toContainText("Growth");
      await expect(page.getByRole("combobox", { name: "Status", exact: true })).toContainText("Ready");
      await table.getByRole("link", { name: "Model Choice Labs", exact: true }).click();
      const tenant = page.getByRole("region", { name: "Model Choice Labs", exact: true });
      await expect(tenant).toBeVisible();
      await expect(page).toHaveURL(listURL.replace("/tenants?", "/tenants/org_preview?"));
      await page.reload();
      await expect(tenant).toBeVisible();
      await tenant.getByRole("link", { name: "← Back to tenants" }).click();
      await expect(page).toHaveURL(listURL);
    });
  }
}

for (const role of ["viewer", "support", "billing", "admin"]) {
  test(`${role} tenant actions`, async ({ page }) => {
    await openTenants(page, { role, path: "/platform/tenants?tenant=org_preview" });
    const sheet = page.getByRole("region", { name: "Model Choice Labs", exact: true });
    await expect(sheet.getByRole("region", { name: "Members (1)" })).toBeVisible();
    await expect(sheet.getByRole("button", { name: "Start support access" })).toHaveCount(["admin", "support"].includes(role) ? 1 : 0);
    await expect(sheet.getByRole("button", { name: "Grant complimentary plan" })).toHaveCount(["admin", "billing"].includes(role) ? 1 : 0);
    await expect(sheet.getByRole("button", { name: "Resume provisioning" })).toHaveCount(0);
  });
}
