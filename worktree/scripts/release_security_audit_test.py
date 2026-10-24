import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class ReleaseSecurityAuditTest(unittest.TestCase):
    def test_controller_results(self):
        script = Path(__file__).with_name("release-security-audit.sh")
        for name, status, reason, infrastructure, exit_code, succeeds in (
            ("clean", "clean", "", [], 0, True),
            ("confirmed", "findings", "", [], 0, True),
            ("needs validation", "needs_validation", "", [], 0, True),
            ("allowance exhausted", "incomplete", "allowance_exhausted", [], 1, True),
            ("infrastructure", "failed", "infrastructure_failure", ["provider unavailable"], 1, False),
            ("publication failed", "failed", "infrastructure_failure", ["publication unavailable"], 1, False),
            ("incomplete with infrastructure", "incomplete", "allowance_exhausted", ["report failed"], 1, False),
            ("startup failed", None, "", [], 1, False),
            ("wrong commit", "incomplete", "allowance_exhausted", [], 1, False),
        ):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                commit = "a" * 40
                snapshot = {
                    "identity": {"commit": "b" * 40 if name == "wrong commit" else commit},
                    "status": status,
                    "reason": reason,
                    "infrastructure": infrastructure,
                    "findings": [{"verdict": "confirmed"}] if name == "confirmed" else [],
                }
                controller = root / "controller"
                controller.write_text(
                    "#!/usr/bin/env python3\n"
                    "import json, os, sys\n"
                    "assert os.environ['DETENT_RELEASE_AUDIT'] == 'true'\n"
                    "request = json.load(sys.stdin)\n"
                    "assert request['identity']['project_id'] == 'prj_example'\n"
                    "assert request['identity']['repository'] == 'example-org/example-project'\n"
                    "assert request['identity']['commit'] == 'a' * 40\n"
                    "assert request['identity']['release_series'] == 'release/v1.0'\n"
                    "assert request['identity']['run_id'] == '123'\n"
                    "assert request['identity']['attempt'] == '2'\n"
                    "assert request['identity']['job_id'] == 'release-security-audit'\n"
                    "assert request['identity']['run_url'] == 'https://example.test/example-org/example-project/actions/runs/123/attempts/2'\n"
                    "assert request['repository_directory'] == os.path.join(os.environ['TMPDIR'], 'source')\n"
                    "assert request['model'] == 'project-selected-model'\n"
                    "assert request['allowance'] == {'invocations': 40, 'usage': {'input_tokens': 200000, 'output_tokens': 50000, 'cached_input_tokens': 150000}}\n"
                    "print('Target execution sandbox is unavailable; execution-dependent leads remain needs_validation.')\n"
                    + (f"print({json.dumps(snapshot)!r})\n" if status is not None else "")
                    + f"sys.exit({exit_code})\n"
                )
                controller.chmod(0o755)
                env = {
                    "PATH": os.environ["PATH"],
                    "TMPDIR": directory,
                    "DETENT_PROJECT_ID": "prj_example",
                    "GITHUB_REPOSITORY": "example-org/example-project",
                    "CI_DEVELOP_SHA": commit,
                    "CI_RELEASE_BRANCH": "release/v1.0",
                    "GITHUB_RUN_ID": "123",
                    "GITHUB_RUN_ATTEMPT": "2",
                    "GITHUB_SERVER_URL": "https://example.test",
                    "DETENT_RELEASE_AUDIT_MODEL": "project-selected-model",
                }
                result = subprocess.run(
                    ["bash", str(script), str(controller), str(root / "source")],
                    env=env, capture_output=True, text=True, check=False,
                )
                self.assertEqual(result.returncode == 0, succeeds, result.stdout + result.stderr)
                self.assertTrue((root / "release-security-audit-request.json").exists())
                self.assertTrue((root / "release-security-audit-result.jsonl").exists())
                self.assertTrue((root / "release-security-audit-diagnostic.log").exists())


if __name__ == "__main__":
    unittest.main()
