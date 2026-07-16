const fs = require("node:fs");
const path = require("node:path");
const { execFileSync } = require("node:child_process");
const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;
let privateURL;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("private-dashboard-auth", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
  privateURL = updatePrivateURL("enable");
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("private token entry establishes clean desktop and mobile navigation", async ({
  page,
}) => {
  await expect
    .poll(async () => {
      const response = await page.goto(runtime.url, {
        waitUntil: "domcontentloaded",
      });
      return response.status();
    })
    .toBe(404);

  await page.goto(privateURL, { waitUntil: "domcontentloaded" });
  await expect(page).toHaveURL(`${runtime.url}/`);
  await expect(
    page.getByRole("button", { name: /Kanban demo backlog intake/ }),
  ).toBeVisible();

  const streamOpened = await page.evaluate(
    () =>
      new Promise((resolve) => {
        const source = new EventSource("/events?view=board");
        const timeout = setTimeout(() => {
          source.close();
          resolve(false);
        }, 5000);
        source.onopen = () => {
          clearTimeout(timeout);
          source.close();
          resolve(true);
        };
        source.onerror = () => {
          clearTimeout(timeout);
          source.close();
          resolve(false);
        };
      }),
  );
  expect(streamOpened).toBe(true);

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${runtime.url}/reports`, { waitUntil: "domcontentloaded" });
  await expect(page.locator("#reports-kpis")).toBeVisible();
  await expect(page.getByRole("button", { name: "Open navigation" })).toBeVisible();
});

test("private dashboard is read-only by default", async ({ page }) => {
  await page.goto(privateURL, { waitUntil: "domcontentloaded" });
  const mutation = await page.evaluate(async () => {
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
  expect(mutation.status).toBe(403);
  expect(mutation.body).toContain("dashboard_read_only");
});

test("rotation invalidates old sessions without logging tokens", async ({ page }) => {
  await page.goto(privateURL, { waitUntil: "domcontentloaded" });
  const oldToken = new URL(privateURL).searchParams.get("token");
  const rotatedURL = updatePrivateURL("rotate");
  const rotatedToken = new URL(rotatedURL).searchParams.get("token");
  expect(rotatedToken).not.toBe(oldToken);

  await expect
    .poll(async () => {
      const response = await page.goto(`${runtime.url}/reports`, {
        waitUntil: "domcontentloaded",
      });
      return response.status();
    })
    .toBe(404);

  await page.goto(rotatedURL, { waitUntil: "domcontentloaded" });
  await expect(page).toHaveURL(`${runtime.url}/`);
  await expect(
    page.getByRole("button", { name: /Kanban demo backlog intake/ }),
  ).toBeVisible();

  const runtimeLog = fs.readFileSync(runtime.logPath, "utf8");
  expect(runtimeLog).not.toContain(oldToken);
  expect(runtimeLog).not.toContain(rotatedToken);
});

function updatePrivateURL(operation) {
  const binary = process.env.DETENT_BINARY || path.join(process.cwd(), "tmp", "detent");
  const output = execFileSync(
    binary,
    [
      "--config",
      path.join(runtime.home, "global.yaml"),
      "--format",
      "json",
      "auth",
      "token",
      operation,
      "--base-url",
      runtime.url,
    ],
    { cwd: process.cwd(), encoding: "utf8" },
  );
  return JSON.parse(output).url;
}
