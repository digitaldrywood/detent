const fs = require("node:fs");
const path = require("node:path");

async function providersRunnersPreview() {
  const { build } = require(path.resolve("web/conversation/node_modules/esbuild"));
  const result = await build({
    entryPoints: ["tests/visual/providers-runners-preview.tsx"],
    bundle: true,
    write: false,
    format: "iife",
    minify: true,
    jsx: "automatic",
    define: { "process.env.NODE_ENV": '"production"', "import.meta.env.DEV": "false" },
    alias: {
      "~": path.resolve("web/conversation/src"),
      "lucide-react/dynamic": path.resolve("web/conversation/src/browser/lucideDynamicIcon.tsx"),
      "@pierre/diffs/utils/parsePatchFiles": path.resolve("web/conversation/node_modules/@pierre/diffs/dist/utils/parsePatchFiles.js"),
      "@pierre/diffs/types": path.resolve("web/conversation/node_modules/@pierre/diffs/dist/types.js"),
    },
    loader: { ".css": "empty" },
    plugins: [{
      name: "vite-worker-stub",
      setup(build) {
        build.onResolve({ filter: /\?worker$/ }, (args) => ({ path: args.path, namespace: "vite-worker-stub" }));
        build.onLoad({ filter: /.*/, namespace: "vite-worker-stub" }, () => ({ contents: "export default class {}", loader: "js" }));
      },
    }],
    tsconfig: "web/conversation/tsconfig.json",
    nodePaths: [path.resolve("web/conversation/node_modules")],
  });
  let css = fs.readFileSync("static/app/conversation/app.css", "utf8");
  for (const font of ["Geist-Variable.woff2", "GeistMono-Variable.woff2"]) {
    const value = fs.readFileSync(path.join("static/fonts", font)).toString("base64");
    css = css.replaceAll("../../fonts/" + font, "data:font/woff2;base64," + value);
  }
  return '<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Providers & runners</title><style>' + css + '</style></head><body><div id="root"></div><script>' + result.outputFiles[0].text.replaceAll("</script", "<\\/script") + ';document.currentScript.remove();</script></body></html>';
}

module.exports = { providersRunnersPreview };
