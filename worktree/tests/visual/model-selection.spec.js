const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });
let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("model-selection");
});
test.afterAll(async () => { await hub?.stop(); });

test("organization model selection saves and survives reload", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/settings/organization`);
  await expect(page.getByLabel("Normal model", { exact: true })).toHaveValue("gpt-6.1-sol");
  await expect(page.getByLabel("Plan stage effort ceiling")).toHaveValue("low");
  await expect(page.getByLabel("Validator stage effort ceiling")).toHaveValue("medium");
  await page.getByLabel("Normal model", { exact: true }).fill("gpt-6-sol");
  await page.getByLabel("Plan stage effort ceiling").selectOption("medium");
  await page.getByRole("button", { name: "Save model selection", exact: true }).click();
  await expect(page.getByText("Model selection saved.", { exact: true })).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("Normal model", { exact: true })).toHaveValue("gpt-6-sol");
  await expect(page.getByLabel("Plan stage effort ceiling")).toHaveValue("medium");
  await expect(page.getByLabel("Very complex labels")).toHaveValue("complexity:very-complex");
});

test("project overrides org selection and can restore inheritance", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/settings/integrations?project=${hub.fixture.project_id}`);
  const override = page.getByRole("switch", { name: "Override organization model selection" });
  await expect(override).not.toBeChecked();
  await expect(page.getByLabel("Normal model", { exact: true })).toHaveValue("gpt-6-sol");
  await expect(page.getByLabel("Normal model", { exact: true })).toBeDisabled();
  await override.click();
  await page.getByLabel("Normal model", { exact: true }).fill("gpt-6-astra");
  await page.getByLabel("Code stage effort ceiling").selectOption("medium");
  await page.getByRole("button", { name: "Save model selection", exact: true }).click();
  await expect(page.getByText("Model selection saved.", { exact: true })).toBeVisible();
  await page.reload();
  await expect(override).toBeChecked();
  await expect(page.getByLabel("Normal model", { exact: true })).toHaveValue("gpt-6-astra");
  await expect(page.getByLabel("Code stage effort ceiling")).toHaveValue("medium");
  await override.click();
  await page.getByRole("button", { name: "Save model selection", exact: true }).click();
  await expect(page.getByText("Model selection saved.", { exact: true })).toBeVisible();
  await page.reload();
  await expect(override).not.toBeChecked();
  await expect(page.getByLabel("Normal model", { exact: true })).toHaveValue("gpt-6-sol");
  await expect(page.getByLabel("Code stage effort ceiling")).toHaveValue("");
});

test("viewer can read model selection without editing", async ({ page }) => {
  await page.goto(hub.fixture.accounts.viewer);
  for (const path of ["/settings/organization", `/settings/integrations?project=${hub.fixture.project_id}`]) {
    await page.goto(`${hub.fixture.url}${path}`);
    await expect(page.getByLabel("Normal model", { exact: true })).toHaveValue("gpt-6-sol");
    await expect(page.getByLabel("Normal model", { exact: true })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Save model selection", exact: true })).toBeDisabled();
  }
});
