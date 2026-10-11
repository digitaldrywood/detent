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

test("runner diagnostics show on the card, filter by count, and clear on heartbeat", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("response", (response) => { if (response.status() >= 500) errors.push(`${response.status()} ${response.url()}`); });
  await page.goto(hub.fixture.accounts.owner);
  const url = new URL("/settings/runners", hub.fixture.url).toString();
  await page.goto(url);
  const card = page.getByTestId("host-card").filter({ hasText: "Settings runner" });
  await expect(card.getByText("Needs attention", { exact: true })).toBeVisible();
  await expect(card).toContainText("codex: not signed in");
  await expect(card).toContainText("Not logged in");
  await expect(card).toContainText("codex login --device-auth");
  await expect(card.getByRole("alert")).toContainText("Needs human:");
  await expect(page.getByRole("alert")).toHaveCount(1);
  await card.getByRole("button", { name: "Manage Settings runner" }).click();
  const sheet = page.getByRole("dialog", { name: "Settings runner" });
  await expect(sheet.getByRole("alert")).toContainText("codex: not signed in");
  await expect(sheet.getByRole("alert")).toContainText("Not logged in");
  await expect(sheet.getByRole("alert")).toContainText("codex login status");
  await expect(sheet.getByRole("alert")).toContainText("codex login --device-auth");
  await expect(sheet.getByRole("alert")).toContainText("Last report");
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(1);
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
  const attention = page.getByRole("region", { name: "Runners needing attention" });
  await expect(attention).toContainText("Settings runner");
  await expect(attention).toContainText("Not logged in");
  await expect(attention).toContainText("codex login --device-auth");
  const fleetResponse = await page.request.get(`${hub.fixture.url}/api/v2/organizations/org_browser_preview/fleet`);
  expect(fleetResponse.ok()).toBe(true);
  const fleet = await fleetResponse.json();
  const lastReport = "2026-10-08T14:41:00Z";
  await page.route("**/api/v2/organizations/org_browser_preview/fleet", (route) => route.fulfill({ json: { ...fleet, runners: fleet.runners.map((runner) => runner.id === hub.fixture.problem_runner.runner_id ? { ...runner, connection_health: "offline", last_heartbeat_at: lastReport, problems: runner.problems.map((problem) => ({ ...problem, reported_at: lastReport })) } : runner) } }));
  await page.goto(url);
  await expect(card).toContainText("Offline since");
  await expect(card).toContainText("last report");
  await card.getByRole("button", { name: "Manage Settings runner" }).click();
  await expect(sheet).toContainText("Offline since");
  await page.keyboard.press("Escape");
  await page.goto(new URL("/diagnostics", hub.fixture.url).toString());
  await expect(attention).toContainText("Offline since");
  await expect(attention).toContainText("last report");
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
  await expect(page.getByRole("region", { name: "Runners needing attention" })).toHaveCount(0);
  expect(errors).toEqual([]);
});
