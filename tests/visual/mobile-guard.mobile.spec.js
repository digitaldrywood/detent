const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });
test.skip(({ isMobile }) => !isMobile, "mobile project only");
test.setTimeout(120_000);

const portraitViewports = [
  { width: 390, height: 844 },
  { width: 414, height: 896 },
];
const landscapeViewport = { width: 844, height: 390 };
const onboardingScenarios = [
  "onboarding-tracker-choice",
  "onboarding-github-credentials",
  "onboarding-project-selection",
  "onboarding-agent-config",
  "onboarding-write-summary",
  "onboarding-validation-errors",
  "onboarding-write-exists",
  "onboarding-write-success",
];
const routes = [
  {
    name: "board",
    path: "/",
    scenario: "fleet-kanban-blocked-alerts",
    waitSelector: "#board-lanes",
  },
  {
    name: "fleet",
    path: "/fleet",
    scenario: "fleet-dense-multiproject",
    waitSelector: "#snapshot",
  },
  { name: "library", path: "/library", waitSelector: "main" },
  {
    name: "reports",
    path: "/reports",
    scenario: "reports-model-heavy",
    waitSelector: "#reports-kpis",
  },
  {
    name: "analytics",
    path: "/analytics",
    scenario: "diagnostics-healthy",
    waitSelector: "#analytics-log",
  },
  {
    name: "health",
    path: "/health/ui",
    scenario: "github-api-primary-exhausted",
    waitSelector: "#health-verdict",
  },
  { name: "api-keys", path: "/api-keys", waitSelector: "main" },
  {
    name: "settings",
    path: "/settings",
    scenario: "settings-long-paths",
    waitSelector: "#settings-global",
  },
  {
    name: "project-overview",
    path: "/projects/billing-api",
    scenario: "project-hot-path",
    waitSelector: "#snapshot",
  },
  {
    name: "project-kanban",
    path: "/projects/dogfood/kanban",
    scenario: "kanban-dense-overflow",
    waitSelector: "#board-lanes",
  },
  {
    name: "project-runs",
    path: "/projects/dogfood/runs",
    scenario: "runs-long-content",
    waitSelector: "#snapshot",
  },
  {
    name: "project-configuration",
    path: "/projects/dogfood/configuration",
    scenario: "settings-project-context",
    waitSelector: "#settings-global",
  },
  {
    name: "project-diagnostics",
    path: "/projects/dogfood/diagnostics",
    scenario: "diagnostics-rate-limit-pressure",
    waitSelector: "#snapshot",
  },
  ...onboardingScenarios.map((scenario) => ({
    name: scenario,
    path: "/onboarding",
    scenario,
    waitSelector: "#onboarding-step",
  })),
];

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("mobile-overflow-guard", [
    "--demo",
    "screenshots",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

for (const viewport of portraitViewports) {
  test(`every route fits ${viewport.width}x${viewport.height}`, async ({
    page,
  }) => {
    await page.setViewportSize(viewport);
    for (const route of routes) {
      await test.step(route.name, async () => {
        await openRoute(page, route);
        await expectNoHorizontalScroll(page);
      });
    }
    await assertTransientSurfaces(page);
  });
}

test("every route keeps a usable landscape shell", async ({ page }) => {
  await page.setViewportSize(landscapeViewport);
  for (const route of routes) {
    await test.step(route.name, async () => {
      await openRoute(page, route);
      await expectNoHorizontalScroll(page);
      if (!route.name.startsWith("onboarding")) {
        await expect(page.locator("#app-sidebar")).toBeVisible();
        await expect(
          page.getByRole("group", { name: "Density" }),
        ).toBeVisible();
        await expect(page.locator("main")).toBeVisible();
      }
    });
  }
});

async function openRoute(page, route) {
  await page.setExtraHTTPHeaders(
    route.scenario ? { "X-Detent-Demo-Scenario": route.scenario } : {},
  );
  await page.goto(`${runtime.url}${route.path}`, {
    waitUntil: "domcontentloaded",
  });
  await page.locator(route.waitSelector).waitFor({ state: "visible" });
  await page.evaluate(() => document.fonts?.ready);
}

async function assertTransientSurfaces(page) {
  await openRoute(page, {
    path: "/",
    scenario: "fleet-healthy-parallel-work",
    waitSelector: "#board-lanes",
  });

  const navigation = page.getByRole("button", { name: "Open navigation" });
  await navigation.tap();
  await expect(page.locator("#app-sidebar")).toBeVisible();
  await expectNoHorizontalScroll(page);
  await page.locator("[data-mobile-sidebar-scrim]").tap({
    position: { x: 380, y: 100 },
  });

  const lanePicker = page.locator("#board-lane-picker");
  await lanePicker.locator("summary").tap();
  await expect(lanePicker).toHaveAttribute("open", "");
  await expectNoHorizontalScroll(page);
  await lanePicker.locator("summary").tap();

  await page.locator("#board-lanes article[id^='card-']").first().tap();
  await expect(page.locator("[data-detail-sheet]")).toBeVisible();
  await expectNoHorizontalScroll(page);
}

async function expectNoHorizontalScroll(page) {
  const dimensions = await page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    viewportWidth: window.innerWidth,
  }));
  expect(dimensions.scrollWidth).toBeLessThanOrEqual(dimensions.viewportWidth);
}
