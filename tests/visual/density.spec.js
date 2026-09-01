const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

const desktopViewport = { width: 1440, height: 1100 };

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("density", ["--demo", "screenshots"]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("card density changes rendered information and persists", async ({
  page,
}) => {
  await page.setViewportSize(desktopViewport);
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "fleet-healthy-parallel-work",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  const runningCard = page.locator("article", {
    hasText: "Implement page-addressable screenshot scenarios",
  });
  const reviewCard = page.locator("article", {
    hasText: "Review deterministic chart colors",
  });
  const absentAuthorCard = page.locator("article", {
    hasText: "Backlog observability fixture intake",
  });
  const matchingOriginActorCard = page.locator("article", {
    hasText: "Add screenshot manifest smoke test",
  });
  const aiDebugAction = runningCard.locator("[data-ai-debug-card-action]");

  await expect(page.locator("html")).toHaveAttribute("data-density", "cozy");
  await expect(runningCard.locator('[data-board-card-content="compact"]')).toBeHidden();
  await expect(runningCard.locator('[data-board-card-content="cozy"]')).toBeVisible();
  await expect(runningCard.locator('[data-board-card-content="comfy"]')).toBeHidden();
  await expect(aiDebugAction).toBeHidden();

  await page.locator('[data-density-choice="compact"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-density", "compact");
  await expect(runningCard.locator('[data-board-card-content="compact"]')).toBeVisible();
  await expect(runningCard.locator('[data-board-card-content="cozy"]')).toBeHidden();
  await expect(runningCard.locator('[data-board-card-content="comfy"]')).toBeHidden();
  await expect(aiDebugAction).toBeHidden();
  await expect(
    reviewCard
      .locator('[data-board-card-content="compact"]')
      .getByRole("link", { name: "PR #5290", exact: true }),
  ).toBeVisible();

  await page.locator('[data-density-choice="comfy"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-density", "comfy");
  await expect(runningCard.locator('[data-board-card-content="compact"]')).toBeHidden();
  await expect(runningCard.locator('[data-board-card-content="cozy"]')).toBeVisible();
  await expect(runningCard.locator('[data-board-card-content="comfy"]')).toBeVisible();
  await expect(aiDebugAction).toBeVisible();
  await expect(runningCard.locator("[data-board-card-labels]")).toContainText("enhancement");
  await expect(runningCard.locator("[data-board-card-effort]")).toContainText("xhigh");
  await expect(runningCard.locator("[data-board-card-activity]")).toContainText(
    "Rendered manifest and route smoke checks.",
  );
  await expect(reviewCard.locator("[data-board-card-pr-status]")).toContainText(
    "PR #5290",
  );
  await expect(runningCard.locator("[data-board-card-author]")).toHaveText(
    "Filed by @corylanou",
  );
  await expect(absentAuthorCard.locator("[data-board-card-author]")).toHaveCount(0);
  await expect(matchingOriginActorCard.locator("[data-board-card-author]")).toHaveCount(0);
  await expect(matchingOriginActorCard.locator("[data-board-card-origin]")).toContainText(
    "via human · @corylanou",
  );

  const originalAuthor = await runningCard
    .locator("[data-board-card-author]")
    .elementHandle();
  const snapshotHTML = await page.locator("#snapshot").evaluate(
    (snapshot) => snapshot.innerHTML,
  );
  await morphSnapshot(page, snapshotHTML);
  await expect(runningCard.locator("[data-board-card-author]")).toHaveCount(1);
  expect(
    await runningCard.locator("[data-board-card-author]").evaluate(
      (author, original) => author === original,
      originalAuthor,
    ),
  ).toBe(true);

  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= document.documentElement.clientWidth,
    ),
  ).toBe(true);
  await page.setViewportSize(desktopViewport);
  await expect(page).toHaveScreenshot("board-comfy.png");

  await page.reload({ waitUntil: "domcontentloaded" });
  await page.locator("#board-lanes").waitFor({ state: "visible" });
  await expect(page.locator("html")).toHaveAttribute("data-density", "comfy");
});

async function morphSnapshot(page, incoming) {
  await page.evaluate(
    (snapshotHTML) =>
      new Promise((resolve) => {
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
        const target = document.querySelector("#snapshot");
        const event = new CustomEvent("htmx:sseBeforeMessage", {
          bubbles: true,
          cancelable: true,
          detail: { elt: target, data: snapshotHTML },
        });
        target.dispatchEvent(event);
        if (!event.defaultPrevented) {
          window.htmx.swap(
            target,
            snapshotHTML,
            { swapStyle: target.getAttribute("hx-swap") || "innerHTML" },
            { contextElement: target },
          );
        }
      }),
    incoming,
  );
}
