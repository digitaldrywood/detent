const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;

test.beforeEach(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("workflow-revisions", { env: { DETENT_HOSTED_BROWSER_WORKFLOW_REVISIONS: "1" } });
});

test.afterEach(async () => {
  await hub?.stop();
});

async function openWorkflow(page, account = "owner", projectId = hub.fixture.project_id) {
  await page.goto(hub.fixture.accounts[account]);
  await page.goto(`${hub.fixture.url}/settings/integrations?project=${projectId}`);
  const workflow = page.locator("section").filter({ has: page.getByRole("heading", { name: "Workflow", exact: true }) });
  await expect(workflow.getByRole("heading", { name: "Workflow", exact: true })).toBeVisible();
  return workflow;
}

function row(section, field) {
  return section.getByRole("rowheader", { name: field, exact: true }).locator("..");
}

async function approvePendingRevision(page, workflow, projectId, setup) {
  const pending = workflow.locator('[data-slot="settings-row"]').filter({ has: page.getByRole("heading", { name: "Pending revision", exact: true }) });
  const approvalRequest = page.waitForRequest((request) =>
    request.method() === "PUT" && request.url().endsWith(`/projects/${projectId}/onboarding/policy`),
  );
  await pending.getByRole("button", { name: /Approve updated policy/ }).click();
  expect((await approvalRequest).postDataJSON()).toEqual({
    expected_policy_id: setup.policy.policy.policy_id,
    policy: setup.observed_policies[0].policy,
  });
  await expect(workflow.getByRole("heading", { name: "Pending revision", exact: true })).toHaveCount(0);
}

test("applied and pending workflow revisions show stored before/after changes newest first", async ({ page }, testInfo) => {
  const workflow = await openWorkflow(page);
  const policyResponse = await page.request.get(`${hub.fixture.url}/api/v2/organizations/org_browser_preview/projects/${hub.fixture.project_id}/policy`);
  expect(policyResponse.ok()).toBe(true);
  const stored = await policyResponse.json();
  expect(stored.history.map((revision) => revision.commit)).toEqual(["b".repeat(40), "a".repeat(40)]);
  const [storedApplied, storedOriginal] = stored.history;
  expect(storedApplied.previous_definition).toEqual(storedOriginal.definition);
  expect(storedApplied.definition).toEqual(stored.policy);
  expect(storedApplied.previous_definition_digest).toBe(storedOriginal.definition_digest);
  expect(storedApplied.runner_id).toMatch(/^runner_/);
  expect(storedApplied.applied_by).toMatch(/^hosted_/);
  expect(storedApplied.previous_definition.gates.auto_promote).toBe(false);
  expect(storedApplied.definition.gates.auto_promote).toBe(true);
  const revisions = workflow.locator('[data-slot="settings-row"]').filter({ has: page.getByRole("table", { name: "Workflow revision changes" }) });
  await expect(revisions).toHaveCount(3);
  const pending = revisions.nth(0);
  const applied = revisions.nth(1);
  const original = revisions.nth(2);
  await expect(pending.getByRole("heading", { name: "Pending revision", exact: true })).toBeVisible();
  await expect(applied.getByRole("heading", { name: "Applied revision", exact: true })).toBeVisible();
  await expect(applied).toContainText("acme/orders");
  await expect(applied).toContainText("b".repeat(40));
  await expect(applied).toContainText(/Applied by hosted_/);
  await expect(original.getByRole("heading", { name: "Applied revision", exact: true })).toBeVisible();
  await expect(original).toContainText("a".repeat(40));
  await expect(applied.locator("time")).toHaveAttribute("datetime", /\d{4}-\d{2}-\d{2}T/);
  await expect(row(applied, "Lane: Archive").getByRole("cell")).toHaveText(["Terminal", "Removed"]);
  await expect(row(applied, "Lane: Review").getByRole("cell")).toHaveText(["Not present", "Nondispatchable"]);
  await expect(row(applied, "Lane order").getByRole("cell")).toHaveText([
    "Todo → In Progress → Done → Archive → Blocked",
    "Todo → Review → In Progress → Done → Blocked",
  ]);
  await expect(row(applied, "Transitions from Todo").getByRole("cell")).toHaveText(["Done, In Progress", "In Progress, Review"]);
  await expect(row(applied, "Execution · Auto promote").getByRole("cell")).toHaveText(["false", "true"]);
  await expect(pending).toContainText("c".repeat(40));
  await expect(row(pending, "Lane: Review").getByRole("cell")).toHaveText(["Nondispatchable", "Dispatchable"]);
  await expect(row(pending, "Lane order").getByRole("cell")).toHaveText([
    "Todo → Review → In Progress → Done → Blocked",
    "Todo → In Progress → Review → Done → Blocked",
  ]);
  await expect(row(original, "Lane order").getByRole("cell")).toHaveText(["None", "Todo → In Progress → Done → Archive → Blocked"]);
  await expect(row(original, "Execution · Auto promote").getByRole("cell")).toHaveText(["Not set", "false"]);
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

test("selecting a project offers its exact runner policy and approval records the same diff", async ({ page }) => {
  const workflow = await openWorkflow(page);
  const selectedProject = new URL(hub.fixture.private_project).pathname.split("/").pop();
  const setupResponse = page.waitForResponse((response) =>
    response.request().method() === "GET" && response.url().endsWith(`/projects/${selectedProject}/onboarding`),
  );
  await page.getByRole("combobox", { name: "Project", exact: true }).click();
  await page.getByRole("option", { name: "Owner private project", exact: true }).click();
  const setup = await (await setupResponse).json();
  expect(setup.observed_policies).toHaveLength(1);
  const candidate = setup.observed_policies[0];
  expect(candidate.conflict).toBe(false);
  expect(candidate.policy.authored.version).toBe(2);
  expect(candidate.policy.authored.files["detent.yaml"]).toContain("lanes:");
  expect(candidate.policy.configuration.behavior).toBeNull();
  await approvePendingRevision(page, workflow, selectedProject, setup);
  const latest = workflow.locator('[data-slot="settings-row"]').filter({ has: page.getByRole("heading", { name: "Applied revision", exact: true }) }).first();
  await expect(latest).toContainText("c".repeat(40));
  await expect(row(latest, "Lane: Review").getByRole("cell")).toHaveText(["Nondispatchable", "Dispatchable"]);
  await expect(row(latest, "Execution · Auto promote").getByRole("cell")).toHaveText(["true", "false"]);
  await expect(latest).toContainText(/Applied by hosted_/);
});

test("older revision pages keep their stored comparisons", async ({ page }) => {
  const selectedProject = new URL(hub.fixture.private_project).pathname.split("/").pop();
  const workflow = await openWorkflow(page, "owner", selectedProject);
  const setupResponse = await page.request.get(`${hub.fixture.url}/api/v2/organizations/org_browser_preview/projects/${selectedProject}/onboarding`);
  expect(setupResponse.ok()).toBe(true);
  await approvePendingRevision(page, workflow, selectedProject, await setupResponse.json());
  await page.route(/\/policy(?:\?.*)?$/, async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    const url = new URL(route.request().url());
    url.searchParams.set("limit", "1");
    const response = await route.fetch({ url: url.toString() });
    await route.fulfill({ response });
  });
  await openWorkflow(page, "owner", selectedProject);
  const tables = workflow.getByRole("table", { name: "Workflow revision changes" });
  const revisions = workflow.locator('[data-slot="settings-row"]').filter({ has: page.getByRole("table", { name: "Workflow revision changes" }) });
  await expect(tables).toHaveCount(1);
  await expect(revisions.nth(0)).toContainText("c".repeat(40));
  await workflow.getByRole("button", { name: "Load older revisions" }).click();
  await expect(tables).toHaveCount(2);
  await expect(revisions.nth(1)).toContainText("b".repeat(40));
  await expect(row(revisions.nth(1), "Lane: Archive").getByRole("cell")).toHaveText(["Terminal", "Removed"]);
  await expect(row(revisions.nth(1), "Execution · Auto promote").getByRole("cell")).toHaveText(["false", "true"]);
  await workflow.getByRole("button", { name: "Load older revisions" }).click();
  await expect(tables).toHaveCount(3);
  await expect(revisions.nth(2)).toContainText("a".repeat(40));
  await expect(workflow.getByRole("button", { name: "Load older revisions" })).toHaveCount(0);
});
