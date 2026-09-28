// Screenshots of the platform admin prototype for design review.
//
// It drives `npm run dev:mock` in web/conversation, so it runs only when that
// server is up and PLATFORM_PREVIEW_URL points at it. PLATFORM_LIVE_URL may
// point at a second Vite server started without VITE_PLATFORM_PREVIEW against
// the same mock, to capture what production shows for unbuilt endpoints.
// Output goes to tests/visual/platform-admin/ and is committed for review.
const { test } = require("@playwright/test");
const path = require("node:path");

const preview = process.env.PLATFORM_PREVIEW_URL;
const live = process.env.PLATFORM_LIVE_URL;
const out = path.join(__dirname, "platform-admin");

test.skip(!preview, "Set PLATFORM_PREVIEW_URL to a running `npm run dev:mock`.");
test.describe.configure({ mode: "serial" });

const PAGES = [
  ["overview", "/platform"],
  ["organizations", "/platform/organizations"],
  ["organization-overview", "/platform/organizations/org_harbor"],
  ["organization-members", "/platform/organizations/org_harbor/members"],
  ["organization-projects", "/platform/organizations/org_harbor/projects"],
  ["organization-runners", "/platform/organizations/org_harbor/runners"],
  ["organization-usage", "/platform/organizations/org_harbor/usage"],
  ["organization-plan", "/platform/organizations/org_harbor/plan"],
  ["organization-billing", "/platform/organizations/org_harbor/billing"],
  ["organization-provisioning", "/platform/organizations/org_oakline/provisioning"],
  ["organization-audit", "/platform/organizations/org_harbor/audit"],
  ["users", "/platform/users"],
  ["user", "/platform/users/user_priya_nort"],
  ["runners", "/platform/runners"],
  ["provisioning", "/platform/provisioning"],
  ["plans", "/platform/plans"],
  ["billing", "/platform/billing"],
  ["support", "/platform/support"],
  ["audit", "/platform/audit"],
  ["settings", "/platform/settings"],
];

const VIEWPORTS = [
  ["desktop", { width: 1440, height: 900 }],
  ["mobile", { width: 390, height: 844 }],
];

async function scenario(page, base, query) {
  await page.request.get(`${base}/__mock/platform?${query}`);
}

/** Grows the viewport to the scrolled content so the capture shows the whole page. */
async function capture(page, name, viewport) {
  await page.waitForLoadState("networkidle");
  await page.waitForTimeout(300);
  const height = await page.evaluate(() => {
    const scrollers = [...document.querySelectorAll(".overflow-y-auto")];
    const content = Math.max(0, ...scrollers.map((element) => element.scrollHeight + element.getBoundingClientRect().top));
    return Math.ceil(Math.max(content, document.documentElement.scrollHeight));
  });
  await page.setViewportSize({ width: viewport.width, height: Math.max(viewport.height, height) });
  await page.waitForTimeout(150);
  await page.screenshot({ path: path.join(out, `${name}.png`), animations: "disabled" });
  await page.setViewportSize(viewport);
}

for (const [label, viewport] of VIEWPORTS) {
  test(`every page, ${label}`, async ({ browser }) => {
    test.setTimeout(180_000);
    const context = await browser.newContext({ viewport, colorScheme: "dark", isMobile: label === "mobile", hasTouch: label === "mobile" });
    const page = await context.newPage();
    await scenario(page, preview, "scenario=full&role=admin");
    for (const [name, route] of PAGES) {
      await page.goto(`${preview}${route}`);
      await capture(page, `${label}-${name}`, viewport);
    }
    if (label === "mobile") {
      await page.goto(`${preview}/platform`);
      await page.getByRole("button", { name: "Open navigation" }).click();
      await page.waitForTimeout(400);
      await page.screenshot({ path: path.join(out, "mobile-navigation.png") });
    }
    await context.close();
  });
}

test("states and dialogs, desktop", async ({ browser }) => {
  test.setTimeout(180_000);
  const viewport = { width: 1440, height: 900 };
  const context = await browser.newContext({ viewport, colorScheme: "dark" });
  const page = await context.newPage();

  await scenario(page, preview, "scenario=full&role=admin");
  await page.goto(`${preview}/platform/organizations/org_harbor`);
  await page.getByRole("button", { name: "Suspend" }).click();
  await page.waitForTimeout(400);
  await page.screenshot({ path: path.join(out, "dialog-suspend.png") });
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "Start support access" }).click();
  await page.waitForTimeout(400);
  await page.screenshot({ path: path.join(out, "dialog-support.png") });

  await scenario(page, preview, "scenario=empty&role=admin");
  for (const [name, route] of [["overview", "/platform"], ["organizations", "/platform/organizations"], ["billing", "/platform/billing"]]) {
    await page.goto(`${preview}${route}`);
    await capture(page, `empty-${name}`, viewport);
  }

  await scenario(page, preview, "scenario=error&role=admin");
  await page.goto(`${preview}/platform/runners`);
  await capture(page, "error-runners", viewport);

  await scenario(page, preview, "scenario=full&role=staff");
  await page.goto(`${preview}/platform/organizations/org_harbor/plan`);
  await capture(page, "staff-organization-plan", viewport);

  await scenario(page, preview, "scenario=full&role=forbidden");
  await page.goto(`${preview}/platform`);
  await capture(page, "forbidden", viewport);

  if (live) {
    await scenario(page, preview, "scenario=full&role=admin");
    await page.goto(`${live}/platform`);
    await capture(page, "production-overview", viewport);
    await page.goto(`${live}/platform/runners`);
    await capture(page, "production-runners", viewport);
  }

  await scenario(page, preview, "scenario=full&role=admin");
  await context.close();
});
