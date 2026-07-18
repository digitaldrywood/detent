const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test.describe.configure({ mode: "serial" });

let runtime;

test.beforeAll(async () => {
  runtime = await startDetentRuntime("card-detail-async", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);
});

test.afterAll(async () => {
  await runtime?.stop();
});

test("card shell paints before delayed enrichments and hydrates in place", async ({
  page,
}, testInfo) => {
  await openBoard(page);

  let releaseCard;
  const cardGate = new Promise((resolve) => {
    releaseCard = resolve;
  });
  await page.route("**/api/v1/board/card?**", async (route) => {
    await cardGate;
    await route.continue();
  });

  const held = [];
  for (const [name, pattern] of [
    ["receipt", "**/api/v1/board/receipt?**"],
    ["activity", "**/api/v1/board/activity?**"],
    ["conversation", "**/api/v1/board/conversation?**"],
  ]) {
    await page.route(pattern, async (route) => {
      held.push({ name, route });
    });
  }

  const card = page.locator("#board-lanes article[id^='card-']").first();
  const title = (await card.locator("[data-board-card-title]").textContent()).trim();
  const state = (await card.locator("[data-board-card-state]").textContent()).trim();
  await card.click();

  const sheet = page.locator("[data-detail-sheet]");
  await expect(sheet).toBeVisible();
  await expect(sheet).toHaveAttribute("data-detail-sheet-immediate", "");
  await expect(sheet.getByRole("heading", { level: 2 })).toHaveText(title);
  await expect(sheet.locator("[data-immediate-state]")).toHaveText(state);
  await expect(sheet.getByRole("button", { name: "Close details" })).toBeVisible();
  await testInfo.attach("card-detail-pending-desktop.png", {
    body: await page.screenshot({ animations: "disabled", caret: "hide" }),
    contentType: "image/png",
  });

  releaseCard();
  await expect(sheet).not.toHaveAttribute("data-detail-sheet-immediate", "");
  await expect
    .poll(() => new Set(held.map((entry) => entry.name)).size)
    .toBe(3);
  for (const region of await sheet.locator("[data-detail-enrichment][aria-busy='true']").all()) {
    await expect(region).toBeVisible();
  }

  await Promise.all(held.map(({ route }) => route.continue()));
  await expect(sheet.locator("#efficiency-receipt")).toHaveAttribute(
    "aria-busy",
    "false",
  );
  await expect(sheet.locator("#board-activity-stream")).toHaveAttribute(
    "aria-busy",
    "false",
  );
  await expect(sheet.locator("#kanban-issue-comments-panel")).toHaveAttribute(
    "aria-busy",
    "false",
  );
});

test("newer cards and close actions invalidate delayed card responses", async ({
  page,
}) => {
  await openBoard(page);

  const held = [];
  await page.route("**/api/v1/board/card?**", async (route) => {
    held.push(route);
  });

  const cards = page.locator("#board-lanes article[id^='card-']");
  const first = cards.nth(0);
  const second = cards.nth(1);
  const firstTitle = (await first.locator("[data-board-card-title]").textContent()).trim();
  const secondTitle = (await second.locator("[data-board-card-title]").textContent()).trim();
  await first.click();
  await expect.poll(() => held.length).toBe(1);
  await expect(page.locator("[data-immediate-title]")).toHaveText(firstTitle);

  await second.evaluate((element) => element.click());
  await expect.poll(() => held.length).toBe(2);
  await expect(page.locator("[data-immediate-title]")).toHaveText(secondTitle);

  await held[1].continue();
  const sheet = page.locator("[data-detail-sheet]");
  await expect(sheet.getByRole("heading", { level: 2 })).toHaveText(secondTitle);
  await held[0]
    .fulfill({
      status: 200,
      contentType: "text/html; charset=utf-8",
      body: `<div data-detail-sheet><h2>${firstTitle}</h2></div>`,
    })
    .catch(() => {});
  await nextPaint(page);
  await expect(sheet.getByRole("heading", { level: 2 })).toHaveText(secondTitle);

  await page.keyboard.press("Escape");
  await expect(sheet).toHaveCount(0);
  await first.click();
  await expect.poll(() => held.length).toBe(3);
  await expect(page.locator("[data-immediate-title]")).toHaveText(firstTitle);
  await sheet.getByRole("button", { name: "Close details" }).click();
  await expect(sheet).toHaveCount(0);
  await held[2]
    .fulfill({
      status: 200,
      contentType: "text/html; charset=utf-8",
      body: `<div data-detail-sheet><h2>${firstTitle}</h2></div>`,
    })
    .catch(() => {});
  await nextPaint(page);
  await expect(sheet).toHaveCount(0);
});

test("snapshot morphs refresh only core facts and preserve sheet state", async ({
  page,
}) => {
  let conversationRequests = 0;
  let coreRequests = 0;
  page.on("request", (request) => {
    if (request.url().includes("/api/v1/board/conversation?")) {
      conversationRequests += 1;
    }
    if (request.url().includes("/api/v1/board/card/core?")) {
      coreRequests += 1;
    }
  });
  await openBoard(page);

  await page.locator("#board-lanes article[id^='card-']").first().click();
  const sheet = page.locator("[data-detail-sheet]");
  await expect(sheet.locator("#kanban-issue-comments-panel")).toHaveAttribute(
    "aria-busy",
    "false",
  );
  const input = sheet.locator("#kanban-thread-comment-body");
  await expect(input).toBeVisible();
  await input.fill("preserve this draft");

  const liveSession = sheet.getByRole("tab", { name: "Live session" });
  await liveSession.click();
  await expect(liveSession).toHaveAttribute("aria-selected", "true");
  const scroll = sheet.locator("[data-detail-sheet-scroll]");
  const scrollTop = await scroll.evaluate((element) => {
    element.scrollTop = Math.min(120, element.scrollHeight - element.clientHeight);
    return element.scrollTop;
  });
  const conversationsBefore = conversationRequests;
  const coresBefore = coreRequests;

  await morphCurrentSnapshot(page);
  await expect.poll(() => coreRequests).toBeGreaterThan(coresBefore);
  await expect.poll(() => conversationRequests).toBe(conversationsBefore);
  await expect(liveSession).toHaveAttribute("aria-selected", "true");
  await expect(input).toHaveValue("preserve this draft");
  expect(await scroll.evaluate((element) => element.scrollTop)).toBe(scrollTop);
});

async function openBoard(page) {
  await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
  await page.locator("#board-lanes").waitFor({ state: "visible" });
  await page.evaluate(() => document.fonts?.ready);
}

async function nextPaint(page) {
  await page.evaluate(
    () =>
      new Promise((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(resolve)),
      ),
  );
}

async function morphCurrentSnapshot(page) {
  const incoming = await page.locator("#snapshot").evaluate(
    (snapshot) => snapshot.innerHTML,
  );
  const path = "/__card-detail-async-snapshot";
  await page.route(`**${path}`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "text/html; charset=utf-8",
      body: incoming,
    });
  });
  await page.evaluate(
    (url) =>
      new Promise((resolve) => {
        document.addEventListener("htmx:afterSettle", resolve, { once: true });
        window.htmx.ajax("GET", url, {
          target: "#snapshot",
          swap: "morph:innerHTML",
        });
      }),
    path,
  );
  await page.unroute(`**${path}`);
}
