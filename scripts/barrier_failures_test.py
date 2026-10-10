import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import barrier_failures


class GoFailuresTest(unittest.TestCase):
    def test_extracts(self):
        cases = [
            ("top-level failures map to their package", [
                "--- FAIL: TestA (0.1s)", "    --- FAIL: TestA/sub (0.0s)", "--- FAIL: TestB (0.1s)",
                "FAIL", "FAIL\tgithub.com/x/pkg\t1.2s", "ok  \tgithub.com/x/other\t0.3s",
            ], ["github.com/x/pkg:TestA|TestB"]),
            ("a timeout reruns the whole package", [
                "panic: test timed out after 10m0s", "\trunning tests:", "\t\tTestSlow (9m)",
                "FAIL\tgithub.com/x/slow\t600.1s",
            ], ["github.com/x/slow"]),
            ("json events from test gates", [
                '{"Action":"fail","Package":"github.com/x/ws","Test":"TestJ"}',
                '{"Action":"fail","Package":"github.com/x/ws","Test":"TestJ/case"}',
                '{"Action":"fail","Package":"github.com/x/ws"}',
            ], ["github.com/x/ws:TestJ"]),
            ("passing output has no failures", ["ok  \tgithub.com/x/pkg\t1.0s"], []),
        ]
        for name, lines, want in cases:
            with self.subTest(name):
                self.assertEqual(barrier_failures.go_failures(lines), want)


class BrowserFailuresTest(unittest.TestCase):
    def test_extracts(self):
        lines = [
            "  ✓ 1 [chromium] › tests/visual/a.spec.js:3:1 › passes",
            "  3 failed",
            "    [chromium] › tests/visual/board.spec.js:7:1 › boot renders",
            "    [mobile-chromium] › tests/visual/board.spec.js:7:1 › boot renders",
            "    [chromium] › tests/visual/chat.spec.js:619:1 › lists the chat",
            "  44 did not run",
            "    [chromium] › tests/visual/ignored.spec.js:1:1 › not a failure",
        ]
        self.assertEqual(barrier_failures.browser_failures(lines), ["tests/visual/board.spec.js", "tests/visual/chat.spec.js"])
        self.assertEqual(barrier_failures.browser_failures(["  472 passed (3.8m)"]), [])


class SelectTest(unittest.TestCase):
    def test_selects_scope(self):
        lines = ["noise", "detent-barrier-failed: go a b:TestX", "detent-barrier-failed: browser tests/x.spec.js:3"]
        self.assertEqual(barrier_failures.select(lines, "go"), ["a", "b:TestX"])
        self.assertEqual(barrier_failures.select(lines, "browser"), ["tests/x.spec.js:3"])
        self.assertEqual(barrier_failures.select(["noise"], "go"), [])


if __name__ == "__main__":
    unittest.main()
