// End-to-end accessibility and keyboard cover for the account screens.
//
// Like `conversation.spec.js`, this drives the real thing: `hosted-hub.js`
// starts the Go preview test, which serves a hosted hub with real sessions,
// real authorization and a fixture organization with an `owner` and a `viewer`
// account. Everything below happens through the browser, with the keyboard and
// through accessible names, so a control a screen reader or a keyboard cannot
// reach fails the test rather than passing on a class name.
//
// The React routes these tests exercise are served by the hub for every
// non-API path (decisions.md §12, "Serving"), and they read the JSON endpoints
// of §12. The public login uses the client shell; `/organization` remains
// a server-rendered page.
const { test, expect } = require("@playwright/test");
const AxeBuilder = require("@axe-core/playwright").default;
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("account", { env: { DETENT_HOSTED_BROWSER_UNSIGNED_MEMBER: "1" } });
});

test.afterAll(async () => {
  await hub?.stop();
  hub = undefined;
});

/** Collects everything the page logs as an error, for a per-test assertion. */
function watchConsole(page) {
  const errors = [];
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  page.on("pageerror", (error) => errors.push(String(error)));
  return errors;
}

/** Signs in as one of the fixture's accounts and opens an application route. */
async function openAs(page, account, route) {
  await page.goto(hub.fixture.accounts[account], { waitUntil: "domcontentloaded" });
  await page.goto(new URL(route, hub.fixture.url).toString(), { waitUntil: "domcontentloaded" });
}

/**
 * Fails on any serious or critical axe violation, naming the rules it found.
 * `page-has-heading-one` is gated too: axe scores it "moderate", but a route
 * with no level-one heading gives a screen reader nothing to jump to, which is
 * the finding these specs exist to keep fixed.
 */

const CONVERSATION_THREAD_ROW = /data-testid="sidebar-row-(card|slim)"/;

function withoutConversationThreadRowNesting(violations) {
  return violations.flatMap((violation) => {
    if (violation.id !== "nested-interactive") return [violation];
    const nodes = violation.nodes.filter((node) => !CONVERSATION_THREAD_ROW.test(node.html ?? ""));
    return nodes.length === 0 ? [] : [{ ...violation, nodes }];
  });
}

async function expectNoSeriousAxeViolations(page, label) {
  const results = await new AxeBuilder({ page }).analyze();
  const describe = (violation) =>
    `${violation.id} (${violation.impact}) x${violation.nodes.length}: ${violation.nodes[0]?.target?.join(" ")}`;

  const serious = withoutConversationThreadRowNesting(results.violations).filter(
    (violation) => violation.impact === "serious" || violation.impact === "critical",
  );
  expect(serious.map(describe), `serious or critical axe violations on ${label}`).toEqual([]);

  const headings = results.violations.filter(
    (violation) => violation.id === "page-has-heading-one",
  );
  expect(headings.map(describe), `page-has-heading-one on ${label}`).toEqual([]);
}

/** Asserts the route names itself once, at level one. */
async function expectOneHeadingOne(page, text) {
  const heading = page.getByRole("heading", { level: 1 });
  await expect(heading).toHaveCount(1);
  if (text !== undefined) await expect(heading).toContainText(text);
}

/** Tabs until the named control holds focus, so "reachable" means reachable. */
async function tabTo(page, name, limit = 40) {
  for (let step = 0; step < limit; step += 1) {
    const focused = await page.evaluate(() => {
      const active = document.activeElement;
      if (active === null) return "";
      return (
        active.getAttribute("aria-label") ??
        active.textContent?.trim().slice(0, 80) ??
        ""
      );
    });
    if (focused.includes(name)) return true;
    await page.keyboard.press("Tab");
  }
  return false;
}

test("the sign-in page renders without a session and offers sign-in and account creation", async ({ page }) => {
  const errors = watchConsole(page);
  await page.goto(new URL("/login", hub.fixture.url).toString(), {
    waitUntil: "domcontentloaded",
  });

  await expectOneHeadingOne(page, "Sign in");
  await expect(page.getByRole("link", { name: "Sign in" })).toHaveAttribute(
    "href",
    "/auth/oidc/start",
  );
  await expect(page.getByRole("link", { name: "Create account" })).toHaveAttribute(
    "href",
    "/auth/oidc/start?screen_hint=sign-up",
  );

  expect(await tabTo(page, "Sign in")).toBe(true);
  await expectNoSeriousAxeViolations(page, "/login");
  expect(errors, "console errors on /login").toEqual([]);
});

test("the login card says what went wrong when the callback failed", async ({ page }) => {
  await page.goto(new URL("/login?error=no_membership", hub.fixture.url).toString(), {
    waitUntil: "domcontentloaded",
  });
  await expect(page.getByRole("alert")).toContainText("not a member of this organization");
  await expect(page.locator("body")).not.toContainText("no_membership");
});

test("the organization page lists the members and gates the controls by role", async ({
  page,
}) => {
  const errors = watchConsole(page);
  // The hub still renders `/organization` itself, so the client's
  // Organization section is opened by its own path.
  await openAs(page, "owner", "/settings/organization");

  await expect(page).toHaveURL(/\/settings\/organization$/);
  await expectOneHeadingOne(page, "Settings");
  const table = page.getByRole("table");
  await expect(table).toBeVisible();
  const identities = table.locator("tbody tr td:first-child > div:first-child");
  await expect(identities).toHaveCount(3);
  for (const identity of await identities.all()) {
    await expect(identity).not.toHaveText(/^\s*$/);
  }
  const unsigned = table.getByRole("row").filter({ hasText: "unsigned@example.test" });
  await expect(unsigned).toContainText("Unsigned Member");
  await expect(unsigned).toContainText("Hasn't signed in yet");
  await expect(unsigned.getByRole("button", { name: "Remove" })).toBeEnabled();
  // An owner gets the mutating controls.
  await expect(page.getByRole("button", { name: "Remove" }).first()).toBeVisible();
  await expectNoSeriousAxeViolations(page, "/organization as owner");
  expect(errors, "console errors on /organization").toEqual([]);
  await unsigned.getByRole("button", { name: "Remove" }).click();
  await page.getByRole("button", { name: "Remove", exact: true }).last().click();
  await expect(unsigned).toHaveCount(0);

  // A viewer sees the same data and none of the controls (decisions.md §10.11).
  await openAs(page, "viewer", "/settings/organization");
  await expect(page.getByRole("table")).toBeVisible();
  await expect(page.getByRole("button", { name: "Remove" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Send invitation" })).toHaveCount(0);
  await expectNoSeriousAxeViolations(page, "/organization as viewer");
});

test("sending invitations resets the form and shows the newest invitation with its enforced expiry", async ({ page }) => {
  await openAs(page, "owner", "/settings/organization");
  const invitations = page.locator("section").filter({ has: page.getByRole("heading", { name: "Invitations", exact: true }) });
  for (const email of ["older-invite@example.test", "newest-invite@example.test"]) {
    await page.getByLabel("Email", { exact: true }).fill(email);
    await page.getByLabel("Role", { exact: true }).selectOption("viewer");
    const responsePromise = page.waitForResponse((response) =>
      response.request().method() === "POST" && response.url().endsWith("/members/invitations"),
    );
    await page.getByRole("button", { name: "Send invitation", exact: true }).click();
    const response = await responsePromise;
    expect(response.status()).toBe(201);
    const created = await response.json();
    expect(created.email).toBe(email);
    expect(created.expires_at).toBeTruthy();
    await expect(page.getByLabel("Email", { exact: true })).toHaveValue("");
    await expect(page.getByLabel("Role", { exact: true })).toHaveValue("member");
    await expect(page.getByRole("status").filter({ hasText: `Invitation sent to ${email}.` })).toBeVisible();
    const row = invitations.locator('[data-slot="settings-row"]').first();
    await expect(row).toBeVisible();
    await expect(row).toContainText(email);
    await expect(row).toContainText(`Invited as viewer. Expires ${created.expires_at}.`);
    await page.reload();
    await expect(invitations.locator('[data-slot="settings-row"]').first()).toContainText(`Expires ${created.expires_at}.`);
  }
});

test("removing the last owner is refused by the hub and said in the page", async ({ page }) => {
  await openAs(page, "owner", "/settings/organization");

  // The owner's row by the owner's address, with no fallback. This used to
  // read `hub.fixture.accounts.owner_email ?? "@"`, and `accounts` maps an
  // account *name* to a sign-in URL and has no `owner_email` key at all, so the
  // filter quietly degraded to "@" — which matches every member row. `.first()`
  // then took whichever member the hub happened to list first, and the test
  // removed the viewer instead of the owner and waited for a refusal that was
  // never coming.
  const ownerRow = page.getByRole("row").filter({ hasText: hub.fixture.owner_email });
  await expect(ownerRow).toHaveCount(1);
  const remove = ownerRow.getByRole("button", { name: "Remove" }).first();
  await expect(remove).toBeVisible();
  await remove.click();
  await page.getByRole("button", { name: "Remove", exact: true }).last().click();
  // The alert is the hub's refusal said in the page, which arrives after a
  // round trip; under a loaded machine that has taken longer than the default
  // expectation window, so this one waits as long as a navigation.
  await expect(page.getByRole("alert")).toContainText("owner", { timeout: 30_000 });
});

test("the settings navigation is the sidebar, not a second column", async ({ page }) => {
  const errors = watchConsole(page);
  // `/settings` redirects to the default section (§17.3).
  await openAs(page, "owner", "/settings");

  await expectOneHeadingOne(page, "Settings");
  await expect(page.getByLabel("Settings breadcrumb")).toContainText("General");

  const sidebar = page.locator("[data-app-sidebar]");
  await expect(sidebar.getByRole("button", { name: "General" })).toBeVisible();
  await expect(sidebar.getByRole("button", { name: "Organization" })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Settings", exact: true })).toHaveCount(0);

  // §17.2: one brand row. It used to be drawn twice on a settings route — once
  // by the layout's titlebar band and once by the sidebar the stub re-rendered.
  await expect(page.getByLabel("Go to Detent Cloud")).toHaveCount(1);

  const search = sidebar.getByRole("combobox", { name: "Search settings" });
  await expect(search).toBeVisible();
  await search.fill("invoice");
  await expect(page.getByRole("listbox", { name: "Settings search results" })).toBeVisible();
  await page.getByRole("button", { name: "Clear settings search" }).click();

  await sidebar.getByRole("button", { name: "Projects" }).click();
  await expect(page).toHaveURL(/\/settings\/projects$/);
  await expect(page.getByRole("heading", { name: "Projects", exact: true })).toBeVisible();

  await expect(sidebar.getByRole("button", { name: "Appearance" })).toHaveAttribute(
    "aria-disabled",
    "true",
  );

  await expect(sidebar.getByRole("button", { name: "Back" })).toBeVisible();

  await expectNoSeriousAxeViolations(page, "/settings as owner");
  expect(errors, "console errors on /settings").toEqual([]);

  await openAs(page, "viewer", "/settings");
  // Plan and billing are owner or admin only (§12, "Plan, billing, support").
  await expect(page.getByRole("button", { name: "Billing" })).toHaveCount(0);
  await expect(page.getByLabel("Go to Detent Cloud")).toHaveCount(1);
  await expectNoSeriousAxeViolations(page, "/settings as viewer");
});

test("project settings show the integration and refuse to edit it for a viewer", async ({
  page,
}) => {
  const errors = watchConsole(page);
  // §17.3: this path redirects into the Integrations section of settings.
  await openAs(page, "owner", `/projects/${hub.fixture.project_id}/settings`);

  await expect(page).toHaveURL(/\/settings\/integrations\?project=/);
  await expectOneHeadingOne(page, "Settings");
  await expect(page.getByLabel("Go to Detent Cloud")).toHaveCount(1);
  await expect(page.getByLabel("Intake", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Projection", { exact: true })).toBeVisible();
  await expectNoSeriousAxeViolations(page, "/projects/:project/settings as owner");
  // `GET {nativeBase}/policy` answers `409 policy_mismatch` on a project with
  // no approved descriptor, which is this fixture's state and a state the
  // screen renders rather than an error it hit. The browser logs the response
  // either way, so the expected refusal is filtered and nothing else is.
  expect(
    errors.filter((message) => !/409 \(Conflict\)/.test(message)),
    "console errors on project settings",
  ).toEqual([]);

  await openAs(page, "viewer", `/projects/${hub.fixture.project_id}/settings`);
  const intake = page.getByLabel("Intake", { exact: true });
  await expect(intake).toBeVisible();
  await expect(intake).toBeDisabled();
  await expect(page.getByRole("button", { name: "Save changes" })).toHaveCount(0);
});

test("pending invitations can be resent and revoked from their row", async ({ page }) => {
  const email = "withdraw@example.test";
  await openAs(page, "owner", "/settings/organization");
  await page.getByLabel("Email", { exact: true }).fill(email);
  await page.getByRole("button", { name: "Send invitation", exact: true }).click();
  const row = page.locator('[data-slot="settings-row"]').filter({
    has: page.getByRole("heading", { name: email, exact: true }),
  });
  await expect(row).toBeVisible();
  const providerInvitations = async () => {
    const response = await page.request.get(`${hub.fixture.url}/__preview/invitations`);
    expect(response.ok()).toBe(true);
    return Object.values(await response.json()).filter((invitation) => invitation.email === email);
  };
  const [invitation] = await providerInvitations();
  expect(invitation.state).toBe("pending");
  await row.getByRole("button", { name: "Resend", exact: true }).click();
  await expect(row.getByRole("status")).toHaveText("Invitation resent.");
  expect(await providerInvitations()).toHaveLength(1);
  await row.getByRole("button", { name: "Revoke", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText(`Revoke invitation to ${email}?`);
  await dialog.getByRole("button", { name: "Keep invitation", exact: true }).click();
  await expect(row).toBeVisible();
  const revokeURL = `**/members/invitations/${invitation.id}`;
  await page.route(revokeURL, (route) => route.fulfill({
    status: 503, contentType: "application/json",
    body: JSON.stringify({ code: "unavailable", message: "The invitation could not be revoked. Try again." }),
  }));
  await row.getByRole("button", { name: "Revoke", exact: true }).click();
  await dialog.getByRole("button", { name: "Revoke invitation", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("could not be revoked");
  await expect(row).toBeVisible();
  expect((await providerInvitations())[0].state).toBe("pending");
  await page.unroute(revokeURL);
  await dialog.getByRole("button", { name: "Revoke invitation", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(row).toHaveCount(0);
  expect((await providerInvitations())[0].state).toBe("revoked");
  await openAs(page, "viewer", "/settings/organization");
  await expect(page.getByRole("button", { name: "Revoke", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Resend", exact: true })).toHaveCount(0);
});

test("the first-run wizard reports the hub's four onboarding steps", async ({ page }) => {
  const errors = watchConsole(page);
  await openAs(page, "owner", `/projects/${hub.fixture.project_id}/setup`);

  const steps = page.getByRole("list", { name: "Setup steps" }).getByRole("listitem");
  await expect(steps).toHaveCount(4);
  await expect(steps.first()).toContainText("Execution runner");
  // Exactly one step is the current one, and it is reachable from the keyboard.
  await expect(page.locator('[aria-current="step"]')).toHaveCount(1);
  expect(await tabTo(page, "Continue")).toBe(true);
  await expectNoSeriousAxeViolations(page, "/projects/:project/setup");
  expect(errors, "console errors on the wizard").toEqual([]);
});

for (const viewport of [
  { name: "desktop", width: 1440, height: 900 },
  { name: "phone", width: 390, height: 844 },
]) {
  test(`the setup wizard fits a ${viewport.name} and its choices show what is selected`, async ({ page }) => {
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    const errors = watchConsole(page);
    await openAs(page, "owner", `/projects/${hub.fixture.project_id}/setup`);

    const tabs = page.getByRole("list", { name: "Setup steps" }).getByRole("button");
    await expect(tabs).toHaveCount(4);
    await tabs.nth(3).click();
    await expect(page.getByRole("heading", { level: 1, name: "Artifact history" })).toBeVisible();

    // Every tab and the whole step fit inside the viewport: nothing is
    // clipped at either edge.
    for (const locator of [page.getByRole("list", { name: "Setup steps" }), page.getByRole("heading", { level: 1 })]) {
      const box = await locator.boundingBox();
      expect(box, "wizard element has a box").not.toBeNull();
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
    }

    // Clicking anywhere on a card chooses it, and the choice is visible.
    const local = page.getByRole("radio", { name: "Local history" });
    const selectedChoice = page.locator("[data-checked]").filter({ has: page.getByRole("radio") });
    await page.getByText("Local history", { exact: true }).click();
    await expect(local).toBeChecked();
    await expect(selectedChoice).toHaveCount(1);
    await expect(selectedChoice).toContainText("Local history");
    await page.getByText("Customer service", { exact: true }).click();
    await expect(page.getByRole("radio", { name: "Customer service" })).toBeChecked();
    await expect(local).not.toBeChecked();
    await expect(selectedChoice).toHaveCount(1);
    await expect(selectedChoice).toContainText("Customer service");

    // Every step stays reachable from the tabs.
    await tabs.nth(0).click();
    await expect(page.getByRole("heading", { level: 1, name: "Execution runner" })).toBeVisible();
    await expectNoSeriousAxeViolations(page, `/projects/:project/setup (${viewport.name})`);
    expect(errors, "console errors on the wizard").toEqual([]);
  });
}

test("the fleet page redirects into settings and renders the hosts and providers", async ({
  page,
}) => {
  const errors = watchConsole(page);
  // §17.3: `/fleet` is the Providers & runners section of settings now. The
  // spend hero, the chart, the range control and the allowance totals moved to
  // `/usage` with the numbers they measure (§17.5).
  await openAs(page, "owner", "/fleet");

  await expect(page).toHaveURL(/\/settings\/runners$/);
  await expectOneHeadingOne(page, "Settings");
  await expect(page.getByLabel("Go to Detent Cloud")).toHaveCount(1);
  await expect(
    page.locator("[data-app-sidebar]").getByRole("button", { name: "Providers & runners" }),
  ).toHaveAttribute("data-active", "true");
  await expect(page.getByRole("heading", { name: "Providers & runners", exact: true })).toBeVisible();
  if (await page.getByTestId("host-card").count()) {
    await expect(page.getByRole("heading", { name: "Runners", exact: true })).toBeVisible();
  } else {
    await expect(page.getByText("No runners yet. Enroll a machine to start taking work.")).toBeVisible();
  }
  await expect(page.getByRole("heading", { name: "Providers" })).toBeVisible();

  const meters = page.getByRole("progressbar");
  if ((await meters.count()) > 0) {
    await expect(meters.first()).toHaveAttribute("aria-valuenow", /\d+/);
  }
  await expectNoSeriousAxeViolations(page, "/settings/runners");
  expect(errors, "console errors on /settings/runners").toEqual([]);

  // A viewer reads the fleet: it has no mutations to gate (§12, "Fleet").
  await openAs(page, "viewer", "/fleet");
  await expectOneHeadingOne(page, "Settings");
  await expectNoSeriousAxeViolations(page, "/settings/runners as viewer");
});

test("Providers & runners enrolls a host and shows the one-time token once", async ({ page }) => {
  const errors = watchConsole(page);

  // The hub gates every runner route on the actor holding the runner grant on
  // *every* project in the organization (`hostedAllRunnerGrants`), and the
  // fixture's owner starts without it. Granting it here is setup, not the
  // thing under test: without it the control is correctly absent and there
  // would be nothing to drive.
  await openAs(page, "owner", "/settings/organization");
  const granted = await page.evaluate(async () => {
    const bootstrap = await (await fetch("/app/bootstrap")).json();
    const base = bootstrap.api_base;
    const members = await (await fetch(`${base}/members`)).json();
    const me = members.members.find((member) => member.user_id === bootstrap.actor.subject);
    if (me === undefined) return false;
    for (const project of bootstrap.projects) {
      const response = await fetch(`${base}/members/${encodeURIComponent(me.id)}/grants`, {
        method: "PUT",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": bootstrap.csrf_token },
        body: JSON.stringify({
          project_id: project.id,
          write: true,
          runner: true,
          revoke: false,
          idempotency_key: `spec-runner-grant-${project.id}`,
        }),
      });
      if (!response.ok) return false;
    }
    return true;
  });
  expect(granted, "the fixture owner could be granted runner management").toBe(true);

  await openAs(page, "owner", "/fleet");
  await expect(page).toHaveURL(/\/settings\/runners$/);

  // The control is reachable from the keyboard and names itself.
  const enroll = page.getByRole("button", { name: "Enroll a runner" });
  await expect(enroll).toBeVisible();
  await enroll.click();

  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("heading", { name: "Enroll a runner" })).toBeVisible();
  // Nothing to copy from the host first: the dialog asks for a name, a
  // capacity and projects, never for runner or machine IDs.
  await expect(dialog.getByLabel("Runner id")).toHaveCount(0);
  await dialog.getByLabel("Name").fill("Build host");
  await dialog.getByLabel("Concurrency", { exact: true }).fill("2");
  await expectNoSeriousAxeViolations(page, "the enrollment dialog");

  await dialog.getByRole("button", { name: "Create command" }).click();

  // One command carries everything the host needs.
  const copy = dialog.getByRole("button", { name: "Copy the register command" });
  await expect(copy).toBeVisible({ timeout: 15_000 });
  await expect(
    dialog.getByText(/^detent hub runner register --url \S+ (--organization \S+ )?--token \S+ --name 'Build host' --capacity 2 --service$/),
  ).toBeVisible();
  await expectNoSeriousAxeViolations(page, "the enrollment dialog with its command");

  const command = await dialog.locator("code").textContent();
  const token = command.match(/--token (\S+)/)[1];
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Waiting to connect" })).toBeVisible();
  await expect(page.getByText("Build host", { exact: true })).toBeVisible();
  expect(await page.locator("body").innerHTML()).not.toContain(token);
  expect(await page.locator("body").innerHTML()).not.toContain("detent hub runner register");
  const description = page.locator("#settings-enrollments p");
  await expect(description).toHaveText(/^No check-in yet · expires /);
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 900 });
    const layout = await description.evaluate((element) => {
      const box = element.getBoundingClientRect();
      const row = element.closest('[data-slot="settings-row"]').getBoundingClientRect();
      return { x: box.x, width: box.width, height: box.height, lineHeight: parseFloat(getComputedStyle(element).lineHeight), rowWidth: row.width };
    });
    expect(layout.height).toBeLessThanOrEqual(layout.lineHeight * 2 + 1);
    expect(layout.width).toBeGreaterThan(Math.min(200, layout.rowWidth - 40));
    expect(layout.x + layout.width).toBeLessThanOrEqual(width);
  }
  expect(errors, "console errors while enrolling a runner").toEqual([]);
});

test("the usage page draws the window, the tabs and the breakdown", async ({ page }) => {
  const errors = watchConsole(page);
  await openAs(page, "owner", "/usage");

  await expectOneHeadingOne(page, "Usage");
  // §17.2: the brand row is drawn once here too. `/usage` is not a settings
  // route, so it keeps the conversation sidebar — and that sidebar draws the
  // band itself.
  await expect(page.getByLabel("Go to Detent Cloud")).toHaveCount(1);

  const metric = page.getByRole("group", { name: "Usage metric" }).first();
  await expect(metric).toBeVisible();
  for (const label of ["Cost", "Tokens", "Limits", "Runners"]) {
    await expect(metric.getByRole("button", { name: label })).toBeVisible();
  }
  await expect(page.getByRole("heading", { name: "Totals" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Breakdown" })).toBeVisible();
  await expect(page.getByText(/API estimate/)).toBeVisible();

  // The window is a control over the hub's own report, so picking one is a
  // re-read rather than a client-side filter.
  const period = page.getByRole("group", { name: "Usage period" }).first();
  await period.getByRole("button", { name: "7 days" }).click();
  await expect(period.getByRole("button", { name: "7 days" })).toHaveAttribute(
    "aria-pressed",
    "true",
  );

  // The breakdown toggle swaps one table for the other.
  const breakdown = page.getByRole("group", { name: "Usage breakdown" }).first();
  await breakdown.getByRole("button", { name: "Day" }).click();
  await expect(page.getByRole("columnheader", { name: "Total" })).toBeVisible();

  await metric.getByRole("button", { name: "Runners" }).click();
  await expect(page.getByRole("heading", { name: "Runners" })).toBeVisible();
  await metric.getByRole("button", { name: "Limits" }).click();
  await expect(page.getByRole("heading", { name: "Limits" })).toBeVisible();
  // The period does not apply to limits, so it stays in place, disabled.
  await expect(period.getByRole("button", { name: "7 days" })).toBeDisabled();

  await expectNoSeriousAxeViolations(page, "/usage");
  expect(errors, "console errors on /usage").toEqual([]);

  // Usage is a report: a viewer reads it, because there is nothing on it to
  // gate (§17.5).
  await openAs(page, "viewer", "/usage");
  await expectOneHeadingOne(page, "Usage");
  await expectNoSeriousAxeViolations(page, "/usage as viewer");
});
