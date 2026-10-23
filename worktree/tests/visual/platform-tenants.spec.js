const { test, expect } = require("@playwright/test");
const { platformPreview } = require("./platform-preview");
const fixture = require("./platform-preview-data.json");

const origin = "https://platform.detent.test";
let html;
test.beforeAll(async () => { html = await platformPreview(); });

const ready = { ...fixture.detail.organization, billing: { available: true, status: "active", customer_id: "cus_preview", plan: "growth", price_label: "Growth monthly" }, plan: "growth", grants: 1, member_count: 1, runner_count: 1 };
const failed = { ...ready, id: "org_failed", name: "Example Co", state: "failed", error_code: "tenant_start_failed", error_detail: "disk floor", step: "allocate_tenant", creator_email: "", plan: null, grants: null, member_count: null, runner_count: null, billing: { available: false }, created_at: "2026-10-06T12:00:00Z" };
const free = { ...ready, id: "org_free", name: "Secondary Example Organization", plan: "free", grants: 0, runner_count: 0, created_at: "2026-10-05T14:00:00Z" };

async function openTenants(page, { role = "admin", path = "/platform/tenants" } = {}) {
  const account = { ...fixture.account, platform_role: role };
  await page.route(origin + "/**", (route) => route.fulfill({ contentType: "text/html", body: html }));
  await page.route("**/api/cloud/session", (route) => route.fulfill({ json: account }));
  await page.route("**/api/cloud/organizations", (route) => route.fulfill({ json: account }));
  await page.route("**/api/cloud/platform/**", (route) => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname.endsWith("/organizations")) return route.fulfill({ json: { ...fixture.organizations, organizations: [ready, free, failed], unavailable: ["plan", "grants", "member_count", "runner_count"] } });
    if (pathname.endsWith("/health")) return route.fulfill({ json: fixture.health });
    if (pathname.endsWith("/audit")) return route.fulfill({ json: { rows: [], events: [], tenants: [], next_cursor: "" } });
    const organization = pathname.endsWith("org_failed") ? failed : pathname.endsWith("org_free") ? free : ready;
    return route.fulfill({ json: { ...fixture.detail, organization: { ...organization, can_support: organization.state === "ready" && ["support", "admin"].includes(role) }, can_grant: ["billing", "admin"].includes(role), can_resume: organization.state === "failed" && role === "admin",
      credit_balance_micros: organization.state === "failed" ? null : fixture.detail.credit_balance_micros,
      ai_credits: organization.state === "failed" ? null : fixture.detail.ai_credits,
      members: organization.state === "failed" ? null : fixture.detail.members, runners: organization.state === "failed" ? null : organization.id === "org_free" ? [] : fixture.detail.runners, projects: organization.state === "failed" ? null : fixture.detail.projects,
      entitlements: organization.state === "failed" ? null : { ...fixture.entitlements, grants: organization.grants ? [{ id: "grant", plan: { id: "growth", version: 1 }, scope: null, starts_at: "2026-10-01T00:00:00Z", expires_at: "2026-10-31T00:00:00Z", reason: "Partner", granted_by: "admin@example.test", granted_at: null }] : [] },
    } });
  });
  await page.goto(origin + path);
  if (path.includes("?tenant=") || path.includes("/tenants/org_")) await expect(page.getByRole("region", { name: "Organization", exact: true })).toBeVisible();
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
      await expect(table.getByRole("row", { name: /Model Choice Labs/ }).getByRole("cell", { name: "$100.00", exact: true })).toBeVisible();
      await expect(table.getByRole("row", { name: /Secondary Example Organization/ }).getByRole("cell", { name: "$100.00", exact: true })).toBeVisible();
      await expect(table.getByRole("row", { name: /Example Co/ })).toContainText("Unavailable");
      expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
      if (viewport.width === 390) {
        const scrolls = await table.evaluate((element) => element.parentElement.scrollWidth > element.parentElement.clientWidth);
        expect(scrolls).toBe(true);
        await table.evaluate((element) => { element.parentElement.scrollLeft = element.parentElement.scrollWidth; });
        await expect(table.getByText("0 runners")).toBeVisible();
        await table.evaluate((element) => { element.parentElement.scrollLeft = 0; });
      }
      await table.evaluate(async (element) => {
        await Promise.all(element.getAnimations({ subtree: true }).map((animation) => animation.finished));
      });
      await page.screenshot({ path: process.env.TMPDIR + `/tenants-${theme}-${viewport.width}.png`, fullPage: true });
      await page.getByRole("button", { name: /^Needs attention/ }).click();
      await expect(table.getByRole("link", { name: "Example Co" })).toBeVisible();
      await expect(table.getByRole("link", { name: "Secondary Example Organization" })).toHaveCount(0);
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
      const tenant = page.getByRole("region", { name: "Organization", exact: true });
      await expect(tenant).toBeVisible();
      await expect(page).toHaveURL(listURL.replace("/tenants?", "/tenants/org_preview?"));
      await page.reload();
      await expect(tenant).toBeVisible();
      await page.evaluate((theme) => document.documentElement.classList.toggle("dark", theme === "dark"), theme);
      await expect(page.getByRole("region", { name: "Organization" })).toContainText("owner@example.test");
      await expect(page.getByRole("meter", { name: "Projects", exact: true })).toHaveAttribute("aria-valuetext", "2 of 5");
      await expect(page.getByRole("meter", { name: "Members", exact: true })).toHaveAttribute("aria-valuetext", "1 used · no plan limit");
      await page.screenshot({ path: process.env.TMPDIR + `/tenant-overview-${theme}-${viewport.width}.png`, fullPage: true });
      await test.info().attach("Tenant overview", { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
      expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
      await page.getByRole("button", { name: "Members 1", exact: true }).click();
      await expect(page.getByRole("region", { name: "Members", exact: true })).toContainText("owner@example.test");
      await page.getByRole("button", { name: "Runners 1", exact: true }).click();
      await expect(page.getByRole("region", { name: "Runners", exact: true })).toContainText("Online");
      await page.getByRole("button", { name: "Projects 1", exact: true }).click();
      await expect(page.getByRole("region", { name: "Projects", exact: true })).toContainText("Detent");
      await page.getByRole("button", { name: "Activity", exact: true }).click();
      await expect(page.getByRole("region", { name: "Provisioning timeline" })).toContainText("Requested");
      await page.reload();
      await expect(page.getByRole("button", { name: "Activity", exact: true })).toHaveAttribute("aria-pressed", "true");
      await page.getByRole("main").getByRole("link", { name: "Tenants", exact: true }).click();
      const expectedList = new URL(listURL);
      await expect(page).toHaveURL((url) =>
        url.pathname === expectedList.pathname &&
        Array.from(expectedList.searchParams).every(([key, value]) => url.searchParams.get(key) === value) &&
        !url.searchParams.has("section"),
      );
    });
  }
}

for (const role of ["viewer", "support", "billing", "admin"]) {
  test(`${role} tenant actions`, async ({ page }) => {
    await openTenants(page, { role, path: "/platform/tenants/org_preview" });
    await expect(page.getByRole("region", { name: "Organization" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Support access" })).toHaveCount(["admin", "support"].includes(role) ? 1 : 0);
    await expect(page.getByRole("button", { name: "Change plan" })).toHaveCount(["admin", "billing"].includes(role) ? 1 : 0);
    if (["admin", "support"].includes(role)) {
      await page.getByRole("button", { name: "Support access" }).click();
      const dialog = page.getByRole("dialog", { name: "Start support access", exact: true });
      await expect(dialog.getByRole("combobox", { name: "Reason" })).toContainText("Customer asked for help");
      await dialog.getByRole("button", { name: "Cancel" }).click();
      await expect(dialog).toBeHidden();
    }
  });
}
