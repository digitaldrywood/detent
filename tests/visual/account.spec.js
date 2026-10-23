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

let hub;

for (const viewport of [
  { name: "desktop-light", width: 1440, height: 900, theme: "light" },
  { name: "desktop-dark", width: 1440, height: 900, theme: "dark" },
  { name: "phone-light", width: 390, height: 844, theme: "light" },
  { name: "phone-dark", width: 390, height: 844, theme: "dark" },
]) {
  test(`repository picker Connect GitHub no-match ${viewport.name}`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    await page.route("**/integration", async (route) => {
      const response = await route.fetch();
      const integration = await response.json();
      await route.fulfill({ json: { ...integration, github_transport_available: true, checkout_repository: "", repository: "" } });
    });
    await page.route("**/api/cloud/account/github/repositories", (route) => route.fulfill({ json: { available: true, connected: false, reconnect: false, repositories: [] } }));
    await openAs(page, "owner", `/projects/${hub.fixture.project_id}/setup`);
    await page.evaluate((theme) => {
      localStorage.setItem("detent.theme", theme);
      document.documentElement.classList.toggle("dark", theme === "dark");
    }, viewport.theme);
    await page.getByRole("button", { name: /Repository action required/ }).click();
    await page.getByRole("combobox", { name: "Search or type owner/name" }).fill("example-org/unlisted-project");
    await expect(page.getByRole("button", { name: /Connect GitHub to list more repositories/ })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath(`connect-github-${viewport.name}.png`), animations: "disabled", fullPage: true });
  });
}

test.beforeEach(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("account", { env: { DETENT_HOSTED_BROWSER_UNSIGNED_MEMBER: "1" } });
});

test.afterEach(async () => {
  await hub?.stop();
  hub = undefined;
});

/** Collects everything the page logs as an error, for a per-test assertion. */
function watchConsole(page, expected = () => false) {
  const errors = [];
  page.on("console", (message) => {
    if (message.type() === "error" && !expected(message)) errors.push(message.text());
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
  await openAs(page, "owner", "/settings/organization");
  await expect(page.getByRole("button", { name: "Overview", exact: true })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("heading", { name: "Your organizations", exact: true })).toBeVisible();
  await page.getByRole("button", { name: /^Members/ }).click();

  await expect(page).toHaveURL(/\/settings\/organization\/members$/);
  await expectOneHeadingOne(page, "Settings");
  const table = page.getByRole("table");
  await expect(table).toBeVisible();
  const identities = table.getByRole("row").filter({ has: page.getByRole("button", { name: /^Actions for/ }) });
  await expect(identities).toHaveCount(3);
  for (const identity of await identities.all()) {
    await expect(identity).not.toHaveText(/^\s*$/);
  }
  const unsigned = table.getByRole("row").filter({ hasText: "unsigned@example.test" });
  await expect(unsigned).toContainText("Unsigned Member");
  await expect(unsigned).toContainText("Hasn't signed in");
  await expect(unsigned.getByRole("button", { name: "Actions for unsigned@example.test" })).toBeEnabled();
  // An owner gets the mutating controls.
  await expect(page.getByRole("button", { name: /^Actions for/ }).first()).toBeVisible();
  await expectNoSeriousAxeViolations(page, "/organization as owner");
  expect(errors, "console errors on /organization").toEqual([]);
  await unsigned.getByRole("button", { name: "Actions for unsigned@example.test" }).click();
  await page.getByRole("menuitem", { name: "Remove from organization" }).click();
  await page.getByRole("button", { name: "Remove", exact: true }).last().click();
  await expect(unsigned).toHaveCount(0);

  // A viewer sees the same data and none of the controls (decisions.md §10.11).
  await openAs(page, "viewer", "/settings/organization/members");
  await expect(page.getByRole("table")).toBeVisible();
  await expect(page.getByRole("button", { name: "Remove" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Invite member", exact: true })).toHaveCount(0);
  await expectNoSeriousAxeViolations(page, "/organization as viewer");
});

test("sending invitations resets the form and shows the newest invitation with its enforced expiry", async ({ page }) => {
  await openAs(page, "owner", "/settings/organization/members");
  const invitations = page.getByRole("region", { name: "Pending invitations" });
  for (const email of ["older-invite@example.test", "newest-invite@example.test"]) {
    await page.getByRole("button", { name: "Invite member", exact: true }).first().click();
    await page.getByLabel("Email addresses", { exact: true }).fill(email);
    await page.getByRole("combobox", { name: "Role", exact: true }).click();
    await page.getByRole("option", { name: "Viewer", exact: true }).click();
    const responsePromise = page.waitForResponse((response) =>
      response.request().method() === "POST" && response.url().endsWith("/members/invitations"),
    );
    await page.getByRole("button", { name: "Send 1 invitation", exact: true }).click();
    const response = await responsePromise;
    expect(response.status()).toBe(201);
    const created = await response.json();
    expect(created.email).toBe(email);
    expect(created.expires_at).toBeTruthy();
    const expiry = await page.evaluate((value) => new Date(value).toLocaleDateString(), created.expires_at);
    await expect(page.getByRole("dialog", { name: "Invite members", exact: true })).toHaveCount(0);
    const row = invitations.getByRole("group", { name: /^Invitation to/ }).first();
    await expect(row).toBeVisible();
    await expect(row).toContainText(email);
    await expect(row).toContainText(`Invited as Viewer. Expires ${expiry}.`);
    await page.reload();
    await expect(invitations.getByRole("group", { name: /^Invitation to/ }).first()).toContainText(`Expires ${expiry}.`);
  }
});

test("removing the last owner is refused by the hub and said in the page", async ({ page }) => {
  await openAs(page, "owner", "/settings/organization/members");

  // The owner's row by the owner's address, with no fallback. This used to
  // read `hub.fixture.accounts.owner_email ?? "@"`, and `accounts` maps an
  // account *name* to a sign-in URL and has no `owner_email` key at all, so the
  // filter quietly degraded to "@" — which matches every member row. `.first()`
  // then took whichever member the hub happened to list first, and the test
  // removed the viewer instead of the owner and waited for a refusal that was
  // never coming.
  const ownerRow = page.getByRole("row").filter({ hasText: hub.fixture.owner_email });
  await expect(ownerRow).toHaveCount(1);
  const remove = ownerRow.getByRole("button", { name: `Actions for ${hub.fixture.owner_email}` });
  await expect(remove).toBeVisible();
  await remove.click();
  await page.getByRole("menuitem", { name: "Remove from organization" }).click();
  await page.getByRole("button", { name: "Remove", exact: true }).last().click();
  // The alert is the hub's refusal said in the page, which arrives after a
  // round trip; under a loaded machine that has taken longer than the default
  // expectation window, so this one waits as long as a navigation.
  await expect(page.getByRole("alertdialog").getByRole("alert")).toContainText("owner", { timeout: 30_000 });
});

test("the settings navigation is the sidebar, not a second column", async ({ page }) => {
  // The fixture's sign-in provider issues no WorkOS widget tokens, so the
  // account widgets on General get `503` from `widget-token` and render their
  // unavailable state. That refusal is expected here and nothing else is.
  const errors = watchConsole(page, (message) => /503 \(Service Unavailable\)/.test(message.text()) && /\/widget-token$/.test(message.location().url ?? ""));
  // `/settings` redirects to the default section (§17.3).
  await openAs(page, "owner", "/settings");

  await expectOneHeadingOne(page, "Settings");
  await expect(page.getByLabel("Settings breadcrumb")).toContainText("General");
  await expect(page.getByText("Profile and security settings are unavailable right now.", { exact: true })).toBeVisible();

  const sidebar = page.locator("[data-app-sidebar]");
  await expect(sidebar.getByRole("button", { name: "General" })).toBeVisible();
  await expect(sidebar.getByRole("button", { name: "Organization", exact: true })).toBeVisible();
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
  await expect(page.getByRole("heading", { name: /^Projects\b/ })).toBeVisible();
  await expect(page.getByRole("table", { name: "Projects in dispatch order", exact: true })).toBeVisible();

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
  await openAs(page, "owner", `/settings/projects/${hub.fixture.project_id}/repository`);

  await expect(page).toHaveURL(/\/settings\/projects\/[^/]+\/repository$/);
  await expectOneHeadingOne(page, /settings$/);
  await expect(page.getByLabel("Go to Detent Cloud")).toHaveCount(1);
  await expect(page.getByText(/Hub GitHub integration is unavailable/)).toBeVisible();
  await expect(page.getByRole("switch", { name: "Pull requests" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Associate runner checkout" })).toBeVisible();
  await expect(page.getByLabel("Intake", { exact: true })).toHaveCount(0);
  await expect(page.getByLabel("Projection", { exact: true })).toHaveCount(0);
  await expectNoSeriousAxeViolations(page, "/projects/:project/settings as owner");
  // `GET {nativeBase}/policy` answers `409 policy_mismatch` on a project with
  // no approved descriptor, which is this fixture's state and a state the
  // screen renders rather than an error it hit. The browser logs the response
  // either way, so the expected refusal is filtered and nothing else is.
  expect(
    errors.filter((message) => !/409 \(Conflict\)/.test(message)),
    "console errors on project settings",
  ).toEqual([]);

  await openAs(page, "viewer", `/settings/projects/${hub.fixture.project_id}/repository`);
  await expect(page.getByText(/Hub GitHub integration is unavailable/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Associate runner checkout" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Save changes" })).toHaveCount(0);
});

test("pending invitations can be resent and revoked from their row", async ({ page }) => {
  const email = "withdraw@example.test";
  await openAs(page, "owner", "/settings/organization/members");
  await page.getByRole("button", { name: "Invite member", exact: true }).first().click();
  await page.getByLabel("Email addresses", { exact: true }).fill(email);
  await page.getByLabel("Email addresses", { exact: true }).press("Enter");
  await page.getByRole("button", { name: "Send 1 invitation", exact: true }).click();
  const row = page.getByRole("group", { name: `Invitation to ${email}`, includeHidden: true });
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
  const dialog = page.getByRole("alertdialog");
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
  await openAs(page, "viewer", "/settings/organization/members");
  await expect(page.getByRole("button", { name: "Revoke", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Resend", exact: true })).toHaveCount(0);
});

for (const state of [
  { name: "installed", installed: true, profile: "native", repository: "example-org/example-project", transport: true, visible: true },
  { name: "not installed", installed: false, profile: "native", repository: "example-org/example-project", transport: true, visible: true },
  { name: "long repository", installed: true, profile: "native", repository: "example-org/" + "long-repository-name-".repeat(5), transport: true, visible: true },
  { name: "compatibility project", installed: true, profile: "github_compatible", repository: "example-org/example-project", transport: true, visible: false },
  { name: "unassociated repository", installed: false, profile: "native", repository: "", transport: true, visible: false },
  { name: "unavailable transport", installed: false, profile: "native", repository: "example-org/example-project", transport: false, visible: false },
].flatMap((state) => state.visible ? [1440, 390].flatMap((width) => ["light", "dark"].map((theme) => ({ ...state, width, theme }))) : [{ ...state, width: 1440, theme: "light" }])) {
  test(`setup and integrations GitHub App installation: ${state.name} ${state.width}px ${state.theme}`, async ({ page }) => {
    await page.setViewportSize({ width: state.width, height: 1100 });
    await page.route("**/api/cloud/account/github/repositories", (route) => route.fulfill({ json: { available: state.transport, connected: false, reconnect: false, repositories: [] } }));

    const integrationURL = "**/projects/*/integration";
    let integrationReady = Promise.resolve();
    await page.route(integrationURL, async (route) => {
      const response = await route.fetch();
      const integration = await response.json();
      await integrationReady;
      await route.fulfill({ response, json: {
        ...integration,
        profile: state.profile,
        intake: state.profile === "native" && state.repository ? "automatic" : "disabled",
        manual_import_enabled: false,
        projection: "summary",
        repository_enabled: true,
        repository: state.repository,
        checkout_repository: state.repository,
        checkout_verified: Boolean(state.repository),
        github_transport_available: state.transport,
        github_app_slug: "detent-cloud",
        github_app_install_url: "https://github.com/apps/detent-cloud/installations/new",
        github_app_installed: state.installed,
      } });
    });
    const setupIntegration = page.waitForResponse(integrationURL);
    await openAs(page, "owner", `/projects/${hub.fixture.project_id}/setup`);
    await (await setupIntegration).finished();
    await page.getByRole("list", { name: "Setup steps" }).getByRole("button", { name: /^.*Repository (ready|action required)$/ }).click();
    if (state.repository) {
      await expect(page.getByRole("combobox", { name: "Search or type owner/name" })).toHaveValue(state.repository);
      await expect(page.getByRole("combobox", { name: "Search or type owner/name" })).toBeDisabled();
    }
    const install = page.getByRole("link", { name: "Install the Detent Cloud GitHub App" });
    if (state.visible) {
      await expect(install).toHaveAttribute("href", "https://github.com/apps/detent-cloud/installations/new");
      await expect(install).toHaveAttribute("rel", "noopener noreferrer");
      await expect(page.getByText(`Detent Cloud is ${state.installed ? "installed" : "not installed"} on ${state.repository}`, { exact: true })).toBeVisible();
      await expect(page.getByText(/New GitHub issues enter Triage automatically once/)).toBeVisible();
      await expectNoSeriousAxeViolations(page, `GitHub App ${state.name}`);
    } else {
      await expect(install).toHaveCount(0);
      await expect(page.getByText(/Detent Cloud is (not )?installed on/)).toHaveCount(0);
    }
    let releaseIntegration;
    integrationReady = new Promise((resolve) => { releaseIntegration = resolve; });
    try {
      await page.goto(new URL(`/settings/projects/${hub.fixture.project_id}/repository`, hub.fixture.url).toString(), { waitUntil: "domcontentloaded" });
      await expect(page.getByRole("heading", { name: /settings$/, level: 1 })).toBeVisible();
      await expect(page.getByRole("status", { name: "Loading project settings" })).toBeVisible();
    } finally {
      releaseIntegration();
    }
    await expect(page.getByRole("heading", { name: "Repository & GitHub", level: 2, exact: true })).toBeVisible();
    await page.evaluate((theme) => {
      document.documentElement.classList.toggle("dark", theme === "dark");
      document.documentElement.dataset.theme = theme;
    }, state.theme);
    if (state.visible) {
      await expect(page.getByRole("heading", { name: "GitHub App", exact: true })).toBeVisible();
      await expect(page.getByText(state.installed ? "Installed" : "Not installed · new issues will not arrive", { exact: true })).toBeVisible();
      const link = page.getByRole("link", { name: state.installed ? "Manage installation" : "Install GitHub App", exact: true });
      await expect(link).toHaveAttribute("href", "https://github.com/apps/detent-cloud/installations/new");
      await expect(link).toHaveAttribute("target", "_blank");
      await expect(link).toHaveAttribute("rel", "noopener noreferrer");
      await expect(page.getByRole("link", { name: "Install GitHub App", exact: true })).toHaveCount(state.installed ? 0 : 1);
      const intake = page.locator('[data-slot="settings-row"]').filter({ has: page.getByRole("heading", { name: "New issues", exact: true }) });
      await expect(intake).toContainText("Automatic");
      await expect(intake).toContainText(state.installed ? "Enter Triage" : "Waiting for the GitHub App");
      await expect(page.getByRole("combobox", { name: "Intake", exact: true })).toHaveCount(0);
      await expect(page.getByRole("button", { name: "Save", exact: true })).toHaveCount(0);
      await expect(page.getByRole("switch", { name: "Existing issues", exact: true })).not.toBeChecked();
      const invalidSelects = await page.locator("select").evaluateAll((selects) => selects.filter((select) =>
        !Array.from(select.options).some((option) => option.value === select.value)).length);
      expect(invalidSelects).toBe(0);
      await link.focus();
      await page.keyboard.press("Tab");
      await expect(page.getByRole("link", { name: "Import issues", exact: true })).toBeFocused();
      const importSwitch = page.getByRole("switch", { name: "Existing issues", exact: true });
      await importSwitch.focus();
      await page.keyboard.press("Space");
      await expect(importSwitch).toBeChecked();
      await expect(page.getByRole("button", { name: "Save", exact: true })).toBeEnabled();
      await page.keyboard.press("Space");
      await expect(page.getByRole("button", { name: "Save", exact: true })).toHaveCount(0);
      const headings = await page.getByRole("heading").allTextContents();
      const setupRows = headings.filter((title) => ["GitHub App", "New issues", "Existing issues", "Pull requests", "Work summaries", "Authority"].includes(title));
      expect(setupRows).toEqual(["GitHub App", "New issues", "Existing issues", "Pull requests", "Work summaries", "Authority"]);
      if (state.width === 390) {
        const newIssuesRow = page.locator('[data-slot="settings-row"]').filter({ has: page.getByRole("heading", { name: "New issues", exact: true }) });
        expect(await newIssuesRow.locator(":scope > div").evaluate((element) => getComputedStyle(element).flexDirection)).toBe("column");
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      }
      await page.screenshot({ path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `integrations-${state.name.replaceAll(" ", "-")}-${state.width}-${state.theme}.png`), fullPage: true });
      await page.getByRole("switch", { name: "Pull requests", exact: true }).click();
      await expect(page.getByRole("button", { name: "Save", exact: true })).toBeEnabled();
      await expectNoSeriousAxeViolations(page, `Integrations GitHub App ${state.name}`);
      if (state.width === 390) {
        // A project's Models tab shows only a summary until its owner edits an
        // override, so the read-only wide tables live on the organization
        // Models page, read by a viewer.
        const viewerContext = await page.context().browser().newContext();
        const models = await viewerContext.newPage();
        await models.setViewportSize({ width: state.width, height: 1100 });
        await models.goto(hub.fixture.accounts.viewer, { waitUntil: "domcontentloaded" });
        await models.goto(new URL("/settings/organization/models", hub.fixture.url).toString(), { waitUntil: "domcontentloaded" });
        for (const name of ["Complexity levels", "Stages"]) {
          const table = models.getByRole("table", { name, exact: true });
          await table.focus();
          await expect(table).toBeFocused();
          const left = await table.evaluate((element) => element.parentElement.scrollLeft);
          await models.keyboard.press("ArrowRight");
          await expect.poll(() => table.evaluate((element) => element.parentElement.scrollLeft)).toBeGreaterThan(left);
        }
        await viewerContext.close();
      }
    } else {
      await expect(page.getByRole("heading", { name: "GitHub App", exact: true })).toBeVisible();
      if (state.profile === "github_compatible") {
        await expect(page.getByRole("link", { name: "Manage installation", exact: true })).toBeVisible();
        await expect(page.getByRole("switch", { name: "Work summaries", exact: true })).toBeDisabled();
      } else {
        await expect(page.getByText(state.transport ? "Associate a runner checkout to configure repository integration." : "Hub GitHub integration is unavailable; PR landing uses the runner’s repository configuration.", { exact: true })).toBeVisible();
        await expect(page.getByRole("switch", { name: "Pull requests", exact: true })).toHaveCount(0);
      }
      await expect(install).toHaveCount(0);
      await expect(page.getByText(/Detent Cloud is (not )?installed on/)).toHaveCount(0);
    }
  });
}

test("the first-run wizard orders runner, repository, checkout and run files", async ({ page }) => {
  const errors = watchConsole(page);
  await openAs(page, "owner", `/projects/${hub.fixture.project_id}/setup`);

  const steps = page.getByRole("list", { name: "Setup steps" }).getByRole("listitem");
  await expect(steps).toHaveCount(4);
  await expect(steps.first()).toContainText("Runner");
  // Exactly one step is the current one, and it is reachable from the keyboard.
  await expect(steps.locator('[aria-current="step"]')).toHaveCount(1);
  expect(await tabTo(page, "Copy the install command")).toBe(true);
  await expectNoSeriousAxeViolations(page, "/projects/:project/setup");
  expect(errors, "console errors on the wizard").toEqual([]);
});

for (const viewport of [
  { name: "desktop-light", width: 1440, height: 900, theme: "light" },
  { name: "desktop-dark", width: 1440, height: 900, theme: "dark" },
  { name: "phone-light", width: 390, height: 844, theme: "light" },
  { name: "phone-dark", width: 390, height: 844, theme: "dark" },
]) {
  test(`the setup wizard fits a ${viewport.name} and its choices show what is selected`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    const errors = watchConsole(page);
    await openAs(page, "owner", `/projects/${hub.fixture.project_id}/setup`);

    await page.evaluate((theme) => {
      localStorage.setItem("detent.theme", theme);
      document.documentElement.classList.toggle("dark", theme === "dark");
    }, viewport.theme);
    const context = page.getByRole("region", { name: "Project setup context" });
    const bootstrap = await page.evaluate(async () => (await fetch("/app/bootstrap")).json());
    const project = bootstrap.projects.find((entry) => entry.id === hub.fixture.project_id);
    await expect(context.getByText(`Setting up a project in ${bootstrap.organization.name}`, { exact: true })).toBeVisible();
    await expect(context.getByText(project.name, { exact: true })).toBeVisible();
    await expect(page).toHaveTitle(`${project.name} setup · Detent`);
    const steps = page.getByRole("list", { name: "Setup steps" });
    for (const label of ["Runner", "Repository", "Checkout", "Run files"]) {
      await expect(steps.getByText(label, { exact: true })).toBeVisible();
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0);
    expect(await steps.evaluate((element) => element.scrollWidth - element.clientWidth)).toBeLessThanOrEqual(0);
    await page.screenshot({ path: testInfo.outputPath(`setup-context-${viewport.name}.png`), animations: "disabled" });
    await context.getByRole("button", { name: "Switch project" }).click();
    await expect(page.getByRole("combobox", { name: "Search projects" })).toBeFocused();
    await page.keyboard.press("Escape");
    if (viewport.width === 390) await page.keyboard.press("Escape");

    const tabs = page.getByRole("list", { name: "Setup steps" }).getByRole("button");
    await expect(tabs).toHaveCount(4);
    await page.getByRole("list", { name: "Setup steps" }).getByRole("button", { name: /Run files/ }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Where should run files go?" })).toBeVisible();

    // Every tab and the whole step fit inside the viewport: nothing is
    // clipped at either edge.
    for (const locator of [page.getByRole("list", { name: "Setup steps" }), page.getByRole("heading", { level: 1 })]) {
      const box = await locator.boundingBox();
      expect(box, "wizard element has a box").not.toBeNull();
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
    }

    // Clicking anywhere on a card chooses it, and the choice is visible.
    const local = page.getByRole("radio", { name: /^Keep them on / });
    const selectedChoice = page.locator("[data-checked]").filter({ has: page.getByRole("radio") });
    await page.getByText(/^Keep them on /).click();
    await expect(local).toBeChecked();
    await expect(selectedChoice).toHaveCount(1);
    await expect(selectedChoice).toContainText("Keep them on ");
    await page.getByText("Your bucket and gateway", { exact: true }).click();
    await expect(page.getByRole("radio", { name: "Your bucket and gateway" })).toBeChecked();
    await expect(local).not.toBeChecked();
    await expect(selectedChoice).toHaveCount(1);
    await expect(selectedChoice).toContainText("Your bucket and gateway");

    // Every step stays reachable from the tabs.
    await page.getByRole("list", { name: "Setup steps" }).getByRole("button", { name: /Runner (ready|action required)/ }).click();
    await expect(page.getByRole("heading", { level: 1, name: `Connect a computer to ${project.name}` })).toBeVisible();
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
    await expect(page.getByText("No runners yet. Add a runner manually or let Luna help you set one up.")).toBeVisible();
  }
  if (await page.getByTestId("host-card").count()) {
    await page.getByText("Fleet capacity and providers", { exact: true }).click();
    await expect(page.getByRole("heading", { name: "Providers", exact: true })).toBeVisible();
  }

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

test("Providers & runners enrolls a host through setup, a masked command, and live check-in", async ({ page }, testInfo) => {
  const errors = watchConsole(page);
  let connected = false;
  const fleet = require("../../web/conversation/src/contracts/fixtures/account-fleet.json");
  const runner = { ...fleet.runners[0], id: "rnr_enrolled", display_name: "Build host", hostname: "build-host.local" };
  await page.route("**/api/v2/organizations/*/fleet", (route) => route.fulfill({
    json: { ...fleet, runners: connected ? [runner] : [] },
  }));
  await page.route(`**/api/v2/organizations/*/runners/${runner.id}/capacity`, (route) => route.fulfill({
    json: {},
  }));
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: {
      writeText: async (value) => { window.copiedEnrollmentCommand = value; },
    } });
  });

  // The hub gates every runner route on the actor holding the runner grant on
  // *every* project in the organization (`hostedAllRunnerGrants`), and the
  // fixture's owner starts without it. Granting it here is setup, not the
  // thing under test: without it the control is correctly absent and there
  // would be nothing to drive.
  await openAs(page, "owner", "/settings/organization/members");
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
  const enroll = page.getByRole("button", { name: "Add runner", exact: true });
  await expect(enroll).toBeVisible();
  await enroll.click();
  await page.getByRole("menuitem", { name: "Manual", exact: true }).click();

  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("heading", { name: "Enroll a runner" })).toBeVisible();
  await expect(dialog.getByLabel("Runner scope")).toHaveValue("organization");
  await dialog.getByLabel("Runner scope").selectOption("projects");
  const projects = await page.evaluate(async () => (await (await fetch("/app/bootstrap")).json()).projects);
  expect(projects.length).toBeGreaterThan(0);
  for (const project of projects) {
    await dialog.getByRole("checkbox", { name: project.name, exact: true }).setChecked(project.id === hub.fixture.project_id);
  }
  await dialog.getByRole("radio", { name: "macOS", exact: true }).check();
  await dialog.getByRole("checkbox", { name: /Install it as a background service/ }).uncheck();
  await expect(dialog.locator('[aria-current="step"]')).toHaveText("1. Set it up");
  // Nothing to copy from the host first: the dialog asks for a name, a
  // capacity and projects, never for runner or machine IDs.
  await expect(dialog.getByLabel("Runner id")).toHaveCount(0);
  await dialog.getByLabel("Name").fill("Build host");
  await dialog.getByLabel("Concurrency", { exact: true }).fill("1");
  await expectNoSeriousAxeViolations(page, "the enrollment dialog");

  await dialog.getByRole("radio", { name: "Sandbox", exact: true }).check();
  const enrollmentResponse = page.waitForResponse((response) => response.url().endsWith("/runner-enrollments") && response.request().method() === "POST");
  await dialog.getByRole("button", { name: "Create command" }).click();
  const response = await enrollmentResponse;
  expect(response.status()).toBe(201);
  expect(response.request().postDataJSON()).toEqual({
    scope: "projects",
    project_ids: [hub.fixture.project_id],
    operations: ["read", "collaborate", "claim", "heartbeat", "events"],
    ttl_seconds: 900,
  });

  // One command carries everything the host needs.
  const copy = dialog.getByRole("button", { name: "Copy the register command" });
  await expect(copy).toBeVisible({ timeout: 15_000 });
  await expect(dialog.locator('[aria-current="step"]')).toHaveText("2. Run the command");
  await expect(dialog).toContainText("detent_••••••••");
  await expect(dialog.getByText(/Waiting for Build host to check in/)).toBeVisible();
  await dialog.getByRole("button", { name: "Show token" }).click();
  const registerCommand = dialog.getByText(/^detent hub runner register --url \S+ (--organization \S+ )?--token \S+ --name 'Build host' --isolation-tier sandbox$/);
  await expect(registerCommand).toBeVisible();
  await expectNoSeriousAxeViolations(page, "the enrollment dialog with its command");

  const command = await registerCommand.textContent();
  const token = command.match(/--token (\S+)/)[1];
  await dialog.getByRole("button", { name: "Hide token" }).click();
  expect(await dialog.innerHTML()).not.toContain(token);
  for (const colorScheme of ["light", "dark"]) {
    await page.emulateMedia({ colorScheme });
    await page.evaluate((theme) => {
      document.documentElement.classList.toggle("dark", theme === "dark");
      document.documentElement.dataset.theme = theme;
    }, colorScheme);
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 1100 });
      const screenshot = require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `selected-project-sandbox-command-${colorScheme}-${width}.png`);
      await page.screenshot({ path: screenshot });
      await testInfo.attach(`selected-project-sandbox-command-${colorScheme}-${width}`, { path: screenshot, contentType: "image/png" });
    }
  }
  await page.emulateMedia({ colorScheme: "light" });
  await page.evaluate(() => {
    document.documentElement.classList.remove("dark");
    document.documentElement.dataset.theme = "light";
  });
  await page.setViewportSize({ width: 1440, height: 1100 });
  await copy.click();
  expect(await page.evaluate(() => window.copiedEnrollmentCommand)).toBe(command);
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
  await enroll.click();
  await page.getByRole("menuitem", { name: "Manual", exact: true }).click();
  await expect(dialog.getByLabel("Runner scope")).toHaveValue("organization");
  await expect(dialog.getByRole("checkbox", { name: /Install it as a background service/ })).toBeChecked();
  await dialog.getByLabel("Name").fill("Build host");
  await dialog.getByLabel("Concurrency", { exact: true }).fill("2");
  await dialog.getByRole("radio", { name: "Full access", exact: true }).check();
  await dialog.getByRole("button", { name: "Create command" }).click();
  await expect(dialog.locator('[aria-current="step"]')).toHaveText("2. Run the command");
  await expect(dialog.getByText(/^detent hub runner register .* --capacity 2 --isolation-tier native-trusted --service$/)).toBeVisible();
  connected = true;
  await expect(dialog.getByText("Build host is connected")).toBeVisible();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(dialog.locator('[aria-current="step"]')).toHaveText("3. Connected");
  await expect(dialog.getByText("Build host is connected")).toBeVisible();
  await expect(dialog.getByText("build-host.local")).toBeVisible();
  await expect(dialog.getByText("darwin / arm64")).toBeVisible();
  await expect(dialog.getByText("2", { exact: true })).toBeVisible();
  await expect(dialog.getByText("codex, claude")).toBeVisible();
  await expect(dialog.locator("code")).toHaveCount(0);
  await expectNoSeriousAxeViolations(page, "the connected enrollment dialog");
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toHaveCount(0);
  await enroll.click();
  await page.getByRole("menuitem", { name: "AI assisted", exact: true }).click();
  const assistance = page.getByRole("dialog", { name: "Add runner with Luna" });
  await expect(assistance.getByLabel("Project", { exact: true })).toHaveValue("");
  await assistance.getByRole("button", { name: "Open Luna" }).click();
  await expect(page).toHaveURL(/\/chat$/);
  await expect(page.getByRole("textbox", { name: "Message" })).toContainText("all current and future projects in this organization");
  await expect(page.getByRole("textbox", { name: "Message" })).toContainText("Fly Sprite or a machine I run");
  await expectNoSeriousAxeViolations(page, "AI assisted runner draft");
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

for (const theme of ["dark", "light"]) {
  for (const width of [1280, 390]) {
    test(`API keys & MCP page fits ${width}px in ${theme} mode`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1000 });
      await page.emulateMedia({ colorScheme: theme });
      const organization = "org_browser_preview";
      const mount = `/organizations/${organization}`;
      const reach = { organization_id: organization, name: "Browser organization", role: "owner", status: "allowed", projects: [{ id: hub.fixture.project_id, name: "Browser collaboration", can_write: true }], last_used_at: "2026-10-01T12:00:00Z" };
      const personal = { id: "key_personal", name: "My cross-org agent", kind: "personal", owner: "browser_owner", owner_email: "owner@example.test", permission: "write", access_context: "global", organizations: [], created_at: "2026-10-01T12:00:00Z", effective_reach: [reach], last_used_at: reach.last_used_at };
      await page.route(`**${mount}/**`, async (route) => {
        const upstream = new URL(route.request().url());
        upstream.pathname = upstream.pathname.slice(mount.length);
        if (upstream.pathname !== "/app/bootstrap" && !route.request().isNavigationRequest()) {
          await route.continue({ url: upstream.toString() });
          return;
        }
        const response = await route.fetch({ url: upstream.toString() });
        if (upstream.pathname === "/app/bootstrap") {
          const bootstrap = await response.json();
          await route.fulfill({ response, json: { ...bootstrap, base_path: mount } });
        } else if (route.request().isNavigationRequest()) {
          const body = (await response.text()).replace('name="detent-base-path" content=""', `name="detent-base-path" content="${mount}"`);
          await route.fulfill({ response, body });
        } else {
          await route.fulfill({ response });
        }
      });
      await page.route("**/api/cloud/**", async (route) => {
        const endpoint = new URL(route.request().url()).pathname;
        const json = endpoint.endsWith("key-context") ? { organizations: [reach, { ...reach, organization_id: "org_other", name: "Other organization", projects: [{ id: "prj_other", name: "Other project", can_write: false }] }], mcp_endpoint: "https://cloud.example.test/mcp" }
          : endpoint.endsWith("key-policy") ? { personal_keys: "approval" }
          : endpoint.endsWith("service-keys") ? { keys: [{ ...personal, id: "key_service", name: "Org integration", kind: "service", access_context: "selected", organizations: [{ organization_id: organization, project_access: "all", project_ids: [] }], effective_reach: undefined }] }
          : endpoint.endsWith("external-keys") ? { keys: [{ ...personal, effective_reach: [{ ...reach, status: "pending" }] }] }
          : endpoint.endsWith("/organizations") ? { email: "owner@example.test", csrf: "fixture-csrf", organizations: [{ id: organization, name: "Browser organization", url: mount }], pending: [], can_create: true }
          : { keys: [personal] };
        await route.fulfill({ json });
      });
      await page.goto(hub.fixture.accounts.owner);
      await page.goto(new URL(`${mount}/settings/api-keys`, hub.fixture.url).toString());
      await expect(page.getByRole("heading", { name: "API keys & MCP", exact: true })).toBeVisible();
      await page.evaluate((mode) => { document.documentElement.dataset.theme = mode; document.documentElement.classList.toggle("dark", mode === "dark"); }, theme);
      await expect(page).toHaveURL(new RegExp(`${mount}/settings/mcp$`));
      await expect(page.getByRole("tab", { name: "Personal 1", exact: true })).toBeVisible();
      await expect(page.getByRole("button", { name: "Create key", exact: true })).toHaveCount(1);
      await expect(page.getByRole("button", { name: "Copy MCP setup prompt", exact: true })).toHaveCount(1);
      await expect(page.getByRole("textbox", { name: "MCP endpoint value" })).toHaveValue("https://cloud.example.test/mcp");
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await page.screenshot({ path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `api-keys-mcp-${theme}-${width}.png`) });
      await page.getByRole("button", { name: "Create key", exact: true }).click();
      const dialog = page.getByRole("dialog");
      await expect(dialog.getByRole("radio", { name: "You", exact: true })).toBeChecked();
      await dialog.getByRole("radio", { name: "You", exact: true }).focus();
      await page.keyboard.press("ArrowDown");
      await expect(dialog.getByRole("radio", { name: "The organization", exact: true })).toBeChecked();
      await page.keyboard.press("Tab");
      await expect(dialog.getByLabel("Name", { exact: true })).toBeFocused();
      await dialog.getByLabel("Reach", { exact: true }).selectOption("project");
      await expect(dialog.getByLabel("Project in Browser organization")).toBeVisible();
      expect(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      await expect(dialog.getByRole("button", { name: "Cancel", exact: true })).toBeInViewport();
      await page.screenshot({ path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `create-key-${theme}-${width}.png`) });
      await page.keyboard.press("Escape");
      await expect(page.getByRole("button", { name: "Create key", exact: true })).toBeFocused();
      await page.goto(new URL(`${mount}/settings/mcp`, hub.fixture.url).toString());
      await page.evaluate((mode) => { document.documentElement.dataset.theme = mode; document.documentElement.classList.toggle("dark", mode === "dark"); }, theme);
      await page.getByRole("tab", { name: /^Member keys 1\b/ }).click();
      await expect(page.getByRole("combobox", { name: "Personal keys in this organization", exact: true })).toHaveValue("approval");
      await expect(page.getByRole("button", { name: "Approve My cross-org agent", exact: true })).toBeVisible();
      await expect(page.getByRole("button", { name: "Block My cross-org agent for this org", exact: true })).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await page.screenshot({ path: require("node:path").join(process.env.TMPDIR || process.env.TMP || process.env.TEMP, `org-keys-${theme}-${width}.png`) });
    });
  }
}
