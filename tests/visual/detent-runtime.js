const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawn } = require("node:child_process");

function detentBinary() {
  return process.env.DETENT_BINARY || path.join(process.cwd(), "tmp", "detent");
}

async function startDetentRuntime(name, args) {
  const binary = detentBinary();
  if (!fs.existsSync(binary)) {
    throw new Error(`Detent binary not found at ${binary}. Run make build first.`);
  }

  const home = fs.mkdtempSync(path.join(os.tmpdir(), `detent-${name}-`));
  const evidenceDir = path.join(process.cwd(), "tmp", "playwright-evidence", name);
  fs.mkdirSync(evidenceDir, { recursive: true });
  const logPath = path.join(evidenceDir, "runtime.log");
  fs.writeFileSync(logPath, "");
  const child = spawn(binary, ["dev-runtime", "--home", home, "--host", "127.0.0.1", "--port", "0", ...args], {
    cwd: process.cwd(),
    env: { ...process.env, NO_COLOR: "1" },
    stdio: ["ignore", "pipe", "pipe"],
  });

  let output = "";
  const url = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => {
      reject(new Error(`Timed out waiting for ${name} runtime URL.\n${output}`));
    }, 30_000);

    function handleData(chunk) {
      const text = chunk.toString();
      output += text;
      fs.appendFileSync(logPath, text);
      const match = output.match(/Dashboard:\s+(http:\/\/[^\s]+)/);
      if (match) {
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
    async stop() {
      if (child.exitCode !== null) {
        return;
      }
      child.kill("SIGTERM");
      await new Promise((resolve) => {
        const timeout = setTimeout(() => {
          if (child.exitCode === null) {
            child.kill("SIGKILL");
          }
          resolve();
        }, 5_000);
        child.once("exit", () => {
          clearTimeout(timeout);
          resolve();
        });
      });
    },
  };
}

module.exports = { startDetentRuntime };
