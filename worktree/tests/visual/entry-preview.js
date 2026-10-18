// Real shared-entry client preview with fixture providers on an ephemeral port.
const fs = require("node:fs");
const path = require("node:path");
const { spawn } = require("node:child_process");

async function startEntryPreview(options = {}) {
  const scratch = process.env.TMPDIR || process.env.TMP || process.env.TEMP;
  if (!scratch) throw new Error("The shared-entry preview requires a scratch directory");
  const directory = fs.mkdtempSync(path.join(scratch, "entry-login-"));
  const binary = process.env.DETENT_ENTRY_PREVIEW_BINARY;
  const child = spawn(binary || "go", binary
    ? ["-test.run=^TestSharedOriginClientPreview$", "-test.v", "-test.timeout=25m"]
    : ["test", "-p", "4", "./internal/cloudentry", "-run", "^TestSharedOriginClientPreview$", "-count=1", "-v", "-timeout", "25m"], {
    env: { ...process.env, DETENT_SHARED_ORIGIN_CLIENT_PREVIEW: directory,
      DETENT_ENTRY_FALLBACK_PREVIEW: options.fallback ? "1" : "" },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let output = "";
  const exited = new Promise((resolve) => child.once("close", resolve));
  const stop = async (fixture) => {
    if (!child.pid || child.exitCode !== null || child.signalCode !== null) return;
    const timeout = setTimeout(() => child.kill("SIGKILL"), 15_000);
    try {
      if (fixture) await fetch(fixture.stop, { method: "POST", signal: AbortSignal.timeout(5_000) }).catch(() => {});
      else child.kill("SIGTERM");
      await exited;
    } finally { clearTimeout(timeout); }
  };
  try {
    await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error(`Shared-entry preview timed out:\n${output}`)), 180_000);
      const finish = (error) => { clearTimeout(timeout); error ? reject(error) : resolve(); };
      const collect = (chunk) => {
        output += chunk.toString();
        fs.appendFileSync(path.join(directory, "preview.log"), chunk);
        if (output.includes("shared-origin client preview:")) finish();
      };
      child.stdout.on("data", collect);
      child.stderr.on("data", collect);
      child.once("error", finish);
      child.once("exit", (code) => finish(new Error(`Shared-entry preview exited ${code}:\n${output}`)));
    });
    const fixture = JSON.parse(fs.readFileSync(path.join(directory, "shared-origin-client-preview.json"), "utf8"));
    return { fixture, stop: () => stop(fixture) };
  } catch (error) { await stop(); throw error; }
}

module.exports = { startEntryPreview };
