const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("hub-titles");
});

test.afterAll(async () => {
  await hub?.stop();
});

test("Hub routes update the browser tab title on navigation", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner, { waitUntil: "domcontentloaded" });

  for (const [route, title] of [
    ["/work", "Work · Browser organization · Detent"],
    ["/chat", "Chat · Browser organization · Detent"],
    ["/settings/general", "Settings · Browser organization · Detent"],
    ["/settings/runners", "Runners · Browser organization · Detent"],
    ["/fleet", "Runners · Browser organization · Detent"],
  ]) {
    await page.goto(new URL(route, hub.fixture.url).toString(), { waitUntil: "domcontentloaded" });
    await expect(page).toHaveTitle(title);
  }

  // Sidebar navigation stays within the mounted app; titles must change there too.
  await page.goto(new URL("/work", hub.fixture.url).toString(), { waitUntil: "domcontentloaded" });
  const sidebar = page.getByRole("complementary", { name: "Conversations" });
  await sidebar.getByTestId("nav-chat").click();
  await expect(page).toHaveTitle("Chat · Browser organization · Detent");
  await sidebar.getByTestId("nav-work").click();
  await expect(page).toHaveTitle("Work · Browser organization · Detent");

  await page.goto(new URL(`/work/i/${hub.fixture.work_item}`, hub.fixture.url).toString(), {
    waitUntil: "domcontentloaded",
  });
  await expect(page).toHaveTitle(/^Issue .+: Renew the lease.* · Browser collaboration · Detent$/);
});

test("conversation detail supplies the title when the first list page omits it", async ({ page }) => {
  await page.goto(hub.fixture.accounts.owner, { waitUntil: "domcontentloaded" });
  const created = await page.evaluate(async (projectId) => {
    const bootstrap = await (await fetch("/chat/bootstrap")).json();
    const response = await fetch(`${bootstrap.api_base}/projects/${projectId}/conversations`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": bootstrap.csrf_token },
      body: JSON.stringify({
        key: "hub-title-outside-list",
        title: "Beyond the first page",
        first_message: { key: "hub-title-first-message", text: "Show this conversation" },
      }),
    });
    if (!response.ok) throw new Error(`Could not create conversation: ${response.status}`);
    return response.json();
  }, hub.fixture.project_id);
  const conversationId = created.conversation.id;

  let omittedFromList = false;
  await page.route(
    (url) => url.pathname.endsWith("/conversations") && url.searchParams.get("limit") === "100",
    async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      omittedFromList = body.conversations.some((conversation) => conversation.id === conversationId);
      await route.fulfill({ response, json: {
        ...body,
        conversations: body.conversations.filter((conversation) => conversation.id !== conversationId),
      } });
    },
  );

  await page.goto(new URL(`/chat/c/${conversationId}`, hub.fixture.url).toString(), {
    waitUntil: "domcontentloaded",
  });
  await expect(page).toHaveTitle("Chat: Beyond the first page · Browser collaboration · Detent");
  expect(omittedFromList).toBe(true);
});
