const path = require("node:path");
const AxeBuilder = require("@axe-core/playwright").default;
const { test, expect } = require("@playwright/test");
const { slackSettingsPreview } = require("./slack-settings-preview");

const origin = "https://slack.detent.test";
const masked = "https://hooks.slack.com/services/••••••••";
const initial = { webhook: masked, channel_name: "#operations", last_success_at: "2026-10-06T18:00:00Z", last_failure_at: "2026-10-06T18:05:00Z", last_delivery_failed: true, last_status_code: 503 };
let html;
test.beforeAll(async () => { html = await slackSettingsPreview(); });

async function openSettings(page, state = initial, viewer = false) {
  const writes = [];
  let current = { ...state };
  await page.route(origin + "/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname.endsWith("/integrations/slack/test")) {
      current = { ...current, last_success_at: "2026-10-06T18:06:00Z", last_delivery_failed: false, last_status_code: 200 };
      writes.push({ method: "POST", body: request.postDataJSON() });
      return route.fulfill({ json: current });
    }
    if (url.pathname.endsWith("/integrations/slack")) {
      if (request.method() === "PUT") {
        const body = request.postDataJSON();
        writes.push({ method: "PUT", body });
        current = { ...current, channel_name: body.channel_name, webhook: body.webhook === "" ? "" : masked };
      }
      return route.fulfill({ json: current });
    }
    return route.fulfill({ contentType: "text/html", body: html });
  });
  await page.goto(origin + "/settings/integrations" + (viewer ? "?viewer=1" : ""));
  return writes;
}

for (const width of [1440, 390]) {
  for (const theme of ["light", "dark"]) {
    test(`Slack settings at ${width}px in ${theme}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1000 });
      await openSettings(page);
      if (theme === "dark") await page.evaluate(() => document.documentElement.classList.add("dark"));
      await expect(page.getByLabel("Slack incoming webhook")).toBeVisible();
      await expect(page.getByText(/Last delivery failed:/)).toBeVisible();
      await expect(page.getByText(/Last successful delivery:/)).toBeVisible();
      await expect(page.getByLabel("Channel name (optional, for display)")).toHaveValue("#operations");
      await expect(page.getByLabel("Slack incoming webhook")).toHaveValue(masked);
      await expect(page.getByLabel("Slack incoming webhook")).toHaveAttribute("type", "password");
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
      const violations = (await new AxeBuilder({ page }).include("#slack-notifications").analyze()).violations;
      expect(violations).toEqual([]);
      await page.screenshot({ path: path.join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `slack-settings-${theme}-${width}.png`) });
    });
  }
}

test("save, test and disable keep the credential write-only", async ({ page }) => {
  const writes = await openSettings(page, { ...initial, webhook: "", channel_name: "", last_success_at: null, last_failure_at: null, last_delivery_failed: false });
  await expect(page.getByText("Slack notifications are off.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Send test message" })).toBeDisabled();
  await page.getByLabel("Slack incoming webhook").fill("https://hooks.slack.com/services/T/B/SLACK_SECRET_SENTINEL");
  await page.getByLabel("Channel name (optional, for display)").fill("#team");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("status")).toHaveText("Slack notifications saved.");
  await expect(page.getByLabel("Slack incoming webhook")).toHaveValue(masked);
  expect(await page.locator("body").innerHTML()).not.toContain("SLACK_SECRET_SENTINEL");
  await page.getByRole("button", { name: "Send test message" }).click();
  await expect(page.getByRole("status")).toHaveText("Test message sent.");
  await page.getByLabel("Slack incoming webhook").fill("");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("status")).toHaveText("Slack notifications disabled.");
  await page.getByLabel("Slack incoming webhook").fill("https://hooks.slack.com/services/T/B/SLACK_SECRET_SENTINEL");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("status")).toHaveText("Slack notifications saved.");
  await page.getByRole("button", { name: "Disable", exact: true }).click();
  await expect(page.getByRole("status")).toHaveText("Slack notifications disabled.");
  expect(writes.map((write) => write.method)).toEqual(["PUT", "POST", "PUT", "PUT", "PUT"]);
  expect(writes[2].body.webhook).toBe("");
  expect(writes[4].body.webhook).toBe("");
});

test("a viewer cannot see organization Slack settings", async ({ page }) => {
  await openSettings(page, initial, true);
  await expect(page.getByText("No projects yet")).toBeVisible();
  await expect(page.getByLabel("Slack incoming webhook")).toHaveCount(0);
});

test("save failure clears the URL and never echoes provider errors", async ({ page }) => {
  await openSettings(page);
  await page.route("**/integrations/slack", (route) => route.request().method() === "PUT"
    ? route.fulfill({ status: 503, json: { message: "SLACK_SECRET_SENTINEL", code: "unavailable" } })
    : route.fulfill({ json: initial }));
  await page.getByLabel("Slack incoming webhook").fill("https://hooks.slack.com/services/T/B/SLACK_SECRET_SENTINEL");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("Could not save Slack settings.");
  await expect(page.getByLabel("Slack incoming webhook")).toHaveValue(masked);
  expect(await page.locator("body").innerHTML()).not.toContain("SLACK_SECRET_SENTINEL");
});
