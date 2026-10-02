const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("invitation-grants");
});
test.afterAll(async () => {
  await hub?.stop();
});

test("invited member receives only the selected read-only project", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/settings/organization`);
  await page.getByLabel("Email", { exact: true }).fill("invitee@example.test");
  const collaboration = page.getByLabel("Access to Browser collaboration for invitation", { exact: true });
  const privateProject = page.getByLabel("Access to Owner private project for invitation", { exact: true });
  await expect(collaboration).toHaveValue("none");
  await expect(privateProject).toHaveValue("none");
  await collaboration.selectOption("read");
  await page.getByRole("button", { name: "Send invitation", exact: true }).click();
  const pendingAccess = page.getByLabel("Access to Browser collaboration for invitee@example.test", { exact: true });
  await expect(pendingAccess).toHaveValue("read");
  await expect(page.getByLabel("Access to Owner private project for invitee@example.test", { exact: true })).toHaveValue("none");

  await pendingAccess.selectOption("write");
  await expect(pendingAccess).toHaveValue("write");
  await expect(pendingAccess).toBeEnabled();
  await pendingAccess.selectOption("read");
  await expect(pendingAccess).toHaveValue("read");
  const invitations = await (await page.request.get(`${hub.fixture.url}/__preview/invitations`)).json();
  const invitation = Object.values(invitations).find((candidate) => candidate.email === "invitee@example.test");
  expect(invitation).toBeTruthy();
  const invitationId = invitation.id;

  await page.goto(hub.fixture.accounts.invitee);
  await page.goto(`${hub.fixture.url}/invite?invitation_token=${encodeURIComponent(invitationId)}`);
  await expect(page).toHaveURL(`${hub.fixture.url}/organization`);
  await page.goto(`${hub.fixture.url}/settings/organization`);
  const member = page.getByRole("row").filter({ hasText: "invitee@example.test" });
  await expect(member).toContainText("Browser collaboration");
  await expect(member).toContainText("read");
  await expect(member).not.toContainText("Owner private project");
  await expect(page.getByRole("button", { name: "Send invitation" })).toHaveCount(0);
  const bootstrap = await (await page.request.get(`${hub.fixture.url}/app/bootstrap`)).json();
  expect(bootstrap.projects.map((project) => project.id)).toEqual([hub.fixture.project_id]);
  expect(bootstrap.projects[0].can_write).toBe(false);
  const write = await page.request.post(`${hub.fixture.url}${bootstrap.api_base}/projects/${hub.fixture.project_id}/work-items`, {
    headers: { "X-CSRF-Token": bootstrap.csrf_token },
    data: { idempotency_key: "read-only-write", title: "Cannot create", state: "Todo" },
  });
  expect(write.status()).toBe(404);
});
