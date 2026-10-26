import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import tailwindcss from "@tailwindcss/vite";
import { defineConfig, type Plugin } from "vitest/config";

/** Include the complete upstream licenses in the distributed client. */
function attributionBanner(): string {
  const licence = readFileSync(new URL("./LICENSE.t3code", import.meta.url), "utf8");
  const voiceLicence = readFileSync(new URL("./LICENSE.voice-activity", import.meta.url), "utf8");
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
    " *",
    " * Detent Cloud uses @ricky0123/vad-web and Silero VAD for browser voice activity detection.",
    " *",
    ...voiceLicence.trimEnd().split("\n").map((line) => line ? ` * ${line}` : " *"),
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

/**
 * Emits THIRD_PARTY_LICENSES.txt next to the bundle: the name, version,
 * declared license, and every LICENSE/NOTICE/COPYING file of each npm package
 * the build pulled in, sorted so the output is deterministic.
 */
function thirdPartyLicenses(): Plugin {
  return {
    name: "detent-third-party-licenses",
    apply: "build",
    async generateBundle(_options, bundle) {
      const { existsSync, readdirSync } = await import("node:fs");
      const { dirname, join, sep } = await import("node:path");
      const marker = `${sep}node_modules${sep}`;
      const roots = new Set<string>();
      for (const output of Object.values(bundle)) {
        if (output.type !== "chunk") continue;
        for (const id of output.moduleIds) {
          const path = id.replace(/^\0/, "").split("?")[0] ?? "";
          const at = path.lastIndexOf(marker);
          if (at < 0) continue;
          const rest = path.slice(at + marker.length).split(sep);
          const depth = rest[0]?.startsWith("@") ? 2 : 1;
          let root = join(path.slice(0, at + marker.length), ...rest.slice(0, depth));
          while (!existsSync(join(root, "package.json")) && root !== dirname(root)) root = dirname(root);
          roots.add(root);
        }
      }
      const entries: string[] = [];
      for (const root of [...roots].sort()) {
        const manifest = JSON.parse(readFileSync(join(root, "package.json"), "utf8")) as {
          name?: string;
          version?: string;
          license?: string;
        };
        const files = readdirSync(root)
          .filter((name) => /^(licen[cs]e|notice|copying)/i.test(name))
          .sort();
        const texts = files.map((name) => readFileSync(join(root, name), "utf8").trimEnd());
        entries.push(
          [
            `${manifest.name ?? root}@${manifest.version ?? "unknown"}`,
            `License: ${manifest.license ?? "see below"}`,
            ...texts,
          ].join("\n\n"),
        );
      }
      const unique = [...new Set(entries)];
      this.emitFile({
        type: "asset",
        fileName: "THIRD_PARTY_LICENSES.txt",
        source: `Third-party software bundled in Detent Cloud.\n\n${unique.join(`\n\n${"-".repeat(72)}\n\n`)}\n`,
      });
    },
  };
}

/**
 * Serves the embedded fonts on the dev server.
 *
 * `index.css` names them as `../../fonts/…`, which is right for the built
 * bundle (`static/app/conversation/app.css` → `static/fonts`). The dev server
 * injects the stylesheet into the page, so the same URL resolves against
 * whatever route is open (`/work/fonts/…`, `/design-system/frame/…/fonts/…`).
 * Answer any `…/fonts/<file>.woff2` request from `static/fonts`.
 */
function devFonts(): Plugin {
  const dir = fileURLToPath(new URL("../../static/fonts/", import.meta.url));
  return {
    name: "detent-dev-fonts",
    apply: "serve",
    configureServer(server) {
      server.middlewares.use((request, response, next) => {
        const match = /\/fonts\/([\w-]+\.woff2)(?:\?.*)?$/.exec(request.url ?? "");
        if (match === null) return next();
        try {
          const body = readFileSync(`${dir}${match[1]}`);
          response.setHeader("Content-Type", "font/woff2");
          response.end(body);
        } catch {
          next();
        }
      });
    },
  };
}

/** The output files whose names carry their content hash. */
/** `[name]-[hash]` with the eight lowercase hex characters `hashCharacters: "hex"` produces. */
const HASHED_FILE = /^(chunks|assets)\/[^/]+-[0-9a-f]{8}\.(js|css|wasm|svg|png|woff2?)$/;
const COMPRESSIBLE = /\.(js|css|svg|txt|json|wasm)$/;

/**
 * Writes a brotli and a gzip sibling next to every compressible output file,
 * which the hub serves by `Accept-Encoding` negotiation, then holds the entry
 * chunk to the committed budget in `bundle-budget.json`: the build fails when
 * the compressed entry grows more than 10% past it. It runs in `closeBundle`,
 * after `mitAttribution` has rewritten the entry on disk.
 */
function precompressAndBudget(): Plugin {
  let outDir = "";
  let client = false;
  return {
    name: "detent-precompress-budget",
    apply: "build",
    configResolved(config) {
      client = config.mode === "production";
      outDir = config.build.outDir.startsWith("/") ? config.build.outDir : `${config.root}/${config.build.outDir}`;
    },
    async closeBundle() {
      if (!client) return;
      const { readdir, readFile, writeFile } = await import("node:fs/promises");
      const { join, relative, sep } = await import("node:path");
      const { promisify } = await import("node:util");
      const zlib = await import("node:zlib");
      const brotli = promisify(zlib.brotliCompress);
      const gzip = promisify(zlib.gzip);
      const files = (await readdir(outDir, { recursive: true, withFileTypes: true }))
        .filter((entry) => entry.isFile())
        .map((entry) => relative(outDir, join(entry.parentPath, entry.name)).split(sep).join("/"));
      const unhashed = files.filter((name) => /^(chunks|assets)\//.test(name) && !HASHED_FILE.test(name));
      if (!files.some((name) => name.startsWith("chunks/"))) throw new Error("The build emitted no route chunks under chunks/.");
      if (unhashed.length > 0) {
        throw new Error(`Files under chunks/ and assets/ are served as immutable and must carry a content hash: ${unhashed.join(", ")}`);
      }
      const sizes = new Map<string, { raw: number; br: number; gzip: number }>();
      await Promise.all(
        files.filter((name) => COMPRESSIBLE.test(name)).map(async (name) => {
          const source = await readFile(join(outDir, name));
          const [br, gz] = await Promise.all([
            brotli(source, {
              params: {
                [zlib.constants.BROTLI_PARAM_QUALITY]: zlib.constants.BROTLI_MAX_QUALITY,
                [zlib.constants.BROTLI_PARAM_SIZE_HINT]: source.length,
              },
            }),
            gzip(source, { level: zlib.constants.Z_BEST_COMPRESSION }),
          ]);
          sizes.set(name, { raw: source.length, br: br.length, gzip: gz.length });
          if (source.length < 1024) return;
          if (br.length < source.length) await writeFile(join(outDir, `${name}.br`), br);
          if (gz.length < source.length) await writeFile(join(outDir, `${name}.gz`), gz);
        }),
      );
      const budget = JSON.parse(await readFile(new URL("./bundle-budget.json", import.meta.url), "utf8")) as {
        entry: string;
        brotli: number;
        gzip: number;
      };
      const entry = sizes.get(budget.entry);
      if (entry === undefined) throw new Error(`The budgeted entry ${budget.entry} was not built.`);
      const over = (["brotli", "gzip"] as const).filter((key) => entry[key === "brotli" ? "br" : "gzip"] > budget[key] * 1.1);
      if (over.length > 0) {
        throw new Error(
          `${budget.entry} is over its budget by more than 10%: brotli ${entry.br} B (budget ${budget.brotli} B), gzip ${entry.gzip} B (budget ${budget.gzip} B). ` +
            "Split the new code into a route chunk, or raise bundle-budget.json deliberately.",
        );
      }
      this.environment?.logger.info(`${budget.entry}: ${entry.raw} B, brotli ${entry.br} B, gzip ${entry.gzip} B (budget brotli ${budget.brotli} B, gzip ${budget.gzip} B)`);
    },
  };
}

/** The hub the dev server proxies API and bootstrap requests to. */
const hubUrl = process.env.DETENT_HUB_URL || "http://127.0.0.1:4100";

export default defineConfig({
  // The bundle loads its chunks, workers and styles relative to its own URL,
  // so one build serves at the root of an origin and under a tenant base
  // (`/organizations/ORG`) behind the shared Cloud entry. The shell is the one
  // file that names the prefix absolutely; the hub rewrites it per base.
  base: "./",
  experimental: {
    renderBuiltUrl: (filename, { hostType }) =>
      hostType === "html" ? `/static/app/conversation/${filename}` : { relative: true },
  },
  plugins: [tailwindcss(), mitAttribution(), thirdPartyLicenses(), devFonts(), precompressAndBudget()],
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
      "lucide-react/dynamic": fileURLToPath(
        new URL("./src/browser/lucideDynamicIcon.tsx", import.meta.url),
      ),
      // lucide 0.5x publishes `module: dist/esm/lucide.js`, but ships the ES
      // entry at dist/esm/lucide/src/lucide.js. MorphIcon reads icon data from it.
      lucide: fileURLToPath(
        new URL("./node_modules/lucide/dist/esm/lucide/src/lucide.js", import.meta.url),
      ),
    },
  },
  // `@pierre/diffs` tokenizes a diff off the main thread, and the copied
  // `components/DiffWorkerPoolProvider.tsx` loads that worker with Vite's
  // `?worker` suffix. Rollup refuses the default `iife` worker format once the
  // graph code-splits, which this one does, so the workers are ES modules —
  // which every browser that can run the rest of this bundle supports.
  worker: {
    format: "es",
    rollupOptions: {
      output: {
        hashCharacters: "hex",
        assetFileNames: (asset) => asset.names?.some((name) => name.endsWith(".mjs"))
          ? "assets/[name]-[hash].js" : "assets/[name]-[hash][extname]",
      },
    },
  },
  build: {
    outDir: "../../static/app/conversation",
    emptyOutDir: true,
    assetsInlineLimit: (path) => path.endsWith("vad.worklet.bundle.min.js") ? false : undefined,
    chunkSizeWarningLimit: 700,
    // `app.js` and `app.css` keep fixed names: the shell references them
    // literally and the hub revalidates them on every load. Everything under
    // `chunks/` and `assets/` is named by its content hash, which is what lets
    // `internal/staticasset` serve those files as immutable.
    cssCodeSplit: false,
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
        hashCharacters: "hex",
        chunkFileNames: "chunks/[name]-[hash].js",
        assetFileNames: (asset) => asset.names?.some((name) => name.endsWith(".mjs"))
          ? "assets/[name]-[hash].js" :
          asset.names?.some((name) => name === "silero_vad_v5.onnx" || name === "vad.worklet.bundle.min.js")
            ? "voice/[name][extname]" :
          asset.names?.some((name) => name.endsWith(".css")) ? "app.css" : "assets/[name]-[hash][extname]",
      },
    },
  },
  server: {
    proxy: {
      // `ws` carries the workspace relay socket under `/api`.
      "/api": { target: hubUrl, changeOrigin: true, ws: true },
      "/app/updates": { target: hubUrl, changeOrigin: true },
      // The project event stream; `/projects/…` is otherwise an app route.
      "^/projects/[^/]+/events": { target: hubUrl, changeOrigin: true },
      // `/app/bootstrap` is what the client asks for first (decisions.md §12);
      // `/chat/bootstrap` is the alias it falls back to.
      "/app/bootstrap": { target: hubUrl, changeOrigin: true },
      "/chat/bootstrap": { target: hubUrl, changeOrigin: true },
      "/__mock": { target: hubUrl, changeOrigin: true },
    },
  },
  test: {
    environmentMatchGlobs: [["tests/components/integrations.test.tsx", "jsdom"], ["tests/components/reports.test.tsx", "jsdom"], ["tests/components/activityPage.test.tsx", "jsdom"], ["tests/components/diagnosticsLive.test.tsx", "jsdom"], ["tests/components/issueAsk.test.tsx", "jsdom"], ["tests/components/sourceRecovery.test.tsx", "jsdom"], ["tests/components/reporter.test.tsx", "jsdom"]],
    include: ["tests/**/*.test.ts", "tests/**/*.test.tsx", "src/**/*.test.ts"],
    setupFiles: ["./tests/setup.tsx"],
    css: false,
    pool: "forks",
    fileParallelism: false,
    maxWorkers: 1,
    minWorkers: 1,
    poolOptions: { forks: { execArgv: ["--no-experimental-webstorage"] } },
    testTimeout: 20_000,
  },
});
