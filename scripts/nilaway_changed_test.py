import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "nilaway_changed", Path(__file__).with_name("nilaway-changed.py")
)
audit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit)
VERSION = re.search(
    r"^NILAWAY_VERSION \?= (\S+)$", (audit.ROOT / "Makefile").read_text(), re.MULTILINE
).group(1)


class ReverseDependentAuditTests(unittest.TestCase):
    def test_provider_change_and_reviewed_importer_baseline(self):
        scratch = next((os.environ[key] for key in ("TMPDIR", "TMP", "TEMP") if os.environ.get(key)), None)
        if scratch is None:
            self.fail("Set TMPDIR, TMP, or TEMP for the NilAway fixture")
        with tempfile.TemporaryDirectory(dir=scratch) as directory:
            root = Path(directory)
            (root / "go.mod").write_text("module example.test/nilaway\n\ngo 1.26\n")
            (root / "provider").mkdir()
            provider = root / "provider" / "provider.go"
            provider.write_text("package provider\n\nfunc Value() *int { return new(int) }\n")
            (root / "importer").mkdir()
            importer = root / "importer" / "importer.go"
            importer.write_text(
                'package importer\n\nimport "example.test/nilaway/provider"\n\n'
                "func Read() int { return *provider.Value() }\n"
            )
            baseline = root / "baseline.json"
            baseline.write_text("[]")

            def git(*args):
                return subprocess.check_output(
                    ["git", "-c", "user.name=NilAway Fixture", "-c", "user.email=nilaway@example.test",
                     "-c", "commit.gpgsign=false", *args], cwd=root, text=True, stderr=subprocess.STDOUT
                ).strip()

            git("init")
            git("add", ".")
            git("commit", "-m", "safe provider")
            git("tag", "v0.0.0")

            def run_audit():
                output = io.StringIO()
                with mock.patch.object(audit, "ROOT", root), mock.patch.object(audit, "BASELINE", baseline), \
                     mock.patch("sys.argv", ["nilaway-changed.py", VERSION, "example.test/nilaway"]), \
                     contextlib.redirect_stdout(output):
                    code = audit.main()
                return code, output.getvalue()

            code, output = run_audit()
            self.assertEqual(code, 0, output)
            provider.write_text("package provider\n\nfunc Value() *int { return nil }\n")
            git("add", "provider/provider.go")
            git("commit", "-m", "provider becomes nullable")
            self.assertEqual(git("diff", "--name-only", "v0.0.0", "HEAD"), "provider/provider.go")

            code, output = run_audit()
            self.assertEqual(code, 1, output)
            diagnostic = next(
                (match for line in output.splitlines() if (match := audit.DIAGNOSTIC.search(line))), None
            )
            self.assertIsNotNone(diagnostic, output)
            self.assertTrue(diagnostic["path"].endswith("importer/importer.go"), output)
            line = int(diagnostic["line"])
            baseline.write_text(json.dumps([{
                "file": "importer/importer.go", "line": line, "column": int(diagnostic["column"]),
                "rule": "Potential nil panic detected",
                "source_sha256": hashlib.sha256(importer.read_text().splitlines()[line - 1].encode()).hexdigest(),
            }]))
            code, output = run_audit()
            self.assertEqual(code, 0, output)
            self.assertIn("1 reviewed legacy diagnostics", output)

            reviewed = f'{importer}:{line}:{diagnostic["column"]}: Potential nil panic detected\n'
            for returncode, failure in ((1, "nilaway: signal: killed\n"), (-9, ""), (2, "")):
                with self.subTest(returncode=returncode, failure=failure), mock.patch.object(
                    audit.subprocess, "run", side_effect=[
                        subprocess.CompletedProcess([], 0, ""),
                        subprocess.CompletedProcess([], returncode, reviewed + failure),
                    ],
                ):
                    code, output = run_audit()
                    self.assertEqual(code, 1, output)
                    self.assertNotIn("Audited all Go packages", output)

            with mock.patch.object(
                audit.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "tool install failed"),
            ) as install:
                code, output = run_audit()
                self.assertEqual(code, 1, output)
                self.assertIn("tool install failed", output)
                self.assertEqual(install.call_count, 1)

            importer.write_text(importer.read_text().replace("return *provider.Value()", "return *provider.Value() /* changed */"))
            code, output = run_audit()
            self.assertEqual(code, 1, output)
            self.assertIn("Unexpected NilAway diagnostics", output)


if __name__ == "__main__":
    unittest.main()
