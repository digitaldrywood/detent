const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawn } = require("node:child_process");

function detentBinary() {
  return process.env.DETENT_BINARY || path.join(process.cwd(), "tmp", "detent");
}

async function startDetentRuntime(name, args, options = {}) {
  const binary = detentBinary();
  if (!options.holdStartup && !fs.existsSync(binary)) {
    throw new Error(`Detent binary not found at ${binary}. Run make build first.`);
  }

  const host = options.host || "127.0.0.1";
  const port = options.port ?? 0;
  const home = options.home || fs.mkdtempSync(path.join(os.tmpdir(), `detent-${name}-`));
  const evidenceDir = path.join(process.cwd(), "tmp", "playwright-evidence", `${name}-worker-${process.env.TEST_WORKER_INDEX ?? 0}`);
  fs.mkdirSync(evidenceDir, { recursive: true });
  const logPath = path.join(evidenceDir, "runtime.log");
  fs.writeFileSync(logPath, "");
  const previewBinary = process.env.DETENT_STARTUP_PREVIEW_BINARY;
  const command = options.holdStartup ? previewBinary || "go" : binary;
  const commandArgs = options.holdStartup
    ? previewBinary
      ? ["-test.run=^TestBoardSnapshotBrowserPreview$", "-test.v", "-test.timeout=2m"]
      : ["test", "-p", "4", "./internal/cli", "-run", "^TestBoardSnapshotBrowserPreview$", "-count=1", "-v", "-timeout=2m"]
    : ["dev-runtime", "--home", home, "--host", host, "--port", String(port), ...args];
  const child = spawn(command, commandArgs, {
    cwd: process.cwd(),
    env: { ...process.env, DETENT_API_TOKEN: "", TMUX: "", TMUX_PANE: "", ...options.env, NO_COLOR: "1" },
    stdio: [options.holdStartup ? "pipe" : "ignore", "pipe", "pipe"],
  });

  let output = "";
  const url = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => {
      reject(new Error(`Timed out waiting for ${name} runtime URL.\n${output}`));
    }, options.holdStartup && !previewBinary ? 180_000 : 30_000);

    function handleData(chunk) {
      const text = chunk.toString();
      output += text;
      fs.appendFileSync(logPath, text);
      const match = output.match(/Dashboard:\s+(http:\/\/[^\s]+)/);
      if (match && (!options.holdStartup || output.includes("Startup hydration held"))) {
        clearTimeout(timeout);
        resolve(match[1]);
      }
    }

    child.stdout.on("data", handleData);
    child.stderr.on("data", handleData);
    child.once("error", (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    child.once("exit", (code, signal) => {
      clearTimeout(timeout);
      reject(new Error(`${name} runtime exited before startup: code=${code} signal=${signal}\n${output}`));
    });
  });

  return {
    url,
    home,
    logPath,
    output() {
      return output;
    },
    releaseStartup() {
      child.stdin.write("hydrate\n");
    },
    async stop() {
      if (child.exitCode !== null) {
        return;
      }
      if (options.holdStartup) {
        child.stdin.end();
      } else {
        child.kill("SIGTERM");
      }
      await new Promise((resolve) => {
        const timeout = setTimeout(() => {
          if (child.exitCode === null) {
            child.kill("SIGKILL");
          }
          resolve();
        }, options.holdStartup ? 15_000 : 5_000);
        child.once("exit", () => {
          clearTimeout(timeout);
          resolve();
        });
      });
      if (options.holdStartup && child.exitCode !== 0) {
        throw new Error(`${name} preview failed: code=${child.exitCode} signal=${child.signalCode}\n${output}`);
      }
    },
  };
}

module.exports = { startDetentRuntime };
