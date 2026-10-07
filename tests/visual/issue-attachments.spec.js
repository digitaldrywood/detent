const { randomBytes } = require("node:crypto");
const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII=", "base64");
let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("issue-attachments");
});
test.afterAll(async () => { await hub?.stop(); });

async function openIssue(page) {
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(`${hub.fixture.url}/work/i/${hub.fixture.work_item}`);
  await expect(page.getByTestId("issue-body")).toBeVisible();
}

async function attachmentStore(page, { fail = false, hold = null } = {}) {
  const files = new Map();
  const reads = [];
  await page.route("**/projects/*/attachments**", async (route) => {
    const request = route.request();
    const match = /\/projects\/([^/]+)\/attachments(?:\/(att_[a-f0-9]+))?$/.exec(new URL(request.url()).pathname);
    if (!match) return route.continue();
    const [, project, id] = match;
    if (request.method() === "POST") {
      if (hold) await hold;
      if (fail) return route.fulfill({ status: 413, json: { code: "payload_too_large", message: "A file must be at most 20 MiB" } });
      expect(request.headers()["x-csrf-token"]).toBeTruthy();
      const name = /filename="([^"]+)"/.exec(request.postData() ?? "")?.[1];
      const image = name?.endsWith(".png");
      const file = { id: `att_${randomBytes(16).toString("hex")}`, project_id: project, name, content_type: image ? "image/png" : "text/plain", size: image ? png.length : 5, width: image ? 1 : 0, height: image ? 1 : 0 };
      files.set(file.id, file);
      return route.fulfill({ status: 201, json: file });
    }
    reads.push(request.url());
    const file = files.get(id);
    if (!file || file.project_id !== project) return route.fulfill({ status: 404 });
    return route.fulfill({ contentType: file.content_type, body: file.content_type.startsWith("image/") ? png : "hello" });
  });
  await page.route("**/projects/*/attachments/*/metadata", async (route) => {
    const [, project, id] = /\/projects\/([^/]+)\/attachments\/(att_[a-f0-9]+)\/metadata$/.exec(new URL(route.request().url()).pathname) ?? [];
    const file = files.get(id);
    return file?.project_id === project ? route.fulfill({ json: file }) : route.fulfill({ status: 404 });
  });
  return { files, reads };
}

async function transfer(editor, kind, name) {
  await editor.evaluate((element, { kind, name, data }) => {
    const bytes = Uint8Array.from(atob(data), (char) => char.charCodeAt(0));
    const transfer = new DataTransfer();
    transfer.items.add(new File([bytes], name, { type: "image/png" }));
    const event = kind === "paste" ? new ClipboardEvent("paste", { clipboardData: transfer, bubbles: true, cancelable: true }) : new DragEvent("drop", { dataTransfer: transfer, bubbles: true, cancelable: true });
    element.dispatchEvent(event);
  }, { kind, name, data: png.toString("base64") });
}

test("paste uploads a comment image, retains markdown across reload and expands it", async ({ page }) => {
  const store = await attachmentStore(page);
  await openIssue(page);
  const editor = page.getByRole("textbox", { name: "Comment", exact: true });
  await transfer(editor, "paste", "pasted.png");
  await expect(editor).toContainText(/!\[pasted.png\]\(attachment:att_/);
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const image = page.getByTestId("issue-comment-body").getByRole("button", { name: "Preview pasted.png" }).last();
  await expect(image).toBeVisible();
  await page.reload();
  await expect(image).toBeVisible();
  expect(await image.evaluate((node) => node.naturalWidth)).toBe(1);
  await image.click();
  await expect(page.getByRole("dialog", { name: "Expanded image preview" })).toBeVisible();
  expect(store.reads.every((url) => new URL(url).origin === new URL(hub.fixture.url).origin && url.includes(`/projects/${hub.fixture.project_id}/attachments/`))).toBe(true);
});

for (const gesture of ["drop", "attach"]) {
  test(`${gesture} works in the issue body and new issue editors`, async ({ page }) => {
    await attachmentStore(page);
    await openIssue(page);
    await page.getByRole("button", { name: "Edit body", exact: true }).click();
    const editor = page.getByRole("textbox", { name: "Issue body", exact: true });
    if (gesture === "drop") await transfer(editor, "drop", "body-drop.png");
    else {
      const chooser = page.waitForEvent("filechooser");
      await page.getByTestId("issue-body").getByRole("button", { name: "Attach files", exact: true }).click();
      await (await chooser).setFiles({ name: "body-attach.png", mimeType: "image/png", buffer: png });
    }
    await expect(editor).toHaveValue(/attachment:att_/);
    await page.getByRole("button", { name: "Save body", exact: true }).click();
    await expect(page.getByTestId("issue-body").getByTestId("issue-attachment-image")).toBeVisible();
    await page.goto(`${hub.fixture.url}/work/p/${hub.fixture.project_id}`);
    await page.getByTestId("board-new-issue").click();
    const dialog = page.getByRole("dialog", { name: "New issue", exact: true });
    await dialog.getByLabel("Title", { exact: true }).fill(`Attachment ${gesture} issue`);
    const body = dialog.getByLabel("What needs doing", { exact: true });
    if (gesture === "drop") await transfer(body, "drop", "new-drop.png");
    else {
      const chooser = page.waitForEvent("filechooser");
      await dialog.getByRole("button", { name: "Attach files", exact: true }).click();
      await (await chooser).setFiles({ name: "notes.txt", mimeType: "text/plain", buffer: Buffer.from("hello") });
    }
    await expect(body).toHaveValue(/attachment:att_/);
    await dialog.getByRole("button", { name: "Create issue", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    await page.getByTestId("issue-card").filter({ hasText: `Attachment ${gesture} issue` }).getByTestId("issue-card-open").click();
    await expect(page.getByTestId("issue-body").getByTestId(gesture === "drop" ? "issue-attachment-image" : "issue-attachment-file")).toBeVisible();
    if (gesture === "attach") {
      const chip = page.getByTestId("issue-attachment-file");
      await expect(chip).toContainText("notes.txt");
      await expect(chip).toContainText("B");
      await expect(chip).toHaveAttribute("download", "notes.txt");
    }
  });
}

test("another project's attachment renders unavailable without requesting bytes", async ({ page }) => {
  const store = await attachmentStore(page);
  await openIssue(page);
  const id = `att_${randomBytes(16).toString("hex")}`;
  store.files.set(id, { id, project_id: "prj_other", name: "private.png", content_type: "image/png", size: png.length, width: 1, height: 1 });
  const metadataReads = [];
  page.on("request", (request) => { if (request.url().includes(id)) metadataReads.push(request.url()); });
  const marker = `Cross-project attachment ${id}`;
  await page.getByRole("textbox", { name: "Comment", exact: true }).fill(`${marker}\n\n![private](attachment:${id})`);
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const comment = page.getByTestId("issue-comment").filter({ hasText: marker });
  await expect(comment.getByTestId("attachment-unavailable")).toBeVisible();
  expect(metadataReads.length).toBeGreaterThan(0);
  expect(metadataReads.every((url) => new URL(url).origin === new URL(hub.fixture.url).origin && url.includes(`/projects/${hub.fixture.project_id}/attachments/`) && url.endsWith("/metadata"))).toBe(true);
  expect(store.reads.filter((url) => url.includes(id))).toEqual([]);
});

test("upload placeholders block saving and failures remove them with an inline error", async ({ page }) => {
  let release;
  const hold = new Promise((resolve) => { release = resolve; });
  await attachmentStore(page, { hold, fail: true });
  try {
    await openIssue(page);
    const editor = page.getByRole("textbox", { name: "Comment", exact: true });
    await editor.fill("Keep my draft");
    await transfer(editor, "paste", "too-large.png");
    await expect(editor).toContainText("Uploading too-large.png");
    await expect(page.getByRole("button", { name: "Waiting for the upload to finish", exact: true })).toBeDisabled();
    release();
    await expect(page.getByRole("alert").filter({ hasText: "A file must be at most 20 MiB" })).toBeVisible();
    await expect(editor).not.toContainText("Uploading");
    await expect(editor).toContainText("Keep my draft");
  } finally { release(); }
});
