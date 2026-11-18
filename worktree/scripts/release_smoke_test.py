import json
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parent.parent


class ReleaseSmokeTest(unittest.TestCase):
    def test_reports_omit_browser_sessions_and_unknown_response_secrets(self):
        result = subprocess.run(
            ["node", "--experimental-vm-modules", "--input-type=module"],
            cwd=ROOT, capture_output=True, text=True, timeout=30, input=r"""
import fs from 'node:fs';
import vm from 'node:vm';
const secrets = ['FAKE_BEARER_CREDENTIAL_1234567890', 'FAKE_RUNNER_CREDENTIAL_1234567890',
  'FAKE_BROWSER_COOKIE_1234567890', 'FAKE_LOCAL_STORAGE_1234567890', 'FAKE_UNKNOWN_RESPONSE_TOKEN', 'FAKE_SHORT'];
const results = [];
for (const secret of secrets) {
 for (const offset of [0, 190]) {
  const listeners = new Map();
  const body = 'x'.repeat(offset) + secret + ' Unavailable';
  let currentURL;
  const response = (url, status) => ({ url: () => url, status: () => status, text: async () => body });
  const page = {
    on: (event, callback) => listeners.set(event, callback),
    off: event => listeners.delete(event),
    waitForResponse: async () => {},
    waitForFunction: async () => {},
    waitForLoadState: async () => {},
    locator: () => ({ first: () => ({ innerText: async () => currentURL.includes('/work/p/') ? 'Example board' : body }) }),
    goto: async url => {
      currentURL = url;
      listeners.get('request')?.({ url: () => 'https://example.test/board/counts' });
      listeners.get('response')?.(response('https://example.test/api/v2/organizations/example-org/projects/example-project/board/status', 500));
      return response(url, 200);
    },
  };
  const playwright = new vm.SyntheticModule(['chromium'], function () {
    this.setExport('chromium', { launch: async () => ({
      newContext: async () => ({ newPage: async () => page, request: { get: async () => response('', 200) } }),
      close: async () => {},
    }) });
  });
  await playwright.link(() => { throw new Error('unexpected import'); });
  await playwright.evaluate();
  const context = vm.createContext({ URL, process: {
    argv: ['node', 'release-smoke', 'https://example.test', 'example-org', 'example-project', 'TEST_TOKEN', 'invalid argument'],
    env: { TEST_TOKEN: secrets[0], TEST_RUNNER: secrets[1], SMOKE_RUNNER_CREDENTIAL_ENV: 'TEST_RUNNER', SMOKE_BROWSER_STATE: 'fake-state' },
    stdout: { write: () => {} },
  }, fetch: async () => ({ status: 200, text: async () => JSON.stringify({ error: secret }) }), AbortSignal });
  const source = fs.readFileSync('scripts/release-smoke.mjs', 'utf8');
  const smoke = new vm.SourceTextModule(source + '\nexport { browserChecks, rows, request, step, tool, main };', {
    context, importModuleDynamically: async name => {
      if (name !== '@playwright/test') throw new Error('unexpected import');
      return playwright;
    },
  });
  await smoke.link(async name => {
    if (name !== 'node:crypto') throw new Error('unexpected import');
    return new vm.SyntheticModule(['randomUUID'], function () { this.setExport('randomUUID', () => 'example-run'); }, { context });
  });
  await smoke.evaluate();
  await smoke.namespace.browserChecks('example-item', { work_item_id: 'example-item', change_id: 'example-change' });
  await smoke.namespace.request('board counts', '/board/counts', { board: true });
  await smoke.namespace.step('MCP session', async () => { throw new Error(secret); });
  context.fetch = async () => ({ status: 200, text: async () => secret });
  await smoke.namespace.step('board status', () => smoke.namespace.request('board status', '/board/status', { board: true }));
  context.fetch = async () => ({ status: 200, text: async () => JSON.stringify({ result: { structuredContent: { data: { token: secret } } } }) });
  await smoke.namespace.tool('work_item');
  const summaries = [];
  for (const [body, board] of [
    [{ items: [{ data: { terminal_failure: { error: secret }, finalization: { error: secret } } }] }, false],
    [{ result: { structuredContent: { data: { terminal_failure: { error: secret } } } } }, false],
    [{ result: { isError: true, content: [{ type: 'text', text: secret }] } }, false],
    [{ result: { structuredContent: { error: secret } } }, false],
    [{ data: { rows: [{ status: 'projection_pending', token: secret }] } }, true],
    [{ rows: [{ error: secret }], projection_pending: false }, true],
  ]) {
    context.fetch = async () => ({ status: 200, text: async () => JSON.stringify(body) });
    const { row } = await smoke.namespace.request('board counts', '/board/counts', { board });
    summaries.push(JSON.parse(row.body));
  }
  context.process.argv.pop();
  context.fetch = async () => ({ status: 200, text: async () => JSON.stringify({ states: [{ name: secret }] }) });
  await smoke.namespace.main();
  page.goto = async () => { throw new Error(secret); };
  await smoke.namespace.step('browser journeys', () => smoke.namespace.browserChecks('example-item', { work_item_id: 'example-item', change_id: 'example-change' }));
  results.push({ rows: smoke.namespace.rows, summaries });
 }
}
console.log(JSON.stringify(results));
""",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("FAKE_", result.stdout)
        for report in json.loads(result.stdout):
            rows = report["rows"]
            browser_rows = [row for row in rows if row["name"].startswith("browser board load ") or row["name"].endswith("detail page")]
            self.assertEqual(len(browser_rows), 10)
            for row in browser_rows:
                with self.subTest(check=row["name"]):
                    self.assertTrue(row["error"])
                    self.assertLessEqual(len(row["error"]), 240)
            self.assertTrue(next(row for row in rows if row["name"] == "MCP session")["error"])
            self.assertTrue(next(row for row in rows if row["name"] == "board status")["error"])
            self.assertTrue(next(row for row in rows if row["name"] == "browser journeys")["error"])
            self.assertEqual(report["summaries"], [
                {"error": None, "projection_pending": False},
                {"error": None, "projection_pending": False},
                {"error": True, "projection_pending": False},
                {"error": True, "projection_pending": False},
                {"error": None, "projection_pending": True},
                {"error": None, "projection_pending": False},
            ])


if __name__ == "__main__":
    unittest.main()
