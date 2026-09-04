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
      .locator("[data-board-card-pr]")
      .getByRole("link", { name: "PR #5290", exact: true }),
  ).toBeVisible();
  await expect(reviewCard.locator("[data-board-card-identity]")).toBeVisible();
  await expect(reviewCard.locator("[data-board-card-project]")).toHaveText(
    "dogfood",
  );
  await expect(
    reviewCard
      .locator("[data-board-card-issue]")
      .getByRole("link", { name: "#5290", exact: true }),
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

test("maximal card keeps project, issue, and PR identity at every density", async ({
  page,
}) => {
  await page.setViewportSize(desktopViewport);
  await page.setExtraHTTPHeaders({
    "X-Detent-Demo-Scenario": "board-card-identity-maximal",
  });
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  await page.locator("#board-lanes").waitFor({ state: "visible" });

  await page.locator("#board-lane-picker summary").click();
  for (const select of await page.locator("[data-board-lane-visibility]").all()) {
    await select.selectOption("show");
  }
  await page.locator("#board-lane-picker summary").click();
  await expect(page.locator("[data-board-lane-count]")).toHaveText("9/9");

  const card = page.locator("article", {
    hasText: "Keep project, issue, and pull request identity visible with maximal metadata",
  });
  for (const density of ["compact", "cozy", "comfy"]) {
    await setDensity(page, density);
    await card.scrollIntoViewIfNeeded();
    await assertCardIdentity(card);
    await expectCardIdentityScreenshot(
      page,
      card,
      `board-card-identity-${density}-desktop.png`,
    );
  }

  await page.setViewportSize({ width: 390, height: 844 });
  for (const density of ["compact", "cozy", "comfy"]) {
    await setDensity(page, density);
    await card.scrollIntoViewIfNeeded();
    await assertCardIdentity(card);
    await expectCardIdentityScreenshot(
      page,
      card,
      `board-card-identity-${density}-narrow.png`,
    );
  }
});

async function expectCardIdentityScreenshot(page, card, name) {
  const viewport = page.viewportSize();
  const box = await card.boundingBox();
  expect(box).not.toBeNull();
  const width = Math.min(360, viewport.width);
  const height = Math.min(540, viewport.height);
  const clip = {
    x: Math.max(0, Math.min(Math.floor(box.x), viewport.width - width)),
    y: Math.max(0, Math.min(Math.floor(box.y), viewport.height - height)),
    width,
    height,
  };
  const screenshot = await page.screenshot({
    animations: "disabled",
    clip,
  });
  expect(screenshot).toMatchSnapshot(name, { maxDiffPixelRatio: 0.1 });
}

async function setDensity(page, density) {
  await page.evaluate((value) => {
    document.documentElement.setAttribute("data-density", value);
  }, density);
  await expect(page.locator("html")).toHaveAttribute("data-density", density);
}

async function assertCardIdentity(card) {
  const identity = card.locator("[data-board-card-identity]");
  const project = card.locator("[data-board-card-project]");
  const issue = card
    .locator("[data-board-card-issue]")
    .getByRole("link", { name: "#5260", exact: true });
  const pullRequest = card
    .locator("[data-board-card-pr]")
    .getByRole("link", { name: "PR #5260", exact: true });

  await expect(identity).toBeVisible();
  await expect(project).toHaveText("digitaldrywood-release-train-platform");
  await expect(issue).toBeVisible();
  await expect(pullRequest).toBeVisible();
  await expect(issue).toHaveAttribute(
    "href",
    "https://github.test/digitaldrywood/release-train-platform/issues/5260",
  );
  await expect(pullRequest).toHaveAttribute(
    "href",
    "https://github.test/digitaldrywood/release-train-platform/pull/5260",
  );

  const layout = await card.evaluate((article) => {
    const identityBlock = article.querySelector("[data-board-card-identity]");
    const projectLabel = article.querySelector("[data-board-card-project]");
    const links = Array.from(
      article.querySelectorAll(
        "[data-board-card-issue] a, [data-board-card-pr] a",
      ),
    );
    const cardRect = article.getBoundingClientRect();
    const identityRect = identityBlock.getBoundingClientRect();
    const projectRect = projectLabel.getBoundingClientRect();
    const contained = [identityBlock, projectLabel, ...links].every((element) => {
      const rect = element.getBoundingClientRect();
      return (
        rect.left >= cardRect.left - 1 &&
        rect.right <= cardRect.right + 1 &&
        rect.top >= cardRect.top - 1 &&
        rect.bottom <= cardRect.bottom + 1
      );
    });
    return {
      contained,
      identityWidth: identityRect.width,
      projectWidth: projectRect.width,
      projectScrollWidth: projectLabel.scrollWidth,
      projectTextOverflow: getComputedStyle(projectLabel).textOverflow,
      projectWhiteSpace: getComputedStyle(projectLabel).whiteSpace,
    };
  });
  expect(layout.contained).toBe(true);
  expect(layout.identityWidth).toBeGreaterThan(0);
  expect(layout.projectWidth).toBeGreaterThan(0);
  expect(layout.projectScrollWidth).toBeLessThanOrEqual(layout.projectWidth + 1);
  expect(layout.projectTextOverflow).not.toBe("ellipsis");
  expect(layout.projectWhiteSpace).toBe("normal");

  await issue.focus();
  await expect(issue).toBeFocused();
  await pullRequest.focus();
  await expect(pullRequest).toBeFocused();
}

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
