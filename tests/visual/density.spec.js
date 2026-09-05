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
  await expect(runningCard.locator("[data-board-card-signals]")).toBeVisible();
  await expect(runningCard.locator('[data-board-card-content="comfy"]')).toBeHidden();
  await expect(aiDebugAction).toBeHidden();

  await page.locator('[data-density-choice="compact"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-density", "compact");
  await expect(runningCard.locator('[data-board-card-priority-details]')).toBeHidden();
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
  await expect(runningCard.locator("[data-board-card-signals]")).toBeVisible();
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

for (const viewport of [desktopViewport, { width: 390, height: 844 }]) {
  test(`busy cards enforce density budgets at ${viewport.width}px`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": "board-card-identity-maximal" });
    await page.goto(runtime.url, { waitUntil: "domcontentloaded" });
    await page.locator("#board-lanes").waitFor({ state: "visible" });
    await page.locator("#board-lane-picker summary").click();
    for (const select of await page.locator("[data-board-lane-visibility]").all()) {
      await select.selectOption("show");
    }
    await page.locator("#board-lane-picker summary").click();
    await expect(page.locator("[data-board-lane-count]")).toHaveText("9/9");
    const card = page.locator("article", { hasText: "Keep project, issue, and pull request identity visible with maximal metadata" });
    const blockers = card.locator("[data-board-card-blockers]");
    const signals = card.locator("[data-board-card-signal]:visible");
    const snapshot = await page.locator("#snapshot").evaluate(el => el.innerHTML);
    let cozyHeight;
    for (const density of ["compact", "cozy", "comfy"]) {
      await chooseDensity(page, density);
      await card.scrollIntoViewIfNeeded();
      await assertCardIdentity(card);
      if (density === "comfy") {
        await expect(blockers).toBeVisible();
        await expect(blockers).toContainText("component-8#597 (In Progress)");
        await expect(blockers).toContainText("completed#589 (Done)");
        await expect(card.locator("[data-board-card-park-summary]")).toBeVisible();
        const detailsGeometry = await card.evaluate(el => {
          const box = el.getBoundingClientRect();
          return [...el.querySelectorAll("[data-board-card-expanded]")].map(row => ({
            lines: row.getBoundingClientRect().height / parseFloat(getComputedStyle(row).lineHeight),
            contained: row.getBoundingClientRect().right <= box.right,
          }));
        });
        for (const row of detailsGeometry) {
          expect(row.lines).toBeLessThanOrEqual(2.05);
          expect(row.contained).toBe(true);
        }
        expect((await card.boundingBox()).height).toBeLessThanOrEqual(680);
      } else {
        await expect(blockers).toBeHidden();
        await expect(card.locator("[data-board-card-details]")).toBeHidden();
        await expect(signals).toHaveText(density === "compact" ? ["Blocked · 8"] : ["Blocked · 8", "Sync error"]);
        const geometry = await card.evaluate(el => {
          const identity = el.querySelector("[data-board-card-identity]");
          const title = el.querySelector("[data-board-card-title]");
          const visibleSignals = [...el.querySelectorAll("[data-board-card-signal]")].filter(x => x.getBoundingClientRect().height);
          return {
            height: el.getBoundingClientRect().height,
            supportingHeight: el.getBoundingClientRect().height - identity.getBoundingClientRect().height,
            titleLines: title.getBoundingClientRect().height / parseFloat(getComputedStyle(title).lineHeight),
            signalLines: visibleSignals.map(x => x.getBoundingClientRect().height / parseFloat(getComputedStyle(x).lineHeight)),
          };
        });
        expect(geometry.supportingHeight).toBeLessThanOrEqual(density === "compact" ? 76 : 130);
        expect(geometry.height).toBeLessThanOrEqual(density === "compact" ? 160 : 215);
        expect(geometry.titleLines).toBeLessThanOrEqual(density === "compact" ? 1.05 : 2.05);
        for (const lines of geometry.signalLines) expect(lines).toBeLessThanOrEqual(1.05);
        if (density === "cozy") cozyHeight = geometry.height;
        const review = page.locator('[data-board-lane="human-review"]');
        await expect(review.locator("[data-board-card-progress]:visible")).toHaveCount(0);
        expect(await review.innerText()).not.toContain("artifact_status_wait");
      }
      for (const theme of ["dark", "light"]) {
        await page.evaluate(value => {
          if (value === "light") document.documentElement.setAttribute("data-theme", "light");
          else document.documentElement.removeAttribute("data-theme");
        }, theme);
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
        await expect(page).toHaveScreenshot(`board-budget-${density}-${theme}-${viewport.width}.png`);
      }

    }
    await chooseDensity(page, "cozy");
    await expect(blockers).toBeHidden();
    await morphSnapshot(page, snapshot);
    await expect(blockers).toBeHidden();
    await expect(signals).toHaveText(["Blocked · 8", "Sync error"]);
    expect((await card.boundingBox()).height).toBe(cozyHeight);
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.locator("html")).toHaveAttribute("data-density", "cozy");
    await expect(blockers).toBeHidden();
    expect((await card.boundingBox()).height).toBe(cozyHeight);
    for (const density of ["compact", "cozy", "comfy"]) {
      await page.goto(runtime.url, { waitUntil: "domcontentloaded" });
      await chooseDensity(page, density);
      await card.focus();
      await page.keyboard.press("Enter");
      const sheet = page.locator("[data-detail-sheet-core]");
      await expect(sheet).toContainText("component-8#597 (In Progress)");
      await expect(sheet.locator("[data-detail-blocker]")).toHaveCount(8);
      for (const blocker of await sheet.locator("[data-detail-blocker]").all()) {
        const layout = await blocker.evaluate(el => ({ contained: el.scrollWidth <= el.clientWidth + 1, whiteSpace: getComputedStyle(el).whiteSpace }));
        expect(layout.contained).toBe(true);
        expect(layout.whiteSpace).toBe("pre-wrap");
      }
      await expect(sheet).toContainText("completed#589 (Done)");
      await expect(sheet).toContainText("artifact_status_wait");
      const fullValue = sheet.locator('[data-sheet-row="Last turn"] [data-sheet-row-value]');
      expect(await fullValue.evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true);
      await page.getByRole("button", { name: "Close details", exact: true }).click();
      await expect(page.getByRole("dialog", { name: "Card details", exact: true })).toBeHidden();
    }
    await page.goto(runtime.url, { waitUntil: "domcontentloaded" });
    await chooseDensity(page, "cozy");
    await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": "board-card-single-blocker" });
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(signals).toHaveText(["Blocked · 1", "Sync error"]);
    expect((await card.boundingBox()).height).toBe(cozyHeight);
  });
}

async function chooseDensity(page, density) {
  const mobile = page.viewportSize().width < 768;
  const toggle = page.getByRole("button", { name: "More topbar controls" });
  if (mobile) await toggle.click();
  await page.locator(`[data-density-choice="${density}"]`).click();
  if (mobile) await toggle.click();
  await expect(page.locator("html")).toHaveAttribute("data-density", density);
}
