const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("runner-settings", { env: { DETENT_HOSTED_BROWSER_RUNNER: "1" } });
});

test.afterAll(async () => { await hub?.stop(); });

test("runner sheet saves through the fleet and persists routing after reload", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/runners", hub.fixture.url).toString());
  const row = page.getByTestId("host-card").filter({ hasText: "Settings runner" });
  await row.getByRole("button", { name: "Manage Settings runner" }).click();
  const sheet = page.getByRole("dialog", { name: "Settings runner", exact: true });
  await expect(sheet).toBeVisible();
  await sheet.getByRole("radio", { name: "Draining Finishes what it has" }).check();
  await sheet.getByLabel("Jobs at once").fill("1");
  const home = sheet.getByRole("checkbox", { name: /^Home / }).first();
  const homeName = await home.getAttribute("aria-label");
  await home.check();
  await sheet.getByRole("radio", { name: /^Full host access/ }).check();
  await sheet.getByLabel("Host services the sandbox may reach").fill("tcp:127.0.0.1:8080");
  await sheet.getByLabel("Availability", { exact: true }).selectOption("hours");
  await sheet.getByLabel("Time zone", { exact: true }).selectOption("America/Chicago");
  await sheet.getByLabel("Stop running work after hours end").fill("30m");
  await sheet.getByLabel("Queued work spillover").selectOption("after");
  await sheet.getByLabel("Queued work wait in minutes").fill("5");
  await sheet.getByRole("button", { name: "Save runner" }).click();
  await expect(sheet).toHaveCount(0);
  await expect(row).toContainText("draining · Limit 1");
  await page.reload();
  await page.getByRole("button", { name: "Manage Settings runner" }).click();
  const saved = page.getByRole("dialog", { name: "Settings runner", exact: true });
  await expect(saved.getByRole("radio", { name: /^Full host access/ })).toBeChecked();
  await expect(saved.getByLabel(homeName, { exact: true })).toBeChecked();
  await expect(saved.getByLabel("Jobs at once")).toHaveValue("1");
  await expect(saved.getByLabel("Host services the sandbox may reach")).toHaveValue("tcp:127.0.0.1:8080");
  await expect(saved.getByLabel("Day range 1")).toHaveValue("Mon-Fri");
  await expect(saved.getByLabel("From 1")).toHaveValue("09:00");
  await expect(saved.getByLabel("Until 1")).toHaveValue("17:00");
  await expect(saved.getByLabel("Stop running work after hours end")).toHaveValue("30m");
  await expect(saved.getByLabel("Queued work wait in minutes")).toHaveValue("5");
  await expect(saved.getByTestId("home-status")).toContainText("Preferring home work");
  await saved.getByLabel("Queued work spillover").selectOption("never");
  await saved.getByRole("button", { name: "Save runner" }).click();
  await expect(saved).toHaveCount(0);
  await page.reload();
  await page.getByRole("button", { name: "Manage Settings runner" }).click();
  await expect(page.getByLabel("Queued work spillover")).toHaveValue("never");
  await expect(page.getByLabel("Queued work wait in minutes")).toHaveCount(0);

  await page.goto(hub.fixture.accounts.viewer);
  await page.goto(new URL("/settings/runners", hub.fixture.url).toString());
  await page.getByRole("button", { name: "Manage Settings runner" }).click();
  const readonly = page.getByRole("dialog");
  await expect(readonly).toContainText("Full host access");
  await expect(readonly.locator("input, select, textarea")).toHaveCount(0);
  await expect(readonly.locator('[data-slot="sheet-footer"]')).toHaveCount(0);
});

test("runner outside hours stays contextual on its row", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/runners", hub.fixture.url).toString());
  const row = page.getByTestId("host-card").filter({ hasText: "Settings runner" });
  await row.getByRole("button", { name: "Manage Settings runner" }).click();
  const sheet = page.getByRole("dialog", { name: "Settings runner", exact: true });
  const later = new Date(Date.now() + 2 * 60 * 60 * 1000);
  const end = new Date(later.getTime() + 60 * 60 * 1000);
  const clock = (date) => date.toISOString().slice(11, 16);
  await sheet.getByRole("radio", { name: "Active Takes new work" }).check();
  await sheet.getByLabel("Time zone", { exact: true }).selectOption("UTC");
  await sheet.getByLabel("Day range 1").selectOption("Mon-Sun");
  await sheet.getByLabel("From 1").fill(clock(later));
  await sheet.getByLabel("Until 1").fill(clock(end));
  await sheet.getByRole("button", { name: "Save runner" }).click();
  await expect(sheet).toHaveCount(0);
  await expect(row.getByText("Outside hours", { exact: true })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.reload();
  await expect(page.getByTestId("host-card").getByText("Outside hours", { exact: true })).toBeVisible();
});
