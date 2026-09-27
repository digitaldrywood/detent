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

/**
 * Runs the Go preview test and resolves once its fixture file is readable.
 *
 * @returns {Promise<{fixture: object, output: () => string, stop: () => Promise<void>}>}
 */
async function startHostedHub(name = "conversation") {
  const evidenceDir = path.join(process.cwd(), "tmp", "playwright-evidence", name);
  fs.mkdirSync(evidenceDir, { recursive: true });
  const logPath = path.join(evidenceDir, "hosted-hub.log");
  fs.writeFileSync(logPath, "");

  const child = spawn(
    "go",
    [
      "test",
      "./internal/hubserver",
      "-run",
      "TestHostedBrowserPreview",
      "-count=1",
      "-v",
      "-timeout",
      "10m",
    ],
    {
      cwd: process.cwd(),
      env: { ...process.env, DETENT_HOSTED_BROWSER_PREVIEW: "1", NO_COLOR: "1" },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );

  let output = "";
  const fixturePath = await new Promise((resolve, reject) => {
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

  const fixture = JSON.parse(fs.readFileSync(fixturePath, "utf8"));
  for (const key of [
    "url",
    "chat",
    "stop",
    "conversation",
    "work_item",
    "project_id",
    "owner_email",
    // The pre-warmed workspace, so a spec can queue a project action run
    // through the API alone (decisions.md §18.12).
    "workspace",
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
      await fetch(fixture.stop, { method: "POST" }).catch(() => {});
      await new Promise((resolve) => {
        const timeout = setTimeout(() => {
          if (child.exitCode === null) child.kill("SIGKILL");
          resolve();
        }, 15_000);
        child.once("exit", () => {
          clearTimeout(timeout);
          resolve();
        });
      });
    },
  };
}

module.exports = { startHostedHub };
