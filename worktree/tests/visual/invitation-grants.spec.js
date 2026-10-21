const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
test.beforeEach(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("invitation-grants");
});
test.afterEach(async () => {
  await hub?.stop();
});

test("invited member receives only the selected read-only project", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/settings/organization/members`);
  await page.getByRole("button", { name: "Invite member", exact: true }).first().click();
  await page.getByLabel("Email addresses", { exact: true }).fill("invitee@example.test");
  const collaboration = page.getByRole("group", { name: "Access to Browser collaboration for invitation", exact: true });
  const privateProject = page.getByRole("group", { name: "Access to Owner private project for invitation", exact: true });
  await expect(collaboration.getByRole("button", { name: "None", exact: true })).toHaveAttribute("aria-pressed", "true");
  await expect(privateProject.getByRole("button", { name: "None", exact: true })).toHaveAttribute("aria-pressed", "true");
  await collaboration.getByRole("button", { name: "Read", exact: true }).click();
  await expect(collaboration.getByRole("button", { name: "Read", exact: true })).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("button", { name: "Send 1 invitation", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "Invite members", exact: true })).toHaveCount(0);
  const row = page.getByRole("group", { name: "Invitation to invitee@example.test" });
  await row.getByRole("button", { name: /1 project · Read/ }).click();
  const pendingAccess = page.getByRole("group", { name: "Access to Browser collaboration for invitee@example.test", exact: true });
  await expect(pendingAccess.getByRole("button", { name: "Read", exact: true })).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByRole("group", { name: "Access to Owner private project for invitee@example.test", exact: true }).getByRole("button", { name: "None", exact: true })).toHaveAttribute("aria-pressed", "true");
  await pendingAccess.getByRole("button", { name: "Write", exact: true }).click();
  await page.getByRole("button", { name: "Save access", exact: true }).click();
  await row.getByRole("button", { name: /1 project · Write/ }).click();
  await pendingAccess.getByRole("button", { name: "Read", exact: true }).click();
  await page.getByRole("button", { name: "Save access", exact: true }).click();
  await expect(row.getByRole("button", { name: /1 project · Read/ })).toBeVisible();
  const invitations = await (await page.request.get(`${hub.fixture.url}/__preview/invitations`)).json();
  const invitation = Object.values(invitations).find((candidate) => candidate.email === "invitee@example.test");
  expect(invitation).toBeTruthy();
  const invitationId = invitation.id;

  await page.goto(hub.fixture.accounts.invitee);
  await page.goto(`${hub.fixture.url}/invite?invitation_token=${encodeURIComponent(invitationId)}`);
  await expect(page).toHaveURL(`${hub.fixture.url}/organization`);
  await page.goto(`${hub.fixture.url}/settings/organization/members`);
  const member = page.getByRole("row").filter({ hasText: "invitee@example.test" });
  await expect(member).toContainText("All 1 · Read");
  await expect(page.getByRole("button", { name: "Invite member", exact: true })).toHaveCount(0);
  const bootstrap = await (await page.request.get(`${hub.fixture.url}/app/bootstrap`)).json();
  expect(bootstrap.projects.map((project) => project.id)).toEqual([hub.fixture.project_id]);
  expect(bootstrap.projects[0].can_write).toBe(false);
  const write = await page.request.post(`${hub.fixture.url}${bootstrap.api_base}/projects/${hub.fixture.project_id}/work-items`, {
    headers: { "X-CSRF-Token": bootstrap.csrf_token },
    data: { idempotency_key: "read-only-write", title: "Cannot create", state: "Todo" },
  });
  expect(write.status()).toBe(404);
});
