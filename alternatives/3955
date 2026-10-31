const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;

test.beforeEach(async ({}, testInfo) => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("runner-settings", { env: { DETENT_HOSTED_BROWSER_RUNNER: "1", ...(testInfo.title.startsWith("capacity settings") ? { DETENT_HOSTED_BROWSER_RUNNER_CAPACITY: "1" } : {}) } });
});

test.afterEach(async () => { await hub?.stop(); });

test("runner page saves through the fleet and persists routing after reload", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/runners", hub.fixture.url).toString());
  const row = page.getByTestId("host-card").filter({ hasText: "Settings runner" });
  await row.getByRole("button", { name: "Manage Settings runner" }).click();
  const sheet = page.getByRole("article", { name: "Settings runner", exact: true });
  await expect(sheet).toBeVisible();
  await expect(page).toHaveURL(/\/settings\/runners\/[^/?]+$/);
  await page.reload();
  await expect(sheet).toBeVisible();
  await sheet.getByRole("radio", { name: "Draining Finishes what it has" }).check();
  await sheet.getByRole("spinbutton", { name: "Total runner slots", exact: true }).fill("8");
  const maximum = sheet.getByRole("spinbutton", { name: /maximum slots$/ }).first();
  const projectLabel = await maximum.evaluate((input) => input.labels[0].textContent);
  await maximum.fill("4");
  const allowed = sheet.getByRole("checkbox").last();
  const projectName = await allowed.locator("..").innerText();
  await allowed.uncheck();
  await sheet.getByRole("radio", { name: /^Full access/ }).check();
  await sheet.getByLabel("Host services the sandbox may reach").fill("tcp:127.0.0.1:8080");
  await sheet.getByLabel("Availability", { exact: true }).selectOption("hours");
  await sheet.getByRole("combobox", { name: /^Time zone/ }).selectOption("America/Chicago");
  await sheet.getByLabel("Stop running work after hours end").fill("30m");
  await sheet.getByRole("button", { name: "Save runner" }).click();
  await expect(page.getByText("Runner saved", { exact: true })).toBeVisible();
  await page.getByRole("link", { name: "Runners", exact: true }).click();
  await expect(row).toContainText("Finishing running work");
  await expect(row).toContainText("Requested total: 8 slots");
  await page.reload();
  await page.getByRole("button", { name: "Manage Settings runner" }).click();
  const saved = page.getByRole("article", { name: "Settings runner", exact: true });
  await expect(saved.getByRole("radio", { name: /^Full access/ })).toBeChecked();
  await expect(saved.getByRole("checkbox", { name: projectName.trim(), exact: true })).not.toBeChecked();
  await expect(saved.getByRole("spinbutton", { name: "Total runner slots", exact: true })).toHaveValue("8");
  await expect(saved.getByRole("spinbutton", { name: projectLabel })).toHaveValue("4");
  await expect(saved.getByLabel("Host services the sandbox may reach")).toHaveValue("tcp:127.0.0.1:8080");
  await expect(saved.getByLabel("Day range 1")).toHaveValue("Mon-Fri");
  await expect(saved.getByLabel("From 1")).toHaveValue("09:00");
  await expect(saved.getByLabel("Until 1")).toHaveValue("17:00");
  await expect(saved.getByLabel("Stop running work after hours end")).toHaveValue("30m");
  await saved.getByRole("checkbox", { name: projectName.trim(), exact: true }).check();
  await saved.getByLabel("Runner scope").selectOption("organization");
  await saved.getByRole("button", { name: "Save runner" }).click();
  await expect(page.getByText("Runner saved", { exact: true })).toBeVisible();
  await page.getByRole("link", { name: "Runners", exact: true }).click();
  await page.reload();
  await page.getByRole("button", { name: "Manage Settings runner" }).click();
  await expect(page.getByRole("article").getByLabel("Runner scope")).toHaveValue("organization");
  await expect(page.getByRole("article")).toContainText("All current and future projects");

  await page.goto(hub.fixture.accounts.viewer);
  await page.goto(new URL("/settings/runners", hub.fixture.url).toString());
  await page.getByRole("button", { name: "Manage Settings runner" }).click();
  const readonly = page.getByRole("article");
  await expect(readonly).toContainText("Full access");
  await expect(readonly.locator("input, select, textarea")).toHaveCount(0);
  await expect(readonly.locator('[data-slot="sheet-footer"]')).toHaveCount(0);
});

test("runner outside hours stays contextual on its row", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/runners", hub.fixture.url).toString());
  const row = page.getByTestId("host-card").filter({ hasText: "Settings runner" });
  await row.getByRole("button", { name: "Manage Settings runner" }).click();
  const sheet = page.getByRole("article", { name: "Settings runner", exact: true });
  const later = new Date(Date.now() + 2 * 60 * 60 * 1000);
  const end = new Date(later.getTime() + 60 * 60 * 1000);
  const clock = (date) => date.toISOString().slice(11, 16);
  await sheet.getByRole("radio", { name: "Active Takes new work" }).check();
  await sheet.getByLabel("Availability", { exact: true }).selectOption("hours");
  await sheet.getByRole("combobox", { name: /^Time zone/ }).selectOption("UTC");
  await sheet.getByLabel("Day range 1").selectOption("Mon-Sun");
  await sheet.getByLabel("From 1").fill(clock(later));
  await sheet.getByLabel("Until 1").fill(clock(end));
  await sheet.getByRole("button", { name: "Save runner" }).click();
  await expect(page.getByText("Runner saved", { exact: true })).toBeVisible();
  await page.getByRole("link", { name: "Runners", exact: true }).click();
  await expect(row.getByText("Outside hours", { exact: true })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.reload();
  await expect(page.getByTestId("host-card").getByText("Outside hours", { exact: true })).toBeVisible();
});

test("organization project rank persists and viewers cannot edit it", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/projects", hub.fixture.url).toString());
  const table = page.getByRole("table", { name: "Projects in dispatch order", exact: true });
  await expect(table).toBeVisible();
  const rows = table.getByRole("row").filter({ has: page.getByRole("cell") });
  const names = () => rows.evaluateAll((elements) => elements.map((row) => row.querySelector(".truncate.font-medium")?.textContent ?? ""));
  // Rows show the project id until the project names load.
  await expect(table.getByText(/prj_[0-9a-f]{32}/)).toHaveCount(0);
  const before = await names();
  expect(before.length).toBeGreaterThan(1);
  const saved = page.waitForResponse((response) => response.request().method() === "PUT"
    && new URL(response.url()).pathname.endsWith("/project-rank"));
  const handle = page.getByRole("button", { name: `Reorder ${before[1]}`, exact: true });
  await handle.focus();
  await page.keyboard.press("Space");
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("Space");
  expect((await saved).ok()).toBe(true);
  const reordered = [before[1], before[0], ...before.slice(2)];
  await expect.poll(names).toEqual(reordered);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.reload();
  await expect.poll(names).toEqual(reordered);
  await page.goto(hub.fixture.accounts.viewer);
  await page.goto(new URL("/settings/projects", hub.fixture.url).toString());
  await expect(table).toBeVisible();
  await expect(table.getByRole("button", { name: /^Reorder / })).toHaveCount(0);
});

test("capacity settings govern real claims independently on two runners", async ({ page }, testInfo) => {
  const fs = require("node:fs");
  const path = require("node:path");
  const scratch = process.env.TMPDIR || process.env.TMP || process.env.TEMP;
  const evidence = path.join(scratch, "runner-capacity-acceptance");
  fs.mkdirSync(evidence, { recursive: true });
  const observations = [];
  const capture = async (state) => {
    for (const colorScheme of ["light", "dark"]) {
      await page.emulateMedia({ colorScheme });
      await page.evaluate((theme) => {
        localStorage.setItem("detent.theme", theme);
        document.documentElement.classList.toggle("dark", theme === "dark");
        document.documentElement.dataset.theme = theme;
      }, colorScheme);
      for (const width of [1440, 390]) {
        await page.setViewportSize({ width, height: 1100 });
        await page.screenshot({ path: path.join(evidence, `${state}-${colorScheme}-${width}.png`), fullPage: true });
      }
    }
    await page.setViewportSize({ width: 1440, height: 1100 });
  };
  const api = async (method, suffix, data) => {
    const bootstrap = await (await page.request.get(`${hub.fixture.url}/chat/bootstrap`)).json();
    const response = await page.request.fetch(`${hub.fixture.url}${bootstrap.api_base}${suffix}`, {
      method, headers: { "X-CSRF-Token": bootstrap.csrf_token }, ...(data ? { data } : {}),
    });
    expect(response.ok(), `${method} ${suffix}: ${await response.text()}`).toBe(true);
    return response.json();
  };
  const base = `${hub.fixture.url}/api/v2/organizations/org_browser_preview`;
  const [first, second] = hub.fixture.capacity_runners;
  const project = hub.fixture.project_id;
  const other = new URL(hub.fixture.private_project).pathname.split("/").at(-1);
  const heartbeat = async (runner, capacity, providerLimit = 8, constraint = "") => {
    const at = new Date().toISOString();
    const response = await page.request.post(`${base}/projects/${project}/machines/${runner.machine_id}/heartbeat`, {
      headers: { Authorization: `Bearer ${runner.credential}` },
      data: {
        display_name: runner === first ? "Settings runner" : "Second settings runner", capacity, version: "test", protocol_major: 2,
        backend_isolation: { test: ["sandbox", "native-trusted"], codex: ["sandbox", "native-trusted"] },
        capacity_configuration: { revision: (capacity === 2 ? "a" : "b").repeat(64), local_limit: capacity, client_limit: capacity, runtime_limit: capacity, manageable: !constraint, constraint, observed_at: at },
        provider_reports: [{ provider: "openai", backend: "codex", account_alias: runner.runner_id, models: ["test-model"], max_concurrent: providerLimit, availability: "available", observed_at: at }],
      },
    });
    expect(response.status(), await response.text()).toBe(200);
    return response.json();
  };
  await page.goto(hub.fixture.accounts.owner);
  const url = new URL("/settings/runners", hub.fixture.url).toString();
  await page.goto(url);
  await expect(page.getByRole("button", { name: "Manage Settings runner", exact: true })).toBeVisible();
  await capture("list");
  const open = async (name = "Settings runner") => {
    await page.goto(url);
    await page.getByRole("button", { name: `Manage ${name}`, exact: true }).click();
    return page.getByRole("article", { name, exact: true });
  };
  let sheet = await open();
  await expect(sheet.getByRole("spinbutton", { name: "Total runner slots", exact: true })).toHaveValue("2");
  const maximum = () => sheet.getByRole("spinbutton", { name: "Browser collaboration maximum slots", exact: true });
  await capture("editor");
  await maximum().fill("10001");
  await sheet.getByRole("button", { name: "Save runner", exact: true }).click();
  await expect(sheet).toContainText("Slot counts must be whole numbers from 0 to 10000");
  await capture("validation");
  await maximum().fill("4");
  await sheet.getByRole("spinbutton", { name: "Total runner slots", exact: true }).fill("8");
  await sheet.getByRole("button", { name: "Save runner", exact: true }).click();
  await expect(sheet.getByText("Runner saved", { exact: true })).toBeVisible();
  await expect(sheet).toContainText("Pending or constrained");
  await expect(sheet).toContainText("Effective total: 2 slots");
  await capture("pending");
  await page.reload();
  await expect(maximum()).toHaveValue("4");
  await expect(sheet.getByRole("spinbutton", { name: "Total runner slots", exact: true })).toHaveValue("8");
  const reconnected = await heartbeat(first, 8);
  expect(reconnected.routing.project_capacity_limits[project]).toBe(4);
  expect(reconnected.routing.capacity_limit).toBe(8);
  await page.reload();
  await expect(sheet).toContainText("Effective total: 8 slots");
  await expect(sheet).not.toContainText("Pending or constrained");
  await capture("saved");
  await heartbeat(first, 8, 3);
  await page.reload();
  await expect(sheet).toContainText("Effective total: 3 slots");
  await expect(sheet).toContainText("Pending or constrained");
  await capture("constrained");
  await heartbeat(first, 2, 8, "The selected configuration could not be written; check the enrolled runner configuration permissions.");
  await page.reload();
  await expect(sheet).toContainText("The selected configuration could not be written; check the enrolled runner configuration permissions.");
  await expect(sheet).toContainText("Effective total: 2 slots");
  await capture("refused");
  await heartbeat(first, 8);
  await page.reload();
  await maximum().fill("5");
  const routingURL = `${base}/runners/${first.runner_id}/routing`;
  await page.route(routingURL, (route) => route.request().method() === "PUT"
    ? route.fulfill({ status: 503, json: { error: { code: "unavailable", message: "Test configuration service unavailable" } } })
    : route.continue());
  await sheet.getByRole("button", { name: "Save runner", exact: true }).click();
  await expect(sheet).toContainText("The hub could not complete the request.");
  await expect(maximum()).toHaveValue("5");
  await expect(sheet.getByText("Runner saved", { exact: true })).toHaveCount(0);
  await capture("error-injected");
  await page.unroute(routingURL);
  const fleetURL = `${base}/fleet`;
  const fleet = await (await page.request.get(fleetURL)).json();
  await page.route(fleetURL, (route) => route.fulfill({ json: {
    ...fleet, runners: fleet.runners.map((runner) => runner.id === first.runner_id ? {
      ...runner, connection_health: "offline", last_heartbeat_at: "2020-01-01T00:00:00Z",
      capacity_configuration: { ...runner.capacity_configuration, effective: null, status: "unknown", applied: null },
    } : runner),
  } }));
  await page.reload();
  await expect(sheet.getByRole("img", { name: "Offline", exact: true })).toBeVisible();
  await expect(sheet).toContainText("Effective total: unknown");
  await capture("offline-injected");
  await page.unroute(fleetURL);
  sheet = await open("Second settings runner");
  await sheet.getByRole("spinbutton", { name: "Total runner slots", exact: true }).fill("8");
  await maximum().fill("1");
  await sheet.getByRole("button", { name: "Save runner", exact: true }).click();
  await expect(sheet.getByText("Runner saved", { exact: true })).toBeVisible();
  const secondReconnect = await heartbeat(second, 8);
  expect(secondReconnect.routing.project_capacity_limits[project]).toBe(1);
  let sequence = 0;
  const claim = async (runner, projectID, expectedStatus) => {
    const key = `capacity-${++sequence}`;
    const item = await api("POST", `/projects/${projectID}/work-items`, {
      idempotency_key: key, title: key, state: "Todo",
      body: "## Acceptance criteria\nExercise isolated capacity admission.\n## Must not break\nPreserve active leases.\n## How we know it worked\nBounded claim responses.\n## Invariants touched\nINV-3.\n## Safety regression\nRunner capacity boundaries.\n\n```detent-agent\nschema: 1\neffort: high\n```",
    });
    const response = await page.request.post(`${base}/projects/${projectID}/claims`, {
      headers: { Authorization: `Bearer ${runner.credential}` },
      data: { policy_id: hub.fixture.policy_id, work_item_id: item.work_item_id, machine_id: runner.machine_id, session_id: key, ttl_seconds: 300, protocol_major: 2, capabilities: ["native_issues", "scoped_collaboration"], provider_candidates: [{ work_item_id: item.work_item_id, revision: item.revision, requirement: { role: "implement", backend: "codex", model: "test-model" } }] },
    });
    expect(response.status(), await response.text()).toBe(expectedStatus);
    return expectedStatus === 200 ? { runner, projectID, lease: await response.json() } : null;
  };
  const firstLeases = [];
  for (let i = 0; i < 4; i++) firstLeases.push(await claim(first, project, 200));
  await claim(first, project, 409);
  for (let i = 0; i < 4; i++) firstLeases.push(await claim(first, other, 200));
  await claim(first, other, 409);
  const secondLease = await claim(second, project, 200);
  await claim(second, project, 409);
  const routing = await api("GET", `/runners/${first.runner_id}/routing`);
  expect(routing.used).toBe(8);
  expect(routing.project_used).toEqual({ [project]: 4, [other]: 4 });
  const secondRouting = await api("GET", `/runners/${second.runner_id}/routing`);
  expect(secondRouting.used).toBe(1);
  expect(secondRouting.project_used[project]).toBe(1);
  expect(secondRouting.project_capacity_limits[project]).toBe(1);
  observations.push({ scenario: "UI save, reload, heartbeat reconnect and real claims", firstRunner: { used: routing.used, project_used: routing.project_used, maximum: routing.project_capacity_limits }, secondRunner: { used: secondRouting.used, maximum: secondRouting.project_capacity_limits }, refused: ["first project fifth", "first runner ninth", "second project second"] });
  await page.goto(url);
  const firstCard = page.getByRole("article").filter({ has: page.getByRole("button", { name: "Manage Settings runner", exact: true }) });
  await expect(firstCard).toContainText("Requested total: 8 slots");
  await capture("saved-list");
  const release = async (work) => {
    const response = await page.request.post(`${base}/projects/${work.projectID}/leases/${work.lease.lease_id}/release`, {
      headers: { Authorization: `Bearer ${work.runner.credential}` }, data: { fencing_token: work.lease.fencing_token, reason: "released" },
    });
    expect(response.status(), await response.text()).toBe(204);
  };
  sheet = await open();
  await maximum().fill("3");
  await sheet.getByRole("button", { name: "Save runner", exact: true }).click();
  await expect(sheet.getByText("Runner saved", { exact: true })).toBeVisible();
  await release(firstLeases.pop());
  await claim(first, project, 409);
  firstLeases.push(await claim(first, other, 200));
  observations.push({ scenario: "Project-only reduction to 3 with total 8", projectActive: 4, newProjectStarts: 0, otherProjectReplacementStarts: 1 });
  await sheet.getByRole("spinbutton", { name: "Total runner slots", exact: true }).fill("3");
  await maximum().fill("3");
  await sheet.getByRole("button", { name: "Save runner", exact: true }).click();
  await expect(sheet.getByText("Runner saved", { exact: true })).toBeVisible();
  await claim(first, project, 409);
  await claim(first, other, 409);
  for (const work of firstLeases) {
    const response = await page.request.post(`${base}/projects/${work.projectID}/leases/${work.lease.lease_id}/renew`, {
      headers: { Authorization: `Bearer ${first.credential}` }, data: { fencing_token: work.lease.fencing_token, ttl_seconds: 300 },
    });
    expect(response.status(), await response.text()).toBe(200);
  }
  observations.push({ scenario: "UI reduction to total 3 / project 3", preservedAndRenewed: 8, newStarts: 0 });
  await capture("reduced");
  await sheet.getByRole("spinbutton", { name: "Total runner slots", exact: true }).fill("8");
  await maximum().fill("4");
  await sheet.getByRole("button", { name: "Save runner", exact: true }).click();
  await expect(sheet.getByText("Runner saved", { exact: true })).toBeVisible();
  await release(firstLeases.pop());
  firstLeases.push(await claim(first, other, 200));
  const increased = await api("GET", `/runners/${first.runner_id}/routing`);
  expect(increased.used).toBe(8);
  observations.push({ scenario: "UI increase back to total 8 / project 4", subsequentReplacementStarts: 1, used: increased.used });
  for (const work of [...firstLeases, secondLease]) await release(work);
  fs.writeFileSync(path.join(evidence, "claim-observations.txt"), `${JSON.stringify({ test: testInfo.title, observations }, null, 2)}\n`);
});
