const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });
let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("workflow-revisions", { env: { DETENT_HOSTED_BROWSER_WORKFLOW_REVISIONS: "1" } });
});

test.afterAll(async () => {
  await hub?.stop();
});

async function openWorkflow(page, account = "owner") {
  await page.goto(hub.fixture.accounts[account]);
  await page.goto(`${hub.fixture.url}/settings/integrations?project=${hub.fixture.project_id}`);
  return page.locator("section").filter({ has: page.getByRole("heading", { name: "Workflow", exact: true }) });
}

function row(section, field) {
  return section.getByRole("rowheader", { name: field, exact: true }).locator("..");
}

test("applied and pending workflow revisions show stored before/after changes newest first", async ({ page }, testInfo) => {
  const workflow = await openWorkflow(page);
  const revisions = workflow.locator('[data-slot="settings-row"]').filter({ has: page.getByRole("table", { name: "Workflow revision changes" }) });
  await expect(revisions).toHaveCount(3);
  const pending = revisions.nth(0);
  const applied = revisions.nth(1);
  await expect(pending.getByRole("heading", { name: "Pending revision", exact: true })).toBeVisible();
  await expect(applied.getByRole("heading", { name: "Applied revision", exact: true })).toBeVisible();
  await expect(applied).toContainText("acme/orders");
  await expect(applied).toContainText("b".repeat(40));
  await expect(applied).toContainText(/Applied by runner_/);
  await expect(applied.locator("time")).toHaveAttribute("datetime", /\d{4}-\d{2}-\d{2}T/);
  await expect(row(applied, "Lane: Archive")).toContainText("Removed");
  await expect(row(applied, "Lane: Review")).toContainText("Not present");
  await expect(row(applied, "Lane order")).toContainText("Todo → In Progress → Done → Archive");
  await expect(row(applied, "Lane order")).toContainText("Todo → Review → In Progress → Done");
  await expect(row(applied, "Transitions from Todo")).toContainText("Done, In Progress");
  await expect(row(applied, "Transitions from Todo")).toContainText("In Progress, Review");
  await expect(row(applied, "Execution · Auto promote").getByRole("cell")).toHaveText(["false", "true"]);
  await expect(pending).toContainText("c".repeat(40));
  await expect(row(pending, "Lane: Review").getByRole("cell")).toHaveText(["Nondispatchable", "Dispatchable"]);
  await expect(row(pending, "Lane order")).toContainText("Todo → In Progress → Review → Done");
  await expect(row(pending, "Execution · Auto promote").getByRole("cell")).toHaveText(["true", "false"]);
  await expect(pending.getByRole("button", { name: /Approve updated policy/ })).toBeEnabled();
  const execution = page.locator("section").filter({ has: page.getByRole("heading", { name: "Execution", exact: true }) });
  await expect(execution.getByRole("button", { name: /Approve updated policy/ })).toHaveCount(0);
  await workflow.scrollIntoViewIfNeeded();
  await testInfo.attach("workflow-revisions-desktop.png", { body: await workflow.screenshot(), contentType: "image/png" });
  await openWorkflow(page, "viewer");
  await expect(workflow.getByRole("table", { name: "Workflow revision changes" })).toHaveCount(3);
  await expect(workflow.getByRole("button", { name: /Approve updated policy/ })).toHaveCount(0);
});

test("approving a pending revision records the same diff as an applied revision", async ({ page }) => {
  const workflow = await openWorkflow(page);
  const pending = workflow.locator('[data-slot="settings-row"]').filter({ has: page.getByRole("heading", { name: "Pending revision", exact: true }) });
  await pending.getByRole("button", { name: /Approve updated policy/ }).click();
  await expect(workflow.getByRole("heading", { name: "Pending revision", exact: true })).toHaveCount(0);
  const latest = workflow.locator('[data-slot="settings-row"]').filter({ has: page.getByRole("heading", { name: "Applied revision", exact: true }) }).first();
  await expect(latest).toContainText("c".repeat(40));
  await expect(row(latest, "Lane: Review").getByRole("cell")).toHaveText(["Nondispatchable", "Dispatchable"]);
  await expect(row(latest, "Execution · Auto promote").getByRole("cell")).toHaveText(["true", "false"]);
  await expect(latest).toContainText(/Applied by hosted_/);
});

test("older revision pages keep their stored comparisons", async ({ page }) => {
  await page.route(/\/policy(?:\?.*)?$/, async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    const url = new URL(route.request().url());
    url.searchParams.set("limit", "1");
    const response = await route.fetch({ url: url.toString() });
    await route.fulfill({ response });
  });
  const workflow = await openWorkflow(page);
  const tables = workflow.getByRole("table", { name: "Workflow revision changes" });
  await expect(tables).toHaveCount(1);
  await workflow.getByRole("button", { name: "Load older revisions" }).click();
  await expect(tables).toHaveCount(2);
  await expect(workflow.getByRole("rowheader", { name: "Lane: Archive", exact: true })).toHaveCount(1);
  await workflow.getByRole("button", { name: "Load older revisions" }).click();
  await expect(tables).toHaveCount(3);
  await expect(workflow.getByRole("button", { name: "Load older revisions" })).toHaveCount(0);
});
