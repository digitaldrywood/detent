const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;

test.beforeEach(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("runner-problems", { env: {
    DETENT_HOSTED_BROWSER_RUNNER: "1",
    DETENT_HOSTED_BROWSER_RUNNER_PROBLEMS: "1",
  } });
});

test.afterEach(async () => { await hub?.stop(); });

test("runner problems stay concise on the list, filter by count, and clear on heartbeat", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("response", (response) => { if (response.status() >= 500) errors.push(`${response.status()} ${response.url()}`); });
  await page.goto(hub.fixture.accounts.owner);
  const url = new URL("/settings/runners", hub.fixture.url).toString();
  await page.goto(url);
  const card = page.getByTestId("host-card").filter({ hasText: "Settings runner" });
  await expect(card.getByRole("img", { name: "Needs attention" })).toBeVisible();
  await expect(card).toContainText("Provider sign-in or setup needed");
  await expect(card).not.toContainText("Not logged in");
  await expect(card).not.toContainText("codex login --device-auth");
  await expect(card.getByRole("alert")).toHaveCount(0);
  await card.getByRole("button", { name: "Manage Settings runner" }).click();
  const sheet = page.getByRole("article", { name: "Settings runner" });
  await expect(sheet.getByRole("alert")).toContainText("Provider sign-in or setup needed");
  await expect(sheet).not.toContainText("Not logged in");
  await expect(sheet).not.toContainText("codex login status");
  await expect(sheet.getByRole("alert")).toContainText("codex login --device-auth");
  await expect(sheet).toContainText("Last check-in");
  await page.getByRole("link", { name: "Runners", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(page.getByTestId("host-card")).toHaveCount(2);
  await page.getByRole("link", { name: "Needs attention 1" }).click();
  await expect(page).toHaveURL(/health=needs_attention/);
  await page.reload();
  await expect(page.getByTestId("host-card")).toHaveCount(1);
  await page.screenshot({ path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, "runner-needs-attention.png") });
  await page.getByRole("link", { name: "All 2" }).click();
  await expect(page.getByTestId("host-card")).toHaveCount(2);
  await page.goto(`${url}?health=needs_attention`);
  await expect(page.getByTestId("host-card")).toHaveCount(1);

  await page.goto(new URL("/diagnostics", hub.fixture.url).toString());
  const attention = page.getByRole("region", { name: "Needs attention" });
  await expect(attention).toContainText("Settings runner");
  await expect(attention).toContainText("Configured agent backend is unavailable");
  await expect(attention).not.toContainText("Not logged in");
  await expect(attention).not.toContainText("codex login --device-auth");
  const fleetResponse = await page.request.get(`${hub.fixture.url}/api/v2/organizations/org_browser_preview/fleet`);
  expect(fleetResponse.ok()).toBe(true);
  const fleet = await fleetResponse.json();
  const lastReport = "2026-10-08T14:41:00Z";
  await page.route("**/api/v2/organizations/org_browser_preview/fleet", (route) => route.fulfill({ json: { ...fleet, runners: fleet.runners.map((runner) => runner.id === hub.fixture.problem_runner.runner_id ? { ...runner, connection_health: "offline", last_heartbeat_at: lastReport, problems: runner.problems.map((problem) => ({ ...problem, reported_at: lastReport })) } : runner) } }));
  await page.goto(url);
  await expect(card.getByRole("img", { name: "Offline" })).toBeVisible();
  await card.getByRole("button", { name: "Manage Settings runner" }).click();
  await expect(sheet.getByRole("img", { name: "Offline" })).toBeVisible();
  await expect(sheet).toContainText("Last check-in");
  await page.getByRole("link", { name: "Runners", exact: true }).click();
  await page.goto(new URL("/diagnostics", hub.fixture.url).toString());
  await expect(page.getByRole("region", { name: "Capacity right now" })).toContainText("Offline");
  await expect(attention).not.toContainText("Offline since");
  await page.unroute("**/api/v2/organizations/org_browser_preview/fleet");
  await page.goto(`${url}?health=needs_attention`);

  const heartbeat = await page.request.post(`${hub.fixture.url}/api/v2/organizations/org_browser_preview/projects/${hub.fixture.project_id}/machines/${hub.fixture.problem_runner.machine_id}/heartbeat`, {
    headers: { Authorization: `Bearer ${hub.fixture.problem_credential}` },
    data: { display_name: "Settings runner", capacity: 2, version: "test", protocol_major: 2, backend_isolation: { test: ["sandbox", "native-trusted"] }, problems: [] },
  });
  expect(heartbeat.status()).toBe(200);
  await page.reload();
  await expect(page.getByText("No runners need attention.")).toBeVisible();
  await expect(page.getByRole("link", { name: "1 runner needs attention" })).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByRole("link", { name: "All 2" }).click();
  await expect(page.getByTestId("host-card")).toHaveCount(2);
  await page.screenshot({ path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, "runner-problems-cleared.png") });
  await page.goto(new URL("/diagnostics", hub.fixture.url).toString());
  await expect(page.getByRole("heading", { name: "Diagnostics", exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "Needs attention" })).not.toContainText("Configured agent backend is unavailable");
  expect(errors).toEqual([]);
});
