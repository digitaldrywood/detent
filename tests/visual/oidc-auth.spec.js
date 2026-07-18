const fs = require("node:fs");
const { test, expect } = require("@playwright/test");
const { startDetentRuntime } = require("./detent-runtime");
const { startFakeOIDC } = require("./fake-oidc");

const allowedEmail = "operator@example.com";

test("OIDC login gates board views, persists sessions, and denies unauthorized identities", async ({
  browser,
}) => {
  const issuer = await startFakeOIDC();
  const runtimeArgs = [
    "--db",
    "detent.db",
    "--demo",
    "kanban",
    "--demo-project",
    "demo-project",
    "--auth-email",
    allowedEmail,
    "--auth-oidc-issuer",
    issuer.url,
    "--auth-oidc-client-id",
    issuer.clientId,
    "--auth-oidc-client-secret",
    issuer.clientSecret,
  ];
  let runtime = await startDetentRuntime("oidc-auth", runtimeArgs);
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    const boardPath = "/projects/demo-project/kanban";
    const gatedAPI = await context.request.get(`${runtime.url}/api/v1/state`, {
      maxRedirects: 0,
    });
    expect(gatedAPI.status()).toBe(303);
    expect(gatedAPI.headers().location).toBe("/login?next=%2Fapi%2Fv1%2Fstate");

    await page.goto(`${runtime.url}${boardPath}`, {
      waitUntil: "domcontentloaded",
    });
    await expect(page).toHaveURL(
      new RegExp(`/login\\?next=${encodeURIComponent(boardPath)}`),
    );
    await expect(page).toHaveScreenshot("oidc-login.png");
    await page
      .getByRole("link", { name: "Continue with identity provider" })
      .click();
    await expect(
      page.getByRole("heading", { name: "Test identity provider" }),
    ).toBeVisible();

    const eventStream = page.waitForResponse((response) => {
      const responseURL = new URL(response.url());
      return responseURL.pathname === "/events" && response.status() === 200;
    });
    await page.getByRole("link", { name: "Sign in as operator" }).click();
    await expect(page).toHaveURL(`${runtime.url}${boardPath}`);
    await expect(
      page.getByRole("button", { name: /Kanban demo backlog intake/ }),
    ).toBeVisible();
    await eventStream;

    const authenticatedAPI = await context.request.get(
      `${runtime.url}/api/v1/state`,
    );
    expect(authenticatedAPI.status()).toBe(200);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByRole("button", { name: "Open navigation" })).toBeVisible();

    const runtimeHome = runtime.home;
    await runtime.stop();
    runtime = await startDetentRuntime("oidc-auth-restart", runtimeArgs, {
      home: runtimeHome,
    });
    await page.goto(`${runtime.url}${boardPath}`, {
      waitUntil: "domcontentloaded",
    });
    await expect(
      page.getByRole("button", { name: /Kanban demo backlog intake/ }),
    ).toBeVisible();

    const deniedContext = await browser.newContext();
    const deniedPage = await deniedContext.newPage();
    await deniedPage.goto(`${runtime.url}${boardPath}`, {
      waitUntil: "domcontentloaded",
    });
    await deniedPage
      .getByRole("link", { name: "Continue with identity provider" })
      .click();
    const deniedResponsePromise = deniedPage.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === "/auth/oidc/callback",
    );
    await deniedPage.getByRole("link", { name: "Sign in as outsider" }).click();
    const deniedResponse = await deniedResponsePromise;
    expect(deniedResponse.status()).toBe(403);
    await expect(
      deniedPage.getByRole("heading", { name: "Access denied" }),
    ).toBeVisible();
    await expect(deniedPage).toHaveScreenshot("oidc-access-denied.png");
    await deniedContext.close();

    const runtimeLog = fs.readFileSync(runtime.logPath, "utf8");
    for (const credential of [
      issuer.clientSecret,
      issuer.accessToken,
      issuer.lastCode(),
    ]) {
      expect(runtimeLog).not.toContain(credential);
    }
  } finally {
    await context.close();
    await runtime.stop();
    await issuer.stop();
  }
});
