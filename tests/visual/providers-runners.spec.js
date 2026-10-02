const { test, expect } = require("@playwright/test");
const { providersRunnersPreview } = require("./providers-runners-preview");
const fleetFixture = require("../../web/conversation/src/contracts/fixtures/account-fleet.json");

let html;
const origin = "https://runners.detent.test";
const problem = { code: "tier_unavailable", message: "Sandbox tooling is unavailable", fix_hint: "Repair the sandbox tooling", first_seen: "2026-09-10T12:00:00Z" };
const fleet = {
  ...fleetFixture,
  runners: [
    { ...fleetFixture.runners[0], claim_refusal_reason: "", problems: [problem] },
    { ...fleetFixture.runners[1], display_name: "Build runner", health: "needs_attention", host_capacity: 4, claim_refusal_reason: "", problems: [problem] },
    { ...fleetFixture.runners[1], id: "runner_outside", display_name: "Night runner", health: "outside_hours", host_capacity: 1, claim_refusal_reason: "", problems: [problem] },
  ],
};

test.beforeAll(async () => { html = await providersRunnersPreview(); });

async function openFleet(page, data = fleet, search = "", spritesPresent = false) {
  await page.clock.setFixedTime(new Date("2026-09-10T12:09:31Z"));
  await page.route(origin + "/**", (route) => {
    if (new URL(route.request().url()).pathname.endsWith("/secrets/fly_sprites_token")) {
      return route.fulfill({ json: { kind: "fly_sprites_token", present: spritesPresent } });
    }
    if (new URL(route.request().url()).pathname.endsWith("/runner-enrollments")) {
      return route.fulfill({ status: 201, json: { id: "enrollment_build", token: "det_enroll_preview", expires_at: "2026-09-10T12:24:31Z" } });
    }
    return route.fulfill({ contentType: "text/html", body: html });
  });
  await page.route("**/fleet", (route) => route.fulfill({ json: data }));
  await page.goto(new URL("/settings/runners" + search, origin).href);
  await expect(page.getByRole("heading", { name: "Providers & runners", exact: true })).toBeVisible();
}

for (const width of [1440, 390]) {
  for (const location of ["machine", "sprite", "sprite without token"]) {
    test(`enrollment on ${location} at ${width}px`, async ({ page, context }) => {
      await page.setViewportSize({ width, height: 1100 });
      await context.grantPermissions(["clipboard-read", "clipboard-write"]);
      await openFleet(page, fleet, "", location === "sprite");
      await page.getByRole("button", { name: "Enroll a runner", exact: true }).click();
      const dialog = page.getByRole("dialog", { name: "Enroll a runner", exact: true });
      const sprite = location.startsWith("sprite");
      if (sprite) {
        await dialog.getByLabel("A Fly Sprite").check();
        await expect(dialog.getByLabel("Name", { exact: true })).toHaveValue(/^detent-[a-z0-9]+$/);
        await expect(dialog.getByText(/The Sprite gets the same name/)).toBeVisible();
        if (location === "sprite without token") {
          await expect(dialog.getByText(/No Sprites token is set/)).toBeVisible();
          await expect(dialog.getByRole("link", { name: "Set a Sprites token" })).toHaveAttribute("href", "/settings/integrations?project=proj_preview#sprites");
        } else {
          await expect(dialog.getByText("Checking Sprites tokens…")).toHaveCount(0);
          await expect(dialog.getByRole("link", { name: "Set a Sprites token" })).toHaveCount(0);
        }
      } else {
        await expect(dialog.getByLabel("A machine I run")).toBeChecked();
      }
      const name = sprite ? "build-sprite" : "Build machine";
      await dialog.getByLabel("Name", { exact: true }).fill(name);
      await dialog.getByRole("button", { name: "Create command" }).click();
      const copy = dialog.getByRole("button", { name: "Copy the register command" });
      await expect(copy).toBeVisible();
      await expect(dialog).toContainText("detent_••••••••");
      expect(await dialog.innerHTML()).not.toContain("det_enroll_preview");
      if (sprite) {
        const instructions = dialog.getByRole("list", { name: "Sprite setup instructions" });
        await expect(instructions.getByRole("listitem")).toHaveCount(8);
        await expect(instructions).toContainText("sprite create build-sprite");
        await expect(instructions).toContainText("sprite console -s build-sprite");
        await expect(instructions).toContainText("/v0.9.1/scripts/sprite-runner-bootstrap.sh");
        await expect(instructions).toContainText("bash sprite-runner-bootstrap.sh --version v0.9.1");
        await expect(instructions).toContainText("claude auth login");
        await expect(instructions).toContainText("codex login --device-auth");
        await expect(instructions).toContainText("Approve the repository policy");
        await expect(dialog.getByRole("button", { name: "Show token" })).toHaveCount(0);
      }
      await copy.click();
      const copied = await page.evaluate(() => navigator.clipboard.readText());
      expect(copied).toContain("--token det_enroll_preview");
      expect(copied).toContain(sprite ? "--name build-sprite" : "--name 'Build machine'");
      expect(copied.includes("--service")).toBe(!sprite);
      expect(await dialog.innerHTML()).not.toContain("det_enroll_preview");
      expect(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
      await page.unroute("**/fleet");
      await page.route("**/fleet", (route) => route.fulfill({ json: { ...fleet, runners: [...fleet.runners, { ...fleetFixture.runners[0], id: "runner_new", display_name: name, hostname: sprite ? name : "build-machine", health: "healthy" }] } }));
      await expect(dialog.getByText(`${name} is connected`)).toBeVisible();
      await expect(dialog.getByText("Sleeps when idle; the Hub wakes it for new work")).toHaveCount(location === "sprite" ? 1 : 0);
      await dialog.getByRole("button", { name: "Done" }).click();
      await expect(dialog).toHaveCount(0);
    });
  }
  test(`sleeping Sprite has no warning at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1100 });
    await openFleet(page, { ...fleet, runners: [{ ...fleetFixture.runners[1], health: "asleep", problems: [], claim_refusal_reason: "", sprite: { name: "build-sprite", status: "warm", can_wake: true, wake_failed: false } }] });
    await expect(page.getByText("Asleep, wakes on new work")).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expect(page.getByText(/slots can't take work/)).toHaveCount(0);
    await expect(page.getByRole("link", { name: "Needs attention 0" })).toBeVisible();
  });
  test("capacity, attention, providers and URL filter at " + width + "px", async ({ page }) => {
    await page.setViewportSize({ width, height: 1100 });
    await openFleet(page);
    await expect(page.getByText("1 of 7 slots running work")).toBeVisible();
    await expect(page.getByText("4 slots can't take work")).toBeVisible();
    const groups = page.getByTestId("runner-capacity");
    await expect(groups).toHaveCount(fleet.runners.length);
    for (const [index, runner] of fleet.runners.entries()) {
      const group = groups.nth(index);
      await expect(group.locator("[data-slot-state]")).toHaveCount(runner.host_capacity);
      await expect(group.locator('[data-slot-state="running"]')).toHaveCount(runner.leases.length);
      for (const lease of runner.leases) {
        await expect(group.locator('[data-slot-state="running"]').first()).toHaveAttribute("title", "#" + lease.work_item_id + " " + lease.title);
      }
    }
    await expect(groups.nth(1).locator('[data-slot-state="unavailable"]')).toHaveCount(4);
    await expect(page.getByRole("alert")).toHaveCount(1);
    await expect(page.getByTestId("runner-attention")).toContainText("Build runner can't take work");
    await expect(page.getByTestId("runner-attention")).toContainText(problem.message);
    await expect(page.getByTestId("runner-attention")).toContainText(problem.fix_hint);
    await expect(page.getByText(problem.message, { exact: true })).toHaveCount(1);
    const providers = page.getByRole("region", { name: "Provider accounts" });
    await expect(providers.getByRole("columnheader")).toHaveText(["Provider", "Accounts", "In use", "Models", "Status"]);
    await expect(providers.getByText("Rate limited until Sep 10, 5:00 PM")).toBeVisible();
    await expect(page.getByTestId("host-card")).toHaveCount(3);
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    expect(await page.locator("[data-settings-page-scroll]").evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
    if (width === 390) {
      await providers.scrollIntoViewIfNeeded();
      expect(await providers.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true);
      await providers.evaluate((element) => { element.scrollLeft = element.scrollWidth; });
      await expect(providers.getByText("Rate limited until Sep 10, 5:00 PM")).toBeInViewport();
      await providers.evaluate((element) => { element.scrollLeft = 0; });
    }
    await page.evaluate(() => document.fonts.ready);
    await page.setViewportSize({ width, height: 2400 });
    await page.locator("[data-settings-page-scroll]").evaluate((element) => { element.scrollTop = 0; });
    await expect(page.locator("[data-settings-page-scroll] > div")).toHaveScreenshot("providers-runners-" + width + ".png");
    await page.setViewportSize({ width, height: 1100 });
    await page.getByRole("link", { name: "Needs attention 1" }).click();
    await expect(page).toHaveURL(/health=needs_attention/);
    await expect(page.getByTestId("host-card")).toHaveCount(1);
    await expect(groups).toHaveCount(3);
    await page.reload();
    await expect(page.getByRole("link", { name: "Needs attention 1" })).toHaveAttribute("aria-current", "true");
    await expect(page.getByTestId("host-card")).toHaveCount(1);
    await page.getByRole("button", { name: "Open Michael's MacBook Pro", exact: true }).click();
    await expect(page.getByTestId("host-card")).toHaveCount(3);
    await expect(page.locator("#runner-rnr_macbook")).toBeFocused();
    await expect(page).not.toHaveURL(/health=needs_attention/);
    await page.getByRole("link", { name: "Needs attention 1" }).click();
    await page.getByRole("link", { name: "All 3" }).click();
    await expect(page).not.toHaveURL(/health=needs_attention/);
    await expect(page.getByTestId("host-card")).toHaveCount(3);
    await page.getByRole("button", { name: "Manage Michael's MacBook Pro" }).click();
    const details = page.getByRole("dialog", { name: "Michael's MacBook Pro" });
    await expect(details).toContainText("Isolation setting: Sandbox");
    await expect(details).toContainText("michael@threefold.solutions");
    await page.keyboard.press("Escape");
    await page.getByRole("button", { name: "Enroll a runner", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Enroll a runner", exact: true });
    await dialog.getByLabel("Name", { exact: true }).fill("New build runner");
    await dialog.getByRole("button", { name: "Create command" }).click();
    const command = await dialog.locator("code").textContent();
    const token = command.match(/--token (\S+)/)[1];
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(page.getByRole("heading", { name: "Waiting to connect" })).toBeVisible();
    expect(await page.locator("body").innerHTML()).not.toContain(token);
    await page.getByRole("button", { name: "Make a new command" }).click();
    await expect(page.getByLabel("Name", { exact: true })).toHaveValue("New build runner");
    await expect(page.getByRole("button", { name: "Create command" })).toBeVisible();
    expect(await page.locator("body").innerHTML()).not.toContain(token);
    await page.keyboard.press("Escape");
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
  });
}

test("empty fleet invites enrollment and version refusals keep the upgrade command", async ({ page }) => {
  await openFleet(page, { ...fleet, runners: [] });
  await expect(page.getByText("No runners yet. Enroll a machine to start taking work.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Enroll a runner", exact: true })).toHaveCount(1);
  await expect(page.getByTestId("runner-capacity")).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Runners", exact: true })).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Capacity right now", exact: true })).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.unroute("**/fleet");
  await page.route("**/fleet", (route) => route.fulfill({ json: { ...fleet, runners: [fleetFixture.runners[0]] } }));
  await page.reload();
  await expect(page.getByTestId("host-update")).toContainText("Too old to take work, needs v0.9.1");
  await expect(page.getByTestId("host-update")).toContainText("detent");
  await expect(page.getByTestId("runner-attention")).toHaveCount(0);
});

test("the attention link preserves unrelated search parameters", async ({ page }) => {
  await openFleet(page, fleet, "?source=capacity&health=needs_attention");
  await expect(page.getByTestId("host-card")).toHaveCount(1);
  await page.getByRole("link", { name: "All 3" }).click();
  await expect(page).toHaveURL(/source=capacity$/);
  await page.getByRole("link", { name: "Needs attention 1" }).click();
  await expect(page).toHaveURL(/source=capacity&health=needs_attention$/);
});
