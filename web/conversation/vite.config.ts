import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import tailwindcss from "@tailwindcss/vite";
import { defineConfig, type Plugin } from "vitest/config";

/** Include the complete upstream licenses in the distributed client. */
function attributionBanner(): string {
  const licence = readFileSync(new URL("./LICENSE.t3code", import.meta.url), "utf8");
  const pierreLicence = readFileSync(new URL("./LICENSE.pierre-diffs", import.meta.url), "utf8");
  return [
    "/*!",
    " * Detent Cloud.",
    " * Portions derived from T3 Code. MIT License.",
    " *",
    ...licence.trimEnd().split("\n").map((line) => line ? ` * ${line}` : " *"),
    " *",
    " * Detent Cloud uses @pierre/diffs, licensed under the Apache License, Version 2.0.",
    " *",
    ...pierreLicence.trimEnd().split("\n").map((line) => line ? ` * ${line}` : " *"),
    " */",
  ].join("\n");
}

/**
 * Puts the notice at the top of the entry chunk, on disk, in `writeBundle` —
 * the last hook there is.
 *
 * Three earlier places do not work. `output.banner` and a `renderChunk` hook
 * are both handed to the minifier, which runs as a `renderChunk` hook of its
 * own and strips the comment. `generateBundle` survived the minifier but stopped
 * being *first* once the build began code-splitting: Vite prepends its own
 * `__vite__mapDeps` preamble to the entry chunk after that hook, so the notice
 * ended up a kilobyte in.
 *
 * Rewriting the written file is the one place nothing runs after. Verify with
 * `head -c 400 static/app/conversation/app.js`; `make check-app` asserts the
 * notice is present and `tests/visual/conversation.spec.js` asserts the served
 * bundle opens with it.
 */
function mitAttribution(): Plugin {
  const banner = attributionBanner();
  return {
    name: "detent-mit-attribution",
    apply: "build",
    enforce: "post",
    async writeBundle(options, bundle) {
      const dir = options.dir;
      if (dir === undefined) return;
      const { readFile, writeFile } = await import("node:fs/promises");
      const { join } = await import("node:path");
      for (const [fileName, output] of Object.entries(bundle)) {
        if (output.type !== "chunk" || !output.isEntry) continue;
        const path = join(dir, fileName);
        const code = await readFile(path, "utf8");
        if (code.startsWith(banner)) continue;
        await writeFile(path, `${banner}\n${code}`, "utf8");
      }
    },
  };
}

/** The hub the dev server proxies API and bootstrap requests to. */
const hubUrl = process.env.DETENT_HUB_URL || "http://127.0.0.1:4100";

export default defineConfig({
  // The hub serves the bundle from the embedded filesystem under this prefix.
  base: "/static/app/conversation/",
  plugins: [tailwindcss(), mitAttribution()],
  resolve: {
    alias: {
      "~": fileURLToPath(new URL("./src", import.meta.url)),
      "@t3tools/contracts/settings": fileURLToPath(
        new URL("./src/app/adapters/settings.ts", import.meta.url),
      ),
      "@t3tools/contracts": fileURLToPath(new URL("./src/contracts/index.ts", import.meta.url)),
      "@t3tools/shared/relayTracing": fileURLToPath(
        new URL("./src/runtime/support/relayTracing.ts", import.meta.url),
      ),
      "@t3tools/shared/usageLimits": fileURLToPath(
        new URL("./src/app/adapters/usageLimits.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/threads": fileURLToPath(
        new URL("./src/state/threads.ts", import.meta.url),
      ),
      "@t3tools/shared/projectFavicon": fileURLToPath(
        new URL("./src/runtime/support/projectFavicon.ts", import.meta.url),
      ),
      "@t3tools/shared/favicon": fileURLToPath(
        new URL("./src/runtime/support/favicon.ts", import.meta.url),
      ),
      "@t3tools/shared/terminalLabels": fileURLToPath(
        new URL("./src/app/adapters/terminalLabels.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/thread-settled": fileURLToPath(
        new URL("./src/runtime/state/threadSettled.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/thread-sort": fileURLToPath(
        new URL("./src/runtime/state/threadSort.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/provider-instance-display": fileURLToPath(
        new URL("./src/runtime/state/providerInstanceDisplay.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/models": fileURLToPath(
        new URL("./src/app/adapters/models.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/projects": fileURLToPath(
        new URL("./src/app/adapters/projects.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/pull-requests": fileURLToPath(
        new URL("./src/app/adapters/pullRequestVcs.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/project-grouping": fileURLToPath(
        new URL("./src/app/adapters/projectGrouping.ts", import.meta.url),
      ),
      "@t3tools/shared/sourceControl": fileURLToPath(
        new URL("./src/runtime/support/sourceControl.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/environment": fileURLToPath(
        new URL("./src/environment/scoped.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/shell": fileURLToPath(
        new URL("./src/app/adapters/shell.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/subagentRuntime": fileURLToPath(
        new URL("./src/app/adapters/subagentRuntime.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/runtime": fileURLToPath(
        new URL("./src/runtime/state/runtime.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/filesystem": fileURLToPath(
        new URL("./src/runtime/state/filesystem.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/codex-artifact-templates": fileURLToPath(
        new URL("./src/runtime/codexArtifactTemplates.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/codex-markdown-directives": fileURLToPath(
        new URL("./src/runtime/codexMarkdownDirectives.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/markdown-images": fileURLToPath(
        new URL("./src/runtime/markdownImages.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/markdown-links": fileURLToPath(
        new URL("./src/runtime/markdownLinks.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/media-reference": fileURLToPath(
        new URL("./src/runtime/mediaReference.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/media-actions": fileURLToPath(
        new URL("./src/runtime/mediaActions.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/media-source": fileURLToPath(
        new URL("./src/runtime/mediaSource.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/providerSkills": fileURLToPath(
        new URL("./src/runtime/providerSkills.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/state/assets": fileURLToPath(
        new URL("./src/state/assets.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/work-log/command-label": fileURLToPath(
        new URL("./src/runtime/workLogCommandLabel.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/work-log/presentation": fileURLToPath(
        new URL("./src/runtime/workLogPresentation.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/work-log/tool-presentation": fileURLToPath(
        new URL("./src/runtime/workLogToolPresentation.ts", import.meta.url),
      ),
      "@t3tools/client-runtime/work-log/scroll-anchor": fileURLToPath(
        new URL("./src/app/lib/scrollAnchor.ts", import.meta.url),
      ),
      "@t3tools/shared/assistantCitations": fileURLToPath(
        new URL("./src/runtime/support/assistantCitations.ts", import.meta.url),
      ),
      "@t3tools/shared/chatList": fileURLToPath(
        new URL("./src/runtime/support/chatList.ts", import.meta.url),
      ),
      "@t3tools/shared/filePreview": fileURLToPath(
        new URL("./src/runtime/support/filePreview.ts", import.meta.url),
      ),
      "@t3tools/shared/hostClassification": fileURLToPath(
        new URL("./src/runtime/support/hostClassification.ts", import.meta.url),
      ),
      "@t3tools/shared/orchestrationTiming": fileURLToPath(
        new URL("./src/runtime/support/orchestrationTiming.ts", import.meta.url),
      ),
      "@t3tools/shared/path": fileURLToPath(
        new URL("./src/runtime/support/path.ts", import.meta.url),
      ),
      "@t3tools/shared/video": fileURLToPath(
        new URL("./src/runtime/support/video.ts", import.meta.url),
      ),

      "@pierre/diffs/utils/parsePatchFiles": fileURLToPath(
        new URL("./node_modules/@pierre/diffs/dist/utils/parsePatchFiles.js", import.meta.url),
      ),
      "@pierre/diffs/types": fileURLToPath(
        new URL("./node_modules/@pierre/diffs/dist/types.js", import.meta.url),
      ),
      // Shiki's full bundle is ~200 grammars; the web bundle is the 79 a
      // browser client meets. See `src/app/adapters/shikiWebBundle.ts`.
      //
      // Vite matches an alias by prefix, so every subpath the adapter itself
      // reaches for has to be named ahead of the bare specifier — otherwise
      // `shiki/wasm` resolves to a path inside the adapter file. Each one
      // points at the package's own module, so only the entry point is
      // narrowed and nothing else about Shiki changes.
      "shiki/wasm": fileURLToPath(new URL("./node_modules/shiki/dist/wasm.mjs", import.meta.url)),
      "shiki/core": fileURLToPath(new URL("./node_modules/shiki/dist/core.mjs", import.meta.url)),
      "shiki/engine/javascript": fileURLToPath(
        new URL("./node_modules/shiki/dist/engine-javascript.mjs", import.meta.url),
      ),
      "shiki/engine/oniguruma": fileURLToPath(
        new URL("./node_modules/shiki/dist/engine-oniguruma.mjs", import.meta.url),
      ),
      "shiki/bundle/web": fileURLToPath(
        new URL("./node_modules/shiki/dist/bundle-web.mjs", import.meta.url),
      ),
      shiki: fileURLToPath(new URL("./src/app/adapters/shikiWebBundle.ts", import.meta.url)),

      "rehype-raw": fileURLToPath(
        new URL("./src/app/adapters/rehypeRawDisabled.ts", import.meta.url),
      ),
      // The vendored `ProjectFavicon.tsx` lazily imports lucide's whole icon
      // map for a branch Detent never takes; the alias keeps that file
      // byte-identical and keeps 1,588 icon chunks out of the build output.
      // See `src/browser/lucideDynamicIcon.tsx`.
      "lucide-react/dynamic": fileURLToPath(
        new URL("./src/browser/lucideDynamicIcon.tsx", import.meta.url),
      ),
    },
  },
  // `@pierre/diffs` tokenizes a diff off the main thread, and the copied
  // `components/DiffWorkerPoolProvider.tsx` loads that worker with Vite's
  // `?worker` suffix. Rollup refuses the default `iife` worker format once the
  // graph code-splits, which this one does, so the workers are ES modules —
  // which every browser that can run the rest of this bundle supports.
  worker: { format: "es" },
  build: {
    outDir: "../../static/app/conversation",
    emptyOutDir: true,
    chunkSizeWarningLimit: 700,
    // Fixed names: Go embeds the directory and the shell template references
    // these two files literally, so a content hash would break every build.
    rollupOptions: {
      // The router ships "use client" directives for frameworks that need
      // them; this bundle is a plain SPA, so they are noise.
      onwarn(warning, warn) {
        if (warning.code === "MODULE_LEVEL_DIRECTIVE") return;
        warn(warning);
      },
      output: {
        entryFileNames: "app.js",
        // Dynamic imports are split, not inlined.
        //
        // This used to be `inlineDynamicImports: true`, when the only dynamic
        // import left in the graph was a tiny one and keeping the output to
        // the three files the shell template names was worth it.
        //
        // Shiki ended that. `@pierre/diffs` highlights through `shiki`, whose
        // entry point is a map of ~200 grammars behind one dynamic import
        // each — the mechanism by which a page pays for the languages it
        // actually shows. Inlining them put all 11 MB of grammar in `app.js`,
        // taking it from 1.9 MB to 13.1 MB for a client that renders a handful
        // of languages in a session.
        //
        // Split, each grammar is its own chunk, fetched the first time a code
        // block in that language is rendered. `embed.go` embeds `static/**`
        // recursively, so the chunks ship and are served from the same prefix.
        inlineDynamicImports: false,
        chunkFileNames: "chunks/[name].js",
        assetFileNames: (asset) =>
          asset.names?.some((name) => name.endsWith(".css")) ? "app.css" : "assets/[name][extname]",
      },
    },
  },
  server: {
    proxy: {
      "/api": { target: hubUrl, changeOrigin: true },
      // `/app/bootstrap` is what the client asks for first (decisions.md §12);
      // `/chat/bootstrap` is the alias it falls back to.
      "/app/bootstrap": { target: hubUrl, changeOrigin: true },
      "/chat/bootstrap": { target: hubUrl, changeOrigin: true },
      "/__mock": { target: hubUrl, changeOrigin: true },
    },
  },
  test: {
    include: ["tests/**/*.test.ts", "tests/**/*.test.tsx", "src/**/*.test.ts"],
    setupFiles: ["./tests/setup.tsx"],
    css: false,
    poolOptions: { forks: { execArgv: ["--no-experimental-webstorage"] } },
    testTimeout: 20_000,
  },
});
