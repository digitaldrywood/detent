const { test, expect } = require("@playwright/test");
const { providersRunnersPreview } = require("./providers-runners-preview");
const fleetFixture = require("../../web/conversation/src/contracts/fixtures/account-fleet.json");
const spritePool = require("./providers-runners-sprite-pool.json");

let html;
const origin = "https://runners.detent.test";
const problem = { code: "tier_unavailable", message: "Sandbox tooling is unavailable", fix_hint: "Repair the sandbox tooling", first_seen: "2026-09-10T12:00:00Z" };
const fleet = {
  ...fleetFixture,
  runners: [
    { ...fleetFixture.runners[0], state: "active", health: "online", claim_refusal_reason: "", problems: [problem] },
    { ...fleetFixture.runners[1], display_name: "Build runner", health: "needs_attention", host_capacity: 4, claim_refusal_reason: "", problems: [problem] },
    { ...fleetFixture.runners[1], id: "runner_outside", machine_id: "machine_outside", display_name: "Night runner", health: "outside_hours", host_capacity: 1, claim_refusal_reason: "", problems: [problem] },
  ],
};

test.beforeAll(async () => { html = await providersRunnersPreview(); });

async function openFleet(page, data = fleet, search = "", spritesPresent = false, pool = spritePool) {
  await page.clock.setFixedTime(new Date("2026-09-10T12:09:31Z"));
  await page.route(origin + "/**", (route) => {
    if (new URL(route.request().url()).pathname.endsWith("/sprite-pool")) {
      return route.fulfill({ json: pool });
    }
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
      const platform = width === 1440 ? "MacIntel" : "Linux x86_64";
      await page.addInitScript((platform) => {
        Object.defineProperty(navigator, "platform", { value: platform });
        Object.defineProperty(navigator, "userAgent", { value: platform });
      }, platform);
      await openFleet(page, fleet, "", location === "sprite");
      await page.getByRole("button", { name: "Add runner", exact: true }).click();
      await page.getByRole("menuitem", { name: "Manual", exact: true }).click();
      const dialog = page.getByRole("dialog", { name: "Enroll a runner", exact: true });
      await expect(dialog.getByRole("button", { name: "Create command" })).toBeDisabled();
      await expect(dialog.getByRole("radio", { name: "Sandbox", exact: true })).not.toBeChecked();
      await expect(dialog.getByRole("radio", { name: "Full access", exact: true })).not.toBeChecked();
      const sprite = location.startsWith("sprite");
      const access = !sprite && width === 1440 ? "Sandbox" : "Full access";
      if (sprite) {
        await dialog.getByLabel("A Fly Sprite").check();
        await expect(dialog.getByLabel("Name", { exact: true })).toHaveValue(/^detent-[a-z0-9]+$/);
        await expect(dialog.getByText(/The Sprite gets the same name/)).toBeVisible();
        if (location === "sprite without token") {
          await expect(dialog.getByText(/No Sprites token is set for the organization/)).toBeVisible();
          await expect(dialog.getByRole("link", { name: "Set a Sprites token" })).toHaveAttribute("href", "/settings/integrations#sprites");
        } else {
          await expect(dialog.getByText("Checking Sprites tokens…")).toHaveCount(0);
          await expect(dialog.getByRole("link", { name: "Set a Sprites token" })).toHaveCount(0);
        }
      } else {
        await expect(dialog.getByLabel("A machine I run")).toBeChecked();
        await expect(dialog.getByRole("radio", { name: width === 1440 ? "macOS" : "Linux", exact: true })).toBeChecked();
        await expect(dialog).toContainText("This Hub requires Detent v0.9.1 or newer");
        await expect(dialog.getByRole("button", { name: "Copy the install command", exact: true })).toBeVisible();
      }
      const name = sprite ? "build-sprite" : "Build machine";
      await dialog.getByLabel("Name", { exact: true }).fill(name);
      await dialog.getByRole("radio", { name: access, exact: true }).check();
      if (!sprite) await page.screenshot({ animations: "disabled", path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `runner-access-enroll-${width}.png`) });
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
      expect(copied).toContain(`--isolation-tier ${access === "Sandbox" ? "sandbox" : "native-trusted"}`);
      expect(copied).toContain(sprite ? "--name build-sprite" : "--name 'Build machine'");
      expect(copied.includes("--service")).toBe(!sprite);
      if (!sprite) {
        await expect(dialog).toContainText("curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh");
        for (const os of ["macOS", "Linux"]) {
          await dialog.getByRole("radio", { name: os, exact: true }).check();
          await expect(dialog).toContainText(`Installs the latest release for ${os}`);
          await expect(dialog.getByRole("button", { name: "Copy the Homebrew command" })).toHaveCount(os === "macOS" ? 1 : 0);
          await dialog.getByRole("button", { name: /^(?:Copy|Copied) the install and register command$/ }).click();
          const combined = await page.evaluate(() => navigator.clipboard.readText());
          expect(combined).toBe(`curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh && export PATH="$HOME/.local/bin:/usr/local/bin:$PATH" && ${copied}`);
          expect(await dialog.innerHTML()).not.toContain("det_enroll_preview");
          await expect(dialog.getByRole("button", { name: "Copied the install and register command" })).toBeVisible();
        }
      }
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
    await openFleet(page, { ...fleet, runners: [{ ...fleetFixture.runners[1], state: "active", reported_capacity: 2, health: "asleep", problems: [], claim_refusal_reason: "", sprite: { name: "build-sprite", status: "warm", can_wake: true, wake_failed: false } }] }, "", true);
    await expect(page.getByText("Asleep, wakes on new work")).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expect(page.getByText(/slots can't take work/)).toHaveCount(0);
    await expect(page.getByRole("link", { name: "Needs attention 0" })).toBeVisible();
  });
  test("capacity, attention, providers and URL filter at " + width + "px", async ({ page }) => {
    await page.setViewportSize({ width, height: 1100 });
    await openFleet(page);
    await page.getByText("Fleet capacity and providers", { exact: true }).click();
    await expect(page.getByText("1 of 7 slots running work")).toBeVisible();
    await expect(page.getByText("5 slots can't take work")).toBeVisible();
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
    const buildRunner = page.getByTestId("host-card").filter({ has: page.getByRole("heading", { name: "Build runner", exact: true }) });
    await expect(buildRunner.getByText("Needs attention", { exact: true })).toBeVisible();
    const buildAlert = buildRunner.getByRole("alert");
    await expect(buildAlert).toContainText("Needs human: tier unavailable");
    await expect(buildAlert).toContainText(problem.message);
    await expect(buildAlert).toContainText(problem.fix_hint);
    await expect(page.getByRole("alert")).toHaveCount(fleet.runners.length);
    await page.getByRole("button", { name: "Manage Build runner" }).click();
    await expect(page.getByRole("dialog").getByRole("alert")).toContainText(problem.message);
    await expect(page.getByRole("dialog").getByRole("alert")).toContainText(problem.fix_hint);
    await page.keyboard.press("Escape");
    const providers = page.getByRole("region", { name: "Provider accounts" });
    await expect(providers.getByRole("columnheader")).toHaveText(["Provider", "Accounts", "In use", "Models", "Status"]);
    await expect(providers.getByText("Rate limited until Sep 10, 5:00 PM")).toBeVisible();
    await expect(page.getByTestId("host-card")).toHaveCount(3);
    await expect(page.getByText("Organization Sprite pool", { exact: true })).toBeVisible();
    const poolRow = page.getByTestId("sprite-pool-row");
    await expect(poolRow).toContainText(`Floor ${spritePool.min_runners} · Ceiling ${spritePool.max_runners} · Idle ${spritePool.idle_seconds}s · 1 members`);
    await expect(page.getByLabel("Minimum runners", { exact: true })).toHaveCount(0);
    await poolRow.getByRole("button", { name: /Configure pool/ }).click();
    const poolDialog = page.getByRole("dialog", { name: "Organization Sprite pool" });
    await expect(poolDialog.getByLabel("Minimum runners", { exact: true })).toHaveValue(String(spritePool.min_runners));
    await expect(poolDialog.getByLabel("Maximum runners", { exact: true })).toHaveValue(String(spritePool.max_runners));
    await expect(poolDialog.getByLabel("Idle seconds", { exact: true })).toHaveValue(String(spritePool.idle_seconds));
    await expect(poolDialog.getByLabel(/^Extra bootstrap/)).toHaveValue(spritePool.bootstrap);
    await poolDialog.getByText("Last bootstrap log", { exact: true }).click();
    await expect(poolDialog.getByText(spritePool.members[0].bootstrap_log, { exact: true })).toBeVisible();
    await page.keyboard.press("Escape");
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
    const scroll = page.locator("[data-settings-page-scroll]");
    const height = await scroll.evaluate((element) => Math.ceil(element.scrollHeight + element.getBoundingClientRect().top));
    await page.setViewportSize({ width, height: Math.max(2400, height) });
    await scroll.evaluate((element) => { element.scrollTop = 0; });
    await page.getByText("Fleet capacity and providers", { exact: true }).click();
    await test.info().attach("providers-runners-" + width + ".png", {
      body: await page.locator("[data-settings-page-scroll] > div").screenshot({ animations: "disabled", caret: "hide" }),
      contentType: "image/png",
    });
    await page.setViewportSize({ width, height: 1100 });
    await page.getByRole("link", { name: "Needs attention 2" }).click();
    await expect(page).toHaveURL(/health=needs_attention/);
    await expect(page.getByTestId("host-card")).toHaveCount(1);
    await expect(groups).toHaveCount(3);
    await page.reload();
    await expect(page.getByRole("link", { name: "Needs attention 2" })).toHaveAttribute("aria-current", "true");
    await expect(page.getByTestId("host-card")).toHaveCount(1);
    await page.getByText("Fleet capacity and providers", { exact: true }).click();
    await page.getByRole("button", { name: "Open Michael's MacBook Pro", exact: true }).click();
    await expect(page.getByTestId("host-card")).toHaveCount(3);
    await expect(page.getByRole("dialog", { name: "Michael's MacBook Pro" })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page).not.toHaveURL(/health=needs_attention/);
    await page.getByRole("link", { name: "Needs attention 2" }).click();
    await page.getByRole("link", { name: "All 3" }).click();
    await expect(page).not.toHaveURL(/health=needs_attention/);
    await expect(page.getByTestId("host-card")).toHaveCount(3);
    await page.getByRole("button", { name: "Manage Michael's MacBook Pro" }).click();
    const details = page.getByRole("dialog", { name: "Michael's MacBook Pro" });
    await expect(details.getByRole("heading", { name: "Agent access", exact: true })).toBeVisible();
    await expect(details).toContainText("Sandbox");
    await expect(details).toContainText("michael@threefold.solutions");
    await page.keyboard.press("Escape");
    await page.getByRole("button", { name: "Add runner", exact: true }).click();
      await page.getByRole("menuitem", { name: "Manual", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Enroll a runner", exact: true });
    await dialog.getByLabel("Name", { exact: true }).fill("New build runner");
    await dialog.getByRole("radio", { name: "Full access", exact: true }).check();
    await dialog.getByRole("button", { name: "Create command" }).click();
    await expect(dialog.getByRole("button", { name: "Copy the register command", exact: true })).toBeVisible();
    const command = await dialog.textContent();
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

for (const width of [1440, 390]) {
  test("runner sheet opens from rows and attention and saves at " + width + "px", async ({ page }) => {
    await page.setViewportSize({ width, height: 844 });
    const runner = {
      ...fleet.runners[1], editable: true, revision: 4, capacity_limit: 4, backend_isolation: { codex: ["native-trusted"] },
      routing: {
        display_name: "Build runner", state: "active", capacity_limit: 4,
        project_ids: ["proj_preview", "prj_unknown"],
        tags: ["linux"], isolation_tier: "sandbox", host_services: [],
        availability: { timezone: "UTC", windows: [], hard_deadline: "" },
      },
    };
    let current = { ...fleet, editable: true, runners: [runner] };
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await openFleet(page, current);
    await page.unroute("**/fleet");
    await page.route("**/fleet", (route) => route.fulfill({ json: current }));
    const writes = [];
    await page.route("**/runners/*/routing", async (route) => {
      const change = route.request().postDataJSON();
      writes.push(change);
      const { expected_revision, ...routing } = change;
      expect(expected_revision).toBe(current.runners[0].revision);
      current = { ...current, runners: [{ ...current.runners[0], state: routing.state, capacity_limit: routing.capacity_limit, routing, revision: expected_revision + 1 }] };
      await route.fulfill({ json: {} });
    });
    const row = page.getByTestId("host-card");
    await row.getByRole("button", { name: "Manage Build runner" }).click();
    let sheet = page.getByRole("dialog", { name: "Build runner" });
    await expect(sheet).toBeVisible();
    const box = await sheet.boundingBox();
    expect(box.x + box.width).toBe(width);
    if (width === 390) expect(box.width).toBe(width);
    await sheet.getByRole("radio", { name: "Draining Finishes what it has" }).check();
    await sheet.getByRole("spinbutton", { name: "Jobs at once", exact: true }).fill("2");
    await sheet.getByRole("button", { name: "Save runner" }).click();
    await expect(sheet).toHaveCount(0);
    await expect(row).toContainText("draining · Limit 2");
    await page.getByRole("button", { name: "Manage Build runner" }).click();
    sheet = page.getByRole("dialog", { name: "Build runner" });
    await expect(sheet.getByRole("alert")).toContainText(problem.message);
    await expect(sheet.getByRole("alert")).toContainText(problem.fix_hint);
    await expect(sheet.getByRole("alert")).toContainText("Offline since");
    await expect(sheet.getByRole("alert")).toContainText("last report");
    await expect(sheet.getByRole("radio", { name: "Sandbox", exact: true })).toBeChecked();
    await sheet.getByRole("button", { name: "Switch to Full access", exact: true }).click();
    await expect(sheet).toHaveCount(0);
    expect(writes[1].isolation_tier).toBe("native-trusted");
    await page.reload();
    await page.getByRole("button", { name: "Manage Build runner" }).click();
    sheet = page.getByRole("dialog", { name: "Build runner" });
    await expect(sheet.getByRole("radio", { name: "Full access", exact: true })).toBeChecked();
    await sheet.getByRole("radio", { name: "Full access", exact: true }).scrollIntoViewIfNeeded();
    await page.screenshot({ animations: "disabled", path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `runner-access-sheet-${width}.png`) });
    await expect(sheet.getByRole("button", { name: "Switch to Full access", exact: true })).toHaveCount(0);
    await expect(sheet.getByRole("checkbox", { name: "Preview project", exact: true })).toBeChecked();
    await expect(sheet.getByRole("checkbox", { name: "prj_unknown", exact: true })).toBeChecked();
    await sheet.getByRole("radio", { name: "Disabled Takes nothing" }).check();
    await sheet.getByRole("spinbutton", { name: "Jobs at once", exact: true }).fill("0");
    await sheet.getByRole("button", { name: "Save runner" }).click();
    await expect(sheet).toHaveCount(0);
    await expect(row).toContainText("disabled · Limit 0");
    expect(writes.map((write) => [write.state, write.capacity_limit])).toEqual([["draining", 2], ["draining", 2], ["disabled", 0]]);
    expect(writes[2].isolation_tier).toBe("native-trusted");
    expect(writes[2].project_ids).toEqual(["proj_preview", "prj_unknown"]);
    expect(writes[1]).not.toHaveProperty("home_project_ids");
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    expect(errors).toEqual([]);
  });
}

test("empty fleet invites enrollment and version refusals keep the upgrade command", async ({ page }) => {
  await openFleet(page, { ...fleet, runners: [] });
  await expect(page.getByText("No runners yet. Add a runner manually or let Luna help you set one up.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Add runner", exact: true })).toHaveCount(1);
  await expect(page.getByTestId("runner-capacity")).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Runners", exact: true })).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Capacity right now", exact: true })).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.unroute("**/fleet");
  await page.route("**/fleet", (route) => route.fulfill({ json: { ...fleet, runners: [fleetFixture.runners[0]] } }));
  await page.reload();
  await page.getByRole("button", { name: "Manage Michael's MacBook Pro" }).click();
  await expect(page.getByTestId("host-update")).toContainText("Too old to take work, needs v0.9.1");
  await expect(page.getByTestId("host-update")).toContainText("detent");
  await expect(page.getByTestId("runner-attention")).toHaveCount(0);
});

test("the attention link preserves unrelated search parameters", async ({ page }) => {
  await openFleet(page, fleet, "?source=capacity&health=needs_attention");
  await expect(page.getByTestId("host-card")).toHaveCount(1);
  await page.getByRole("link", { name: "All 3" }).click();
  await expect(page).toHaveURL(/source=capacity$/);
  await page.getByRole("link", { name: "Needs attention 2" }).click();
  await expect(page).toHaveURL(/source=capacity&health=needs_attention$/);
});

for (const multiple of [false, true]) {
  test(`AI assisted seeds the chosen project's Luna draft (multiple projects: ${multiple})`, async ({ page }) => {
    const selected = multiple ? "proj_second" : "proj_preview";
    await page.addInitScript(({ selected }) => {
      localStorage.setItem(`detent.conversation.draft:org_preview:owner_preview:${selected}:new`, "Keep my existing draft.");
    }, { selected });
    await openFleet(page, fleet, multiple ? "?multipleProjects" : "");
    await expect(page.getByRole("button", { name: "Add runner", exact: true })).toHaveCount(1);
    await page.getByRole("button", { name: "Add runner", exact: true }).focus();
    await page.keyboard.press("Enter");
    await page.getByRole("menuitem", { name: "AI assisted", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Add runner with Luna" });
    await expect(dialog).toContainText("Fly Sprite or your own machine");
    await expect(dialog.getByLabel("Project", { exact: true })).toHaveValue("");
    await expect(dialog.getByRole("option", { name: "Read-only project" })).toHaveCount(0);
    await dialog.getByLabel("Project", { exact: true }).selectOption(selected);
    await dialog.getByRole("button", { name: "Open Luna" }).click();
    await expect(page).toHaveURL(new RegExp(`/chat/p/${selected}$`));
    const draft = await page.evaluate((selected) => localStorage.getItem(`detent.conversation.draft:org_preview:owner_preview:${selected}:new`), selected);
    expect(draft).toContain("Keep my existing draft.");
    expect(draft).toContain("Help me add a runner for this project only.");
    expect(draft).not.toContain("organization Sprite pool");
    expect(draft).toContain("Fly Sprite or a machine I run");
    expect(draft).toContain("enrollment, the register command, provider login and the first connection");
    expect(draft).toContain("install Detent before creating the enrollment token: curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh");
    expect(draft).toContain("brew install digitaldrywood/tap/detent");
    if (multiple) {
      expect(await page.evaluate(() => localStorage.getItem("detent.conversation.draft:org_preview:owner_preview:proj_preview:new"))).toBeNull();
    }
  });
}

for (const version of ["operator-landed-3c51987c563b", "dev"]) {
  test(`manual Sprite setup remains actionable on Hub ${version}`, async ({ page }) => {
    await openFleet(page, { ...fleet, current: version }, `?version=${version}`);
    await page.getByRole("button", { name: "Add runner", exact: true }).click();
    await page.getByRole("menuitem", { name: "Manual", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Enroll a runner", exact: true });
    await dialog.getByLabel("A Fly Sprite").check();
    await expect(dialog.getByRole("link", { name: "Set a Sprites token" })).toHaveAttribute("href", "/settings/integrations#sprites");
    await expect(dialog.getByText(/latest tagged release/)).toBeVisible();
    await dialog.getByRole("radio", { name: "Full access", exact: true }).check();
      await dialog.getByRole("button", { name: "Create command" }).click();
    const instructions = dialog.getByRole("list", { name: "Sprite setup instructions" });
    await expect(instructions).toContainText("github.com/digitaldrywood/detent/releases/latest");
    await expect(instructions).toContainText('bash sprite-runner-bootstrap.sh --version "$detent_release"');
    await expect(dialog.getByRole("button", { name: "Copy the register command" })).toBeEnabled();
  });
}

test("Sprite pool row saves its limits without exposing setup fields on the list", async ({ page }) => {
  await openFleet(page, fleet, "", false, { ...spritePool, min_runners: 0, max_runners: 0, members: [] });
  const row = page.getByTestId("sprite-pool-row");
  await expect(row).toContainText("Not set up. Sprites start runners for every project when work is queued.");
  await expect(row.getByRole("link", { name: "Set up Sprites", exact: true })).toHaveAttribute("href", "/settings/integrations#sprites");
  await expect(row.getByRole("link", { name: "Set a Sprites token" })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Needs attention 1" })).toBeVisible();
  await page.getByRole("link", { name: "Needs attention 1" }).click();
  await expect(row).toHaveCount(0);
  await page.getByRole("link", { name: "All 3" }).click();
  let saved;
  await page.route("**/sprite-pool", async (route) => {
    if (route.request().method() === "PUT") {
      saved = route.request().postDataJSON();
      await route.fulfill({ json: { ...spritePool, ...saved, revision: spritePool.revision + 1 } });
    } else await route.fulfill({ json: spritePool });
  });
  await page.reload();
  await expect(row).toContainText("No organization Sprites token is set, so the Hub cannot start Sprites.");
  await expect(page.getByRole("link", { name: "Needs attention 2" })).toBeVisible();
  await expect(row.getByRole("link", { name: "Set a Sprites token" })).toHaveAttribute("href", "/settings/integrations#sprites");
  await row.getByRole("button", { name: /Configure pool/ }).click();
  const dialog = page.getByRole("dialog", { name: "Organization Sprite pool" });
  await dialog.getByLabel("Minimum runners", { exact: true }).fill("2");
  await dialog.getByLabel("Maximum runners", { exact: true }).fill("5");
  await dialog.getByLabel("Idle seconds", { exact: true }).fill("600");
  await dialog.getByRole("button", { name: "Save pool", exact: true }).click();
  await expect(row).toContainText("Floor 2 · Ceiling 5 · Idle 600s");
  expect(saved).toEqual({ min_runners: 2, max_runners: 5, idle_seconds: 600, bootstrap: spritePool.bootstrap, revision: spritePool.revision });
  await page.keyboard.press("Escape");
  await expect(page.getByLabel("Minimum runners", { exact: true })).toHaveCount(0);
});
