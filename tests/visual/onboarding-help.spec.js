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
  const trigger = page.getByRole("button", { name: `Help for ${label}`, exact: true });
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
    await steps.getByRole("button").nth(0).click();
    await help(page, "Use an existing repository", "This saves your setup choice", width === 1280 ? "hover" : tap);
    await expect(page.getByRole("radio", { name: "Use an existing repository" })).toBeChecked();
    await help(page, "Generate the configuration", "Attach does not generate files", tap);
    await expect(page.getByRole("radio", { name: "Generate the configuration" })).not.toBeChecked();
    await help(page, "Attach a GitHub repository", "GitHub integration", width === 1280 ? "focus" : tap);
    await page.getByLabel("Attach a GitHub repository", { exact: true }).fill("owner/repository");
    await help(page, "Resolved policy descriptor", "policy mismatch prevents claims", tap);
    await help(page, "Approve the resolved policy", "runners must resolve a matching policy", tap);

    await steps.getByRole("button").nth(1).click();
    const doctor = page.getByRole("checkbox", { name: /^detent doctor passes on the execution host/ });
    const checked = await doctor.isChecked();
    await help(page, "detent doctor passes on the execution host", "Hub stores your report", tap);
    expect(await doctor.isChecked()).toBe(checked);
    await help(page, "Signed in to the provider on the execution host", "does not test the sign-in", tap);

    await steps.getByRole("button").nth(2).click();
    await help(page, "Runner eligibility", "runner-reported heartbeat and capacity", tap);
    await help(page, "Route here", "does not fix a stale heartbeat", tap);
    await page.getByRole("button", { name: "Enroll a runner", exact: true }).click();
    const capacity = page.getByLabel("Runs at once", { exact: true });
    await capacity.fill("6");
    await help(page, "Runs at once", "it does not enroll six runners", width === 1280 ? "hover" : tap);
    await help(page, "Runs at once", "Start with 1", width === 1280 ? "focus" : tap);
    await help(page, "Runs at once", "limits can lower effective concurrency", tap, testInfo.outputPath(`capacity-help-${width}.png`));
    await expect(capacity).toHaveValue("6");
    const projects = page.locator('[data-slot="checkbox"][id^="enroll-project-"]');
    const projectState = await projects.evaluateAll((elements) => elements.map((element) => element.getAttribute("data-checked")));
    await help(page, "Projects", "access and routing scope", tap);
    expect(await projects.evaluateAll((elements) => elements.map((element) => element.getAttribute("data-checked")))).toEqual(projectState);
    await page.screenshot({ path: testInfo.outputPath(`enrollment-${width}.png`) });
    await capacity.fill("0");
    await help(page, "Runs at once", "distinct jobs concurrently", tap);
    await expect(page.getByText("A whole number from 1 to 16")).toBeVisible();
    await expect(page.getByRole("button", { name: "Create command" })).toBeDisabled();
    await page.getByRole("button", { name: "Cancel", exact: true }).click();

    await steps.getByRole("button").nth(3).click();
    await help(page, "Local history", "execution host that produced them", tap);
    await help(page, "Customer service", "does not test storage", tap);
    await page.getByRole("radio", { name: "Customer service", exact: true }).check();
    await help(page, "Service id", "does not provision or verify storage", tap);
    await help(page, "Gateway origin", "not the S3 bucket URL", tap);
    await help(page, "Publisher token id", "Hub checks that grant", tap, testInfo.outputPath(`publisher-help-${width}.png`));
    await page.screenshot({ path: testInfo.outputPath(`setup-${width}.png`) });
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    await context.close();
  });
}
