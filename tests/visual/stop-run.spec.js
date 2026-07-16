const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("stop-run", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("operator chooses the stopped item's destination, priority, and reason", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "stop-run-picker",
  });
  await page.goto(`${runtime.url}/fleet`, { waitUntil: "domcontentloaded" });

  await page
    .getByRole("button", { name: /Stop run and route item for .*#5260/ })
    .click();

  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("heading", { name: "Stop run and route item" })).toBeVisible();
  await expect(dialog.getByRole("radio", { name: /Blocked/ })).toBeChecked();

  await dialog.getByRole("radio", { name: /Todo/ }).check();
  await dialog.getByLabel("Todo priority").selectOption("2");
  await dialog
    .getByLabel(/Reason/)
    .fill("Make room for the release blocker");

  const [request, response] = await Promise.all([
    page.waitForRequest(
      (candidate) =>
        candidate.method() === "POST" && candidate.url().endsWith("/runs/0/stop"),
    ),
    page.waitForResponse(
      (candidate) =>
        candidate.request().method() === "POST" &&
        candidate.url().endsWith("/runs/0/stop"),
    ),
    dialog.getByRole("button", { name: "Stop run", exact: true }).click(),
  ]);

  const submitted = new URLSearchParams(request.postData());
  expect(submitted.get("destination")).toBe("Todo");
  expect(submitted.get("priority")).toBe("2");
  expect(submitted.get("reason")).toBe("Make room for the release blocker");
  expect(await response.text()).toContain("moved it to Todo at High priority");
  await expect(dialog).toBeHidden();
});
