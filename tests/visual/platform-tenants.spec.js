const { test, expect } = require("@playwright/test");
const { platformPreview } = require("./platform-preview");
const fixture = require("./platform-preview-data.json");

const origin = "https://platform.detent.test";
let html;
test.beforeAll(async () => { html = await platformPreview(); });

const ready = { ...fixture.detail.organization, billing: { available: true, status: "active", customer_id: "cus_preview", plan: "growth", price_label: "Growth monthly" }, plan: "growth", grants: 1, member_count: 1, runner_count: 1 };
const failed = { ...ready, id: "org_failed", name: "Example Co", state: "failed", error_code: "tenant_start_failed", error_detail: "disk floor", step: "allocate_tenant", creator_email: "", plan: null, grants: null, member_count: null, runner_count: null, billing: { available: false }, created_at: "2026-10-06T12:00:00Z" };
const free = { ...ready, id: "org_free", name: "Parable", plan: "free", grants: 0, created_at: "2026-10-05T14:00:00Z" };

async function openTenants(page, { role = "admin", path = "/platform/tenants" } = {}) {
  const account = { ...fixture.account, platform_role: role };
  await page.route(origin + "/**", (route) => route.fulfill({ contentType: "text/html", body: html }));
  await page.route("**/api/cloud/session", (route) => route.fulfill({ json: account }));
  await page.route("**/api/cloud/organizations", (route) => route.fulfill({ json: account }));
  await page.route("**/api/cloud/platform/**", (route) => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname.endsWith("/organizations")) return route.fulfill({ json: { ...fixture.organizations, organizations: [ready, free, failed], unavailable: ["plan", "grants", "member_count", "runner_count"] } });
    const organization = pathname.endsWith("org_failed") ? failed : ready;
    return route.fulfill({ json: { ...fixture.detail, organization: { ...organization, can_support: organization.state === "ready" && ["support", "admin"].includes(role) }, can_grant: ["billing", "admin"].includes(role), can_resume: organization.state === "failed" && role === "admin",
      members: organization.state === "failed" ? null : fixture.detail.members, runners: organization.state === "failed" ? null : fixture.detail.runners, projects: organization.state === "failed" ? null : fixture.detail.projects,
      entitlements: organization.state === "failed" ? null : fixture.entitlements,
    } });
  });
  await page.goto(origin + path);
  if (path.includes("?tenant=")) await expect(page.getByRole("dialog", { name: "Model Choice Labs", exact: true })).toBeVisible();
  else await expect(page.getByRole("table", { name: "Tenants", exact: true })).toBeVisible();
}

for (const viewport of [{ width: 1440, height: 1100 }, { width: 390, height: 844 }]) {
  test(`tenant search, filters, sorting and detail at ${viewport.width}px`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await openTenants(page);
    const table = page.getByRole("table", { name: "Tenants", exact: true });
    await expect(page.getByText("3 tenants · 2 ready")).toBeVisible();
    await expect(table.getByText("—")).toHaveCount(3);
    await expect(table.getByText("Registered externally")).toBeVisible();
    const search = page.getByRole("textbox", { name: "Search tenants" });
    await search.fill("parable");
    await expect(page.getByText("1 tenants · 1 ready")).toBeVisible();
    await search.fill("org_failed");
    await expect(page.getByText("1 tenants · 0 ready")).toBeVisible();
    await search.fill("owner@example.test");
    await expect(page.getByText("2 tenants · 2 ready")).toBeVisible();
    await search.fill("absent");
    await expect(page.getByText("No tenants match")).toBeVisible();
    await search.clear();
    await page.getByRole("combobox", { name: "State", exact: true }).click();
    await page.getByRole("option", { name: "ready", exact: true }).click();
    await page.getByRole("combobox", { name: "Plan", exact: true }).click();
    await page.getByRole("option", { name: "free", exact: true }).click();
    await expect(page.getByText("1 tenants · 1 ready")).toBeVisible();
    await expect(table.getByRole("button", { name: "Parable", exact: true })).toBeVisible();
    await page.getByRole("combobox", { name: "State", exact: true }).click();
    await page.getByRole("option", { name: "All states" }).click();
    await page.getByRole("combobox", { name: "Plan", exact: true }).click();
    await page.getByRole("option", { name: "All plans" }).click();
    await table.getByRole("button", { name: "Tenant", exact: true }).click();
    await expect(table.getByRole("row").nth(1)).toContainText("Example Co");
    await table.getByRole("button", { name: "Created", exact: true }).click();
    await expect(table.getByRole("row").nth(1)).toContainText("Model Choice Labs");
    await table.getByRole("button", { name: "State", exact: true }).click();
    await expect(table.getByRole("row").nth(1)).toContainText("Example Co");
    await table.getByRole("button", { name: "Model Choice Labs", exact: true }).click();
    const sheet = page.getByRole("dialog", { name: "Model Choice Labs", exact: true });
    await expect(sheet).toBeVisible();
    await expect(page).toHaveURL(origin + "/platform/tenants?tenant=org_preview");
    await expect(sheet.getByRole("region", { name: "Members (1)" })).toContainText("owner@example.test");
    await expect(sheet.getByRole("region", { name: "Runners (1)" })).toContainText("online");
    await expect(sheet.getByRole("region", { name: "Projects (1)" })).toContainText("Detent");
    await expect(sheet.getByRole("region", { name: "Billing", exact: true })).toContainText("Growth monthly");
    await expect(sheet.getByRole("region", { name: "Provisioning timeline" })).toContainText("requested");
    expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
    const bounds = await sheet.boundingBox();
    expect(bounds.width).toBeLessThanOrEqual(Math.min(viewport.width, 576) + 1);
    await page.reload();
    await expect(sheet).toBeVisible();
    await sheet.getByRole("button", { name: "Close", exact: true }).click();
    await expect(page).toHaveURL(origin + "/platform/tenants");
    await table.getByRole("button", { name: "Example Co", exact: true }).click();
    const failedSheet = page.getByRole("dialog", { name: "Example Co", exact: true });
    await expect(failedSheet.getByRole("region", { name: "Members", exact: true })).toContainText("Unavailable");
    await failedSheet.getByRole("button", { name: "Resume provisioning" }).click();
    await expect(page.getByRole("alertdialog")).toBeVisible();
    await page.getByRole("alertdialog").getByRole("button", { name: "Cancel" }).click();
  });
}

for (const role of ["viewer", "support", "billing", "admin"]) {
  test(`${role} tenant actions`, async ({ page }) => {
    await openTenants(page, { role, path: "/platform/tenants?tenant=org_preview" });
    const sheet = page.getByRole("dialog", { name: "Model Choice Labs", exact: true });
    await expect(sheet.getByRole("region", { name: "Members (1)" })).toBeVisible();
    await expect(sheet.getByRole("button", { name: "Start support access" })).toHaveCount(["admin", "support"].includes(role) ? 1 : 0);
    await expect(sheet.getByRole("button", { name: "Grant complimentary plan" })).toHaveCount(["admin", "billing"].includes(role) ? 1 : 0);
    await expect(sheet.getByRole("button", { name: "Resume provisioning" })).toHaveCount(0);
  });
}
