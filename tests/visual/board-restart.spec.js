const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");

test("open board disables moves and recovers after runtime restart", async ({
  page,
}) => {
  const args = ["--demo", "kanban", "--demo-project", "demo-project"];
  let runtime = await startDetentRuntime("board-restart", args);

  try {
    await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
    await page.locator("#board-lanes").waitFor({ state: "visible" });
    const cards = page.locator('[data-kanban-action="move"]');
    await expect(cards.first()).toBeVisible();
    const cardCount = await cards.count();
    expect(cardCount).toBeGreaterThan(0);
    await expect(page.locator("html")).toHaveAttribute(
      "data-detent-sse-status",
      "open",
    );

    await page.evaluate(() => {
      window.__detentRestartMarker = true;
    });
    const { port } = new URL(runtime.url);
    const home = runtime.home;
    await runtime.stop();

    await expect(page.locator("html")).toHaveAttribute(
      "data-detent-connection",
      "disconnected",
    );
    await expect(page.locator('[data-kanban-connection-disabled="true"]')).toHaveCount(
      cardCount,
    );
    await expect(page.locator('[data-kanban-action="move"]')).toHaveCount(0);
    const notice = page.locator("[data-detent-connection-notice]");
    await expect(notice).toBeVisible();
    await expect(notice).toContainText("Connection lost");
    await expect(notice.locator("[data-detent-connection-reload]")).toHaveText(
      "Reload",
    );

    runtime = await startDetentRuntime("board-restart-recovered", args, {
      home,
      port: Number(port),
    });

    await expect(page.locator("html")).toHaveAttribute(
      "data-detent-connection",
      "connected",
    );
    await expect(page.locator('[data-kanban-action="move"]')).toHaveCount(
      cardCount,
    );
    await expect(notice).toBeHidden();
    expect(
      await page.evaluate(() => window.__detentRestartMarker === true),
    ).toBe(true);

    const card = page.locator("[data-kanban-card]", {
      hasText: "Kanban demo backlog intake",
    });
    const targetLane = page.locator('[data-kanban-drop-state="Todo"]');
    const moveRequest = page.waitForRequest((request) => {
      if (
        request.method() !== "POST" ||
        !request.url().endsWith("/api/v1/kanban/move")
      ) {
        return false;
      }
      return (
        new URLSearchParams(request.postData() || "").get("kanban_drag") ===
        "true"
      );
    });
    const cardBox = await card.boundingBox();
    if (!cardBox) {
      throw new Error("Recovered drag source has no bounding box");
    }
    await page.mouse.move(
      cardBox.x + cardBox.width / 2,
      cardBox.y + cardBox.height / 2,
    );
    await page.mouse.down();
    await page.mouse.move(
      cardBox.x + cardBox.width / 2 + 16,
      cardBox.y + cardBox.height / 2 + 16,
      { steps: 5 },
    );
    await expect(targetLane).toBeVisible();
    const targetBox = await targetLane.boundingBox();
    if (!targetBox) {
      throw new Error("Recovered drag target has no bounding box");
    }
    await page.mouse.move(
      targetBox.x + targetBox.width / 2,
      targetBox.y + Math.min(80, targetBox.height / 2),
      { steps: 20 },
    );
    await page.mouse.up();
    await moveRequest;
  } finally {
    await runtime.stop();
  }
});

test("version change after reconnect keeps the board disabled", async ({
  page,
}) => {
  const runtime = await startDetentRuntime("board-version-change", [
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
  ]);

  try {
    await page.goto(`${runtime.url}/`, { waitUntil: "domcontentloaded" });
    await page.locator("#board-lanes").waitFor({ state: "visible" });
    const servedVersion = await page
      .locator("html")
      .getAttribute("data-detent-served-version");
    expect(servedVersion).toBeTruthy();

    await page.evaluate(() => {
      document.dispatchEvent(new CustomEvent("htmx:sseError", { bubbles: true }));
    });
    await expect(page.locator("html")).toHaveAttribute(
      "data-detent-connection",
      "disconnected",
    );

    await page.evaluate(() => {
      const footer = document.getElementById("detent-build-version");
      const incoming = footer.cloneNode(true);
      incoming.textContent = "v99.0.0";
      incoming.setAttribute("title", "v99.0.0");
      incoming.setAttribute("data-detent-build-version", "v99.0.0");
      footer.dispatchEvent(
        new CustomEvent("htmx:sseBeforeMessage", {
          bubbles: true,
          detail: { elt: footer, data: incoming.outerHTML },
        }),
      );
    });

    await expect(page.locator("html")).toHaveAttribute(
      "data-detent-connection",
      "reload-required",
    );
    await expect(page.locator('[data-kanban-action="move"]')).toHaveCount(0);
    await expect(page.locator("[data-detent-build-update]")).toBeVisible();
  } finally {
    await runtime.stop();
  }
});
