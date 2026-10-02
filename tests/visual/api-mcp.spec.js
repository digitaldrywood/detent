const { test, expect } = require("@playwright/test");
const path = require("node:path");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

test.describe.configure({ mode: "serial" });
test.use({ hasTouch: true });

let hub;

test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("api-mcp-settings");
});

test.afterAll(async () => {
  await hub?.stop();
});

async function openSettings(page) {
  await page.addInitScript(() => {
    window.__copiedPrompts = [];
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        writeText: async (value) => {
          window.__copiedPrompts.push(value);
        },
      },
    });
  });
  await page.route("**/app/bootstrap", async (route) => {
    const response = await route.fetch();
    const bootstrap = await response.json();
    bootstrap.organization.public_url = "https://browser.example.test";
    bootstrap.organizations = bootstrap.organizations.map((organization) =>
      organization.id === bootstrap.organization.id
        ? { ...organization, public_url: "https://browser.example.test" }
        : organization,
    );
    await route.fulfill({ response, json: bootstrap });
  });
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL("/settings/mcp", hub.fixture.url).toString());
  await expect(
    page.getByRole("button", { name: "Create key", exact: true }),
  ).toBeVisible();
}

for (const mode of ["all", "selected"]) {
  test(`creates a ${mode} key, copies secret-free prompts, disposes the secret and confirms revocation`, async ({
    page,
  }) => {
    await openSettings(page);
    const name = `${mode} browser agent`;
    await page.getByRole("button", { name: "Create key", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toHaveAccessibleName("Create API key");
    await dialog.getByLabel("Name", { exact: true }).fill(name);
    await expect(
      dialog.getByRole("radio", { name: "All projects", exact: true }),
    ).toBeChecked();
    await expect(dialog.getByLabel("Expires")).toHaveValue("30");
    await expect(dialog.getByRole("checkbox")).toHaveCount(0);
    if (mode === "selected") {
      await dialog
        .getByRole("radio", { name: "Selected projects", exact: true })
        .check();
      await expect(
        dialog.getByRole("button", { name: "Create key", exact: true }),
      ).toBeDisabled();
      await dialog
        .getByRole("checkbox", { name: "Browser collaboration", exact: true })
        .check();
    }
    const response = page.waitForResponse(
      (response) =>
        response.url().endsWith("/api-keys") &&
        response.request().method() === "POST",
    );
    await dialog
      .getByRole("button", { name: "Create key", exact: true })
      .click();
    const created = await response;
    expect(created.status()).toBe(201);
    expect(created.request().postDataJSON()).toMatchObject({
      project_access: mode,
      project_ids: mode === "all" ? [] : [hub.fixture.project_id],
    });
    await expect(dialog).toHaveAccessibleName("Key created");
    const secret = await dialog.getByLabel("New API key").inputValue();
    expect(secret.length > 0).toBe(true);
    await expect(dialog.getByLabel("New API key")).toHaveAttribute(
      "type",
      "password",
    );
    expect((await page.locator("body").textContent()).includes(secret)).toBe(
      false,
    );
    await dialog
      .getByRole("button", { name: "Copy key privately", exact: true })
      .click();
    await expect
      .poll(() =>
        page.evaluate(
          (secret) => window.__copiedPrompts.at(-1) === secret,
          secret,
        ),
      )
      .toBe(true);
    for (const label of ["Copy Direct API prompt", "Copy MCP prompt"]) {
      const copies = await page.evaluate(() => window.__copiedPrompts.length);
      await dialog.getByRole("button", { name: label, exact: true }).click();
      await expect
        .poll(() => page.evaluate(() => window.__copiedPrompts.length))
        .toBe(copies + 1);
      await expect
        .poll(() =>
          page.evaluate((secret) => {
            const prompt = window.__copiedPrompts.at(-1);
            return (
              prompt.includes("DETENT_API_KEY") &&
              !prompt.includes(secret) &&
              prompt.includes("Project access:")
            );
          }, secret),
        )
        .toBe(true);
    }
    await dialog.getByRole("button", { name: "Done", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    await expect(page.getByLabel("New API key")).toHaveCount(0);
    await page.reload();
    await expect(page.getByLabel("New API key")).toHaveCount(0);
    await page.getByRole("button", { name: "Create key", exact: true }).click();
    await expect(dialog).toHaveAccessibleName("Create API key");
    await expect(dialog.getByLabel("New API key")).toHaveCount(0);
    await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    const keyRow = page
      .locator("#settings-api-keys [data-slot=settings-row]")
      .filter({ hasText: name });
    await expect(keyRow).toContainText(
      mode === "all" ? "All projects" : "Browser collaboration",
    );
    await keyRow
      .getByRole("button", { name: `Revoke ${name}`, exact: true })
      .click();
    await expect(dialog).toHaveAccessibleName(`Revoke ${name}?`);
    await expect(dialog).toContainText("stop immediately");
    await dialog.getByRole("button", { name: "Keep key", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    await expect(keyRow).toBeVisible();
    await keyRow
      .getByRole("button", { name: `Revoke ${name}`, exact: true })
      .click();
    await dialog
      .getByRole("button", { name: "Revoke key", exact: true })
      .click();
    await expect(dialog).toHaveCount(0);
    await expect(keyRow).toHaveCount(0);
    const history = page.getByRole("button", {
      name: /Key history · \d+ revoked or expired/,
    });
    await expect(history).toHaveAttribute("aria-expanded", "false");
    await history.click();
    await expect(page.locator("#api-key-history")).toContainText(name);
    await expect(
      page
        .locator("#api-key-history [data-slot=settings-row]")
        .filter({ hasText: name }),
    ).toContainText("Revoked");
    await history.click();
    await expect(page.locator("#api-key-history")).toHaveCount(0);
    await page
      .getByRole("button", { name: "Preview Direct API setup prompt" })
      .click();
    await expect(dialog).toContainText("DETENT_API_KEY");
    await dialog
      .getByRole("button", { name: "Close", exact: true })
      .first()
      .click();
    await expect(dialog).toHaveCount(0);
  });
}

for (const theme of ["dark", "light"]) {
  for (const width of [1280, 390]) {
    test(`API & MCP fits ${width}px in ${theme} mode`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1000 });
      await page.emulateMedia({ colorScheme: theme });
      await openSettings(page);
      await page.evaluate((theme) => {
        document.documentElement.dataset.theme = theme;
        document.documentElement.classList.toggle("dark", theme === "dark");
      }, theme);
      expect(
        await page.evaluate(
          () =>
            document.documentElement.scrollWidth <=
            document.documentElement.clientWidth,
        ),
      ).toBe(true);
      if (width === 390) {
        expect(
          await page.evaluate(() =>
            Array.from(
              document.querySelectorAll(
                "#settings-mcp button, #settings-api-keys button",
              ),
            ).every((button) => {
              const target = getComputedStyle(button, "::after");
              return (
                parseFloat(target.minHeight) >= 44 &&
                parseFloat(target.minWidth) >= 44
              );
            }),
          ),
        ).toBe(true);
      }
      await page.screenshot({
        path: path.join(
          process.env.TMPDIR || process.env.TMP || process.env.TEMP,
          `api-mcp-${theme}-${width}.png`,
        ),
      });
      await page
        .getByRole("button", { name: "Create key", exact: true })
        .click();
      const dialog = page.getByRole("dialog");
      await expect(
        dialog.getByRole("radio", { name: "Read", exact: true }),
      ).toBeVisible();
      await dialog
        .getByRole("radio", { name: "Selected projects", exact: true })
        .check();
      await expect(
        dialog.getByRole("checkbox", {
          name: "Browser collaboration",
          exact: true,
        }),
      ).toBeVisible();
      expect(
        await dialog.evaluate((dialog) => {
          const rect = dialog.getBoundingClientRect();
          return (
            rect.left >= 0 &&
            rect.right <= window.innerWidth &&
            dialog.scrollWidth <= dialog.clientWidth
          );
        }),
      ).toBe(true);
      await expect(
        dialog.getByRole("button", { name: "Cancel", exact: true }),
      ).toBeInViewport();
      await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
    });
  }
}
