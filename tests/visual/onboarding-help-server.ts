// Isolated UI fixture: real React screens, existing mock API, ephemeral ports.
import { readFileSync } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { createRequire } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { startMockHub } from "../../web/conversation/dev/mock-hub.ts";

async function main() {
  const root = path.resolve("web/conversation");
  const require = createRequire(path.join(root, "package.json"));
  const { preview } = await import(pathToFileURL(require.resolve("vite")).href);
  const indexHtml = readFileSync("static/app/conversation/index.html", "utf8");
  const hub = await startMockHub({ port: 0, coordinator: "hub", account: "write", organization: "seeded" });
  process.env.DETENT_HUB_URL = hub.url;
  const vite = await preview({
    root,
    configFile: path.join(root, "vite.config.ts"),
    base: "/",
    build: { outDir: path.resolve("static") },
    preview: { port: 0, host: "127.0.0.1" },
    plugins: [{
      name: "onboarding-fixture-shell",
      configurePreviewServer(server) {
        server.middlewares.use((request: IncomingMessage, response: ServerResponse, next: () => void) => {
          const url = new URL(request.url ?? "/", "http://fixture.invalid");
          if (url.pathname.startsWith("/static/")) {
            request.url = request.url?.slice("/static".length);
          } else if (request.headers.accept?.includes("text/html")) {
            const theme = url.searchParams.get("theme") === "light" ? "light" : "dark";
            response.setHeader("Content-Type", "text/html; charset=utf-8");
            response.end(indexHtml.replace('class="dark"', `class="${theme === "dark" ? "dark" : ""}" data-theme="${theme}"`));
            return;
          }
          next();
        });
      },
    }],
  });
  const address = vite.httpServer.address();
  if (address === null || typeof address === "string") throw new Error("Onboarding fixture has no TCP address");
  console.log(`Onboarding fixture: http://127.0.0.1:${address.port}/`);
  process.stdin.resume();
  process.stdin.on("end", async () => {
    await vite.close();
    await hub.close();
  });
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
