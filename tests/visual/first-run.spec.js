// The first-run experience of a new organization's Work board, against the
// real hosted hub `hosted-hub.js` starts.
//
// The preview seeds two projects, so the empty organization is the same hub
// with the bootstrap's project list emptied in the browser: every screen that
// reads projects reads them from that payload. Creating a project lifts the
// override, and from there the flow runs against the hub's own state.
const fs = require("node:fs");
const path = require("node:path");
const { test, expect } = require("@playwright/test");
const AxeBuilder = require("@axe-core/playwright").default;
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("first-run");
});

test.afterAll(async () => {
  await hub?.stop();
  hub = undefined;
});

const EVIDENCE = path.join(process.cwd(), "tmp", "playwright-evidence", "first-run");
const UNSERVED = [
  /\/api\/v2\/organizations\/[^/]+\/projects\/[^/]+\/actions$/,
  /\/app\/updates$/,
];
const NOT_FOUND = /status of 404\b/;

function watchConsole(page) {
  const errors = [];
  page.on("console", (message) => {
    if (message.type() !== "error") return;
    const url = message.location().url ?? "";
    if (NOT_FOUND.test(message.text()) && UNSERVED.some((pattern) => pattern.test(url))) return;
    errors.push(message.text());
  });
  page.on("pageerror", (error) => errors.push(String(error)));
  return errors;
}

async function evidence(page, name) {
  fs.mkdirSync(EVIDENCE, { recursive: true });
  await page.waitForFunction(() =>
    document.getAnimations().every((animation) => animation.playState !== "running"),
  );
  await page.screenshot({ path: path.join(EVIDENCE, `${name}.png`), animations: "disabled" });
}

const BOOTSTRAP = "**/app/bootstrap";
const CONVERSATIONS = /\/api\/v2\/organizations\/[^/]+\/conversations(\?|$)/;

/**
 * Serves the bootstrap with no projects and the conversation list empty, as a
 * new owner's organization has them.
 */
async function emptyOrganization(page) {
  await page.route(BOOTSTRAP, async (route) => {
    const response = await route.fetch();
    const payload = await response.json();
    await route.fulfill({ response, json: { ...payload, projects: [] } });
  });
  await page.route(CONVERSATIONS, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    await route.fulfill({ json: { conversations: [], next_cursor: null } });
  });
}

async function restoreOrganization(page) {
  await page.unroute(BOOTSTRAP);
  await page.unroute(CONVERSATIONS);
}

async function openEmptyWork(page) {
  await emptyOrganization(page);
  await page.goto(hub.fixture.accounts.owner, { waitUntil: "domcontentloaded" });
  await page.goto(new URL("/work", hub.fixture.url).toString(), {
    waitUntil: "domcontentloaded",
  });
  await expect(page.getByTestId("first-run")).toBeVisible();
}

async function expectNoSeriousAxeViolations(page, label) {
  const results = await new AxeBuilder({ page }).analyze();
  const serious = results.violations.filter(
    (violation) => violation.impact === "serious" || violation.impact === "critical",
  );
  expect(
    serious.map((violation) => `${violation.id}: ${violation.nodes[0]?.target?.join(" ")}`),
    `serious or critical axe violations on ${label}`,
  ).toEqual([]);
}

test("an empty organization's Work board offers the first-run steps", async ({ page }) => {
  const errors = watchConsole(page);
  await openEmptyWork(page);

  const panel = page.getByTestId("first-run");
  await expect(panel.getByRole("heading", { name: "Set up your organization" })).toBeVisible();
  await expect(page.getByTestId("first-run-step-project")).toHaveAttribute("data-done", "false");
  await expect(
    page.getByTestId("first-run-step-runner").getByRole("button", { name: "Enroll a runner" }),
  ).toBeDisabled();
  await expect(
    page.getByTestId("first-run-step-issue").getByRole("button", { name: "New issue" }),
  ).toBeDisabled();
  await expect(page.getByText("Every lane is hidden")).toHaveCount(0);

  const sidebar = page.locator("aside.dc-side");
  await expect(sidebar.getByText("No projects yet")).toBeVisible();
  await expect(sidebar.getByRole("button", { name: "New project" })).toBeVisible();

  await expectNoSeriousAxeViolations(page, "the empty Work board");
  await evidence(page, "empty-work-desktop");
  expect(errors).toEqual([]);
});

test("the palette opens the dialog in place and hides checkout commands", async ({ page }) => {
  const errors = watchConsole(page);
  await openEmptyWork(page);

  await page.keyboard.press("ControlOrMeta+k");
  const palette = page.getByTestId("command-palette");
  await expect(palette).toBeVisible();
  await expect(palette.getByText("Go to file")).toHaveCount(0);
  await expect(palette.getByText("Search project contents")).toHaveCount(0);

  await palette.getByText("Add project").click();
  const dialog = page.getByRole("dialog", { name: "New project" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByLabel("Name")).toBeFocused();
  await expect(page).toHaveURL(/\/work$/);
  expect(errors).toEqual([]);
});

test("the sidebar's empty Projects section opens the same dialog", async ({ page }) => {
  await openEmptyWork(page);
  await page.locator("aside.dc-side").getByRole("button", { name: "New project" }).click();
  await expect(page.getByRole("dialog", { name: "New project" })).toBeVisible();
});

test("creating the first project lands on its board and the checklist advances", async ({
  page,
}) => {
  const errors = watchConsole(page);
  await openEmptyWork(page);

  await page.getByTestId("first-run-step-project").getByRole("button", { name: "New project" }).click();
  const dialog = page.getByRole("dialog", { name: "New project" });
  await dialog.getByLabel("Name").fill("Example Studio");
  await evidence(page, "new-project-dialog-desktop");

  await restoreOrganization(page);
  await dialog.getByRole("button", { name: "Create project" }).click();

  await expect(page).toHaveURL(/\/work\/p\/[^/]+$/);
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Example Studio");
  await expect(page.getByTestId("first-run-step-project")).toHaveAttribute("data-done", "true");
  await expect(page.getByTestId("first-run-progress")).toContainText("of 4 done");
  await expect(page.getByTestId("work-board")).toHaveCount(0);
  // Creating a project grants its owner runner management, so the runner step
  // offers enrollment directly.
  await expect(
    page.getByTestId("first-run-step-runner").getByRole("button", { name: "Enroll a runner" }),
  ).toBeEnabled();

  await page.getByTestId("first-run-step-issue").getByRole("button", { name: "New issue" }).click();
  const issue = page.getByRole("dialog", { name: "New issue" });
  await issue.getByLabel("Title").fill("Write the welcome page");
  await issue.getByRole("button", { name: "Create issue" }).click();
  await expect(issue).toHaveCount(0);

  await expect(page.getByText("Write the welcome page").first()).toBeVisible();
  await expect(page.getByTestId("first-run")).toHaveCount(0);
  expect(errors).toEqual([]);
});

test.describe("on a phone", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("the first-run steps and the dialog fit the screen", async ({ page }) => {
    const errors = watchConsole(page);
    await openEmptyWork(page);

    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(0);
    await evidence(page, "empty-work-mobile");

    await page.getByTestId("first-run-step-project").getByRole("button", { name: "New project" }).click();
    const dialog = page.getByRole("dialog", { name: "New project" });
    await expect(dialog).toBeVisible();
    await dialog.getByLabel("Name").fill("Example Studio");
    await expect(dialog.getByRole("button", { name: "Create project" })).toBeInViewport();
    await evidence(page, "new-project-dialog-mobile");
    expect(errors).toEqual([]);
  });
});
