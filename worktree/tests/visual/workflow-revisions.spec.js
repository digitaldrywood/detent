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
  await page.goto(`${hub.fixture.url}/settings/projects/${projectId}/workflow`);
  const workflow = page.locator("section").filter({ has: page.getByRole("heading", { name: "Lanes", exact: true }) });
  await expect(workflow.getByRole("heading", { name: "Lanes", exact: true })).toBeVisible();
  return workflow;
}

async function storedPolicy(page, projectId) {
  const response = await page.request.get(`${hub.fixture.url}/api/v2/organizations/org_browser_preview/projects/${projectId}/policy`);
  expect(response.ok()).toBe(true);
  return response.json();
}

async function expectNoRevisionHistory(page, workflow) {
  await expect(workflow.getByRole("table", { name: "Workflow revision changes" })).toHaveCount(0);
  await expect(page.getByRole("heading", { name: /^(Applied|Reported) revision$/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Load older revisions" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /Approve/ })).toHaveCount(0);
}

test("workflow revision history stays in the API and out of the Workflow tab", async ({ page }) => {
  const workflow = await openWorkflow(page);
  const stored = await storedPolicy(page, hub.fixture.project_id);
  expect(stored.history.map((revision) => revision.commit)).toEqual(["b".repeat(40), "a".repeat(40)]);
  const [storedApplied, storedOriginal] = stored.history;
  expect(storedApplied.previous_definition).toEqual(storedOriginal.definition);
  expect(storedApplied.definition).toEqual(stored.policy);
  expect(storedApplied.previous_definition_digest).toBe(storedOriginal.definition_digest);
  expect(storedApplied.runner_id).toMatch(/^runner_/);
  expect(storedApplied.applied_by).toMatch(/^hosted_/);
  expect(storedApplied.previous_definition.gates.auto_promote).toBe(false);
  expect(storedApplied.definition.gates.auto_promote).toBe(true);
  await expectNoRevisionHistory(page, workflow);
  await expectNoRevisionHistory(page, await openWorkflow(page, "viewer"));
});

test("an automatically applied runner configuration keeps its stored definition", async ({ page }) => {
  const selectedProject = new URL(hub.fixture.private_project).pathname.split("/").pop();
  const workflow = await openWorkflow(page, "owner", selectedProject);
  const setupResponse = await page.request.get(`${hub.fixture.url}/api/v2/organizations/org_browser_preview/projects/${selectedProject}/onboarding`);
  expect(setupResponse.ok()).toBe(true);
  const setup = await setupResponse.json();
  expect(setup.observed_policies ?? []).toHaveLength(0);
  const candidate = setup.policy.policy;
  expect(candidate.authored.version).toBe(3);
  expect(candidate.authored.files["detent.yaml"]).toContain("lanes:");
  expect(candidate.configuration.behavior).toBeNull();
  const stored = await storedPolicy(page, selectedProject);
  expect(stored.history[0].definition).toEqual(candidate);
  expect(stored.history[0].commit).toBe("c".repeat(40));
  expect(stored.history[0].applied_by).toBe("repository-default-branch");
  await expectNoRevisionHistory(page, workflow);
});
