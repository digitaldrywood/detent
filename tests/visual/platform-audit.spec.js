const { test, expect } = require("@playwright/test");
const { platformPreview } = require("./platform-preview");
const account = require("./platform-preview-data.json").account;
const audit = require("./platform-audit-data.json");

const origin = "https://platform.detent.test";
let html;
test.beforeAll(async () => {
  html = await platformPreview();
});

async function openAudit(page, path = "/platform/audit") {
  const requests = [];
  await page.route(origin + "/**", (route) =>
    route.fulfill({ contentType: "text/html", body: html }),
  );
  await page.route("**/api/cloud/session", (route) =>
    route.fulfill({ json: { ...account, platform_role: "viewer" } }),
  );
  await page.route("**/api/cloud/organizations", (route) =>
    route.fulfill({ json: account }),
  );
  await page.route("**/api/cloud/platform/audit*", (route) => {
    const params = new URL(route.request().url()).searchParams;
    requests.push(params.toString());
    let rows = audit.rows.filter(
      (row) =>
        (!params.get("tenant") ||
          row.organization_id === params.get("tenant")) &&
        (!params.get("actor") || row.actor.includes(params.get("actor"))) &&
        (!params.get("event") || row.event === params.get("event")) &&
        (!params.get("from") || row.at >= params.get("from")) &&
        (!params.get("to") || row.at.slice(0, 10) <= params.get("to")),
    );
    const older = params.has("cursor");
    const next_cursor = !older && rows.length > 3 ? "older-page" : "";
    rows = older ? rows.slice(3) : rows.slice(0, 3);
    return route.fulfill({ json: { ...audit, rows, next_cursor } });
  });
  await page.goto(origin + path);
  await expect(
    page.getByRole("heading", { name: "Audit", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("region", { name: "Audit timeline" }),
  ).not.toHaveAttribute("aria-busy", "true");
  return requests;
}

for (const [name, viewport] of [
  ["desktop", { width: 1440, height: 1100 }],
  ["mobile", { width: 390, height: 844 }],
]) {
  test(`${name} audit supports shared filters and loading older events`, async ({
    page,
  }) => {
    await page.setViewportSize(viewport);
    const requests = await openAudit(page);
    const table = page.getByRole("table", { name: "Audit events" });
    await expect(table.getByRole("row")).toHaveCount(4);
    await expect(table).toContainText("troubleshooting");
    await expect(
      table.getByRole("link", { name: "Parable" }).first(),
    ).toHaveAttribute("href", "/platform/tenants?tenant=org_alpha");
    await expect(page).toHaveTitle("Audit · Platform · Detent");
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth > innerWidth,
      ),
    ).toBe(false);
    await page.getByRole("button", { name: "Load older" }).click();
    await expect(table.getByRole("row")).toHaveCount(7);
    await expect(table).toContainText("generation 1");
    await expect(table).toContainText("hosted_unknown_subject");
    await expect(table.getByText("deleted", { exact: true })).toHaveCount(2);
    await expect(page.getByRole("button", { name: "Load older" })).toHaveCount(
      0,
    );
    expect(requests).toEqual(["", "cursor=older-page"]);

    await page.getByLabel("Actor", { exact: true }).fill("system");
    await expect(page).toHaveURL(origin + "/platform/audit?actor=system");
    await expect(table.getByRole("row")).toHaveCount(2);
    await page.reload();
    await expect(page.getByLabel("Actor", { exact: true })).toHaveValue(
      "system",
    );
    await expect(table).toContainText("organization_ready");
    await page.getByRole("button", { name: "Clear", exact: true }).click();
    await expect(page).toHaveURL(origin + "/platform/audit");
    await expect(table.getByRole("row")).toHaveCount(4);

    await page.getByLabel("Tenant", { exact: true }).fill("Parable");
    await page.getByRole("option", { name: "Parable", exact: true }).click();
    await page.getByLabel("Event", { exact: true }).click();
    await page
      .getByRole("option", { name: "support_started", exact: true })
      .click();
    await page.getByLabel("From", { exact: true }).fill("2026-10-01");
    await page.getByLabel("To", { exact: true }).fill("2026-10-01");
    await expect(page).toHaveURL(
      origin +
        "/platform/audit?tenant=org_alpha&event=support_started&from=2026-10-01&to=2026-10-01",
    );
    await expect(table.getByRole("row")).toHaveCount(2);
    await page.reload();
    await expect(page.getByLabel("Tenant", { exact: true })).toHaveValue(
      "Parable",
    );
    await expect(page.getByLabel("Event", { exact: true })).toContainText(
      "support_started",
    );
    await expect(table).toContainText("support_started");
    await page.getByLabel("Actor", { exact: true }).fill("nobody");
    await expect(
      page.getByText("No events match these filters."),
    ).toBeVisible();
  });
}
