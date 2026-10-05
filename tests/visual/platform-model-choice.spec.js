const { test, expect } = require("@playwright/test");
const { platformPreview } = require("./platform-preview");
const fixture = require("./platform-preview-data.json");

let html;
const origin = "https://platform.detent.test";
test.beforeAll(async () => { html = await platformPreview(); });

async function openPlatform(page, canGrant = true) {
  const plan = fixture.entitlements.base;
  let features = [];
  let grants = [];
  let revision = 1;
  const changes = [];
  await page.route(origin + "/**", (route) => route.fulfill({ contentType: "text/html", body: html }));
  await page.route("**/api/cloud/platform/**", (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/entitlements")) {
      if (route.request().method() === "POST") {
        const change = route.request().postDataJSON();
        changes.push(change);
        features = change.action === "grant" ? ["model_choice"] : [];
        grants = change.action === "grant" ? [{
          id: "model_grant", plan, scope: ["model_choice"], starts_at: "2026-10-05T12:00:00Z",
          expires_at: change.expires_at, reason: change.reason, granted_by: "admin@example.test", granted_at: "2026-10-05T12:00:00Z",
        }] : [];
        revision += 1;
        return route.fulfill({ json: { action: change.action, grant_id: "model_grant" } });
      }
      return route.fulfill({ json: { ...fixture.entitlements, revision, features, grants } });
    }
    if (path.endsWith("/organizations")) return route.fulfill({ json: { ...fixture.organizations, can_grant: canGrant } });
    if (path.endsWith("/allowlist")) return route.fulfill({ json: fixture.allowlist });
    return route.fulfill({ json: fixture.health });
  });
  await page.goto(origin + "/platform");
  await expect(page.getByRole("table", { name: "Organizations", exact: true })).toBeVisible();
  return changes;
}

test("platform administrators grant and revoke model choice without granting a plan", async ({ page }) => {
  const changes = await openPlatform(page);
  const plan = page.getByRole("region", { name: "Plan for Model Choice Labs" });
  await expect(plan.getByText("Model choice: Disabled")).toBeVisible();
  await plan.getByRole("button", { name: "Grant model choice", exact: true }).click();
  const grant = page.getByRole("dialog", { name: "Grant model choice to Model Choice Labs" });
  await expect(grant).toBeVisible();
  const popup = await grant.boundingBox();
  const footer = await grant.locator('[data-slot="dialog-footer"]').boundingBox();
  expect(footer.y + footer.height).toBeLessThanOrEqual(popup.y + popup.height + 1);
  expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
  await expect(grant.getByLabel("Plan", { exact: true })).toHaveCount(0);
  await grant.getByRole("button", { name: "Grant", exact: true }).click();
  await expect(grant.getByRole("alert")).toContainText("Give a reason");
  expect(changes).toHaveLength(0);
  await grant.getByLabel("Reason", { exact: true }).fill("Approved runner model choice");
  await grant.getByLabel("Expires (optional)").fill("2026-12-31");
  await grant.getByRole("button", { name: "Grant", exact: true }).click();
  await expect(grant).toHaveCount(0);
  expect(changes[0]).toEqual({ action: "grant", feature: "model_choice", idempotency_key: expect.any(String),
    expected_revision: 1, expires_at: "2026-12-31T23:59:59Z", reason: "Approved runner model choice" });
  await expect(plan.getByText("Model choice: Enabled")).toBeVisible();
  await expect(plan.getByRole("button", { name: "Grant model choice", exact: true })).toBeDisabled();
  const grants = plan.getByRole("table", { name: "Active grants for Model Choice Labs" });
  await expect(grants.getByText("Model choice", { exact: true })).toBeVisible();
  await expect(grants.getByText("model_choice", { exact: true })).toBeVisible();
  await expect(grants.getByText("admin@example.test", { exact: true })).toBeVisible();
  await page.reload();
  await expect(plan.getByText("Model choice: Enabled")).toBeVisible();
  await grants.getByRole("button", { name: "Revoke", exact: true }).click();
  const revoke = page.getByRole("dialog", { name: "Revoke model choice for Model Choice Labs?" });
  await revoke.getByLabel("Reason", { exact: true }).fill("Model choice access ended");
  await revoke.getByRole("button", { name: "Revoke grant", exact: true }).click();
  await expect(revoke).toHaveCount(0);
  expect(changes[1]).toEqual({ action: "revoke", grant_id: "model_grant", idempotency_key: expect.any(String),
    expected_revision: 2, reason: "Model choice access ended" });
  await expect(plan.getByText("Model choice: Disabled")).toBeVisible();
  await expect(grants).toHaveCount(0);
  await expect(plan.getByRole("button", { name: "Grant model choice", exact: true })).toBeEnabled();
});

test("other platform staff cannot see the model choice grant control", async ({ page }) => {
  await openPlatform(page, false);
  await expect(page.getByRole("button", { name: "Grant model choice", exact: true })).toHaveCount(0);
  await expect(page.getByRole("region", { name: "Complimentary plans", exact: true })).toHaveCount(0);
});
