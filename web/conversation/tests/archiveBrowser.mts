import { createHash } from "node:crypto";
import { createServer as createHTTPServer } from "node:http";
import { mkdir, writeFile } from "node:fs/promises";
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
try {
  for (const theme of ["light", "dark"] as const) {
    for (const width of [1440, 390]) {
      const context = await browser.newContext({ viewport: { width, height: 1000 }, colorScheme: theme });
      const page = await context.newPage();
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.route("**/api/v2/**", async (route) => {
        const url = new URL(route.request().url());
        if (url.pathname.endsWith("/source-recovery")) {
          await route.fulfill({ json: { work_item_id: item, revision: "1", source_machine_id: "", source_runner_name: "", source_runner_id: "", attempt_id: "", version_id: "", head_sha: "", base_sha: "", destination_runner_id: "", available: false, quiesced: false, reason: "", destinations: [] } });
          return;
        }
        const grant = grants.get(url.pathname+url.search);
        if (grant) { await route.fulfill({ json: grant }); return; }
        const response = await route.fetch();
        if (!response.ok() || route.request().method() !== "GET") { await route.fulfill({ response }); return; }
        const pathname = url.pathname;
        let section = "";
        if (pathname.endsWith(`/work-items/${item}`)) section = "issue";
        if (pathname.endsWith(`/changes/${change}`)) section = `change:${change}`;
        if (/\/attempts\/[^/]+\/diff$/.test(pathname) || pathname.endsWith(`/work-items/${item}/diff`)) section = "diff";
        if (!section) { await route.fulfill({ response }); return; }
        const hot = await response.json() as Record<string, unknown>;
        if (section === "issue") { hot.state = "Done"; hot.terminal = true; hot.archived = true; }
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
      await page.screenshot({ path: join(output, `archived-issue-${theme}-${width}.png`), fullPage: true });
      await page.goto(new URL(`/work/i/${item}/changes/${change}?project=proj_alpha`, origin).href);
      await page.evaluate((theme) => { document.documentElement.classList.toggle("dark", theme === "dark"); document.documentElement.dataset.theme = theme; }, theme);
      await expect(page.getByTestId("change-round-card")).toBeVisible();
      await expect(page.getByTestId("diff-files")).toBeVisible();
      await expect(page.getByText("Loading code...", { exact: true })).toHaveCount(0);
      await page.screenshot({ path: join(output, `archived-change-${theme}-${width}.png`), fullPage: true });
      if (errors.length > 0) throw new Error(errors.join("\n"));
      await context.close();
    }
  }
  expect(objectReads.length).toBeGreaterThan(0);
  await writeFile(join(output, "archive-browser.txt"), JSON.stringify({ sections, objectReads, objectOrigin, appOrigin: origin, themes: ["light", "dark"], widths: [1440, 390] }, null, 2)+"\n");
  process.stdout.write(`Verified ${objectReads.length} cross-origin gzip reads; screenshots in ${output}\n`);
} finally {
  await browser.close();
  await vite.close();
  await hub.close();
  await new Promise<void>((resolve, reject) => objectServer.close((error) => error ? reject(error) : resolve()));
}
