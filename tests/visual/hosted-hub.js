// Starts a real hosted hub for the browser, by running the Go preview test.
//
// `TestHostedBrowserPreview` in `internal/hubserver/hosted_browser_test.go`
// opens a hosted hub with the conversation product enabled and a fake WorkOS
// provider on an ephemeral port, writes a JSON fixture describing it, and then
// holds the process open for five minutes or until `POST <stop>`. That test is
// the only thing in this repository that serves the real `/chat` surface with
// real sessions, real authorization and real durable history, so the
// conversation spec drives it rather than a mock.
const fs = require("node:fs");
const path = require("node:path");
const { spawn } = require("node:child_process");

const FIXTURE_LINE = /Hosted browser fixture:\s+(\S+)/;
const STARTUP_TIMEOUT_MS = 180_000;
const STOP_REQUEST_TIMEOUT_MS = 5_000;
const EXIT_TIMEOUT_MS = 15_000;

function hasExited(child) {
  return child.exitCode !== null || child.signalCode !== null;
}

function waitForExit(child, timeoutMs) {
  return new Promise((resolve) => {
    if (hasExited(child)) {
      resolve();
      return;
    }
    const timeout = setTimeout(() => {
      if (!hasExited(child)) child.kill("SIGKILL");
      resolve();
    }, timeoutMs);
    child.once("exit", () => {
      clearTimeout(timeout);
      resolve();
    });
  });
}

async function terminate(child) {
  if (hasExited(child)) return;
  const exited = waitForExit(child, EXIT_TIMEOUT_MS);
  child.kill("SIGTERM");
  await exited;
}

/**
 * Runs the Go preview test and resolves once its fixture file is readable.
 *
 * @returns {Promise<{fixture: object, output: () => string, stop: () => Promise<void>}>}
 */
async function startHostedHub(name = "conversation", options = {}) {
  const scratch = process.env.TMPDIR || process.env.TMP || process.env.TEMP;
  if (!scratch) throw new Error("The hosted preview requires a scratch directory");
  const evidenceDir = path.join(scratch, "playwright-evidence", `${name}-worker-${process.env.TEST_WORKER_INDEX ?? 0}`);
  fs.mkdirSync(evidenceDir, { recursive: true });
  const logPath = path.join(evidenceDir, "hosted-hub.log");
  fs.writeFileSync(logPath, "");

  const child = spawn(
    process.env.DETENT_HOSTED_PREVIEW_BINARY || "go",
    process.env.DETENT_HOSTED_PREVIEW_BINARY ? ["-test.run=^TestHostedBrowserPreview$", "-test.v", "-test.timeout=25m"] : [
      "test",
      "-p",
      "4",
      "./internal/hubserver",
      "-run",
      "^TestHostedBrowserPreview$",
      "-count=1",
      "-v",
      "-timeout",
      "25m",
    ],
    {
      cwd: process.cwd(),
      env: { ...process.env, DETENT_HOSTED_BROWSER_PREVIEW: "1", NO_COLOR: "1", ...options.env },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );

  let output = "";
  const ready = new Promise((resolve, reject) => {
    const timeout = setTimeout(() => {
      reject(new Error(`Timed out waiting for the hosted hub fixture.\n${output}`));
    }, STARTUP_TIMEOUT_MS);

    function handleData(chunk) {
      const text = chunk.toString();
      output += text;
      fs.appendFileSync(logPath, text);
      const match = output.match(FIXTURE_LINE);
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
      reject(
        new Error(
          `The hosted hub preview exited before it was ready: code=${code} signal=${signal}\n${output}`,
        ),
      );
    });
  });
  let fixturePath;
  try {
    fixturePath = await ready;
  } catch (error) {
    await terminate(child);
    throw error;
  }

  const fixture = JSON.parse(fs.readFileSync(fixturePath, "utf8"));
  for (const key of [
    "url",
    "chat",
    "stop",
    "conversation",
    "work_item",
    "project_id",
    "owner_email",
  ]) {
    if (typeof fixture[key] !== "string" || fixture[key].length === 0) {
      throw new Error(`The hosted hub fixture is missing ${key}: ${JSON.stringify(fixture)}`);
    }
  }
  // The spec signs in as more than one account, so the accounts it names have
  // to be there: a missing one would otherwise read as a navigation failure.
  for (const account of ["owner", "viewer"]) {
    if (typeof fixture.accounts?.[account] !== "string") {
      throw new Error(
        `The hosted hub fixture has no ${account} account: ${JSON.stringify(fixture.accounts)}`,
      );
    }
  }

  let stopped = false;
  return {
    fixture,
    logPath,
    output() {
      return output;
    },
    async stop() {
      if (stopped) return;
      stopped = true;
      // The preview test returns from its own handler, so the Go process ends
      // on its own and the temporary database is cleaned up by `t.Cleanup`.
      const exited = waitForExit(child, EXIT_TIMEOUT_MS);
      await fetch(fixture.stop, {
        method: "POST",
        signal: AbortSignal.timeout(STOP_REQUEST_TIMEOUT_MS),
      }).catch(() => {});
      await exited;
    },
  };
}

module.exports = { startHostedHub, STARTUP_TIMEOUT_MS };
