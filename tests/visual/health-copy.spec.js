const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("health-copy", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test.beforeEach(async ({ context, page }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"], {
    origin: runtime.url,
  });
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "github-api-warning",
  });
  await page.goto(`${runtime.url}/health/ui`, { waitUntil: "domcontentloaded" });
});

test("copies every rendered health signal and restores its label", async ({
  page,
}) => {
  const copy = page.getByRole("button", { name: "Copy all health signals" });
  await expect(copy).toBeVisible();
  await expect(copy).toHaveAttribute("id", "health-copy-all");
  await copy.focus();
  await expect(copy).toBeFocused();

  const payload = await copy.getAttribute("data-copy");
  expect(payload).toContain("Detent health —");
  expect(payload).toContain("[OK]   GitHub REST");
  expect(payload).toContain("[OK]   GitHub GraphQL");
  expect(payload).toContain("[WARN] Budget · billing-api");
  expect(payload).toContain("checked 2026-");
  expect(payload).not.toContain("{{detent-time:");
  const copiedRows = payload.split("\n").filter((line) => line.startsWith("["));
  const renderedRows = page.locator(
    '#health-details [id^="health-"]:not(#health-copy-all)',
  );
  await expect(renderedRows).toHaveCount(copiedRows.length);

  await copy.click();
  await expect(copy.locator("[data-copy-label]")).toHaveText("Copied");
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe(payload);
  await expect(copy.locator("[data-copy-label]")).toHaveText("Copy", {
    timeout: 3_000,
  });
});

test("keeps its identity and refreshes its payload across a snapshot morph", async ({
  page,
  request,
}) => {
  const copy = page.locator("#health-copy-all");
  const original = await copy.elementHandle();
  const originalPayload = await copy.getAttribute("data-copy");

  await copy.click();
  await expect(copy.locator("[data-copy-label]")).toHaveText("Copied");

  const response = await request.get(`${runtime.url}/health/ui`, {
    headers: {
      "X-Detent-Demo-Scenario": "github-api-primary-exhausted",
    },
  });
  expect(response.ok()).toBeTruthy();
  const incoming = await page.evaluate((html) => {
    const document = new DOMParser().parseFromString(
      html,
      "text/html",
    );
    return document.querySelector("#snapshot").innerHTML;
  }, await response.text());
  await page.evaluate(
    (snapshotHTML) =>
      new Promise((resolve) => {
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
        const target = document.querySelector("#snapshot");
        window.htmx.swap(
          target,
          snapshotHTML,
          { swapStyle: target.getAttribute("hx-swap") },
          { contextElement: target },
        );
      }),
    incoming,
  );

  await expect(copy).toHaveCount(1);
  expect(await original.evaluate((element) => element.isConnected)).toBe(true);
  const refreshedPayload = await copy.getAttribute("data-copy");
  expect(refreshedPayload).not.toBe(originalPayload);
  expect(refreshedPayload).toContain("[ERR]  GitHub REST");
  await expect(copy.locator("[data-copy-label]")).toHaveText("Copy");

  await copy.click();
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe(refreshedPayload);
});
