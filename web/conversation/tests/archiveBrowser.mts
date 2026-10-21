import { createHash } from "node:crypto";
import { createServer as createHTTPServer } from "node:http";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";
import { chromium, expect } from "@playwright/test";
import { createServer } from "vite";
import { startMockHub } from "../dev/mock-hub.ts";

const output = process.env.ARCHIVE_EVIDENCE_DIR;
if (!output) throw new Error("ARCHIVE_EVIDENCE_DIR is required");
await mkdir(output, { recursive: true });
const root = dirname(dirname(fileURLToPath(import.meta.url)));
const hub = await startMockHub({ port: 0, deltaDelayMs: 0 });
const objects = new Map<string, Uint8Array>();
const objectReads: string[] = [];
const objectServer = createHTTPServer((request, response) => {
  response.setHeader("Access-Control-Allow-Origin", "*");
  const data = objects.get(request.url ?? "");
  objectReads.push(request.url ?? "");
  if (!data) { response.writeHead(404).end(); return; }
  response.writeHead(200, { "Content-Type": "application/gzip", "Content-Length": data.length });
  response.end(data);
});
await new Promise<void>((resolve) => objectServer.listen(0, "127.0.0.1", resolve));
const address = objectServer.address();
if (address === null || typeof address === "string") throw new Error("Object service did not bind");
const objectOrigin = `http://127.0.0.1:${address.port}`;
process.env.DETENT_HUB_URL = hub.url;
const vite = await createServer({ root, optimizeDeps: { include: ["shiki/core", "shiki/engine/javascript", "shiki/engine/oniguruma", "shiki/wasm"] }, server: { host: "127.0.0.1", port: 0 } });
await vite.listen();
const origin = vite.resolvedUrls?.local[0];
if (!origin) throw new Error("App did not bind");
const browser = await chromium.launch({ headless: true });
const item = `wi_${(3304).toString(16).padStart(32, "0")}`;
const change = `change_${(3304).toString(16).padStart(32, "0")}`;
const sections: string[] = [];
const grants = new Map<string, Record<string, unknown>>();
const snapshot = JSON.parse(await readFile(join(root, "src/contracts/fixtures/conversation-snapshot.json"), "utf8"));
snapshot.conversation = { ...snapshot.conversation, organization_id: "org_mock", project_id: "proj_alpha", work_item_id: item,
  status: "settled", message_count: 2, execution: { ...snapshot.conversation.execution, status: "idle", attempt_id: null, turn_id: null,
    capabilities: { steer: false, interrupt: false, answer: false, continue: false } } };
snapshot.has_more = false;
snapshot.questions = [];
const conversation = snapshot.conversation.id as string;
const requestEvidence: unknown[] = [];
const baseline: Record<string, string> = {};
const observedAt = new Date();
try {
  for (const storage of ["hot", "archived"] as const) {
  for (const theme of (storage === "hot" ? ["light"] : ["light", "dark"]) as ("light" | "dark")[]) {
    for (const width of (storage === "hot" ? [1440] : [1440, 390])) {
      const context = await browser.newContext({ viewport: { width, height: 1000 }, colorScheme: theme });
      const page = await context.newPage();
      await page.clock.setFixedTime(observedAt);
      const compare = async (name: string, testId: string) => {
        const text = (await page.getByTestId(testId).textContent())?.replace(/\s+/g, " ").trim() ?? "";
        if (storage === "hot") baseline[name] = text;
        else await expect(page.getByTestId(testId)).toHaveText(baseline[name]!);
      };
      const errors: string[] = [];
      const requestedSections: string[] = [];
      const requestedAccess: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      page.on("response", (response) => { if ([401, 403, 404].includes(response.status())) process.stderr.write(`${storage}: ${response.status()} ${response.url()}\n`); });
      await page.route("**/api/v2/**", async (route) => {
        const url = new URL(route.request().url());
        if (url.pathname.endsWith("/source-recovery")) {
          await route.fulfill({ json: { work_item_id: item, revision: "1", source_machine_id: "", source_runner_name: "", source_runner_id: "", attempt_id: "", version_id: "", head_sha: "", base_sha: "", destination_runner_id: "", available: false, quiesced: false, reason: "", destinations: [] } });
          return;
        }
        const grant = grants.get(url.pathname+url.search);
        if (grant) { requestedAccess.push(url.searchParams.get("section") ?? ""); await route.fulfill({ json: grant }); return; }
        if (url.pathname.endsWith(`/conversations/${conversation}/events`)) {
          await route.fulfill({ contentType: "text/event-stream", body: 'event: closed\ndata: {"reason":"archived"}\n\n' }); return;
        }
        if (url.pathname.endsWith("/conversations")) {
          await route.fulfill({ json: { conversations: [snapshot.conversation], next_cursor: null } }); return;
        }
        const conversationRead = url.pathname.endsWith(`/conversations/${conversation}`) || url.pathname.endsWith(`/work-items/${item}/conversation`);
        if (conversationRead && url.searchParams.get("metadata") === "true") {
          await route.fulfill({ json: { ...snapshot, messages: [], questions: [] } }); return;
        }
        const response = conversationRead ? null : await route.fetch();
        if (response !== null && (!response.ok() || route.request().method() !== "GET")) { await route.fulfill({ response }); return; }
        const pathname = url.pathname;
        let section = conversationRead ? `conversation:${conversation}` : "";
        if (pathname.endsWith(`/work-items/${item}`)) section = "issue";
        if (pathname.endsWith(`/changes/${change}`)) section = `change:${change}`;
        if (/\/attempts\/[^/]+\/diff$/.test(pathname) || pathname.endsWith(`/work-items/${item}/diff`)) section = "diff";
        if (pathname.endsWith(`/work-items/${item}/attempts`)) section = "attempts";
        if (!section) { await route.fulfill({ response: response! }); return; }
        const hot = conversationRead ? snapshot : await response!.json() as Record<string, unknown>;
        if (section === "issue") { hot.state = "Done"; hot.terminal = true; hot.archived = storage === "archived"; }
        if (storage === "hot") { await route.fulfill({ json: hot }); return; }
        const project = String(hot.project_id ?? (hot.change as Record<string, unknown> | undefined)?.project_id ?? "proj_alpha");
        let data: unknown = hot;
        if (section === "issue") {
          const attempts = await fetch(`${hub.url}/api/v2/organizations/org_mock/projects/${project}/work-items/${item}/attempts`).then((value) => value.json()) as { items: unknown[] };
          data = { body: hot.body, attempts: attempts.items };
        }
        const bytes = gzipSync(JSON.stringify({ schema_version: 1, section_id: section, data }));
        const digest = createHash("sha256").update(bytes).digest("hex");
        const access = `/api/v2/organizations/org_mock/projects/${project}/work-items/${item}/archives/${digest}/access?section=${encodeURIComponent(section)}&revision=1`;
        objects.set(`/${digest}`, bytes);
        grants.set(access, { sha256: digest, url: `${objectOrigin}/${digest}`, expires_at: new Date(Date.now()+300_000).toISOString() });
        sections.push(section);
        requestedSections.push(section);
        await route.fulfill({ json: {
          archive: { organization_id: "org_mock", project_id: project, work_item_id: item, revision: "1", section_id: section, sha256: digest,
            compressed_size: bytes.length, uncompressed_size: Buffer.byteLength(JSON.stringify({ schema_version: 1, section_id: section, data })), codec: "gzip", schema_version: 1, access_path: access },
          current: section === "issue" ? { ...hot, body: "" } : null, projection: section === "issue" ? "issue" : "replace",
        } });
      });
      await page.goto(new URL(`/work/i/${item}`, origin).href);
      await page.evaluate((theme) => { document.documentElement.classList.toggle("dark", theme === "dark"); document.documentElement.dataset.theme = theme; }, theme);
      await expect(page.getByRole("button", { name: "Timeline", exact: true }).or(page.getByRole("tab", { name: "Timeline", exact: true }))).toBeVisible();
      await expect(page.getByText("Archived detail is unavailable", { exact: false })).toHaveCount(0);
      await expect(page.getByRole("button", { name: /Conversation with|^Conversation/ }).first()).toBeVisible();
      if (storage === "archived") {
        expect([...new Set(requestedSections)]).toEqual(["issue"]);
        expect([...new Set(requestedAccess)]).toEqual(["issue"]);
      }
      await page.getByTestId("view-conversation").click();
      await expect(page.getByTestId("user-turn")).toContainText(snapshot.messages[0].text);
      await expect(page.getByTestId("assistant-turn")).toContainText(snapshot.messages[1].text);
      await expect(page.getByTestId("issue-activity")).toContainText(`sent the runner: ${snapshot.messages[0].text}`);
      await expect(page.getByTestId("resource-conversation")).toContainText("2 messages");
      if (storage === "archived") expect(requestedAccess).toContain(`conversation:${conversation}`);
      await page.getByRole("button", { name: "Close Conversation", exact: true }).click();
      if (width === 390) {
        await page.keyboard.press("Escape");
        await expect(page.getByRole("dialog", { includeHidden: true })).toBeHidden();
      } else await page.getByRole("button", { name: /^Toggle right panel/, pressed: true }).first().click();
      await expect(page.getByTestId("issue-activity")).toContainText(`sent the runner: ${snapshot.messages[0].text}`);
      await compare("timeline", "issue-activity");
      await expect(page.getByRole("tab", { name: "Diagnostics", exact: true })).toBeVisible();
      if (storage === "archived") {
        await page.getByRole("heading", { name: "runner: isolate cleanup process discovery", exact: true }).scrollIntoViewIfNeeded();
        await page.screenshot({ path: join(output, `archived-issue-${theme}-${width}.png`), fullPage: true, animations: "disabled" });
      }
      await page.getByRole("tab", { name: "Diagnostics", exact: true }).click();
      await expect(page.getByRole("table", { name: "Attempts", exact: true })).toBeVisible();
      await expect(page.getByText("Loading diagnostics…", { exact: true })).toHaveCount(0);
      await compare("runs", "issue-diagnostics-panel");
      if (storage === "archived") expect(requestedAccess).toContain("attempts");
      await page.goto(new URL(`/work/i/${item}/changes/${change}?project=proj_alpha`, origin).href);
      await page.evaluate((theme) => { document.documentElement.classList.toggle("dark", theme === "dark"); document.documentElement.dataset.theme = theme; }, theme);
      await expect(page.getByTestId("change-round-card")).toBeVisible();
      await expect(page.getByTestId("diff-files")).toBeVisible();
      await expect(page.getByText("Loading code...", { exact: true })).toHaveCount(0);
      await expect(page.getByTestId("change-round").first()).toBeVisible();
      await expect(page.getByText("Reviews", { exact: true }).first()).toBeVisible();
      await compare("round-reviews", "change-round-card");
      await compare("diff", "diff-files");
      if (storage === "archived") {
        expect(requestedAccess).toContain(`change:${change}`);
        expect(requestedAccess).toContain("diff");
        await page.screenshot({ path: join(output, `archived-change-${theme}-${width}.png`), fullPage: true, animations: "disabled" });
      }
      requestEvidence.push({ storage, theme, width, requestedSections, requestedAccess });
      if (errors.length > 0) throw new Error(errors.join("\n"));
      await context.close();
    }
  }
  }
  for (const role of ["reporter", "limited-grant"]) {
    const context = await browser.newContext();
    const page = await context.newPage();
    const requests: string[] = [];
    const before = objectReads.length;
    await page.route("**/app/bootstrap", async (route) => {
      const response = await route.fetch();
      const bootstrap = await response.json();
      bootstrap.actor = { ...bootstrap.actor, role: role === "reporter" ? "reporter" : "viewer" };
      bootstrap.projects = role === "reporter" ? bootstrap.projects : bootstrap.projects.filter((project: { id: string }) => project.id !== "proj_alpha");
      await route.fulfill({ json: bootstrap });
    });
    await page.route("**/api/v2/**", async (route) => {
      const url = new URL(route.request().url());
      requests.push(url.pathname + url.search);
      if (url.pathname.includes("/archives/")) { await route.fulfill({ status: 404, json: { code: "not_found", message: "Not found" } }); return; }
      if (role === "limited-grant" && (url.pathname.includes("/projects/proj_alpha/") || url.pathname.endsWith(`/work-items/${item}`))) {
        await route.fulfill({ status: 404, json: { code: "not_found", message: "Not found" } }); return;
      }
      if (url.pathname.endsWith("/conversations")) { await route.fulfill({ json: { conversations: [], next_cursor: null } }); return; }
      const response = await route.fetch();
      if (url.pathname.endsWith(`/work-items/${item}`) && response.ok()) {
        const issue = await response.json();
        await route.fulfill({ json: { ...issue, archived: true, body: "Public report history" } }); return;
      }
      await route.fulfill({ response });
    });
    await page.goto(new URL(`/work/i/${item}`, origin).href);
    if (role === "reporter") await expect(page.getByText("Public report history", { exact: false })).toBeVisible();
    else await expect(page.getByText(/not found|not available|could not load/i).first()).toBeVisible();
    expect(requests.filter((path) => /\/archives\/|\/attempts|\/changes|\/diff|\/conversations\//.test(path))).toEqual([]);
    expect(objectReads.length).toBe(before);
    requestEvidence.push({ role, requests, objectReads: objectReads.length - before });
    await context.close();
  }
  expect(objectReads.length).toBeGreaterThan(0);
  await writeFile(join(output, "archive-browser.txt"), JSON.stringify({ sections, objectReads, requestEvidence, objectOrigin, appOrigin: origin, themes: ["light", "dark"], widths: [1440, 390] }, null, 2)+"\n");
  process.stdout.write(`Verified ${objectReads.length} cross-origin gzip reads; screenshots in ${output}\n`);
} finally {
  await browser.close();
  await vite.close();
  await hub.close();
  await new Promise<void>((resolve, reject) => objectServer.close((error) => error ? reject(error) : resolve()));
}
