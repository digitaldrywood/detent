const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("runner-settings", { env: { DETENT_HOSTED_BROWSER_RUNNER: "1" } });
});

test.afterAll(async () => {
  await hub?.stop();
});

test("runner settings save through the fleet and remain visible after reload", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/runners", hub.fixture.url).toString());
  const row = page.getByTestId("host-card").filter({ hasText: "Settings runner" });
  await row.getByRole("button", { name: "Manage Settings runner" }).click();
  const card = page.getByRole("dialog", { name: "Settings runner", exact: true });
  await expect(card).toBeVisible();
  await card.getByText("Edit runner settings").click();
  await card.getByLabel("Home project IDs").fill(hub.fixture.project_id);
  await card.getByLabel("Isolation tier").selectOption("native-trusted");
  await card.getByLabel("Host services, one per line").fill("tcp:127.0.0.1:8080");
  await card.getByLabel("Availability timezone").fill("America/Chicago");
  await card.getByLabel("Weekly windows, one per line").fill("Mon-Fri 09:00-17:00");
  await card.getByLabel("Hard deadline after window closes").fill("30m");
  await card.getByLabel("Spillover", { exact: true }).selectOption("after");
  await card.getByLabel("Spillover wait in minutes").fill("5");
  await card.getByRole("button", { name: "Save runner" }).click();
  await expect(card).toContainText("Trusted only: full host access");
  await expect(card).toContainText("Mon-Fri 09:00-17:00 (America/Chicago)");

  await expect(card.getByTestId("home-status")).toContainText(`Home projects: ${hub.fixture.project_id}`);
  await expect(card.getByTestId("home-status")).toContainText("Preferring home work");
  await page.screenshot({ path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, "runner-home-projects.png") });

  await page.reload();
  await page.getByRole("button", { name: "Manage Settings runner" }).click();
  const saved = page.getByRole("dialog", { name: "Settings runner", exact: true });
  await expect(saved).toContainText("Trusted only: full host access");
  await saved.getByText("Edit runner settings").click();
  await expect(saved.getByLabel("Home project IDs")).toHaveValue(hub.fixture.project_id);
  await expect(saved.getByLabel("Host services, one per line")).toHaveValue("tcp:127.0.0.1:8080");
  await expect(saved.getByLabel("Hard deadline after window closes")).toHaveValue("30m");
  await expect(saved.getByLabel("Spillover wait in minutes")).toHaveValue("5");
  await saved.getByLabel("Spillover", { exact: true }).selectOption("never");
  await saved.getByRole("button", { name: "Save runner" }).click();
  await page.reload();
  await page.getByRole("button", { name: "Manage Settings runner" }).click();
  await page.getByRole("dialog").getByText("Edit runner settings").click();
  await expect(page.getByLabel("Spillover wait in minutes")).toHaveValue("0");

  await page.goto(hub.fixture.accounts.viewer);
  await page.goto(new URL("/settings/runners", hub.fixture.url).toString());
  await page.getByRole("button", { name: "Manage Settings runner" }).click();
  await expect(page.getByRole("dialog")).toContainText("Trusted only: full host access");
  await expect(page.getByText("Edit runner settings")).toHaveCount(0);
});

test("runner outside hours stays contextual on the fleet card", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/runners", hub.fixture.url).toString());
  const row = page.getByTestId("host-card").filter({ hasText: "Settings runner" });
  await row.getByRole("button", { name: "Manage Settings runner" }).click();
  const card = page.getByRole("dialog", { name: "Settings runner", exact: true });
  await card.getByText("Edit runner settings").click();
  const later = new Date(Date.now() + 2 * 60 * 60 * 1000);
  const end = new Date(later.getTime() + 60 * 60 * 1000);
  const clock = (date) => date.toISOString().slice(11, 16);
  await card.getByLabel("Availability timezone").fill("UTC");
  await card.getByLabel("Weekly windows, one per line").fill(`Mon-Sun ${clock(later)}-${clock(end)}`);
  await card.getByRole("button", { name: "Save runner" }).click();
  await page.keyboard.press("Escape");
  await expect(row.getByText("Outside hours", { exact: true })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.reload();
  await expect(page.getByTestId("host-card").getByText("Outside hours", { exact: true })).toBeVisible();
  await page.screenshot({ path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, "runner-outside-hours.png") });
});
