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
  await page.goto(`${hub.fixture.url}/settings/organization/models`);
  await expect(page.getByRole("combobox", { name: "Normal model", exact: true })).toContainText("gpt-6.1-sol");
  await expect(page.getByRole("button", { name: "Plan stage effort ceiling: low", exact: true })).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByRole("button", { name: "Validator stage effort ceiling: medium", exact: true })).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("combobox", { name: "Normal model", exact: true }).click();
  await page.getByRole("option", { name: "Other model ID…", exact: true }).click();
  await page.getByRole("textbox", { name: "Normal model: other model ID", exact: true }).fill("gpt-6-sol");
  await page.getByRole("button", { name: "Use", exact: true }).click();
  await page.getByRole("button", { name: "Plan stage effort ceiling: medium", exact: true }).click();
  await page.getByRole("region", { name: "Unsaved model selection", exact: true }).getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(page.getByText("Model selection saved.", { exact: true })).toBeVisible();
  await page.reload();
  await expect(page.getByRole("combobox", { name: "Normal model", exact: true })).toContainText("gpt-6-sol");
  await expect(page.getByRole("button", { name: "Plan stage effort ceiling: medium", exact: true })).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByRole("button", { name: "Remove complexity:very-complex from Very complex", exact: true })).toBeVisible();
});

test("project overrides org selection and can restore inheritance", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/settings/projects/${hub.fixture.project_id}/models`);
  const override = page.getByRole("switch", { name: "Override organization model selection" });
  await expect(override).not.toBeChecked();
  await expect(page.getByRole("combobox", { name: "Normal model", exact: true })).toContainText("gpt-6-sol");
  await expect(page.getByRole("combobox", { name: "Normal model", exact: true })).toBeDisabled();
  await override.click();
  await page.getByRole("combobox", { name: "Normal model", exact: true }).click();
  await page.getByRole("option", { name: "Other model ID…", exact: true }).click();
  await page.getByRole("textbox", { name: "Normal model: other model ID", exact: true }).fill("gpt-6-astra");
  await page.getByRole("button", { name: "Use", exact: true }).click();
  await page.getByRole("button", { name: "Code stage effort ceiling: medium", exact: true }).click();
  await page.getByRole("region", { name: "Unsaved model selection", exact: true }).getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(page.getByText("Model selection saved.", { exact: true })).toBeVisible();
  await page.reload();
  await expect(override).toBeChecked();
  await expect(page.getByRole("combobox", { name: "Normal model", exact: true })).toContainText("gpt-6-astra");
  await expect(page.getByRole("button", { name: "Code stage effort ceiling: medium", exact: true })).toHaveAttribute("aria-pressed", "true");
  await override.click();
  await page.getByRole("region", { name: "Unsaved model selection", exact: true }).getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(page.getByText("Model selection saved.", { exact: true })).toBeVisible();
  await page.reload();
  await expect(override).not.toBeChecked();
  await expect(page.getByRole("combobox", { name: "Normal model", exact: true })).toContainText("gpt-6-sol");
  await expect(page.getByRole("button", { name: "Code stage effort ceiling: level", exact: true })).toHaveAttribute("aria-pressed", "true");
});

test("viewer can read model selection without editing", async ({ page }) => {
  await page.goto(hub.fixture.accounts.viewer);
  for (const path of ["/settings/organization/models", `/settings/projects/${hub.fixture.project_id}/models`]) {
    await page.goto(`${hub.fixture.url}${path}`);
    await expect(page.getByRole("combobox", { name: "Normal model", exact: true })).toContainText("gpt-6-sol");
    await expect(page.getByRole("combobox", { name: "Normal model", exact: true })).toBeDisabled();
    await expect(page.getByRole("region", { name: "Unsaved model selection", exact: true })).toBeHidden();
  }
});
