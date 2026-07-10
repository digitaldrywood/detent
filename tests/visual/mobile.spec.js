const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });
test.skip(({ isMobile }) => !isMobile, "mobile project only");

const routes = [
  { name: "board", path: "/", scenario: "fleet-kanban-multiproject" },
  { name: "fleet", path: "/fleet", scenario: "fleet-healthy-parallel-work" },
  { name: "library", path: "/library" },
  { name: "reports", path: "/reports", scenario: "reports-normal-window" },
  { name: "analytics", path: "/analytics", scenario: "diagnostics-healthy" },
  { name: "health", path: "/health/ui", scenario: "github-api-healthy" },
  { name: "api-keys", path: "/api-keys" },
  { name: "settings", path: "/settings", scenario: "settings-loaded-fleet" },
  {
    name: "project-kanban",
    path: "/projects/detent/kanban",
    scenario: "kanban-full-integration",
  },
];

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("mobile-shell", [
    "--demo",
    "screenshots",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

for (const route of routes) {
  test(`${route.name} uses the mobile app shell`, async ({ page }) => {
    await page.setExtraHTTPHeaders(
      route.scenario ? { "X-Detent-Demo-Scenario": route.scenario } : {},
    );
    await page.goto(`${runtime.url}${route.path}`, {
      waitUntil: "domcontentloaded",
    });
    const sidebar = page.locator("#app-sidebar");
    const scrim = page.locator("[data-mobile-sidebar-scrim]");
    const toggle = page.getByRole("button", { name: "Open navigation" });
    const topbarToggle = page.getByRole("button", {
      name: "More topbar controls",
    });
    const topbarControls = page.locator("[data-mobile-topbar-controls]");
    await toggle.waitFor({ state: "visible" });
    await page.evaluate(() => document.fonts?.ready);

    await expect(sidebar).toBeHidden();
    await expect(toggle).toBeVisible();
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    const toggleBox = await toggle.boundingBox();
    expect(toggleBox?.width).toBeGreaterThanOrEqual(44);
    expect(toggleBox?.height).toBeGreaterThanOrEqual(44);
    await expect(topbarToggle).toBeVisible();
    const topbarToggleBox = await topbarToggle.boundingBox();
    expect(topbarToggleBox?.width).toBeGreaterThanOrEqual(44);
    expect(topbarToggleBox?.height).toBeGreaterThanOrEqual(44);
    await expect(page).toHaveScreenshot(`${route.name}.png`);
    await expectNoHorizontalScroll(page);

    await topbarToggle.click();
    await expect(topbarControls).toBeVisible();
    await expect(page.locator("#live-indicator")).toBeVisible();
    await expect(page.getByRole("group", { name: "Density" })).toBeVisible();
    await topbarToggle.click();
    await expect(topbarControls).toBeHidden();

    await toggle.click();
    await expect(sidebar).toBeVisible();
    await expect(scrim).toBeVisible();
    await expect(toggle).toHaveAttribute("aria-expanded", "true");
    await scrim.click({ position: { x: 380, y: 422 } });
    await expect(sidebar).toBeHidden();
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
  });
}

test("drawer closes with Escape and a navigation tap", async ({ page }) => {
  await page.context().addCookies([
    { name: "sidebar_state", value: "false", url: runtime.url },
  ]);
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-kanban-multiproject",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });

  const sidebar = page.locator("#app-sidebar");
  const toggle = page.getByRole("button", { name: "Open navigation" });
  await toggle.click();
  const sidebarBox = await sidebar.boundingBox();
  expect(sidebarBox?.width).toBe(208);
  await expect(sidebar.locator("[data-sidebar-nav-label]").first()).toBeVisible();
  await expect(
    sidebar.getByRole("button", { name: "Toggle sidebar" }),
  ).toBeHidden();
  await page.keyboard.press("Escape");
  await expect(sidebar).toBeHidden();

  await toggle.click();
  await page.locator("#app-sidebar a").first().evaluate((link) => {
    link.addEventListener("click", (event) => event.preventDefault(), {
      once: true,
    });
  });
  await page.locator("#app-sidebar a").first().click();
  await expect(sidebar).toBeHidden();
});

test("shared figures use a compact mobile grid", async ({ page }) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-kanban-multiproject",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });

  const figures = page.locator("#board-figures > div");
  await expect(figures).toBeVisible();
  await expect(figures).toHaveCSS("display", "grid");
  const columns = await figures.evaluate(
    (element) => getComputedStyle(element).gridTemplateColumns.split(" ").length,
  );
  expect(columns).toBe(3);
});

async function expectNoHorizontalScroll(page) {
  const dimensions = await page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    viewportWidth: window.innerWidth,
  }));
  expect(dimensions.scrollWidth).toBeLessThanOrEqual(dimensions.viewportWidth);
}
