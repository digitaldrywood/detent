const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });

let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("mobile-chat", {
    env: { DETENT_HOSTED_BROWSER_ISSUE_ASK: "1" },
  });
});
test.afterAll(async () => { await hub?.stop(); });

async function openWork(page, project = false) {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/work${project ? `/p/${hub.fixture.project_id}` : ""}`);
  await expect(page.getByTestId("work-board")).toBeVisible();
}

async function theme(page, value) {
  await page.emulateMedia({ colorScheme: value });
  await page.evaluate((value) => {
    document.documentElement.dataset.theme = value;
    document.documentElement.classList.toggle("dark", value === "dark");
  }, value);
}

test.describe("phone chat", () => {
  test.use({ isMobile: true, hasTouch: true });

  for (const width of [375, 431]) {
    for (const color of ["light", "dark"]) {
      test(`Ask, menu and keyboard viewport at ${width}px in ${color}`, async ({ page }, testInfo) => {
        await page.setViewportSize({ width, height: 844 });
        await openWork(page, width === 431);
        await theme(page, color);
        const actions = page.getByRole("group", { name: "Work actions" });
        await expect(actions.getByRole("button", { name: "Ask", exact: true })).toBeVisible();
        await expect(page.getByTestId("board-new-issue")).toBeHidden();
        await actions.getByRole("button", { name: "More work actions" }).click();
        for (const name of [/Ask Detent/, /New issue/, /Recent chats/]) {
          const row = page.getByRole("menuitem", { name });
          await expect(row).toBeVisible();
          expect((await row.boundingBox()).height).toBeGreaterThanOrEqual(44);
        }
        await page.screenshot({ path: testInfo.outputPath(`ask-menu-${width}-${color}.png`) });
        await page.getByRole("menuitem", { name: /New issue/ }).click();
        await expect(page.getByRole("dialog", { name: "New issue" })).toBeVisible();
        await page.keyboard.press("Escape");
        await expect(page.getByRole("dialog", { name: "New issue" })).toBeHidden();
        await actions.getByRole("button", { name: "More work actions" }).click();
        await page.getByRole("menuitem", { name: /Recent chats/ }).click();
        await expect(page.getByRole("dialog", { name: "Sidebar", exact: true })).toBeVisible();
        await expect(page.locator('[data-sidebar="sidebar"][data-mobile="true"]')).toContainText("Chat");
        await page.keyboard.press("Escape");
        await actions.getByRole("button", { name: "Ask", exact: true }).click();
        await expect(page).toHaveURL(new RegExp(width === 431 ? `/chat/p/${hub.fixture.project_id}$` : "/chat$"));
        const editor = page.getByRole("textbox", { name: "Message", exact: true });
        await expect(editor).toBeVisible();
        await expect(page.getByRole("button", { name: "Project actions", exact: true })).toBeHidden();
        if (width === 375) {
          await page.getByRole("button", { name: "Choose a project", exact: true }).click();
          await page.getByRole("menuitemradio").first().click();
        }
        await expect(page.locator('meta[name="viewport"]')).toHaveAttribute("content", /viewport-fit=cover/);

        await page.evaluate(() => {
          Object.defineProperty(window.visualViewport, "height", { configurable: true, get: () => 460 });
          Object.defineProperty(window.visualViewport, "offsetTop", { configurable: true, get: () => 24 });
          window.visualViewport.dispatchEvent(new Event("resize"));
          window.visualViewport.dispatchEvent(new Event("scroll"));
        });
        const assertComposer = async () => {
          const box = await editor.boundingBox();
          expect(box.y).toBeGreaterThanOrEqual(24);
          expect(box.y + box.height).toBeLessThanOrEqual(484);
          expect(await editor.evaluate((element) => parseFloat(getComputedStyle(element).fontSize))).toBeGreaterThanOrEqual(16);
          const send = page.getByRole("button", { name: "Send message", exact: true });
          const sendBox = await send.boundingBox();
          expect(sendBox.y + sendBox.height).toBeLessThanOrEqual(484);
          expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
          expect(await page.evaluate(() => window.innerHeight)).toBe(844);
        };
        await editor.fill("What should I work on next?");
        await assertComposer();
        await page.getByRole("button", { name: "Send message", exact: true }).click();
        await expect(page).toHaveURL(/\/chat\/c\//);
        expect((await page.getByRole("button", { name: "Thread actions for What should I work on next?" }).boundingBox()).width).toBeGreaterThan(140);
        const answer = page.getByText("This is a project-wide chat.", { exact: true });
        await expect(answer).toBeVisible();
        expect(await answer.evaluate((element) => element.textContent)).toBe("This is a project-wide chat.");
        await editor.fill("Follow up from phone dictation");
        await assertComposer();
        await page.getByRole("button", { name: "Send message", exact: true }).click();
        await expect(answer).toHaveCount(2);
        await expect(page.getByTestId("pending-turn")).toHaveCount(0);
        await expect(page.getByText("Follow up from phone dictation", { exact: true })).toBeVisible();
        await page.screenshot({ path: testInfo.outputPath(`chat-keyboard-${width}-${color}.png`) });

        await editor.fill(Array.from({ length: 40 }, (_, index) => `Phone question line ${index + 1}`).join("\n"));
        await assertComposer();
        await page.getByRole("button", { name: "Send message", exact: true }).click();
        await expect(page.getByText(/Phone question line 40/)).toBeVisible();
        await expect(page.getByTestId("pending-turn")).toHaveCount(0);
        await expect(answer.last()).toBeVisible();
        await expect.poll(() => page.evaluate(() => {
          const scrollable = [...document.querySelectorAll('[data-assistant-citation-viewport] *')].find((element) =>
            /auto|scroll/.test(getComputedStyle(element).overflowY) && element.scrollHeight > element.clientHeight,
          );
          if (!scrollable) return false;
          scrollable.scrollTop = 0;
          return true;
        })).toBe(true);
        await expect.poll(() => page.evaluate(() => document.scrollingElement.scrollTop)).toBe(0);
        await editor.fill("The composer is still available while reading earlier messages");
        await assertComposer();

        await page.evaluate(() => {
          delete window.visualViewport.height;
          delete window.visualViewport.offsetTop;
          window.visualViewport.dispatchEvent(new Event("resize"));
        });
        await expect.poll(async () => (await page.locator(".dc-chat-viewport").boundingBox()).height).toBe(844);
      });
    }
  }

  test("Ask Detent menu action opens project chat", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await openWork(page, true);
    await page.getByRole("button", { name: "More work actions" }).click();
    await page.getByRole("menuitem", { name: /Ask Detent/ }).click();
    await expect(page).toHaveURL(new RegExp(`/chat/p/${hub.fixture.project_id}$`));
  });
});

test("desktop retains New issue and the centered draft composer", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 });
  await openWork(page);
  await expect(page.getByTestId("board-new-issue")).toBeVisible();
  await expect(page.getByRole("group", { name: "Work actions" })).toBeHidden();
  await page.goto(`${hub.fixture.url}/chat`);
  await expect(page.getByRole("textbox", { name: "Message", exact: true })).toBeVisible();
  expect(await page.locator(".dc-chat-viewport").evaluate((element) => getComputedStyle(element).position)).toBe("relative");
  expect(await page.locator(".dc-hero").evaluate((element) => getComputedStyle(element).justifyContent)).toBe("center");
});
