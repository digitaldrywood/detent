const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("github-triage", {
    env: { DETENT_HOSTED_BROWSER_GITHUB_TRIAGE: "1" },
  });
});
test.afterAll(async () => { await hub?.stop(); });

test("public GitHub reports appear in the first holding lane", async ({ page }, testInfo) => {
  await page.goto(hub.fixture.accounts.owner, { waitUntil: "domcontentloaded" });
  await page.goto(new URL(`/work/p/${hub.fixture.project_id}`, hub.fixture.url).toString());
  const lanes = page.getByTestId("board-lane");
  await expect(lanes.first()).toHaveAttribute("data-lane", "Triage");
  const triage = page.locator('[data-testid="board-lane"][data-lane="Triage"]');
  await expect(triage).toBeVisible();
  await expect(triage).not.toHaveAttribute("data-terminal", "true");
  await expect(triage).toContainText("Public GitHub report awaiting triage");
  await page.screenshot({ path: testInfo.outputPath("triage.png"), fullPage: true });
  await triage.getByText("Public GitHub report awaiting triage", { exact: true }).click();
  await expect(page.getByTestId("issue-body")).toBeVisible();
  await expect(page.getByRole("complementary", { name: "Properties", exact: true })).toContainText("Triage");
  await expect(page.getByTestId("issue-linked-source").getByRole("link", { name: "GitHub source", exact: true })).toHaveAttribute("href", "https://github.com/acme/orders/issues/12");
});
