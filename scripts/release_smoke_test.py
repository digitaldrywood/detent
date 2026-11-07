import json
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parent.parent


class ReleaseSmokeTest(unittest.TestCase):
    def test_browser_samples_redact_credentials_before_truncation(self):
        result = subprocess.run(
            ["node", "--experimental-vm-modules", "--input-type=module"],
            cwd=ROOT, capture_output=True, text=True, timeout=30, input=r"""
import fs from 'node:fs';
import vm from 'node:vm';
const secrets = ['FAKE_BEARER_CREDENTIAL_1234567890', 'FAKE_RUNNER_CREDENTIAL_1234567890'];
const results = [];
for (const secret of secrets) {
  const listeners = new Map();
  const body = 'Unavailable ' + 'x'.repeat(178) + secret;
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
  } });
  const source = fs.readFileSync('scripts/release-smoke.mjs', 'utf8');
  const smoke = new vm.SourceTextModule(source + '\nexport { browserChecks, rows };', {
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
  results.push(smoke.namespace.rows.filter(row => row.name.startsWith('browser board load ') || row.name.endsWith('detail page')));
}
console.log(JSON.stringify(results));
""",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        for credential_rows in json.loads(result.stdout):
            self.assertEqual(len(credential_rows), 5)
            for row in credential_rows:
                with self.subTest(check=row["name"]):
                    self.assertNotIn("FAKE_", row["error"])
                    self.assertIn("[REDACTED]", row["error"])
                    for sample in row["error"].split("; "):
                        self.assertLessEqual(len(sample), 210)


if __name__ == "__main__":
    unittest.main()
