const { test, expect } = require("@playwright/test");
const { spawn } = require("node:child_process");
const path = require("node:path");

let server;
let origin;

test.beforeEach(async () => {
  server = spawn(process.execPath, ["--import", path.resolve("web/conversation/node_modules/tsx/dist/loader.mjs"), "tests/visual/onboarding-help-server.ts"], { stdio: ["pipe", "pipe", "pipe"] });
  origin = await new Promise((resolve, reject) => {
    let output = "";
    const timeout = setTimeout(() => reject(new Error(output)), 30_000);
    const read = (data) => {
      output += data;
      const match = output.match(/Onboarding fixture: (\S+)/);
      if (match) { clearTimeout(timeout); resolve(match[1]); }
    };
    server.stdout.on("data", read);
    server.stderr.on("data", read);
    server.once("error", reject);
    server.once("exit", (code) => { clearTimeout(timeout); reject(new Error(`Fixture exited ${code}: ${output}`)); });
  });
});

test.afterEach(async () => {
  if (server?.exitCode === null) {
    await new Promise((resolve) => { server.once("exit", resolve); server.stdin.end(); });
  }
});

async function help(page, label, content, interaction = "click", screenshot, fieldLabel = label) {
  const trigger = page.getByRole("button", { name: `Help for ${label}`, exact: true }).first();
  if (interaction === "hover") await trigger.hover();
  else if (interaction === "focus") {
    // Actual keyboard focus, including the focus-visible browser behavior.
    await page.getByLabel(fieldLabel, { exact: true }).focus();
    await page.keyboard.press("Shift+Tab");
    await expect(trigger).toBeFocused();
  } else if (interaction === "tap") await trigger.tap();
  else await trigger.click();
  const popup = interaction === "focus" || interaction === "hover" ? page.locator(`[data-slot="tooltip-popup"][aria-label="Help for ${label}"]`) : page.getByRole("dialog", { name: `Help for ${label}`, exact: true });
  await expect(popup).toBeVisible();
  await expect(popup).toContainText(content);
  const bounds = await popup.boundingBox();
  expect(bounds.x).toBeGreaterThanOrEqual(0);
  expect(bounds.x + bounds.width).toBeLessThanOrEqual(page.viewportSize().width);
  if (screenshot) await page.screenshot({ path: screenshot });
  await page.keyboard.press("Escape");
  await expect(popup).not.toBeVisible();
  await page.mouse.move(0, 0);
  if (interaction === "focus") await page.keyboard.press("Tab");
}

for (const width of [1280, 390]) {
  for (const colorScheme of ["light", "dark"]) {
  test(`onboarding help explains setup and enrollment at ${width}px in ${colorScheme}`, async ({ browser }, testInfo) => {
    const context = await browser.newContext({ viewport: { width, height: 900 }, colorScheme, hasTouch: width === 390, reducedMotion: "reduce" });
    const page = await context.newPage();
    const tap = width === 390 ? "tap" : "click";
    await page.goto(new URL(`/projects/proj_alpha/setup?theme=${colorScheme}`, origin).href);
    const steps = page.getByRole("list", { name: "Setup steps" });
    await expect(steps.getByRole("button").nth(0)).toContainText("Runner");
    await steps.getByRole("button", { name: /^.*Repository (ready|action required)$/ }).click();
    const repository = page.getByRole("combobox", { name: "Search or type owner/name" });
    await expect(repository).toHaveValue("");
    await expect(page.getByRole("button", { name: "Use owner/name", exact: true })).toBeDisabled();
    await repository.fill("example-");
    await expect(page.getByRole("option", { name: /example-org\/example-project/ })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath(`repository-list-${width}.png`) });
    await repository.fill("example-org/unlisted");
    await expect(page.getByRole("option", { name: "Use example-org/unlisted as typed" })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath(`repository-typed-${width}.png`) });
    await page.getByRole("button", { name: "Not listed?", exact: true }).click();
    await expect(page.getByText("Type owner/name even if it is not listed.", { exact: false })).toBeVisible();
    await expect(page.getByRole("button", { name: "Use example-org/unlisted", exact: true })).toBeEnabled();
    await steps.getByRole("button", { name: /Checkout/ }).click();
    await expect(page.getByRole("checkbox")).toHaveCount(0);
    let configurationMissing = false;
    await page.route("**/projects/proj_alpha/onboarding", async (route) => {
      if (route.request().method() !== "GET") { await route.continue(); return; }
      const response = await route.fetch();
      const data = await response.json();
      const at = new Date(Date.now() - 12_000).toISOString();
      await route.fulfill({ response, json: { ...data, runners: data.runners.slice(0, 1).map((entry) => ({ ...entry, runner: { ...entry.runner, display_name: "Example runner", connection_health: "online" }, local_checks: { checkout: "passed", configuration: configurationMissing ? "missing" : "passed", doctor: configurationMissing ? "pending" : "running", provider: "pending", observed_at: at } })), steps: data.steps.map((step) => step.name === "Checkout" ? { ...step, state: "action_required" } : step) } });
    });
    await page.getByRole("button", { name: "Refresh runner checks" }).click();
    const checks = page.getByRole("list", { name: "Runner checks" });
    await expect(checks.getByText("Running", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Continue", exact: true })).toBeDisabled();
    for (const name of ["Back", "Continue in background", "Continue"]) {
      const button = page.getByRole("button", { name, exact: true });
      await button.scrollIntoViewIfNeeded();
      await expect(button).toBeInViewport({ ratio: 1 });
    }
    await page.screenshot({ path: testInfo.outputPath(`checkout-running-${width}.png`) });
    configurationMissing = true;
    await page.getByRole("button", { name: "Refresh runner checks" }).click();
    await expect(checks.getByText("detent.yaml and WORKFLOW.md not found")).toBeVisible();
    await expect(page.getByRole("radio", { name: "Create starter files on Example runner" })).toBeVisible();
    await expect(page.getByRole("radio", { name: "I’ll add them myself" })).toBeVisible();
    await expect(page.getByText(/restart/i)).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath(`checkout-missing-${width}.png`) });

    await steps.getByRole("button", { name: /Runner (ready|action required)/ }).click();
    await expect(page.getByRole("group", { name: "Computer for this project" })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath(`runner-${width}.png`) });
    const enroll = page.getByRole("button", { name: "Enroll a runner", exact: true });
    await expect(page.getByRole("region", { name: "Import GitHub issues", exact: true })).toHaveCount(0);
    await enroll.click();
    const dialog = page.getByRole("region", { name: "Enroll a runner", exact: true });
    await expect(page.getByText("Reading runners before creating a command…", { exact: true })).toHaveCount(0);
    await expect(dialog.getByRole("button", { name: "Create command", exact: true })).toBeDisabled();
    await dialog.getByRole("radio", { name: "Sandbox", exact: true }).check();
    await expect(dialog.getByRole("button", { name: "Create command", exact: true })).toBeEnabled();
    await dialog.evaluate((element) => Promise.all(element.getAnimations({ subtree: true }).map((animation) => animation.finished)));
    const capacity = page.getByLabel("Concurrency", { exact: true });
    await expect(capacity).toHaveValue("1");
    await capacity.fill("6");
    await help(page, "Concurrency", "it does not enroll six runners", width === 1280 ? "hover" : tap);
    await help(page, "Concurrency", "Start with 1", width === 1280 ? "focus" : tap);
    await help(page, "Concurrency", "limits can lower effective concurrency", tap, testInfo.outputPath(`capacity-help-${width}.png`));
    await expect(capacity).toHaveValue("6");
    const projects = page.locator('[data-slot="checkbox"][id^="enroll-project-"]');
    const projectState = await projects.evaluateAll((elements) => elements.map((element) => element.getAttribute("data-checked")));
    await help(page, "Projects", "access and routing scope", tap);
    expect(await projects.evaluateAll((elements) => elements.map((element) => element.getAttribute("data-checked")))).toEqual(projectState);
    const nameBounds = await dialog.getByLabel("Name", { exact: true }).boundingBox();
    const capacityBounds = await capacity.boundingBox();
    expect(nameBounds).not.toBeNull();
    expect(capacityBounds).not.toBeNull();
    if (width === 1280) {
      expect(Math.abs(nameBounds.y - capacityBounds.y), "desktop inputs share a top edge").toBeLessThanOrEqual(1);
      expect(capacityBounds.x).toBeGreaterThan(nameBounds.x + nameBounds.width);
      const labelLines = await dialog.locator('label[for="enroll-runner-capacity"]').evaluate((label) => {
        const range = document.createRange();
        range.selectNodeContents(label);
        return range.getClientRects().length;
      });
      expect(labelLines, "the concurrency label stays on one line").toBe(1);
    } else {
      expect(capacityBounds.y).toBeGreaterThan(nameBounds.y + nameBounds.height);
      expect(capacityBounds.x).toBe(nameBounds.x);
      expect(capacityBounds.width).toBe(nameBounds.width);
    }
    await page.setViewportSize({ width, height: 2600 });
    await page.getByRole("heading", { level: 1 }).scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath(`enrollment-${width}.png`), fullPage: true });
    await page.setViewportSize({ width, height: 900 });
    await capacity.fill("0");
    await help(page, "Concurrency", "independent work items concurrently", tap);
    await expect(page.getByText("A whole number from 1 to 16")).toBeVisible();
    await expect(page.getByRole("button", { name: "Create command" })).toBeDisabled();
    await capacity.fill("1");
    await dialog.getByLabel("Name", { exact: true }).fill("Example build runner");
    await dialog.getByRole("button", { name: "Create command", exact: true }).click();
    await expect(dialog.getByRole("status")).toContainText("Waiting for Example build runner to check in");
    await page.setViewportSize({ width, height: 1800 });
    await page.getByRole("heading", { level: 1 }).scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath(`enrollment-command-${width}.png`), fullPage: true });
    await page.setViewportSize({ width, height: 900 });
    await page.getByRole("button", { name: "Use an existing runner", exact: true }).click();

    await steps.getByRole("button", { name: /Run files/ }).click();
    await page.screenshot({ path: testInfo.outputPath(`run-files-${width}.png`) });
    await help(page, "Keep them on Example runner", "losing it also loses those files", tap);
    await help(page, "Your bucket and gateway", "you run and pay for", tap);
    await page.getByRole("radio", { name: "Your bucket and gateway", exact: true }).check();
    await help(page, "Service id", "does not create storage or check that it works", tap);
    await help(page, "Gateway origin", "not the S3 bucket URL", tap);
    await help(page, "Publisher token id", "not the secret token itself", tap, testInfo.outputPath(`publisher-help-${width}.png`));
    await page.screenshot({ path: testInfo.outputPath(`setup-${width}.png`) });
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    await steps.getByRole("button", { name: /^.*Repository (ready|action required)$/ }).click();
    await repository.fill("mockorg/alpha");
    await page.getByRole("button", { name: "Use mockorg/alpha", exact: true }).click();
    await steps.getByRole("button", { name: /Checkout/ }).click();
    await expect(page.getByText("Configuration files", { exact: true })).toBeVisible();
    await expect(page.getByRole("radio", { name: "Create starter files on Example runner" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Advanced policy details", exact: true })).toHaveCount(0);
    await expect(page.getByLabel("Resolved policy descriptor", { exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Approve settings", exact: true })).toHaveCount(0);
    await page.route("**/projects/proj_alpha/onboarding", async (route) => {
      const response = await route.fetch();
      const onboarding = await response.json();
      await route.fulfill({ json: { ...onboarding, ready: true, progress: { ...onboarding.progress, artifacts: "local" }, steps: onboarding.steps.map(step => ({ ...step, state: "ready" })) } });
    });
    await page.reload();
    await expect(page.getByRole("heading", { name: "alpha is ready for work", exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath(`ready-${width}.png`) });
    await page.getByRole("button", { name: "Import issues", exact: true }).click();
    await expect(page.getByRole("region", { name: "Import GitHub issues", exact: true })).toBeVisible();
    await context.close();
  });
}
}
