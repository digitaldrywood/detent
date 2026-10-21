// The static design-system gallery: `npm run build:gallery` writes it to
// `dist-gallery/` (gitignored). It shares the client's aliases, plugins and
// CSS but has its own HTML entry and output directory, so the production
// bundle in `static/app/conversation` is untouched. Built in the `gallery`
// mode, which switches the gallery to hash routing (see `gallery/frame.tsx`).
import { cp, rename } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { mergeConfig, type Plugin, type UserConfig } from "vite";

import base from "./vite.config.ts";

/**
 * Serves the build as `index.html`, so a static server opens it at its root,
 * and copies the embedded fonts to `fonts/`, where the stylesheet's
 * `../../fonts/…` URLs land when served from the directory root.
 */
function galleryIndex(): Plugin {
  return {
    name: "detent-gallery-index",
    apply: "build",
    async writeBundle(options) {
      if (options.dir === undefined) return;
      await rename(join(options.dir, "gallery.html"), join(options.dir, "index.html"));
      await cp(fileURLToPath(new URL("../../static/fonts", import.meta.url)), join(options.dir, "fonts"), {
        recursive: true,
      });
    },
  };
}

const { build: _build, experimental: _experimental, test: _test, server: _server, ...shared } = base as UserConfig & {
  test?: unknown;
};

export default mergeConfig(shared, {
  base: "./",
  plugins: [galleryIndex()],
  build: {
    outDir: fileURLToPath(new URL("./dist-gallery", import.meta.url)),
    emptyOutDir: true,
    chunkSizeWarningLimit: 4000,
    rollupOptions: {
      input: fileURLToPath(new URL("./gallery.html", import.meta.url)),
      onwarn(warning, warn) {
        if (warning.code === "MODULE_LEVEL_DIRECTIVE") return;
        warn(warning);
      },
    },
  },
} satisfies UserConfig);
