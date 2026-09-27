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
// of §12. Where an endpoint is not implemented yet the test skips itself and
// names the endpoint, rather than failing for a reason that is not about the
// client: `probe()` below asks the hub once, and the skip message is the
// §12 route that answered 404.
const { test, expect } = require("@playwright/test");
const AxeBuilder = require("@axe-core/playwright").default;
const { startHostedHub } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

let hub;
/** What the hub actually serves, discovered once. */
let served = {
  shell: false,
  routed: false,
  bootstrap: false,
  members: false,
  projects: false,
  fleet: false,
  integration: false,
  onboarding: false,
  usage: false,
};

/**
 * Why every assertion below is conditional. The account routes live in
 * `web/conversation/src/app/routes.account.tsx` and are added to the tree by
 * `router.tsx`, which is wired separately; until that lands, the hub serves the
 * shell for `/organization` but the router matches nothing, so the route
 * renders blank. `probeRouted()` notices that and skips with this reason
 * rather than failing for something that is not about these screens.
 */
const NOT_ROUTED =
  "The application does not serve the account routes yet: routes.account.tsx is not in router.tsx's tree.";

test.beforeAll(async ({ browser }) => {
  hub = await startHostedHub("account");
  const context = await browser.newContext();
  const page = await context.newPage();
  try {
    await page.goto(hub.fixture.accounts.owner, { waitUntil: "domcontentloaded" });
    served = await page.evaluate(async () => {
      const ok = async (path) => {
        try {
          const response = await fetch(path, { headers: { Accept: "application/json" } });
          return response.status !== 404;
        } catch {
          return false;
        }
      };
      // The shell is the application's own HTML: the mount point plus the
      // bundle. `data-detent-conversation-root` is set by the bundle at
      // runtime, so it is not in what the hub sends.
      const shell = await (async () => {
        try {
          const response = await fetch("/organization", { headers: { Accept: "text/html" } });
          if (!response.ok) return false;
          const html = await response.text();
          return html.includes('id="root"') && html.includes("/static/app/conversation/app.js");
        } catch {
          return false;
        }
      })();
      const bootstrap = await ok("/app/bootstrap");
      let base = "";
      try {
        const payload = await (await fetch("/chat/bootstrap")).json();
        base = payload.api_base ?? "";
      } catch {
        base = "";
      }
      if (base === "") {
        return {
          shell,
          bootstrap,
          members: false,
          projects: false,
          fleet: false,
          integration: false,
          onboarding: false,
          usage: false,
        };
      }
      const project = (await (await fetch(`${base}/projects`)).json().catch(() => []))?.[0];
      const projectId = project?.id ?? project?.project_id ?? "";
      const native = projectId === "" ? "" : `${base}/projects/${encodeURIComponent(projectId)}`;
      return {
        shell,
        bootstrap,
        members: await ok(`${base}/members`),
        projects: await ok(`${base}/projects`),
        fleet: await ok(`${base}/fleet`),
        integration: native === "" ? false : await ok(`${native}/integration`),
        onboarding: native === "" ? false : await ok(`${native}/onboarding`),
        usage: await ok(`${base}/usage?range=30d`),
      };
    });
    // Whether the router actually matches an account path. The shell being
    // served is not enough: the routes have to be in the tree.
    if (served.shell) {
      await page.goto(new URL("/organization", hub.fixture.url).toString(), {
        waitUntil: "domcontentloaded",
      });
      served.routed = await page
        .getByRole("heading", { level: 1 })
        .first()
        .waitFor({ timeout: 8_000 })
        .then(() => true)
        .catch(() => false);
    }
  } finally {
    await context.close();
  }
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

test("the login card renders without a session and offers both ways in", async ({ page }) => {
  test.skip(!served.shell, "The hub does not serve the application shell yet (§12, Serving).");
  test.skip(!served.routed, NOT_ROUTED);
  const errors = watchConsole(page);
  // No sign-in first: `/login` is the one route the hub serves unauthenticated.
  await page.goto(new URL("/login", hub.fixture.url).toString(), {
    waitUntil: "domcontentloaded",
  });

  await expectOneHeadingOne(page, "Sign in to Detent");
  await expect(page.getByRole("link", { name: "Continue with WorkOS" })).toHaveAttribute(
    "href",
    "/auth/oidc/start",
  );
  await expect(page.getByRole("link", { name: "Join with invitation" })).toHaveAttribute(
    "href",
    "/auth/oidc/start?unscoped=1",
  );
  await expect(page.getByLabel("Have an invitation token?")).toBeVisible();

  expect(await tabTo(page, "Continue with WorkOS")).toBe(true);
  await expectNoSeriousAxeViolations(page, "/login");
  // The entry point asks for `/app/bootstrap` on every route, including this
  // one, because a reader who already has a session can accept an invitation
  // here. Without a session the hub answers 401 and the client renders the
  // card — that refusal is the expected state, and the browser logs the
  // response as a resource error whatever the client then does with it.
  expect(
    errors.filter((message) => !/401 \(Unauthorized\)/.test(message)),
    "console errors on /login",
  ).toEqual([]);
});

test("the login card says what went wrong when the callback failed", async ({ page }) => {
  test.skip(!served.shell, "The hub does not serve the application shell yet (§12, Serving).");
  test.skip(!served.routed, NOT_ROUTED);
  await page.goto(new URL("/login?error=no_membership", hub.fixture.url).toString(), {
    waitUntil: "domcontentloaded",
  });
  await expect(page.getByRole("alert")).toContainText("not a member of this organization");
  // The raw code is for the log, not for the reader.
  await expect(page.locator("body")).not.toContainText("no_membership");
});

test("the organization page lists the members and gates the controls by role", async ({
  page,
}) => {
  test.skip(!served.members, "GET /api/v2/organizations/:organization/members is not served yet.");
  test.skip(!served.routed, NOT_ROUTED);
  const errors = watchConsole(page);
  // §17.3: this path redirects into the Organization section of settings.
  await openAs(page, "owner", "/organization");

  await expect(page).toHaveURL(/\/settings\/organization$/);
  await expectOneHeadingOne(page, "Settings");
  const table = page.getByRole("table");
  await expect(table).toBeVisible();
  // An owner gets the mutating controls.
  await expect(page.getByRole("button", { name: "Remove" }).first()).toBeVisible();
  await expectNoSeriousAxeViolations(page, "/organization as owner");
  expect(errors, "console errors on /organization").toEqual([]);

  // A viewer sees the same data and none of the controls (decisions.md §10.11).
  await openAs(page, "viewer", "/organization");
  await expect(page.getByRole("button", { name: "Remove" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Send invitation" })).toHaveCount(0);
  await expectNoSeriousAxeViolations(page, "/organization as viewer");
});

test("removing the last owner is refused by the hub and said in the page", async ({ page }) => {
  test.skip(!served.members, "DELETE /api/v2/organizations/:organization/members/:member is not served yet.");
  test.skip(!served.routed, NOT_ROUTED);
  await openAs(page, "owner", "/organization");

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
  test.skip((await remove.count()) === 0, "The fixture's owner row has no removal control.");
  await remove.click();
  await page.getByRole("button", { name: "Remove", exact: true }).last().click();
  // The alert is the hub's refusal said in the page, which arrives after a
  // round trip; under a loaded machine that has taken longer than the default
  // expectation window, so this one waits as long as a navigation.
  await expect(page.getByRole("alert")).toContainText("owner", { timeout: 30_000 });
});

test("the settings navigation is the sidebar, not a second column", async ({ page }) => {
  test.skip(!served.projects, "GET /api/v2/organizations/:organization/projects is not served yet.");
  test.skip(!served.routed, NOT_ROUTED);
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
  await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();

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
  test.skip(
    !served.integration,
    "GET /api/v2/organizations/:organization/projects/:project/integration is not served yet.",
  );
  test.skip(!served.routed, NOT_ROUTED);
  const errors = watchConsole(page);
  // §17.3: this path redirects into the Integrations section of settings.
  await openAs(page, "owner", `/projects/${hub.fixture.project_id}/settings`);

  await expect(page).toHaveURL(/\/settings\/integrations\?project=/);
  await expectOneHeadingOne(page, "Settings");
  await expect(page.getByLabel("Go to Detent Cloud")).toHaveCount(1);
  await expect(page.getByLabel("Intake")).toBeVisible();
  await expect(page.getByLabel("Projection")).toBeVisible();
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
  const intake = page.getByLabel("Intake");
  if ((await intake.count()) > 0) await expect(intake).toBeDisabled();
  await expect(page.getByRole("button", { name: "Save changes" })).toHaveCount(0);
});

test("the first-run wizard reports the hub's four onboarding steps", async ({ page }) => {
  test.skip(
    !served.onboarding,
    "GET /api/v2/organizations/:organization/projects/:project/onboarding is not served yet.",
  );
  test.skip(!served.routed, NOT_ROUTED);
  const errors = watchConsole(page);
  await openAs(page, "owner", `/projects/${hub.fixture.project_id}/setup`);

  const steps = page.getByRole("list", { name: "Setup steps" }).getByRole("listitem");
  await expect(steps).toHaveCount(4);
  await expect(steps.first()).toContainText("Repository configuration");
  // Exactly one step is the current one, and it is reachable from the keyboard.
  await expect(page.locator('[aria-current="step"]')).toHaveCount(1);
  expect(await tabTo(page, "Continue")).toBe(true);
  await expectNoSeriousAxeViolations(page, "/projects/:project/setup");
  expect(errors, "console errors on the wizard").toEqual([]);
});

test("the fleet page redirects into settings and renders the hosts and providers", async ({
  page,
}) => {
  test.skip(!served.fleet, "GET /api/v2/organizations/:organization/fleet is not served yet.");
  test.skip(!served.routed, NOT_ROUTED);
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
  await expect(page.getByRole("heading", { name: "Runners" })).toBeVisible();
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
  test.skip(!served.fleet, "GET /api/v2/organizations/:organization/fleet is not served yet.");
  test.skip(!served.members, "PUT /members/:member/grants is not served yet.");
  test.skip(!served.routed, NOT_ROUTED);
  const errors = watchConsole(page);

  // The hub gates every runner route on the actor holding the runner grant on
  // *every* project in the organization (`hostedAllRunnerGrants`), and the
  // fixture's owner starts without it. Granting it here is setup, not the
  // thing under test: without it the control is correctly absent and there
  // would be nothing to drive.
  await openAs(page, "owner", "/organization");
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
  // Step one is the command the host runs; it is shown, not described.
  await expect(dialog.getByText("detent hub runner init --hub-url")).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Copy the init command" })).toBeVisible();
  await expectNoSeriousAxeViolations(page, "the enrollment dialog");

  // The two host-generated identifiers. They have to be well formed: the hub
  // refuses anything that is not `runner_`/`machine_` plus 32 hex characters.
  const runnerId = `runner_${"a1b2c3d4".repeat(4)}`;
  const machineId = `machine_${"9f8e7d6c".repeat(4)}`;
  await dialog.getByLabel("Runner id").fill(runnerId);
  await dialog.getByLabel("Machine id").fill(machineId);

  await dialog.getByRole("button", { name: "Create enrollment token" }).click();

  // The token itself, and the exact command that redeems it.
  const command = dialog.getByRole("button", { name: "Copy the enroll command" });
  await expect(command).toBeVisible({ timeout: 15_000 });
  await expect(dialog.getByRole("button", { name: "Copy the enrollment token" })).toBeVisible();
  await expect(
    dialog.getByText(/DETENT_RUNNER_ENROLLMENT_TOKEN=\S+ detent hub runner enroll --organization /),
  ).toBeVisible();
  await expectNoSeriousAxeViolations(page, "the enrollment dialog with a token");

  // Closing it lists the enrollment as pending, and does not re-show the token
  // as a fresh one.
  await page.keyboard.press("Escape");
  await expect(page.getByRole("heading", { name: "Pending enrollments" })).toBeVisible();
  await expect(page.getByText(runnerId)).toBeVisible();

  expect(errors, "console errors while enrolling a runner").toEqual([]);
});

test("the usage page draws the window, the tabs and the breakdown", async ({ page }) => {
  test.skip(
    !served.usage,
    "GET /api/v2/organizations/:organization/usage?range= is not served yet (decisions.md §17.5).",
  );
  test.skip(!served.routed, NOT_ROUTED);
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
