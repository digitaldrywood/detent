const { test, expect } = require("@playwright/test");
const { platformPreview } = require("./platform-preview");
const fixture = require("./platform-preview-data.json");

const origin = "https://platform.detent.test";
let html;
test.beforeAll(async () => { html = await platformPreview(); });

const member = (email, role, bootstrap = false) => ({ email, role, bootstrap, added_by: bootstrap ? "bootstrap" : "admin@example.test",
  added_at: "2026-10-07T12:00:00Z", updated_at: "2026-10-07T12:00:00Z" });

async function openStaff(page, role = "admin") {
  let revision = 4;
  let members = [member("admin@example.test", "admin"), member("bootstrap@example.test", "admin", true), member("support@example.test", "support")];
  const writes = [];
  let conflict = false;
  const account = { ...fixture.account, email: "admin@example.test", csrf: "staff-csrf", platform_role: role };
  await page.route(origin + "/**", (route) => route.fulfill({ contentType: "text/html", body: html }));
  await page.route("**/api/cloud/session", (route) => route.fulfill({ json: account }));
  await page.route("**/api/cloud/organizations", (route) => route.fulfill({ json: account }));
  await page.route("**/api/cloud/platform/members**", async (route) => {
    const request = route.request();
    if (request.method() === "GET") return route.fulfill({ json: { members, revision, self: { email: account.email, role } } });
    const change = request.postDataJSON();
    writes.push({ method: request.method(), csrf: request.headers()["x-csrf-token"], ...change });
    const email = change.email ?? decodeURIComponent(new URL(request.url()).pathname.split("/").pop());
    if (conflict || change.expected_revision !== revision) {
      conflict = false;
      revision += 1;
      members = members.map((member) => member.email === "support@example.test" ? { ...member, role: "viewer" } : member);
      return route.fulfill({ status: 409, json: { code: "revision_conflict", message: "changed" } });
    }
    if (request.method() === "POST" && members.some((member) => member.email === email)) {
      return route.fulfill({ status: 409, json: { code: "already_member", message: "Already a member" } });
    }
    members = request.method() === "POST" ? [...members, member(email, change.role)] : request.method() === "DELETE" ?
      members.filter((member) => member.email !== email) : members.map((member) => member.email === email ? { ...member, role: change.role } : member);
    revision += 1;
    return route.fulfill({ json: { email, role: change.role ?? "", revision } });
  });
  await page.goto(origin + "/platform/staff");
  return { writes, stale: () => { conflict = true; } };
}

for (const viewport of [{ name: "desktop", width: 1440, height: 1100 }, { name: "mobile", width: 390, height: 844 }]) {
  test(`${viewport.name} staff dialogs, protections and stale reload`, async ({ page }) => {
    await page.setViewportSize(viewport);
    const state = await openStaff(page);
    const table = page.getByRole("table", { name: "Platform members" });
    await expect(table).toBeVisible();
    await expect(page).toHaveTitle("Staff · Platform · Detent");
    for (const email of ["admin@example.test", "bootstrap@example.test"]) {
      const row = table.getByRole("row").filter({ has: page.getByRole("combobox", { name: `Role for ${email}` }) });
      await expect(row.getByRole("combobox")).toBeDisabled();
      await expect(row.getByRole("button", { name: "Remove" })).toHaveCount(0);
    }
    await expect(table.getByText("bootstrap", { exact: true }).first()).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
    await page.screenshot({ path: process.env.TMPDIR + `/platform-staff-${viewport.name}.png` });

    const add = page.getByRole("button", { name: "Add member" });
    await add.click();
    let dialog = page.getByRole("dialog", { name: /^(Add member|Change role for)/ });
    await expect(dialog.getByRole("combobox", { name: "Role", exact: true })).toContainText("Viewer");
    await dialog.getByLabel("Email").fill(" NEW@Example.test ");
    await dialog.getByRole("button", { name: "Add", exact: true }).click();
    await expect(dialog.getByRole("alert")).toContainText("Give a reason");
    await dialog.getByLabel("Reason").fill("Add teammate");
    await page.screenshot({ path: process.env.TMPDIR + `/platform-staff-add-${viewport.name}.png` });
    await dialog.getByRole("button", { name: "Add", exact: true }).click();
    const added = table.getByRole("row").filter({ has: page.getByText("new@example.test", { exact: true }) });
    await expect(added).toBeVisible();
    expect(state.writes[0]).toMatchObject({ method: "POST", email: "new@example.test", role: "viewer", reason: "Add teammate", expected_revision: 4, csrf: "staff-csrf" });
    expect(state.writes[0].idempotency_key).toBeTruthy();

    const role = table.getByRole("combobox", { name: "Role for support@example.test" });
    await role.click();
    await page.getByRole("option", { name: "Billing", exact: true }).click();
    dialog = page.getByRole("dialog", { name: /^(Add member|Change role for)/ });
    await expect(dialog.getByRole("heading")).toHaveText("Change role for support@example.test from Support to Billing");
    await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(role).toContainText("Support");
    await role.click();
    await page.getByRole("option", { name: "Billing", exact: true }).click();
    await dialog.getByLabel("Reason").fill("Rotate duties");
    await dialog.getByRole("button", { name: "Change role", exact: true }).click();
    await expect(role).toContainText("Billing");
    expect(state.writes[1]).toMatchObject({ method: "PATCH", role: "billing", expected_revision: 5, reason: "Rotate duties" });

    await added.getByRole("button", { name: "Remove", exact: true }).click();
    const removal = page.getByRole("alertdialog");
    await removal.getByRole("button", { name: "Remove member", exact: true }).click();
    await expect(removal.getByRole("alert")).toContainText("Give a reason");
    await removal.getByLabel("Reason").fill("Teammate left");
    await removal.getByRole("button", { name: "Remove member", exact: true }).click();
    await expect(added).toHaveCount(0);
    expect(state.writes[2]).toMatchObject({ method: "DELETE", reason: "Teammate left", expected_revision: 6 });

    await add.click();
    await dialog.getByLabel("Email").fill("support@example.test");
    await dialog.getByLabel("Reason").fill("Duplicate");
    await dialog.getByRole("button", { name: "Add", exact: true }).click();
    await expect(dialog.getByRole("alert")).toHaveText("Already a member");
    await dialog.getByRole("button", { name: "Cancel", exact: true }).click();

    state.stale();
    await role.click();
    await page.getByRole("option", { name: "Support", exact: true }).click();
    await dialog.getByLabel("Reason").fill("Concurrent edit");
    await dialog.getByRole("button", { name: "Change role", exact: true }).click();
    await expect(page.getByText("Someone changed the staff list; reload", { exact: true })).toBeVisible();
    await expect(add).toBeDisabled();
    await page.getByRole("button", { name: "Reload", exact: true }).click();
    await expect(role).toContainText("Viewer");
    await expect(add).toBeEnabled();
  });
}

test("non-admin Staff visit is refused in the platform shell", async ({ page }) => {
  await openStaff(page, "support");
  await expect(page.getByRole("alert")).toHaveText("You do not have permission to do that.");
  await expect(page.getByRole("table")).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Staff", exact: true })).toHaveCount(0);
});

test("Staff supports keyboard dialog dismissal and dark theme", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openStaff(page);
  await page.evaluate(() => document.documentElement.classList.add("dark"));
  const add = page.getByRole("button", { name: "Add member" });
  await add.focus();
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "Add member", exact: true });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByLabel("Email")).toBeFocused();
  await page.screenshot({ path: process.env.TMPDIR + "/platform-staff-add-dark-mobile.png" });
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(add).toBeFocused();
});
