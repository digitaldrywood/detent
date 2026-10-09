const { test, expect } = require("@playwright/test");
const { spawn } = require("node:child_process");
const path = require("node:path");

let server;
let origin;

test.beforeAll(async () => {
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

test.afterAll(async () => {
  if (server?.exitCode === null) {
    await new Promise((resolve) => { server.once("exit", resolve); server.stdin.end(); });
  }
});

async function help(page, label, content, interaction = "click", screenshot) {
  const trigger = page.getByRole("button", { name: `Help for ${label}`, exact: true }).first();
  if (interaction === "hover") await trigger.hover();
  else if (interaction === "focus") {
    // Actual keyboard focus, including the focus-visible browser behavior.
    await page.getByLabel(label, { exact: true }).focus();
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
  test(`onboarding help explains setup and enrollment at ${width}px`, async ({ browser }, testInfo) => {
    const context = await browser.newContext({ viewport: { width, height: 900 }, hasTouch: width === 390, reducedMotion: "reduce" });
    const page = await context.newPage();
    const tap = width === 390 ? "tap" : "click";
    await page.goto(new URL("/projects/proj_alpha/setup", origin).href);
    const steps = page.getByRole("list", { name: "Setup steps" });
    await expect(steps.getByRole("button").nth(0)).toContainText("Execution runner");
    await steps.getByRole("button").nth(2).click();
    await help(page, "Use an existing repository", "This saves your setup choice", width === 1280 ? "hover" : tap);
    await expect(page.getByRole("radio", { name: "Use an existing repository" })).toBeChecked();
    await help(page, "Generate the configuration", "Associate does not generate files", tap);
    await expect(page.getByRole("radio", { name: "Generate the configuration" })).not.toBeChecked();
    await help(page, "Associate the runner checkout", "GitHub API integration is optional", width === 1280 ? "focus" : tap);
    await page.getByLabel("Associate the runner checkout", { exact: true }).fill("owner/repository");
    await help(page, "Resolved policy descriptor", "policy mismatch prevents claims", tap);
    await help(page, "Approve the resolved policy", "runners must resolve a matching policy", tap);

    await steps.getByRole("button").nth(1).click();
    await expect(page.getByRole("checkbox")).toHaveCount(0);
    await help(page, "Project checkout and configuration", "execution host", tap);
    await help(page, "detent doctor", "not a browser confirmation", tap);
    await help(page, "Provider sign-in", "Credentials and account details stay local", tap);

    await steps.getByRole("button").nth(0).click();
    await help(page, "Runner eligibility", "runner-reported heartbeat and capacity", tap);
    await help(page, "Route here", "does not fix a stale heartbeat", tap);
    const enroll = page.getByRole("button", { name: "Enroll a runner", exact: true });
    const intake = page.getByRole("region", { name: "Import GitHub issues", exact: true });
    await expect(intake).toBeVisible();
    expect(await enroll.evaluate(element => Boolean(element.compareDocumentPosition(document.querySelector('[aria-label="Import GitHub issues"]')) & Node.DOCUMENT_POSITION_FOLLOWING))).toBe(true);
    await enroll.click();
    const dialog = page.getByRole("dialog", { name: "Enroll a runner", exact: true });
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
    await dialog.screenshot({ path: testInfo.outputPath(`enrollment-${width}.png`) });
    await capacity.fill("0");
    await help(page, "Concurrency", "independent work items concurrently", tap);
    await expect(page.getByText("A whole number from 1 to 16")).toBeVisible();
    await expect(page.getByRole("button", { name: "Create command" })).toBeDisabled();
    await page.getByRole("button", { name: "Cancel", exact: true }).click();

    await steps.getByRole("button").nth(3).click();
    await help(page, "Your runner (local history)", "Detent stores no artifacts", tap);
    await help(page, "Your bucket and gateway", "you run and pay for", tap);
    await page.getByRole("radio", { name: "Your bucket and gateway", exact: true }).check();
    await help(page, "Service id", "does not provision or verify storage", tap);
    await help(page, "Gateway origin", "not the S3 bucket URL", tap);
    await help(page, "Publisher token id", "Hub checks that grant", tap, testInfo.outputPath(`publisher-help-${width}.png`));
    await page.screenshot({ path: testInfo.outputPath(`setup-${width}.png`) });
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    await context.close();
  });
}
