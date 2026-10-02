const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("runner-problems", { env: {
    DETENT_HOSTED_BROWSER_RUNNER: "1",
    DETENT_HOSTED_BROWSER_RUNNER_PROBLEMS: "1",
  } });
});

test.afterAll(async () => { await hub?.stop(); });

test("runner diagnostics stay contextual, filter by count, and clear on heartbeat", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("response", (response) => { if (response.status() >= 500) errors.push(`${response.status()} ${response.url()}`); });
  await page.goto(hub.fixture.accounts.owner);
  const url = new URL("/settings/runners", hub.fixture.url).toString();
  await page.goto(url);
  const card = page.getByTestId("host-card").filter({ hasText: "Settings runner" });
  await expect(card.getByText("Needs attention", { exact: true })).toBeVisible();
  await expect(page.getByTestId("runner-attention")).toContainText("The configured isolation tier is unavailable.");
  await expect(card.getByRole("alert")).toContainText("Install or repair the sandbox tooling");
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

  const heartbeat = await page.request.post(`${hub.fixture.url}/api/v2/organizations/org_browser_preview/projects/${hub.fixture.project_id}/machines/${hub.fixture.problem_runner.machine_id}/heartbeat`, {
    headers: { Authorization: `Bearer ${hub.fixture.problem_credential}` },
    data: { display_name: "Settings runner", capacity: 2, version: "test", protocol_major: 2, backend_isolation: { test: ["sandbox", "native-trusted"] }, problems: [] },
  });
  expect(heartbeat.status()).toBe(200);
  await page.reload();
  await expect(page.getByText("No runners need attention.")).toBeVisible();
  await expect(page.getByRole("link", { name: "1 runner needs attention" })).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByRole("link", { name: "All runners" }).click();
  await expect(page.getByTestId("host-card")).toHaveCount(2);
  await page.screenshot({ path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, "runner-problems-cleared.png") });
  expect(errors).toEqual([]);
});
