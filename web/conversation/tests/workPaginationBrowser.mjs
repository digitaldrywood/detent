import { build } from "esbuild";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const scratch = process.env.TMPDIR || process.env.TMP || process.env.TEMP;
if (!scratch) throw new Error("Worker scratch is required");
const root = fileURLToPath(new URL("../", import.meta.url));
const output = await mkdtemp(join(scratch, "work-pagination-browser-"));
await writeFile(join(output, "crypto.js"), 'export function createHash() { throw new Error("WebSocket upgrades are unavailable in browser fixtures"); }');
await build({
  absWorkingDir: root,
  entryPoints: ["tests/workPaginationBrowser.tsx"],
  outfile: join(output, "app.js"),
  bundle: true,
  format: "iife",
  platform: "browser",
  alias: { "node:crypto": join(output, "crypto.js"), "lucide-react/dynamic": resolve(root, "src/browser/lucideDynamicIcon.tsx") },
  minify: true,
  define: { "process.env.NODE_ENV": '"production"' },
  plugins: [{ name: "fixture-shell", setup(builder) {
    builder.onLoad({ filter: /\/src\/app\/App\.tsx$/ }, () => ({
      contents: 'export function useShell() { return { connection: { tone: "dc-ok", label: "Connected", detail: null, tooltip: "Fixture" } }; }',
      loader: "js",
    }));
  } }],
});
const css = await readFile(resolve(root, "../../static/app/conversation/app.css"), "utf8");
await writeFile(join(output, "app.css"), css);
await writeFile(join(output, "index.html"), '<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="app.css"></head><body><div id="app" style="height:100vh;display:flex;flex-direction:column"></div><script src="app.js"></script></body></html>');
process.stdout.write(`${pathToFileURL(join(output, "index.html")).href}\n`);
