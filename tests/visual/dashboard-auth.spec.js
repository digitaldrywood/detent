const http = require("node:http");
const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

const dashboardHost = "dashboard.detent.test";

test.use({
  launchOptions: {
    args: [`--host-resolver-rules=MAP ${dashboardHost} 127.0.0.1`],
  },
});

const authModes = [
  {
    name: "no-token",
    env: { DETENT_API_TOKEN: "" },
  },
  {
    name: "ui-cookie",
    env: { DETENT_API_TOKEN: "detent-playwright-token" },
  },
];

for (const mode of authModes) {
  test(`dashboard card actions work on wildcard bind with ${mode.name}`, async ({ page }) => {
    const runtime = await startDetentRuntime(`dashboard-auth-${mode.name}`, [
      "--demo",
      "kanban",
      "--demo-project",
      "demo-project",
    ], {
      host: "0.0.0.0",
      env: mode.env,
    });
    const unexpectedAPIResponses = collectUnexpectedAPIResponses(page);

    try {
      const dashboardURL = nonLoopbackDashboardURL(runtime.url);

      await page.goto(`${dashboardURL}/`, { waitUntil: "domcontentloaded" });
      await expect(page.getByRole("button", { name: /Kanban demo backlog intake/ })).toBeVisible();

      await page.goto(`${dashboardURL}/projects/demo-project/kanban`, { waitUntil: "domcontentloaded" });
      await expect(page.getByRole("button", { name: /Kanban demo backlog intake/ })).toBeVisible();

      await page.getByRole("button", { name: /Kanban demo backlog intake/ }).click();
      const sheet = page.locator("[data-detail-sheet]");
      await expect(sheet).toBeVisible();
      await expect(sheet.getByText("Kanban demo backlog intake")).toBeVisible();
      await expect(sheet.getByText("digitaldrywood/detent#9511")).toBeVisible();
      await expect(sheet.getByRole("link", { name: /Open on GitHub/ })).toHaveAttribute("href", /9511/);

      await sheet.getByRole("button", { name: /^Move$/ }).click();
      const moveDialog = actionDialog(page, "Move card");
      await expect(moveDialog.getByRole("heading", { name: "Move card" })).toBeVisible();
      await moveDialog.getByLabel("Target state").selectOption("Todo");
      await moveDialog.getByRole("button", { name: /^Move$/ }).click();
      await expect(page.locator('[data-board-lane="todo"]').getByRole("button", { name: /Kanban demo backlog intake/ })).toBeVisible();

      await page.getByRole("button", { name: /Kanban demo backlog intake/ }).click();
      const commentSheet = page.locator("[data-detail-sheet]");
      await expect(commentSheet).toBeVisible();
      await commentSheet.getByRole("button", { name: /Comment on issue/ }).click();
      const commentDialog = actionDialog(page, "Comment on issue");
      await expect(commentDialog.getByRole("heading", { name: "Comment on issue" })).toBeVisible();
      await commentDialog.getByLabel("Comment").fill("Playwright dashboard auth comment");
      await commentDialog.getByRole("button", { name: /^Comment$/ }).click();
      await expect(page.locator("[data-detail-sheet]")).toHaveCount(0);

      await page.getByRole("button", { name: /Kanban demo backlog intake/ }).click();
      const removeSheet = page.locator("[data-detail-sheet]");
      await expect(removeSheet).toBeVisible();
      page.once("dialog", async (dialog) => {
        expect(dialog.message()).toContain("Remove this item from the project?");
        await dialog.accept();
      });
      await removeSheet.getByRole("button", { name: /^Remove$/ }).click();
      await expect(page.getByRole("button", { name: /Kanban demo backlog intake/ })).toHaveCount(0);

      const refresh = await page.evaluate(async () => {
        const response = await fetch("/api/v1/refresh", {
          method: "POST",
          headers: {
            "Content-Type": "application/x-www-form-urlencoded",
            "HX-Request": "true",
            "HX-Current-URL": window.location.href,
            "HX-Target": "manual-refresh-status",
          },
          body: "",
        });
        return { status: response.status, body: await response.text() };
      });
      expect(refresh.status).toBe(200);
      expect(refresh.body).toContain('id="manual-refresh-status"');

      expect(unexpectedAPIResponses).toEqual([]);
    } finally {
      await runtime.stop();
    }
  });
}

test("api keys page creates first key on wildcard bind without token", async ({ page }) => {
  const runtime = await startDetentRuntime("dashboard-api-keys-no-token", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ], {
    host: "0.0.0.0",
    env: { DETENT_API_TOKEN: "" },
  });
  const unexpectedAPIResponses = collectUnexpectedAPIResponses(page);

  try {
    const dashboardURL = nonLoopbackDashboardURL(runtime.url);

    await page.goto(`${dashboardURL}/api-keys`, { waitUntil: "domcontentloaded" });
    await expect(page.getByRole("button", { name: "Create your first key" })).toBeVisible();
    await page.getByRole("button", { name: "Create your first key" }).click();

    const dialog = page.getByRole("dialog").first();
    await expect(dialog.getByRole("heading", { name: "Create API key" })).toBeVisible();
    await dialog.getByLabel("Name").fill("Playwright first key");
    await dialog.getByRole("button", { name: /^Create$/ }).click();
    await expect(dialog.getByRole("heading", { name: "API key created" })).toBeVisible();
    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(page.locator("#api-keys-table")).toContainText("Playwright first key");

    expect(unexpectedAPIResponses).toEqual([]);
  } finally {
    await runtime.stop();
  }
});

test("wildcard dashboard denies non-local API access without token", async () => {
  const runtime = await startDetentRuntime("dashboard-auth-negative", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ], {
    host: "0.0.0.0",
    env: { DETENT_API_TOKEN: "" },
  });

  try {
    const port = new URL(runtime.url).port;
    const dashboardURL = nonLoopbackDashboardURL(runtime.url);
    const dashboardURLHost = new URL(dashboardURL).host;
    const deniedRawCard = await rawHTTPStatus(runtime.url, {
      path: "/api/v1/board/card?project=demo-project&issue=digitaldrywood%2Fdetent%239511&actions=board&scope=project",
      headers: {
        Host: dashboardURLHost,
      },
    });
    expect(deniedRawCard).toBe(403);

    const deniedMissingHTMXTarget = await rawHTTPStatus(runtime.url, {
      path: "/api/v1/board/card?project=demo-project&issue=digitaldrywood%2Fdetent%239511&actions=board&scope=project",
      headers: {
        Host: dashboardURLHost,
        "HX-Request": "true",
        "HX-Current-URL": `${dashboardURL}/projects/demo-project/kanban`,
      },
    });
    expect(deniedMissingHTMXTarget).toBe(403);

    const deniedSpoofedSource = await rawHTTPStatus(runtime.url, {
      path: "/api/v1/board/card?project=demo-project&issue=digitaldrywood%2Fdetent%239511&actions=board&scope=project",
      headers: {
        Host: dashboardURLHost,
        "HX-Request": "true",
        "HX-Current-URL": `http://dashboard.example.test:${port}/projects/demo-project/kanban`,
        "HX-Target": "detail-sheet-host",
      },
    });
    expect(deniedSpoofedSource).toBe(403);

    const deniedRawKeyCreate = await rawHTTPStatus(runtime.url, {
      method: "POST",
      path: "/api/v1/keys",
      headers: {
        Host: dashboardURLHost,
        "Content-Type": "application/x-www-form-urlencoded",
        "HX-Request": "true",
        "HX-Current-URL": `${dashboardURL}/api-keys`,
        "HX-Target": "api-key-modal-body",
      },
      body: "name=Spoofed&scopes=read&all_projects=true&expires_in=90d",
    });
    expect(deniedRawKeyCreate).toBe(403);

    const deniedState = await rawHTTPStatus(runtime.url, {
      path: "/api/v1/state",
      headers: {
        Host: dashboardURLHost,
      },
    });
    expect(deniedState).toBe(403);
  } finally {
    await runtime.stop();
  }
});

function collectUnexpectedAPIResponses(page) {
  const unexpected = [];
  page.on("response", (response) => {
    const url = new URL(response.url());
    if (url.pathname.startsWith("/api/v1/") && response.status() >= 400) {
      unexpected.push(`${response.status()} ${url.pathname}${url.search}`);
    }
  });
  return unexpected;
}

function actionDialog(page, title) {
  return page.getByRole("dialog").filter({ hasText: title }).first();
}

function nonLoopbackDashboardURL(runtimeURL) {
  const url = new URL(runtimeURL);
  return `http://${dashboardHost}:${url.port}`;
}

async function rawHTTPStatus(baseURL, options) {
  const target = new URL(baseURL);
  return new Promise((resolve, reject) => {
    const req = http.request({
      hostname: "127.0.0.1",
      port: target.port,
      method: options.method || "GET",
      path: options.path,
      headers: {
        Connection: "close",
        ...options.headers,
      },
    }, (res) => {
      res.resume();
      res.on("end", () => resolve(res.statusCode));
    });
    req.once("error", reject);
    req.end(options.body);
  });
}
