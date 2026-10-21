// Isolated UI fixture: real React screens, existing mock API, ephemeral ports.
import { mkdtempSync, rmSync } from "node:fs";
import { createRequire } from "node:module";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { startMockHub } from "../../web/conversation/dev/mock-hub.ts";

async function main() {
  const root = path.resolve("web/conversation");
  const require = createRequire(path.join(root, "package.json"));
  const { createServer } = await import(pathToFileURL(require.resolve("vite")).href);
  const hub = await startMockHub({ port: 0, coordinator: "hub", account: "write", organization: "seeded" });
  process.env.DETENT_HUB_URL = hub.url;
  const cacheDir = mkdtempSync(path.join(os.tmpdir(), "detent-onboarding-vite-"));
  const vite = await createServer({ root, cacheDir, configFile: path.join(root, "vite.config.ts"), server: { host: "127.0.0.1", port: 0 } });
  await vite.listen();
  console.log(`Onboarding fixture: ${vite.resolvedUrls.local[0]}`);
  process.stdin.resume();
  process.stdin.on("end", async () => {
    await vite.close();
    await hub.close();
    rmSync(cacheDir, { recursive: true, force: true });
  });
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
