const { test, expect } = require("@playwright/test");
const { platformPreview } = require("./platform-preview");
const fixture = require("./platform-preview-data.json");
const users = require("./platform-users-data.json");
const audit = require("./platform-audit-data.json");

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
    if (path.startsWith("/api/cloud/platform/organizations/")) return route.fulfill({ json: fixture.detail });
    if (path.endsWith("/users")) return route.fulfill({ json: { ...users, users: users.users.filter((user) => user.email.includes(new URL(route.request().url()).searchParams.get("q") ?? "")) } });
    if (path.endsWith("/audit")) return route.fulfill({ json: audit });
    if (path.endsWith("/members")) return route.fulfill({ json: { members: [
      { email: account.email, role: "admin", bootstrap: true, added_by: "bootstrap", added_at: "2026-10-07T12:00:00Z", updated_at: "2026-10-09T15:00:00Z" },
      { email: "support@example.test", role: "support", bootstrap: false, added_by: account.email, added_at: "2026-10-07T12:00:00Z", updated_at: "2026-10-09T15:00:00Z" },
    ], revision: 1, self: { email: account.email, role } } });
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
  for (const title of ["Tenants", "Users", "Audit log", "Health", "Sign-up allowlist", "Staff"]) await expect(nav.getByRole("link", { name: title, exact: true })).toBeVisible();
  await expect(nav.getByRole("link", { name: "Tenants" })).toHaveAttribute("data-active", "true");
  await expect(page.getByRole("table", { name: "Tenants", exact: true })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Your organizations" }).getByRole("link", { name: "Model Choice Labs" })).toHaveAttribute("href", fixture.account.organizations[0].url);
  const documentRequests = [];
  page.on("request", (request) => { if (request.isNavigationRequest()) documentRequests.push(request.url()); });
  await nav.getByRole("link", { name: "Health" }).click();
  await expect(page.getByRole("alert", { name: "Service health" })).toBeVisible();
  await expect(page).toHaveTitle("Health · Platform · Detent");
  await expect(nav.getByRole("link", { name: "Health" })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("table", { name: "Tenants", exact: true })).toHaveCount(0);
  await nav.getByRole("link", { name: "Sign-up allowlist" }).click();
  await expect(page.getByRole("region", { name: "Sign-up allowlist" })).toContainText("example.test");
  await expect(page.getByText("Platform", { exact: true })).toBeVisible();
  await expect(page.getByRole("alert", { name: "Service health" })).toHaveCount(0);
  expect(documentRequests).toEqual([]);
  await page.reload();
  await expect(page.getByRole("region", { name: "Sign-up allowlist" })).toBeVisible();
});

for (const role of ["support", "billing", "viewer"]) {
  test(`${role} has readable sections and no organizations without memberships`, async ({ page }) => {
    await openPlatform(page, { role, organizations: [] });
    const nav = page.getByRole("navigation", { name: "Platform navigation" });
    for (const title of ["Tenants", "Users", "Audit log", "Health", "Sign-up allowlist"]) await expect(nav.getByRole("link", { name: title, exact: true })).toBeVisible();
    await expect(nav.getByRole("link", { name: "Staff" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Open organization" })).toHaveCount(0);
    await expect(page.getByRole("contentinfo", { name: "Platform account" }).getByText(role[0].toUpperCase() + role.slice(1), { exact: true })).toBeVisible();
  });
}

test("mobile platform navigation opens a sheet and closes on selection", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openPlatform(page, { path: "/platform/health" });
  await expect(page.getByRole("alert", { name: "Service health" })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Platform navigation" })).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
  await test.info().attach("platform-health-mobile.png", {
    body: await page.screenshot({ animations: "disabled", caret: "hide" }),
    contentType: "image/png",
  });
  await page.getByRole("button", { name: "Toggle Sidebar" }).click();
  const sheet = page.getByRole("dialog", { name: "Sidebar" });
  await expect(sheet).toBeVisible();
  const bounds = await sheet.boundingBox();
  expect(bounds.width).toBeGreaterThan(390 * 0.75);
  expect(bounds.width).toBeLessThanOrEqual(390);
  await expect(sheet.getByRole("link", { name: "Staff" })).toBeVisible();
  await test.info().attach("platform-navigation-mobile.png", {
    body: await page.screenshot({ animations: "disabled", caret: "hide" }),
    contentType: "image/png",
  });
  await sheet.getByRole("link", { name: "Sign-up allowlist" }).click();
  await expect(sheet).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Sign-up allowlist", exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "Sign-up allowlist" })).toBeVisible();
});

test("the chooser exposes the platform entry only for platform members", async ({ page }) => {
  await openPlatform(page, { path: "/organizations?switch=1" });
  await expect(page.getByRole("link", { name: "Model Choice Labs", exact: true })).toBeVisible();
  const link = page.getByRole("link", { name: "Platform console", exact: true });
  await expect(link).toHaveAttribute("href", "/platform/tenants");
  await test.info().attach("platform-chooser-desktop.png", {
    body: await page.screenshot({ animations: "disabled", caret: "hide" }),
    contentType: "image/png",
  });
  await link.click();
  await expect(page.getByRole("heading", { name: "Tenants", exact: true })).toBeVisible();
  await openPlatform(page, { role: "", path: "/organizations?switch=1" });
  await expect(page.getByRole("link", { name: "Model Choice Labs", exact: true })).toBeVisible();
  await expect(link).toHaveCount(0);
  await expect(page.getByText("Platform console", { exact: true })).toHaveCount(0);
});

for (const theme of ["light", "dark"]) {
  for (const width of [1440, 390]) {
    test(`all platform routes keep the shell in ${theme} at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 980 });
      await page.emulateMedia({ colorScheme: theme });
      await openPlatform(page);
      await expect(page.getByRole("heading", { name: "Tenants", exact: true, level: 1 })).toBeVisible();
      await page.evaluate((theme) => {
        document.documentElement.dataset.theme = theme;
        document.documentElement.classList.toggle("dark", theme === "dark");
      }, theme);
      for (const [path, title] of [["tenants", "Tenants"], ["users", "Users"], ["audit", "Audit log"], ["health", "Health"], ["allowlist", "Sign-up allowlist"], ["staff", "Staff"]]) {
        if (width === 390) await page.getByRole("button", { name: "Toggle Sidebar" }).click();
        await page.getByRole("navigation", { name: "Platform navigation" }).getByRole("link", { name: title, exact: true }).click();
        const heading = page.getByRole("heading", { name: title, exact: true, level: 1 });
        await expect(heading).toBeVisible();
        await expect.poll(() => page.evaluate(() => getComputedStyle(document.documentElement).colorScheme)).toBe(theme);
        if (path === "health") {
          await expect(page.getByRole("alert", { name: "Service health" })).toContainText("1 of 1 tenant Hubs running");
          await expect(page.getByRole("table", { name: "Tenant Hubs" })).toContainText("Running");
        } else {
          await expect(page.getByRole("alert")).toHaveCount(0);
        }
        if (path === "allowlist") {
          const access = page.getByRole("region", { name: "Access", exact: true });
          await expect(access.getByText("Restricted", { exact: true })).toBeVisible();
          await expect(page.getByRole("table", { name: "Domains", exact: true }).getByText("example.test", { exact: true })).toBeVisible();
          await expect(page.getByRole("searchbox", { name: "Filter domains and emails" })).toBeVisible();
        }
        if (path === "staff") {
          const own = page.getByRole("group", { name: fixture.account.email, exact: true });
          await expect(own.getByText("You", { exact: true })).toBeVisible();
          await expect(own.getByText("Admin", { exact: true })).toBeVisible();
          await expect(own.getByRole("combobox")).toHaveCount(0);
          await expect(own.getByText("You can't change your own role")).toBeVisible();
          const member = page.getByRole("group", { name: "support@example.test", exact: true });
          await expect(member).toContainText(`Added 2026-10-07 by ${fixture.account.email} · changed 2026-10-09`);
          await expect(member.getByRole("combobox")).toBeVisible();
          await expect(member.getByRole("button", { name: "Remove" })).toBeVisible();
          await expect(page.getByRole("region", { name: "Roles", exact: true })).toContainText("Read-only access to this console.");
        }
        expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
        await test.info().attach(`platform-shell-${path}-${theme}-${width}.png`, {
          body: await page.screenshot({ animations: "disabled", caret: "hide" }),
          contentType: "image/png",
        });
      }
    });
  }
}

test("the palette opens tenants and users with keyboard navigation", async ({ page }) => {
  await openPlatform(page);
  await expect(page.getByRole("table", { name: "Tenants", exact: true })).toBeVisible();
  const navigations = [];
  page.on("request", (request) => { if (request.isNavigationRequest()) navigations.push(request.url()); });
  await page.keyboard.press("Meta+k");
  let dialog = page.getByRole("dialog", { name: "Find tenant or user" });
  await dialog.getByRole("combobox", { name: "Find tenant or user" }).fill("model");
  await expect(dialog.getByRole("option", { name: /Model Choice Labs/ })).toBeVisible();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(origin + "/platform/tenants/org_preview");
  await expect(page.getByRole("heading", { name: "Model Choice Labs" })).toBeVisible();
  await page.keyboard.press("Escape");
  await page.keyboard.press("Meta+k");
  dialog = page.getByRole("dialog", { name: "Find tenant or user" });
  await dialog.getByRole("combobox", { name: "Find tenant or user" }).fill("alex");
  await expect(dialog.getByRole("option", { name: /alex/ })).toBeVisible();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(origin + "/platform/users/alex%2Btest%40example.test");
  await expect(page.getByRole("region", { name: "Account" })).toContainText("alex+test@example.test");
  expect(navigations).toEqual([]);
});
